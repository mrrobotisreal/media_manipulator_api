package burst

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// Offer is what the scheduler hook (MM127) hands the planner.
type Offer struct {
	JobID           string
	Kind            string // scheduler Kind, resolved through the profile table
	Tier            string
	Priority        int
	PredictedWaitMs int64
	// InstanceTypeOverride is used by burst-ctl launch-test only.
	InstanceTypeOverride string
	// Lab marks a job created by a Lab route (MM138). LaunchDirect refuses a
	// LabOnly profile without it, and a product profile with it.
	Lab bool
}

// Errors the caller (MM127) branches on.
var (
	ErrNotBurstable = errors.New("burst: kind is not burstable")
	ErrDisabled     = errors.New("burst: workload not enabled")
	ErrMaxInstances = errors.New("burst: max live instances reached")
	ErrStaleImage   = errors.New("burst: worker image is stale vs host (BURST_REFUSE_STALE_IMAGES)")
	// ErrLabOnly: a LabOnly profile was offered for a job without the lab flag
	// (or the reverse). The licence isolation of ADR-0021 lives here too.
	ErrLabOnly = errors.New("burst: lab profile refused for a non-lab job")
)

// UserDataFunc builds the instance user-data (MM125's template). The Tailscale
// key it receives is a secret and goes nowhere but into the returned string.
type UserDataFunc func(run Run, profile Profile, tailscaleKey string) (string, error)

// Planner does launch/terminate bookkeeping for burst runs. It has no
// scheduler integration (MM127 adds the hook).
type Planner struct {
	Cfg      config.BurstConfig
	Provider Provider
	Store    Store
	Ledger   *Ledger
	Minter   KeyMinter
	Prefixes PrefixStore // may be nil
	UserData UserDataFunc
	// Stale reports whether a family's image differs from the host runtime
	// (MM126 drift check). Nil = never stale. Only consulted when
	// Cfg.RefuseStaleImages is set; otherwise stale images still run.
	Stale func(family string) bool
	// LabLedger enforces the Lab sub-caps (LAB_DAILY/MONTHLY_SPEND_CAP_USD) on
	// runs whose Offer.Lab is set, on top of Ledger. nil = no sub-cap.
	LabLedger *Ledger
	Log       *slog.Logger
	Now       func() time.Time

	mu   sync.Mutex
	live map[string]*Run
	// extras are the additional reservations a Lab session took for reused
	// runs, keyed by run id; Terminate releases them.
	extras map[string][]string
}

func (p *Planner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Planner) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Planner) save(ctx context.Context, r *Run, insert bool) {
	if p.Store == nil {
		return
	}
	var err error
	if insert {
		err = p.Store.Insert(ctx, r)
	} else {
		err = p.Store.Update(ctx, r)
	}
	if err != nil {
		p.log().Error("burst: ledger write failed", "run", r.ID, "state", string(r.State), "error", err.Error())
	}
}

func (p *Planner) track(r *Run) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live == nil {
		p.live = map[string]*Run{}
	}
	p.live[r.ID] = r
}

func (p *Planner) untrack(id string) {
	p.mu.Lock()
	delete(p.live, id)
	p.mu.Unlock()
}

// Live returns a snapshot of the in-memory runs, oldest first.
func (p *Planner) Live() []Run {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Run, 0, len(p.live))
	for _, r := range p.live {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OfferedAt.Before(out[j].OfferedAt) })
	return out
}

func (p *Planner) rates(instanceType string) Rates { return RatesFor(p.Cfg, instanceType) }

// Launch reserves spend, mints a key, builds user-data, launches and records
// the row. On any failure the row ends terminal (cap_refused / launch_failed)
// and the reservation is released. The returned run is in state booting.
func (p *Planner) Launch(ctx context.Context, offer Offer) (*Run, error) {
	prof, ok := Lookup(offer.Kind)
	if !ok || prof.Never {
		return nil, fmt.Errorf("%w: %s", ErrNotBurstable, offer.Kind)
	}
	if prof.LabOnly || offer.Lab {
		// The product table never holds a LabOnly row; a lab job reaches the
		// planner only through LaunchDirect.
		return nil, fmt.Errorf("%w: %s", ErrLabOnly, offer.Kind)
	}
	if offer.InstanceTypeOverride == "" && !p.Cfg.WorkloadEnabled(prof.Key) {
		return nil, fmt.Errorf("%w: %s", ErrDisabled, prof.Key)
	}
	return p.launch(ctx, prof, offer)
}

