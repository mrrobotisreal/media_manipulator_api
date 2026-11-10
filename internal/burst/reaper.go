package burst

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// LaunchTestKind is the Kind burst-ctl launch-test writes; the periodic reaper
// spares a fresh launch-test instance (a separate process owns it).
const LaunchTestKind = "launch-test"

// BuildMaxAge is how long an image-builder instance (tag mm-burst-build) is
// spared by the reaper. Builders shut themselves down after 3 h.
const BuildMaxAge = 4 * time.Hour

// Reaper terminates anything that is not a live run and clears staging
// prefixes with no live run. It runs whenever AWS credentials are configured,
// even with BURST_ENABLED=false, so a flag flip cannot strand instances.
type Reaper struct {
	Cfg      config.BurstConfig
	Provider Provider
	Store    Store       // may be nil
	Prefixes PrefixStore // may be nil
	// Live returns the in-memory runs of THIS process (nil = none).
	Live   func() []Run
	Ledger *Ledger // may be nil
	Log    *slog.Logger
	Now    func() time.Time
}

// Report is one pass's outcome.
type Report struct {
	DryRun          bool
	Terminated      []string // instance ids terminated (or that would be)
	Reasons         map[string]string
	RowsOrphaned    []string // run ids marked orphaned
	PrefixesDeleted []string // run ids whose staging prefix was deleted
	Errors          []string
}

func (r *Reaper) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reaper) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Reaper) maxAge() time.Duration {
	return time.Duration(r.Cfg.MaxInstanceMinutes+15) * time.Minute
}

// Pass runs one reaper pass. boot=true is the first pass of a new process:
// every non-terminal row from a previous process is settled and orphaned, and
// the launch-test grace does not apply. dry=true changes nothing.
func (r *Reaper) Pass(ctx context.Context, boot, dry bool) Report {
	rep := Report{DryRun: dry, Reasons: map[string]string{}}
	now := r.now()
	liveRuns := map[string]Run{}
	liveInst := map[string]bool{}
	if r.Live != nil {
		for _, run := range r.Live() {
			liveRuns[run.ID] = run
			if run.InstanceID != "" {
				liveInst[run.InstanceID] = true
			}
		}
	}
	var rows map[string]Run
	if r.Store != nil {
		list, err := r.Store.ListUnterminated(ctx)
		if err != nil {
			rep.Errors = append(rep.Errors, "list rows: "+err.Error())
		}
		rows = map[string]Run{}
		for _, row := range list {
			rows[row.ID] = row
		}
	}

	// 1. Instances.
	insts, err := r.Provider.ListTagged(ctx)
	if err != nil {
		rep.Errors = append(rep.Errors, "list tagged: "+err.Error())
		r.log().Error("burst: reaper could not list instances", "error", err.Error())
	}
	instByRun := map[string]Instance{}
	var kill []string
	for _, in := range insts {
		_, runLive := liveRuns[in.RunID]
		isLive := liveInst[in.ID] || runLive
		reason := ""
		if in.Build != "" {
			// MM126 image builder: not a run. Spared until it outlives its own
			// 3 h self-shutdown by a wide margin.
			if in.LaunchTime.IsZero() || now.Sub(in.LaunchTime) <= BuildMaxAge {
				continue
			}
			isLive = true
		}
		switch {
		case !isLive:
			reason = "orphan: no live run"
			if row, ok := rows[in.RunID]; ok && !boot && row.Kind == LaunchTestKind && now.Sub(row.OfferedAt) < 15*time.Minute {
				reason = "" // a launch-test from another process, still in its window
			}
		case !in.LaunchTime.IsZero() && now.Sub(in.LaunchTime) > r.maxAge():
			reason = "older than max lifetime"
		}
		if reason == "" {
			continue
		}
		rep.Terminated = append(rep.Terminated, in.ID)
		rep.Reasons[in.ID] = reason
		instByRun[in.RunID] = in
		kill = append(kill, in.ID)
	}
	if len(kill) > 0 {
		r.log().Warn("burst: reaper terminating instances", "dryRun", dry, "ids", kill)
		if !dry {
			if err := r.Provider.Terminate(ctx, kill); err != nil {
				rep.Errors = append(rep.Errors, "terminate: "+err.Error())
				r.log().Error("burst: reaper terminate failed", "error", err.Error())
			}
		}
	}

	// 2. Rows: previous-process rows at boot; rows whose instance was just reaped.
	killed := map[string]bool{}
	for _, id := range kill {
		killed[id] = true
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		row := rows[id]
		if _, live := liveRuns[id]; live {
			continue
		}
		inst, reaped := instByRun[id]
		if !boot && !reaped && !(row.InstanceID != "" && killed[row.InstanceID]) {
			continue
		}
		rep.RowsOrphaned = append(rep.RowsOrphaned, id)
		if dry {
			continue
		}
		row.State = StateOrphaned
		end := now
		row.TerminatedAt = &end
		// Best known cost: launched_at (or the instance's LaunchTime) -> now.
		var from *time.Time
		if row.LaunchedAt != nil {
			from = row.LaunchedAt
		} else if !inst.LaunchTime.IsZero() {
			t := inst.LaunchTime
			from = &t
		}
		typ := row.InstanceType
		if typ == "" {
			typ = inst.Type
		}
		if from != nil && (row.InstanceID != "" || inst.ID != "") {
			ebs := 0
			if pr, ok := Lookup(row.Kind); ok {
				ebs = pr.RootGiB
			}
			row.CostMicros = CostMicros(RatesFor(r.Cfg, typ), *from, end, ebs, row.BytesOut, false)
		}
		if r.Ledger != nil {
			r.Ledger.Settle(row.ID, row.CostMicros)
		}
		if err := r.Store.Update(ctx, &row); err != nil {
			rep.Errors = append(rep.Errors, "update row: "+err.Error())
		}
	}

	// 3. Staging prefixes with no live run (terminal or unknown).
	if r.Prefixes != nil {
		pre, err := r.Prefixes.ListRunIDs(ctx)
		if err != nil {
			rep.Errors = append(rep.Errors, "list prefixes: "+err.Error())
		}
		for _, id := range pre {
			if _, live := liveRuns[id]; live {
				continue
			}
			if row, ok := rows[id]; ok && !boot && !killed[row.InstanceID] && row.Kind == LaunchTestKind {
				continue
			}
			rep.PrefixesDeleted = append(rep.PrefixesDeleted, id)
			if dry {
				continue
			}
			if err := r.Prefixes.DeleteRun(ctx, id); err != nil {
				rep.Errors = append(rep.Errors, "delete prefix: "+err.Error())
			}
		}
	}
	return rep
}

// Run loops: once at boot, then every BURST_REAPER_INTERVAL_SECONDS.
func (r *Reaper) Run(ctx context.Context) {
	interval := time.Duration(r.Cfg.ReaperIntervalSecs) * time.Second
	if interval <= 0 {
		interval = time.Minute
	}
	r.log().Info("burst: reaper started", "interval", interval.String(), "burstEnabled", r.Cfg.Enabled)
	r.Pass(ctx, true, false)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Pass(ctx, false, false)
		}
	}
}
