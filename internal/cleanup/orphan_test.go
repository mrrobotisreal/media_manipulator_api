package cleanup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// agedDir creates <root>/<id>/result.bin and backdates the directory.
func agedDir(t *testing.T, root, id string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
	return dir
}

type fakeOrphans struct {
	live  map[string]bool
	err   error
	calls int
	asked []string
}

func (f *fakeOrphans) LiveLedgerLocators(_ context.Context, _, locators []string) (map[string]bool, error) {
	f.calls++
	f.asked = append(f.asked, locators...)
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]bool{}
	for _, l := range locators {
		if f.live[l] {
			out[l] = true
		}
	}
	return out, nil
}

// enforcing builds a worker with the safety switches OFF (real deletion).
func enforcing(outputs string, orphans OrphanLedger) *Worker {
	w := NewWorker(&config.Config{
		OutputDir: outputs, OutputRetention: 24 * time.Hour,
		CleanupAuditMaxPathsPerRun: 100, RetentionSweepDryRun: false,
	}, nil, nil, quiet(), nil)
	w.Orphans = orphans
	return w
}

// Acceptance 4: an entry WITH a live ledger row survives past 24 h, one without
// is removed at 24 h, and the old .mm-retention marker is no longer consulted.
func TestOrphanWalkerKeepsLedgerTrackedAndReapsOrphans(t *testing.T) {
	outputs := t.TempDir()
	tracked := agedDir(t, outputs, "tracked-job", 72*time.Hour)
	orphan := agedDir(t, outputs, "orphan-job", 72*time.Hour)
	young := agedDir(t, outputs, "young-orphan", time.Hour)

	enforcing(outputs, &fakeOrphans{live: map[string]bool{"tracked-job": true}}).tick(context.Background())

	if !exists(tracked) {
		t.Error("an entry with a live ledger row must survive the orphan walker, however old")
	}
	if exists(orphan) {
		t.Error("an entry with no ledger row is reaped at the global 24 h")
	}
	if !exists(young) {
		t.Error("a young orphan is not reaped")
	}
}