// LaunchDirect launches an instance for an explicit profile, skipping the
// scheduler trigger and the BURST_WORKLOADS switch (MM138 decision 6). It keeps
// everything else: max instances, stale-image refusal, spend caps (the burst
// caps and, for lab jobs, the Lab sub-cap), the reaper and termination rules.
// A LabOnly profile needs offer.Lab, and offer.Lab needs a LabOnly profile.
func (p *Planner) LaunchDirect(ctx context.Context, prof Profile, offer Offer) (*Run, error) {
	if prof.Never {
		return nil, fmt.Errorf("%w: %s", ErrNotBurstable, prof.Key)
	}
	if prof.LabOnly != offer.Lab {
		return nil, fmt.Errorf("%w: %s", ErrLabOnly, prof.Key)
	}
	if len(prof.Candidates) == 0 {
		return nil, fmt.Errorf("%w: %s has no instance candidates", ErrNotBurstable, prof.Key)
	}
	if offer.Kind == "" {
		offer.Kind = prof.Key
	}
	return p.launch(ctx, prof, offer)
}

func (p *Planner) launch(ctx context.Context, prof Profile, offer Offer) (*Run, error) {
	if len(p.Live()) >= p.Cfg.MaxInstances {
		return nil, ErrMaxInstances
	}
	if p.Cfg.RefuseStaleImages && p.Stale != nil && p.Stale(prof.Family) {
		return nil, fmt.Errorf("%w: %s", ErrStaleImage, prof.Family)
	}
	cands := CandidatesFor(prof, p.Cfg.InstanceTypes)
	if offer.InstanceTypeOverride != "" {
		cands = []string{offer.InstanceTypeOverride}
	}
	now := p.now()
	run := &Run{
		ID: uuid.NewString(), JobID: offer.JobID, Workload: prof.Key, Kind: offer.Kind, Family: prof.Family,
		Arch: ArchOf(cands[0]), State: StateOffered, Tier: offer.Tier, Priority: offer.Priority,
		PredictedWaitMs: offer.PredictedWaitMs, OfferedAt: now, Purpose: PurposeBurst,
	}
	if offer.Lab {
		run.Purpose = PurposeLab
	}
	// Reserve against the most expensive candidate: the launch may fall through to it.
	worst := Rates{}
	for _, c := range cands {
		if r := p.rates(c); r.PriceUSDPerHour > worst.PriceUSDPerHour || worst.PriceUSDPerHour == 0 {
			worst = r
		}
	}
	hold := time.Duration(prof.SeedHoldSeconds) * time.Second
	boot := time.Duration(p.Cfg.BootTimeoutSeconds) * time.Second / 2
	run.ReservedMicros = ReserveMicros(worst, boot, hold, 2*time.Minute, prof.RootGiB)
	reserveErr := error(nil)
	if offer.Lab && p.LabLedger != nil {
		reserveErr = p.LabLedger.Reserve(ctx, run.ID, run.ReservedMicros)
	}
	if reserveErr == nil {
		if reserveErr = p.Ledger.Reserve(ctx, run.ID, run.ReservedMicros); reserveErr != nil && offer.Lab && p.LabLedger != nil {
			p.LabLedger.Release(run.ID)
		}
	}
	if err := reserveErr; err != nil {
		run.State = StateCapRefused
		run.ErrorType = err.Error()
		t := now
		run.TerminatedAt = &t
		run.ReservedMicros = 0
		p.save(ctx, run, true)
		p.log().Warn("burst: offer refused by spend cap", "run", run.ID, "kind", offer.Kind, "reason", err.Error())
		return nil, err
	}
	p.track(run)
	p.save(ctx, run, true) // the row exists before anything is launched
	fail := func(err error, errType string) (*Run, error) {
		t := p.now()
		run.State, run.TerminatedAt, run.ErrorType = StateLaunchFailed, &t, errType
		p.releaseAll(run)
		run.ReservedMicros = 0
		p.untrack(run.ID)
		p.save(ctx, run, false)
		return nil, err
	}
	if err := run.Transition(StateLaunching, p.now(), p.log()); err != nil {
		return fail(err, "transition")
	}
	key, err := p.Minter.Mint(ctx)
	if err != nil {
		p.log().Error("burst: key mint failed", "run", run.ID, "error", err.Error())
		return fail(err, "tailscale")
	}
	ud := ""
	if p.UserData != nil {
		if ud, err = p.UserData(*run, prof, key); err != nil {
			return fail(err, "userdata")
		}
	}
	amiFor := func(t string) string { return p.Cfg.AMIs[prof.Family+":"+ArchOf(t)] }
	if amiFor(cands[0]) == "" {
		return fail(fmt.Errorf("burst: no AMI configured for %s:%s", prof.Family, ArchOf(cands[0])), "no_ami")
	}
	inst, err := p.Provider.Launch(ctx, LaunchRequest{
		RunID: run.ID, JobID: run.JobID, Workload: prof.Key, Candidates: cands, Subnets: p.Cfg.SubnetIDs,
		AMIFor: amiFor, SecurityGroupID: p.Cfg.SecurityGroupID, InstanceProfile: p.Cfg.InstanceProfile,
		RootGiB: prof.RootGiB, RootDevice: p.Cfg.RootDevice, UserData: ud,
	})
	if err != nil {
		errType := "error"
		switch {
		case errors.Is(err, ErrNoCapacity):
			errType = "no_capacity"
		case errors.Is(err, ErrQuota):
			errType = "quota"
		case errors.Is(err, ErrDenied):
			errType = "denied"
		}
		p.log().Error("burst: launch failed", "run", run.ID, "kind", offer.Kind, "errorType", errType, "error", err.Error())
		return fail(err, errType)
	}
	run.InstanceID, run.InstanceType, run.AZ, run.AMIID = inst.ID, inst.Type, inst.AZ, inst.AMI
	run.Arch = ArchOf(inst.Type)
	if !inst.LaunchTime.IsZero() {
		t := inst.LaunchTime
		run.LaunchedAt = &t
	}
	if err := run.Transition(StateBooting, p.now(), p.log()); err != nil {
		return fail(err, "transition")
	}
	p.save(ctx, run, false)
	return run, nil
}

