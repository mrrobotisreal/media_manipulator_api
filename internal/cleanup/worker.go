// Package cleanup runs the background retention worker.
//
// Since MM23 each tick does, in order:
//
//  1. the ledger sweeper (internal/retention.Sweeper): deletes what
//     mm_retained_objects says has expired - disk dirs, S3 objects, Darkroom
//     projects, job rows. This is the primary mechanism.
//  2. the orphan walker (sweepRoot): the old mtime walker, now ONLY for entries
//     that have no live ledger row (pre-MM22 data, crashed writers, temp files),
//     at the global 24 h / 24 h / 1 h windows.
//  3. JobManager.CleanupOldJobs: drops finished jobs from the in-memory map.
//  4. once an hour, the Postgres purges (counters, incidents, raw IPs, old runs).
//
// Everything is dry-run while RETENTION_SWEEP_DRY_RUN is true (the default).
// We avoid deleting jobs that the in-memory JobManager still considers active
// (pending/processing) and we never traverse symlinks out of the configured
// roots.
package cleanup

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// ActiveJobs reports the set of in-flight job ids that should not be
// touched by the sweeper. Implementations should return a snapshot of
// currently pending/processing jobs.
type ActiveJobs interface {
	ActiveJobIDs() map[string]struct{}
}

// JobJanitor trims the in-memory job map (JobManager.CleanupOldJobs).
type JobJanitor interface {
	CleanupOldJobs(maxAge time.Duration)
}

// OrphanLedger answers which disk locators still have a LIVE ledger row. An
// entry with one belongs to the sweeper, never to the mtime walker.
type OrphanLedger interface {
	LiveLedgerLocators(ctx context.Context, kinds, locators []string) (map[string]bool, error)
}

// EventEmitter is the slice of *analyticsemit.Emitter the worker uses.
type EventEmitter interface {
	Emit(ev analyticsemit.Event)
}

// legacyMarkerFile is the per-directory retention marker pre-MM22 builds wrote
// (".mm-retention", hours as text). It is NEVER read any more. It survives only
// as a transition guard: a directory that still contains one was promised a
// longer-than-24 h window by an old build, so the orphan walker leaves it alone
// until legacyMarkerGrace has passed (the longest window those builds ever wrote,
// premium = 30 days). Delete this guard, and the constant, 31 days after MM23
// reaches production.
const (
	legacyMarkerFile  = ".mm-retention"
	legacyMarkerGrace = 30 * 24 * time.Hour
)

// Worker periodically scans the configured directories.
type Worker struct {
	Cfg     *config.Config
	Store   *telemetry.Store
	Metrics *metrics.Registry
	Logger  *slog.Logger
	Active  ActiveJobs

	// MM23 collaborators. All optional: nil means "that stage is skipped".
	Sweeper *retention.Sweeper
	Purger  *retention.Purger
	Janitor JobJanitor
	Orphans OrphanLedger
	Emitter EventEmitter

	lastPurge time.Time
	now       func() time.Time
	stop      atomic.Bool
}

// NewWorker constructs a Worker. Active may be nil — in that case nothing is
// considered active.
func NewWorker(cfg *config.Config, store *telemetry.Store, m *metrics.Registry, logger *slog.Logger, active ActiveJobs) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{Cfg: cfg, Store: store, Metrics: m, Logger: logger, Active: active}
}