func TestOrphanWalkerIgnoresRetentionMarkerFiles(t *testing.T) {
	outputs := t.TempDir()
	// A 30-day marker on a 40-day-old dir: the old walker honoured it; the new one never reads it,
	// and past the legacy grace window the directory is an ordinary orphan.
	old := agedDir(t, outputs, "marked-old", 40*24*time.Hour)
	if err := os.WriteFile(filepath.Join(old, ".mm-retention"), []byte("720"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A 2-day-old dir with a (never read) 1-hour marker: stays on the transition guard, NOT reaped early.
	guarded := agedDir(t, outputs, "marked-recent", 48*time.Hour)
	if err := os.WriteFile(filepath.Join(guarded, ".mm-retention"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Writing the markers touched the directories; age them again.
	for dir, age := range map[string]time.Duration{old: 40 * 24 * time.Hour, guarded: 48 * time.Hour} {
		when := time.Now().Add(-age)
		if err := os.Chtimes(dir, when, when); err != nil {
			t.Fatal(err)
		}
	}
	enforcing(outputs, &fakeOrphans{}).tick(context.Background())
	if exists(old) {
		t.Error("marker no longer extends life beyond the grace window")
	}
	if !exists(guarded) {
		t.Error("a directory carrying a legacy marker is protected during the transition grace window")
	}
}

func TestOrphanWalkerFailsSafeWhenLedgerLookupFails(t *testing.T) {
	outputs := t.TempDir()
	dir := agedDir(t, outputs, "who-knows", 72*time.Hour)
	enforcing(outputs, &fakeOrphans{err: errors.New("db down")}).tick(context.Background())
	if !exists(dir) {
		t.Error("when the ledger cannot be consulted nothing may be reaped as an orphan")
	}
}

func TestOrphanWalkerLedgerLookupIsBatched(t *testing.T) {
	outputs := t.TempDir()
	for _, id := range []string{"a", "b", "c", "d"} {
		agedDir(t, outputs, id, 72*time.Hour)
	}
	f := &fakeOrphans{live: map[string]bool{"a": true, "c": true}}
	enforcing(outputs, f).tick(context.Background())
	if f.calls != 1 || len(f.asked) != 4 {
		t.Errorf("one batched lookup expected, got %d calls for %v", f.calls, f.asked)
	}
	if !exists(filepath.Join(outputs, "a")) || !exists(filepath.Join(outputs, "c")) ||
		exists(filepath.Join(outputs, "b")) || exists(filepath.Join(outputs, "d")) {
		t.Error("only the untracked entries b and d go")
	}
}

func TestOrphanWalkerSkipsActiveJobs(t *testing.T) {
	outputs := t.TempDir()
	dir := agedDir(t, outputs, "running", 72*time.Hour)
	w := enforcing(outputs, &fakeOrphans{})
	w.Active = activeSet{"running": {}}
	w.tick(context.Background())
	if !exists(dir) {
		t.Error("active job directories are never reaped")
	}
}

type activeSet map[string]struct{}

func (a activeSet) ActiveJobIDs() map[string]struct{} { return a }

// The shipped default: nothing is deleted until RETENTION_SWEEP_DRY_RUN=false.
func TestDefaultConfigIsDryRunAndDeletesNothing(t *testing.T) {
	cfg := config.Load()
	if !cfg.RetentionSweepDryRun {
		t.Fatal("RETENTION_SWEEP_DRY_RUN must default to true")
	}
	outputs := t.TempDir()
	dir := agedDir(t, outputs, "pre-mm22-job", 90*24*time.Hour)
	w := NewWorker(&config.Config{OutputDir: outputs, OutputRetention: 24 * time.Hour,
		RetentionSweepDryRun: cfg.RetentionSweepDryRun, CleanupAuditMaxPathsPerRun: 10}, nil, nil, quiet(), nil)
	w.Orphans = &fakeOrphans{}
	w.tick(context.Background())
	if !exists(dir) {
		t.Fatal("dry run (the default) must not delete the orphan walker's candidates either")
	}
}

func TestLegacyCleanupDryRunFlagStillHolds(t *testing.T) {
	outputs := t.TempDir()
	dir := agedDir(t, outputs, "j", 72*time.Hour)
	w := NewWorker(&config.Config{OutputDir: outputs, OutputRetention: 24 * time.Hour, CleanupDryRun: true,
		RetentionSweepDryRun: false, CleanupAuditMaxPathsPerRun: 10}, nil, nil, quiet(), nil)
	w.Orphans = &fakeOrphans{}
	w.tick(context.Background())
	if !exists(dir) {
		t.Error("CLEANUP_DRY_RUN=true must keep protecting the orphan walker")
	}
}

// --- worker wiring -------------------------------------------------------------

type fakeJanitor struct {
	calls int
	age   time.Duration
}

func (j *fakeJanitor) CleanupOldJobs(age time.Duration) { j.calls++; j.age = age }

// Acceptance 7: CleanupOldJobs is called from the worker (24 h by default).
func TestTickCallsCleanupOldJobs(t *testing.T) {
	j := &fakeJanitor{}
	w := NewWorker(&config.Config{RetentionSweepDryRun: true}, nil, nil, quiet(), nil)
	w.Janitor = j
	w.tick(context.Background())
	if j.calls != 1 || j.age != 24*time.Hour {
		t.Errorf("janitor calls=%d age=%v, want 1 call at 24h", j.calls, j.age)
	}
	w.Cfg.RetentionJobMapMaxAge = 6 * time.Hour
	w.tick(context.Background())
	if j.calls != 2 || j.age != 6*time.Hour {
		t.Errorf("configured age not honoured: %d %v", j.calls, j.age)
	}
}

type fakePurger struct{ calls, dry int }

func (f *fakePurger) PurgeBatch(_ context.Context, _ string, _ time.Time, _ int, dry bool) (int64, error) {
	f.calls++
	if dry {
		f.dry++
	}
	return 0, nil
}
func (f *fakePurger) UnparkMetadataReports(context.Context, time.Time) (int64, error) { return 0, nil }

func TestPurgesRunAtMostOncePerHourAndHonourDryRun(t *testing.T) {
	fp := &fakePurger{}
	w := NewWorker(&config.Config{RetentionSweepDryRun: true}, nil, nil, quiet(), nil)
	w.Purger = retention.NewPurger(fp, quiet())
	clock := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return clock }

	w.tick(context.Background())
	first := fp.calls
	if first == 0 || fp.dry != first {
		t.Fatalf("first tick should run the purges, all in dry-run: calls=%d dry=%d", first, fp.dry)
	}
	clock = clock.Add(15 * time.Minute)
	w.tick(context.Background())
	if fp.calls != first {
		t.Error("purges must not run again within the hour")
	}
	clock = clock.Add(50 * time.Minute)
	w.Cfg.RetentionSweepDryRun = false
	w.tick(context.Background())
	if fp.calls == first || fp.dry != first {
		t.Errorf("after an hour the purges run again, now for real: calls=%d dry=%d", fp.calls, fp.dry)
	}
}

type captureEmitter struct{ evs []analyticsemit.Event }

func (c *captureEmitter) Emit(e analyticsemit.Event) { c.evs = append(c.evs, e) }

func TestEmitSweptOncePerKindWithBucketAndNeverInDryRun(t *testing.T) {
	em := &captureEmitter{}
	w := NewWorker(&config.Config{}, nil, nil, quiet(), nil)
	w.Emitter = em
	res := retention.Result{PerKind: map[string]*retention.KindResult{
		retention.KindOutputDir: {Deleted: 12}, retention.KindS3Object: {Deleted: 1}, retention.KindUploadDir: {Deleted: 0, Errors: 2},
	}}
	w.emitSwept(res)
	if len(em.evs) != 2 {
		t.Fatalf("one event per kind that deleted something, got %d", len(em.evs))
	}
	got := map[string]string{}
	for _, e := range em.evs {
		if e.EventName != analyticsemit.ServerRetentionSwept {
			t.Errorf("event name %q", e.EventName)
		}
		got[e.Properties["kind"].(string)] = e.Properties["countBucket"].(string)
	}
	if got[retention.KindOutputDir] != "10-99" || got[retention.KindS3Object] != "1" {
		t.Errorf("buckets: %v", got)
	}
	em.evs = nil
	res.DryRun = true
	w.emitSwept(res)
	if len(em.evs) != 0 {
		t.Error("dry run must not emit")
	}
}

func TestCountBuckets(t *testing.T) {
	for n, want := range map[int]string{0: "0", 1: "1", 2: "2-9", 9: "2-9", 10: "10-99", 99: "10-99", 100: "100+", 5000: "100+"} {
		if got := analyticsemit.CountBucket(n); got != want {
			t.Errorf("CountBucket(%d) = %q, want %q", n, got, want)
		}
	}
}

// --- worker + real sweeper ---------------------------------------------------------

type oneRowDB struct {
	retention.SweepStore
	row     telemetry.LedgerRow
	claimed bool
	stamped []string
}

func (d *oneRowDB) ClaimDue(context.Context, time.Time, int, time.Duration, int) ([]telemetry.LedgerRow, error) {
	if d.claimed {
		return nil, nil
	}
	d.claimed = true
	return []telemetry.LedgerRow{d.row}, nil
}
func (d *oneRowDB) PeekDue(context.Context, time.Time, int, int, time.Time, string) ([]telemetry.LedgerRow, error) {
	if d.claimed {
		return nil, nil
	}
	d.claimed = true
	return []telemetry.LedgerRow{d.row}, nil
}
func (d *oneRowDB) LedgerBacklog(context.Context, time.Time, int) (telemetry.Backlog, error) {
	return telemetry.Backlog{}, nil
}
func (d *oneRowDB) MarkLedgerDeleted(_ context.Context, ids []string, _ time.Time) error {
	d.stamped = append(d.stamped, ids...)
	return nil
}
func (d *oneRowDB) JobHasLiveRows(context.Context, string) (bool, error) { return true, nil }

func TestTickRunsTheSweeperThenOrphans(t *testing.T) {
	outputs := t.TempDir()
	tracked := agedDir(t, outputs, "ledger-job", 72*time.Hour)
	db := &oneRowDB{row: telemetry.LedgerRow{ID: "id1", Kind: retention.KindOutputDir, Locator: "ledger-job", JobID: "ledger-job", OwnerSession: "s"}}
	cfg := &config.Config{OutputDir: outputs, OutputRetention: 24 * time.Hour, CleanupAuditMaxPathsPerRun: 10}
	w := NewWorker(cfg, nil, nil, quiet(), nil)
	w.Sweeper = retention.NewSweeper(db, nil, nil, nil, nil, quiet(), retention.SweepConfig{OutputDir: outputs, MaxAttempts: 10})
	w.Orphans = &fakeOrphans{live: map[string]bool{"ledger-job": true}}
	w.tick(context.Background())
	if exists(tracked) || len(db.stamped) != 1 {
		t.Error("the ledger sweeper deletes the expired tracked directory (enforce mode)")
	}
}

type kindAwareOrphans struct{ asked [][]string }

func (k *kindAwareOrphans) LiveLedgerLocators(_ context.Context, kinds, locators []string) (map[string]bool, error) {
	k.asked = append(k.asked, kinds)
	out := map[string]bool{}
	for _, kind := range kinds {
		if kind == retention.KindMetadata {
			for _, l := range locators {
				out[l] = true // a live parked metadata_report row exists for every candidate
			}
		}
	}
	return out, nil
}

// The kept-metadata directory (output_dir row already swept, parked
// metadata_report row live) must not be reaped by the orphan walker.
func TestOrphanWalkerKeepsDirectoriesTrackedByParkedMetadataRows(t *testing.T) {
	outputs := t.TempDir()
	dir := agedDir(t, outputs, "kept-metadata", 72*time.Hour)
	enforcing(outputs, &kindAwareOrphans{}).tick(context.Background())
	if !exists(dir) {
		t.Fatal("outputs/<id> holding kept metadata reports must survive the orphan walker")
	}
}