// Advance moves a live run along the lifecycle (booting -> ready -> ...).
func (p *Planner) Advance(ctx context.Context, run *Run, to State) error {
	if to.Terminal() {
		return fmt.Errorf("burst: use Terminate for terminal state %s", to)
	}
	if err := run.Transition(to, p.now(), p.log()); err != nil {
		return err
	}
	p.save(ctx, run, false)
	return nil
}

// Terminate ends a run: terminate the instance, settle the cost, delete the
// staging prefix, stamp terminated_at. outcome must be a terminal state legal
// from the run's current state. An illegal outcome returns an error and logs;
// the reaper remains the backstop for the instance.
func (p *Planner) Terminate(ctx context.Context, run *Run, outcome State) error {
	if !outcome.Terminal() {
		return fmt.Errorf("burst: outcome %s is not terminal", outcome)
	}
	if err := run.Transition(outcome, p.now(), p.log()); err != nil {
		return err
	}
	var termErr error
	if run.InstanceID != "" {
		termErr = p.Provider.Terminate(ctx, []string{run.InstanceID})
		if termErr != nil {
			p.log().Error("burst: terminate failed; the reaper will retry", "run", run.ID, "instance", run.InstanceID, "error", termErr.Error())
		}
	}
	end := p.now()
	run.TerminatedAt = &end
	if run.LaunchedAt != nil && run.InstanceID != "" {
		ebs := 0
		if pr, ok := LookupAny(run.Kind); ok {
			ebs = pr.RootGiB
		}
		run.CostMicros = CostMicros(p.rates(run.InstanceType), *run.LaunchedAt, end, ebs, run.BytesOut, false)
		p.releaseExtras(run.ID)
		p.settleAll(run)
	} else {
		p.releaseAll(run) // never launched: nothing billed
	}
	run.SessionUntil = nil
	if p.Prefixes != nil {
		if err := p.Prefixes.DeleteRun(ctx, run.WorkerRunID()); err != nil {
			p.log().Warn("burst: staging prefix delete failed; the reaper will retry", "run", run.ID, "error", err.Error())
		}
	}
	p.untrack(run.ID)
	p.save(ctx, run, false)
	return termErr
}

// ledgers are the ledgers a run is accounted in: always the burst Ledger, plus
// the Lab sub-cap Ledger for a lab run.
func (p *Planner) ledgers(run *Run) []*Ledger {
	out := []*Ledger{p.Ledger}
	if run.Purpose == PurposeLab && p.LabLedger != nil {
		out = append(out, p.LabLedger)
	}
	return out
}

func (p *Planner) settleAll(run *Run) {
	for _, l := range p.ledgers(run) {
		l.Settle(run.ID, run.CostMicros)
	}
}

func (p *Planner) releaseAll(run *Run) {
	p.releaseExtras(run.ID)
	for _, l := range p.ledgers(run) {
		l.Release(run.ID)
	}
}