// Run blocks until ctx is cancelled, ticking at CleanupInterval.
func (w *Worker) Run(ctx context.Context) {
	if !w.Cfg.CleanupEnabled {
		w.Logger.Info("cleanup worker disabled via config")
		return
	}
	interval := w.Cfg.CleanupInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Run once immediately.
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *Worker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// dryRun is true unless BOTH switches say enforce: the legacy CLEANUP_DRY_RUN
// stays honoured, and RETENTION_SWEEP_DRY_RUN (default true) gates everything
// MM23 added, including the orphan walker.
func (w *Worker) dryRun() bool {
	return w.Cfg.CleanupDryRun || w.Cfg.RetentionSweepDryRun
}

// tick runs a single pass: ledger sweep, orphan walk, job-map trim, purges.
func (w *Worker) tick(ctx context.Context) {
	if w.stop.Load() {
		return
	}
	started := time.Now().UTC()
	dry := w.dryRun()
	run := telemetry.CleanupRun{
		StartedAt: started,
		Status:    "ok",
		UploadDir: w.Cfg.UploadDir,
		OutputDir: w.Cfg.OutputDir,
		TempDir:   w.Cfg.TempDir,
	}
	logger := w.Logger.With("stage", "cleanup", "dryRun", dry)
	logger.Info("cleanup tick started")

	var (
		files    int64
		dirs     int64
		bytes    int64
		paths    []telemetry.CleanupPath
		maxPaths = w.Cfg.CleanupAuditMaxPathsPerRun
		errMsgs  []string
		props    = map[string]any{"dryRun": dry}
	)
	if maxPaths <= 0 {
		maxPaths = 1000
	}
	addErr := func(msg string) {
		run.Status = "error"
		errMsgs = append(errMsgs, msg)
		if w.Metrics != nil {
			w.Metrics.CleanupError()
		}
	}

	// 1. Ledger sweep.
	if w.Sweeper != nil {
		res := w.Sweeper.Sweep(ctx)
		files += res.Files
		dirs += res.Dirs
		bytes += res.Bytes
		for k, v := range res.Properties() {
			props[k] = v
		}
		room := maxPaths - len(paths)
		if room > 0 && len(res.Paths) > 0 {
			if len(res.Paths) > room {
				res.Paths = res.Paths[:room]
			}
			paths = append(paths, res.Paths...)
		}
		if res.Err != "" {
			addErr("sweeper: " + res.Err)
		}
		w.emitSwept(res)
		logger.Info("retention sweep complete", "dryRun", res.DryRun, "total", res.Total(),
			"backlogDue", res.BacklogDue, "gaveUp", res.GaveUpRows, "truncated", res.Truncated)
	}

	// 2. Orphan walker: only entries with no live ledger row.
	var active map[string]struct{}
	if w.Active != nil {
		active = w.Active.ActiveJobIDs()
	}
	type sweepSpec struct {
		root      string
		retention time.Duration
		jobAware  bool
		kinds     []string
	}
	sweeps := []sweepSpec{
		{root: w.Cfg.UploadDir, retention: w.Cfg.UploadRetention, jobAware: true, kinds: []string{retention.KindUploadDir}},
		{root: w.Cfg.OutputDir, retention: w.Cfg.OutputRetention, jobAware: true, kinds: []string{retention.KindOutputDir}},
		{root: w.Cfg.TempDir, retention: w.Cfg.TempRetention, jobAware: false},
	}
	var orphanFiles, orphanDirs int64
	for _, sp := range sweeps {
		if strings.TrimSpace(sp.root) == "" || sp.retention <= 0 {
			continue
		}
		f, d, b, p, err := w.sweepRoot(ctx, sp.root, sp.retention, sp.jobAware, sp.kinds, active, maxPaths-len(paths))
		files += f
		dirs += d
		bytes += b
		orphanFiles += f
		orphanDirs += d
		paths = append(paths, p...)
		if err != nil {
			addErr(err.Error())
		}
		if len(paths) >= maxPaths {
			break
		}
	}
	props["orphanDirs"], props["orphanFiles"] = orphanDirs, orphanFiles

	// 3. In-memory job map.
	if w.Janitor != nil {
		age := w.Cfg.RetentionJobMapMaxAge
		if age <= 0 {
			age = 24 * time.Hour
		}
		w.Janitor.CleanupOldJobs(age)
	}

	// 4. Postgres purges, at most hourly.
	if w.Purger != nil && w.clock().Sub(w.lastPurge) >= retention.PurgeInterval {
		w.lastPurge = w.clock()
		purged := w.Purger.Run(ctx, dry)
		out := make(map[string]any, len(purged))
		for k, v := range purged {
			out[k] = v
		}
		props["purge"] = out
		logger.Info("retention purges complete", "dryRun", dry, "counts", purged)
	}

	run.DeletedFiles = files
	run.DeletedDirs = dirs
	run.DeletedBytes = bytes
	run.ErrorMessage = strings.Join(errMsgs, "; ")
	run.Properties = props
	completed := time.Now().UTC()
	run.CompletedAt = &completed
	logger.Info("cleanup tick complete",
		"deletedFiles", files, "deletedDirs", dirs,
		"deletedBytes", bytes, "errors", run.ErrorMessage)
	if w.Metrics != nil && !dry {
		w.Metrics.CleanupDeleted(files, bytes)
	}
	if w.Store != nil {
		runID := w.Store.InsertCleanupRun(ctx, run)
		if runID != "" && len(paths) > 0 {
			for i := range paths {
				paths[i].CleanupRunID = runID
			}
			w.Store.InsertCleanupPaths(ctx, paths)
		}
	}
}

// emitSwept sends server_retention_swept {kind, countBucket} once per kind that
// actually deleted something (never in dry run).
func (w *Worker) emitSwept(res retention.Result) {
	if w.Emitter == nil || res.DryRun {
		return
	}
	for kind, kr := range res.PerKind {
		if kr.Deleted <= 0 {
			continue
		}
		w.Emitter.Emit(analyticsemit.Event{
			EventName:  analyticsemit.ServerRetentionSwept,
			EventTS:    time.Now().UTC(),
			Properties: map[string]any{"kind": kind, "countBucket": analyticsemit.CountBucket(kr.Deleted)},
		})
	}
}

// sweepRoot is the ORPHAN walker. It walks root and deletes top-level entries
// older than retention, but ONLY entries that have no live ledger row - the
// ledger sweeper owns everything it knows about, and a job directory the ledger
// still tracks must survive here however old its mtime is. Pre-MM22 data (no
// ledger rows) is therefore reaped at the global windows, exactly as before.
//
// If the ledger cannot be consulted, jobAware roots are skipped for this tick
// (fail safe): without the lookup we cannot tell an orphan from a user's file.
//
// kinds names the ledger kinds that govern this root; nil (the temp root) means
// no ledger involvement. Dry run (RETENTION_SWEEP_DRY_RUN) records what would go.
func (w *Worker) sweepRoot(ctx context.Context, root string, retention time.Duration, jobAware bool, kinds []string, active map[string]struct{}, capPaths int) (int64, int64, int64, []telemetry.CleanupPath, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	stat, err := os.Stat(absRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, 0, nil, nil
		}
		return 0, 0, 0, nil, err
	}
	if !stat.IsDir() {
		return 0, 0, 0, nil, errors.New("cleanup root is not a directory: " + absRoot)
	}
	now := time.Now()
	dry := w.dryRun()
	var (
		files int64
		dirs  int64
		bytes int64
		paths []telemetry.CleanupPath
	)

	// Top-level entries: typically job-id-named subdirs for upload/output,
	// or arbitrary temp files for temp.
	entries, err := os.ReadDir(absRoot)
	if err != nil {
		return 0, 0, 0, nil, err
	}

	// Pass 1: which entries are old enough to be candidates at all.
	type candidate struct {
		e    os.DirEntry
		name string
		full string
		info os.FileInfo
		age  time.Duration
	}
	var cands []candidate
	for _, e := range entries {
		if ctx.Err() != nil {
			return files, dirs, bytes, paths, ctx.Err()
		}
		name := e.Name()
		// Skip dot files (hidden ops artifacts).
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(absRoot, name)
		// Guard against symlink escapes.
		if !isWithin(full, absRoot) {
			w.Logger.Warn("cleanup skipping path outside root", "path", "<root>/"+name)
			continue
		}
		if jobAware && e.IsDir() {
			if _, busy := active[name]; busy {
				continue
			}
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		if age < retention {
			continue
		}
		cands = append(cands, candidate{e: e, name: name, full: full, info: info, age: age})
	}

	// Pass 2: drop candidates the ledger still tracks (batched, one query).
	tracked := map[string]bool{}
	if len(kinds) > 0 && len(cands) > 0 {
		if w.Orphans == nil {
			// No ledger to ask (no database): the safe reading is "unknown", not "orphan".
			// Without a database there are no ledger rows either, so this is the
			// pre-MM22 world and the walker keeps its old behaviour.
			tracked = map[string]bool{}
		} else {
			names := make([]string, 0, len(cands))
			for _, c := range cands {
				names = append(names, c.name)
			}
			got, err := w.Orphans.LiveLedgerLocators(ctx, kinds, names)
			if err != nil && !errors.Is(err, telemetry.ErrNoStore) {
				w.Logger.Warn("orphan walker: ledger lookup failed; skipping this root", "root", filepath.Base(absRoot), "error", err.Error())
				return 0, 0, 0, nil, nil
			}
			if err == nil {
				tracked = got
			}
		}
	}

	for _, c := range cands {
		if ctx.Err() != nil {
			return files, dirs, bytes, paths, ctx.Err()
		}
		if tracked[c.name] {
			continue // the ledger sweeper owns it
		}
		// Transition guard for directories an old build marked with a longer window.
		if jobAware && c.e.IsDir() && c.age < legacyMarkerGrace {
			if _, err := os.Stat(filepath.Join(c.full, legacyMarkerFile)); err == nil {
				continue
			}
		}
		size := int64(0)
		pathType := "file"
		if c.e.IsDir() {
			pathType = "dir"
			sub, subBytes, subErr := dirSize(c.full)
			size = subBytes
			dirs++
			files += sub
			if subErr != nil {
				w.Logger.Warn("cleanup walk error", "err", subErr.Error())
			}
		} else {
			size = c.info.Size()
			files++
		}
		bytes += size
		audit := telemetry.CleanupPath{
			PathRedacted: filepath.Base(absRoot) + "/" + c.name,
			PathType:     pathType,
			AgeSeconds:   int(c.age / time.Second),
			SizeBytes:    size,
			DeletedAt:    time.Now().UTC(),
		}
		if dry {
			if len(paths) < capPaths {
				audit.ErrorMessage = "dry_run"
				paths = append(paths, audit)
			}
			continue
		}
		var removeErr error
		if c.e.IsDir() {
			removeErr = os.RemoveAll(c.full)
		} else {
			removeErr = os.Remove(c.full)
		}
		if removeErr != nil {
			w.Logger.Warn("cleanup remove failed", "path", filepath.Base(absRoot)+"/"+c.name, "err", removeErr.Error())
			if w.Metrics != nil {
				w.Metrics.CleanupError()
			}
			if len(paths) < capPaths {
				audit.ErrorMessage = removeErr.Error()
				paths = append(paths, audit)
			}
			continue
		}
		if len(paths) < capPaths {
			paths = append(paths, audit)
		}
	}
	return files, dirs, bytes, paths, nil
}

// isWithin returns true if candidate is inside root (no symlink-escape).
//
// filepath.Rel returns "../…" when candidate lies outside root, and an error
// when the paths span different volumes — both treated as escape.
func isWithin(candidate, root string) bool {
	cand, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	r := filepath.Clean(root)
	rel, err := filepath.Rel(r, cand)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

// dirSize totals up files and bytes in a directory tree.
func dirSize(root string) (int64, int64, error) {
	var (
		files int64
		bytes int64
	)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes, err
}