func (p *Planner) releaseExtras(runID string) {
	p.mu.Lock()
	ids := p.extras[runID]
	delete(p.extras, runID)
	p.mu.Unlock()
	for _, id := range ids {
		p.Ledger.Release(id)
		if p.LabLedger != nil {
			p.LabLedger.Release(id)
		}
	}
}

// ExtendSession reserves the spend of one more job on a Lab session instance
// that is about to be reused (hold = the job's estimated seconds plus staging).
// A cap refusal returns the cap error and leaves the session untouched; the
// caller then ends the session. The extra holds are released when the run
// terminates; the actual cost settles once, from launch to termination.
func (p *Planner) ExtendSession(ctx context.Context, run *Run, hold time.Duration) error {
	if run.Purpose != PurposeLab {
		return fmt.Errorf("%w: not a lab run", ErrLabOnly)
	}
	micros := ReserveMicros(p.rates(run.InstanceType), 0, hold, 2*time.Minute, 0)
	p.mu.Lock()
	if p.extras == nil {
		p.extras = map[string][]string{}
	}
	id := fmt.Sprintf("%s+%d", run.ID, len(p.extras[run.ID])+1)
	p.mu.Unlock()
	if p.LabLedger != nil {
		if err := p.LabLedger.Reserve(ctx, id, micros); err != nil {
			return err
		}
	}
	if err := p.Ledger.Reserve(ctx, id, micros); err != nil {
		if p.LabLedger != nil {
			p.LabLedger.Release(id)
		}
		return err
	}
	p.mu.Lock()
	p.extras[run.ID] = append(p.extras[run.ID], id)
	p.mu.Unlock()
	return nil
}

// SetSession stamps (or clears, with nil) the time a parked Lab session
// instance is kept until, and persists it. The reaper treats the instance as
// live only while this is in the future.
func (p *Planner) SetSession(ctx context.Context, run *Run, until *time.Time) {
	p.mu.Lock()
	run.SessionUntil = until
	p.mu.Unlock()
	p.save(ctx, run, false)
}

// Adopt hands a still-booting instance to another eligible job instead of
// terminating it (design §6.1). The old row ends as adopted with its cost
// settled to now; a new row (adopted_from = old) continues on the same
// instance. A cap refusal leaves the old run untouched.
func (p *Planner) Adopt(ctx context.Context, old *Run, newJob Offer) (*Run, error) {
	if old.Purpose == PurposeLab {
		return nil, fmt.Errorf("%w: a lab instance is never adopted", ErrLabOnly)
	}
	if old.InstanceID == "" || (old.State != StateBooting && old.State != StateReady) {
		return nil, fmt.Errorf("burst: run %s is not adoptable in state %s", old.ID, old.State)
	}
	prof, ok := Lookup(newJob.Kind)
	if !ok || prof.Never || prof.Family != old.Family {
		return nil, fmt.Errorf("%w: %s does not fit this instance", ErrNotBurstable, newJob.Kind)
	}
	now := p.now()
	nr := *old
	nr.WorkerID = old.WorkerRunID()
	nr.ID, nr.JobID, nr.Kind, nr.Workload = uuid.NewString(), newJob.JobID, newJob.Kind, prof.Key
	nr.Tier, nr.Priority, nr.PredictedWaitMs = newJob.Tier, newJob.Priority, newJob.PredictedWaitMs
	nr.OfferedAt, nr.AdoptedFrom, nr.CostMicros, nr.TerminatedAt, nr.FinishedAt = now, old.ID, 0, nil, nil
	t := now
	nr.LaunchedAt = &t // billing for the new row starts at the hand-over
	nr.ReservedMicros = ReserveMicros(p.rates(old.InstanceType), 0, time.Duration(prof.SeedHoldSeconds)*time.Second, 2*time.Minute, prof.RootGiB)
	if err := p.Ledger.Reserve(ctx, nr.ID, nr.ReservedMicros); err != nil {
		return nil, err
	}
	if err := old.Transition(StateAdopted, now, p.log()); err != nil {
		p.Ledger.Release(nr.ID)
		return nil, err
	}
	end := now
	old.TerminatedAt = &end
	if old.LaunchedAt != nil {
		old.CostMicros = CostMicros(p.rates(old.InstanceType), *old.LaunchedAt, end, 0, 0, false)
	}
	p.Ledger.Settle(old.ID, old.CostMicros)
	p.untrack(old.ID)
	p.save(ctx, old, false)
	p.track(&nr)
	p.save(ctx, &nr, true)
	return &nr, nil
}
