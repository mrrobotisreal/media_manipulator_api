package retention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

const user1 = "11111111-1111-1111-1111-111111111111"

// --- disk kinds ------------------------------------------------------------

func TestSweepOutputDirExpiredDeletedUnexpiredUntouched(t *testing.T) {
	e := newSweepEnv(t)
	gone := e.jobDir(e.outputs, "job-old")
	kept := e.jobDir(e.outputs, "job-new")
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-old", JobID: "job-old", OwnerSession: "s"})
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-new", JobID: "job-new", OwnerSession: "s",
		ExpiresAt: sweepT0.Add(time.Hour)})

	res := e.sweep()

	if exists(gone) {
		t.Error("expired output dir should be removed")
	}
	if !exists(kept) {
		t.Error("unexpired output dir must be untouched")
	}
	if e.db.byLocator(KindOutputDir, "job-old").DeletedAt == nil {
		t.Error("expired row must be stamped deleted")
	}
	if e.db.byLocator(KindOutputDir, "job-new").DeletedAt != nil {
		t.Error("unexpired row must not be stamped")
	}
	if res.PerKind[KindOutputDir].Deleted != 1 || e.meter.swept[KindOutputDir] != 1 {
		t.Errorf("tally wrong: %+v meter %v", res.PerKind[KindOutputDir], e.meter.swept)
	}
}

func TestSweepUploadDirDeleted(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.uploads, "job-u", "incoming_a.mp4")
	e.db.add(telemetry.LedgerRow{Kind: KindUploadDir, Locator: "job-u", JobID: "job-u", OwnerSession: "s"})
	e.sweep()
	if exists(dir) {
		t.Error("upload dir should be removed")
	}
}

func TestSweepActiveJobIsSkippedNotDeleted(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-run")
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-run", JobID: "job-run", OwnerSession: "s"})
	e.active["job-run"] = struct{}{}

	res := e.sweep()

	if !exists(dir) {
		t.Fatal("a running job's directory must survive")
	}
	r := e.db.byLocator(KindOutputDir, "job-run")
	if r.DeletedAt != nil || r.Attempts != 0 || r.NextAt == nil {
		t.Errorf("active job must be deferred without counting an attempt: %+v", r)
	}
	if res.PerKind[KindOutputDir].Deferred != 1 {
		t.Errorf("deferred tally: %+v", res.PerKind[KindOutputDir])
	}
	// Once the job finishes it goes on the next due pass.
	delete(e.active, "job-run")
	sw := e.sweeper()
	sw.SetClock(func() time.Time { return sweepT0.Add(time.Hour) })
	sw.Sweep(t.Context())
	if exists(dir) {
		t.Error("after the job finished the dir should go")
	}
}

func TestSweepRefusesUnsafeLocators(t *testing.T) {
	e := newSweepEnv(t)
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "precious"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, loc := range []string{"../" + filepath.Base(victim), "..", ".", "", "a/b", "/etc", ".hidden", victim} {
		e.db.add(telemetry.LedgerRow{ID: fmt.Sprintf("00000000-0000-0000-0000-9999000000%02d", i), Kind: KindOutputDir, Locator: loc, OwnerSession: "s"})
	}
	e.sweep()
	if !exists(filepath.Join(victim, "precious")) {
		t.Fatal("a corrupt locator reached outside the output root")
	}
	if len(e.disk.removed) != 0 {
		t.Errorf("nothing may be removed for unsafe locators, removed %v", e.disk.removed)
	}
	if !exists(e.outputs) {
		t.Error("the root itself must never be removed")
	}
	for _, r := range e.db.rows {
		if r.DeletedAt != nil {
			t.Errorf("unsafe row %q must not be stamped deleted", r.Locator)
		}
		if r.Attempts != 1 {
			t.Errorf("unsafe row %q should record a failed attempt, got %d", r.Locator, r.Attempts)
		}
	}
}

func TestSweepDiskErrorSetsAttemptsAndBackoff(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-bad")
	e.disk.failOn[dir] = errBoom
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-bad", JobID: "job-bad", OwnerSession: "s"})

	e.sweep()

	r := e.db.byLocator(KindOutputDir, "job-bad")
	if r.DeletedAt != nil || r.Attempts != 1 || r.Err == "" {
		t.Fatalf("error path: %+v", r)
	}
	if want := sweepT0.Add(5 * time.Minute); r.NextAt == nil || !r.NextAt.Equal(want) {
		t.Errorf("next_attempt_at = %v, want %v", r.NextAt, want)
	}
	if e.meter.errs[KindOutputDir] != 1 {
		t.Errorf("error metric: %v", e.meter.errs)
	}

	// Honours the backoff: a sweep 1 minute later does not touch the row.
	calls := len(e.disk.removed)
	sw := e.sweeper()
	sw.SetClock(func() time.Time { return sweepT0.Add(time.Minute) })
	sw.Sweep(t.Context())
	if len(e.disk.removed) != calls || r.Attempts != 1 {
		t.Error("row inside its backoff window must not be retried")
	}

	// After the backoff it is retried (and fails again: attempts 2, wait 10 min).
	sw.SetClock(func() time.Time { return sweepT0.Add(6 * time.Minute) })
	sw.Sweep(t.Context())
	if r.Attempts != 2 {
		t.Fatalf("attempts after retry = %d", r.Attempts)
	}
	if want := sweepT0.Add(6*time.Minute + 10*time.Minute); !r.NextAt.Equal(want) {
		t.Errorf("second backoff = %v, want %v", r.NextAt, want)
	}
}

func TestBackoffDoublesAndCapsAtSixHours(t *testing.T) {
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute,
		160 * time.Minute, 320 * time.Minute, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		if got := Backoff(i); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i, got, w)
		}
	}
	if Backoff(-3) != 5*time.Minute || Backoff(500) != 6*time.Hour {
		t.Error("Backoff must clamp")
	}
}

func TestSweepStopsRetryingAtMaxAttemptsAndKeepsRow(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-stuck")
	e.disk.failOn[dir] = errBoom
	row := e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-stuck", JobID: "job-stuck", OwnerSession: "s", Attempts: 9})
	row.Attempts = 9

	res := e.sweep()

	if row.Attempts != 10 {
		t.Fatalf("attempts = %d", row.Attempts)
	}
	if res.PerKind[KindOutputDir].GaveUp != 1 || e.meter.gaveUp[KindOutputDir] != 1 {
		t.Errorf("give-up not counted: %+v %v", res.PerKind[KindOutputDir], e.meter.gaveUp)
	}
	// Never claimed again, even far in the future; still there for a human.
	sw := e.sweeper()
	sw.SetClock(func() time.Time { return sweepT0.Add(72 * time.Hour) })
	before := len(e.disk.removed)
	r2 := sw.Sweep(t.Context())
	if len(e.disk.removed) != before {
		t.Error("a row at max attempts must not be retried")
	}
	if row.DeletedAt != nil || !exists(dir) {
		t.Error("the row and its files are kept for a human")
	}
	if r2.GaveUpRows != 1 {
		t.Errorf("backlog should report 1 stuck row, got %d", r2.GaveUpRows)
	}
}

func TestSweepBatchesUntilEmpty(t *testing.T) {
	e := newSweepEnv(t)
	e.cfg.Batch = 3
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("job-%d", i)
		e.jobDir(e.outputs, id)
		e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: id, JobID: id, OwnerSession: "s"})
	}
	res := e.sweep()
	if res.PerKind[KindOutputDir].Deleted != 8 {
		t.Fatalf("deleted %d of 8", res.PerKind[KindOutputDir].Deleted)
	}
	if e.db.claims < 3 {
		t.Errorf("expected several claim batches, got %d", e.db.claims)
	}
}

func TestSweepBudgetStopsTheTick(t *testing.T) {
	e := newSweepEnv(t)
	e.cfg.Batch = 1
	e.cfg.Budget = time.Minute
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("job-%d", i)
		e.jobDir(e.outputs, id)
		e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: id, JobID: id, OwnerSession: "s"})
	}
	sw := e.sweeper()
	now := sweepT0
	sw.SetClock(func() time.Time { now = now.Add(40 * time.Second); return now }) // each look at the clock burns 40s
	res := sw.Sweep(t.Context())
	if !res.Truncated {
		t.Error("budget exhaustion should be reported")
	}
	if got := res.PerKind[KindOutputDir]; got == nil || got.Deleted >= 5 {
		t.Errorf("tick should stop before finishing the backlog: %+v", got)
	}
}

// --- S3 ----------------------------------------------------------------------

func TestSweepS3BatchOf1001KeysSplits1000Plus1(t *testing.T) {
	e := newSweepEnv(t)
	e.cfg.Batch = 5000
	for i := 0; i < 1001; i++ {
		e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: fmt.Sprintf("results/20261001/s/%04d.tar.gz", i), OwnerSession: "s"})
	}
	e.sweep()
	if len(e.s3.calls) != 2 || len(e.s3.calls[0]) != 1000 || len(e.s3.calls[1]) != 1 {
		var sizes []int
		for _, c := range e.s3.calls {
			sizes = append(sizes, len(c))
		}
		t.Fatalf("DeleteObjects calls = %v, want [1000 1]", sizes)
	}
	for _, r := range e.db.rows {
		if r.DeletedAt == nil {
			t.Fatalf("row %s not stamped", r.Locator)
		}
	}
	for _, b := range e.s3.buckets {
		if b != "b" {
			t.Errorf("wrong bucket %q", b)
		}
	}
}

func TestSweepS3IncludesDerivedKeysInOneCall(t *testing.T) {
	e := newSweepEnv(t)
	orig := "studio/20261001/s/aaa.mp4"
	e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: orig, OwnerSession: "s"})
	for _, d := range []string{"studio/20261001/s/a_proxy.mp4", "studio/20261001/s/a_sprite.jpg", "studio/20261001/s/a_peaks.json"} {
		// Derivatives are not themselves due: they ride with the parent.
		e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: d, ParentLocator: orig, OwnerSession: "s",
			ExpiresAt: sweepT0.Add(24 * time.Hour)})
	}
	e.sweep()
	if len(e.s3.calls) != 1 || len(e.s3.calls[0]) != 4 {
		t.Fatalf("want one call with 4 keys, got %v", e.s3.calls)
	}
	for _, r := range e.db.rows {
		if r.DeletedAt == nil {
			t.Errorf("derived row %s should be stamped with its parent", r.Locator)
		}
	}
}

func TestSweepS3NoSuchKeyIsSuccess(t *testing.T) {
	e := newSweepEnv(t)
	e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: "results/20261001/s/gone.tar.gz", OwnerSession: "s"})
	e.sweep() // the fake treats an absent key as deleted, exactly like S3
	if e.db.rows[e.db.order[0]].DeletedAt == nil {
		t.Error("an already-gone key counts as success")
	}
}

func TestSweepS3FailureRetriesWithBackoff(t *testing.T) {
	e := newSweepEnv(t)
	key := "results/20261001/s/x.tar.gz"
	e.s3.failKey[key] = errors.New("AccessDenied")
	row := e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: key, OwnerSession: "s"})
	e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: "results/20261001/s/y.tar.gz", OwnerSession: "s"})

	e.sweep()

	if row.DeletedAt != nil || row.Attempts != 1 || !row.NextAt.Equal(sweepT0.Add(5*time.Minute)) {
		t.Errorf("failed key row: %+v", row)
	}
	if e.db.byLocator(KindS3Object, "results/20261001/s/y.tar.gz").DeletedAt == nil {
		t.Error("one bad key must not block the others in the batch")
	}
}

func TestSweepS3WholeCallFailureFailsEveryRowInChunk(t *testing.T) {
	e := newSweepEnv(t)
	e.s3.failAll = errBoom
	a := e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: "videos/20261001/s/a.mp4", OwnerSession: "s"})
	b := e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: "videos/20261001/s/b.mp4", OwnerSession: "s"})
	e.sweep()
	if a.Attempts != 1 || b.Attempts != 1 || a.DeletedAt != nil || b.DeletedAt != nil {
		t.Error("whole-call failure must fail both rows retryably")
	}
}

func TestSweepS3RefusesKeysOutsideAllowedPrefixes(t *testing.T) {
	e := newSweepEnv(t)
	for _, k := range []string{"double-raven/desktop/mac/x.dmg", "documents/d/assets/a.png", "feedback/c/a.png", "results/../documents/x", "results/", "x"} {
		e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: k, OwnerSession: "s"})
	}
	e.sweep()
	if len(e.s3.calls) != 0 {
		t.Fatalf("keys outside videos/ results/ studio/ must never reach S3: %v", e.s3.calls)
	}
}

func TestSweepS3NotConfiguredLeavesRowsRetryable(t *testing.T) {
	e := newSweepEnv(t)
	row := e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: "results/20261001/s/x", OwnerSession: "s"})
	sw := NewSweeper(e.db, e.disk, nil, nil, e.meter, nil, e.cfg)
	sw.SetClock(func() time.Time { return sweepT0 })
	sw.Sweep(t.Context())
	if row.DeletedAt != nil || row.Attempts != 1 {
		t.Errorf("no S3 client: row stays and counts an attempt: %+v", row)
	}
}

// --- Darkroom ------------------------------------------------------------------

func TestSweepStudioAssetDeletesKeysThenRow(t *testing.T) {
	e := newSweepEnv(t)
	e.db.assets["asset-1"] = []string{"studio/20261001/s/o.mp4", "studio/20261001/s/o_proxy.mp4"}
	row := e.db.add(telemetry.LedgerRow{Kind: KindStudioAsset, Locator: "asset-1", ProjectID: "proj-1", OwnerSession: "s"})
	e.sweep()
	if row.DeletedAt == nil {
		t.Fatal("asset row should be done")
	}
	if _, still := e.db.assets["asset-1"]; still {
		t.Error("asset DB row should be deleted")
	}
	if len(e.s3.allKeys()) != 2 {
		t.Errorf("keys deleted: %v", e.s3.allKeys())
	}
}

func TestSweepStudioAssetKeepsRowWhenS3Fails(t *testing.T) {
	e := newSweepEnv(t)
	e.db.assets["asset-1"] = []string{"studio/20261001/s/o.mp4"}
	e.s3.failKey["studio/20261001/s/o.mp4"] = errBoom
	row := e.db.add(telemetry.LedgerRow{Kind: KindStudioAsset, Locator: "asset-1", ProjectID: "proj-1", OwnerSession: "s"})
	e.sweep()
	if _, still := e.db.assets["asset-1"]; !still {
		t.Fatal("the asset row names the keys; it must survive a failed S3 delete")
	}
	if row.DeletedAt != nil || row.Attempts != 1 {
		t.Errorf("asset ledger row must stay retryable: %+v", row)
	}
}

func projectFixture(e *sweepEnv) (*fakeRow, []string) {
	keys := []string{"studio/20261001/s/a.mp4", "studio/20261001/s/b.wav"}
	e.db.projects["proj-1"] = true
	e.db.assets["asset-a"], e.db.assetProject["asset-a"] = keys[:1], "proj-1"
	e.db.assets["asset-b"], e.db.assetProject["asset-b"] = keys[1:], "proj-1"
	e.db.add(telemetry.LedgerRow{Kind: KindStudioAsset, Locator: "asset-a", ProjectID: "proj-1", OwnerSession: "s",
		ExpiresAt: sweepT0.Add(time.Hour)})
	for _, k := range keys {
		e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: k, ProjectID: "proj-1", OwnerSession: "s",
			ExpiresAt: sweepT0.Add(time.Hour)})
	}
	return e.db.add(telemetry.LedgerRow{Kind: KindStudioProject, Locator: "proj-1", ProjectID: "proj-1", OwnerSession: "s"}), keys
}

func TestSweepStudioProjectCascadeRowsFirstThenS3(t *testing.T) {
	e := newSweepEnv(t)
	row, keys := projectFixture(e)

	// Observe the order: when DeleteObjects runs, the project's rows must already be gone.
	rowsGoneAtS3Time := false
	e.s3.failKey = map[string]error{}
	orig := e.s3
	probe := &orderProbe{inner: orig, check: func() { rowsGoneAtS3Time = !e.db.projects["proj-1"] && len(e.db.assets) == 0 }}
	sw := NewSweeper(e.db, e.disk, probe, nil, e.meter, nil, e.cfg)
	sw.SetClock(func() time.Time { return sweepT0 })
	sw.Sweep(t.Context())

	if !rowsGoneAtS3Time {
		t.Error("cascade order: project/version/asset rows must be deleted BEFORE the S3 keys")
	}
	if row.DeletedAt == nil {
		t.Fatal("project ledger row should be done")
	}
	for _, k := range keys {
		if !orig.gone[k] {
			t.Errorf("key %s not deleted", k)
		}
		if e.db.byLocator(KindS3Object, k).DeletedAt == nil {
			t.Errorf("s3 ledger row %s should be stamped", k)
		}
	}
	if e.db.byLocator(KindStudioAsset, "asset-a").DeletedAt == nil {
		t.Error("the project's asset ledger rows should be stamped with it")
	}
}

type orderProbe struct {
	inner *fakeS3
	check func()
}

func (p *orderProbe) DeleteObjects(ctx context.Context, b string, k []string) (map[string]error, error) {
	p.check()
	return p.inner.DeleteObjects(ctx, b, k)
}

func TestSweepStudioProjectPartialS3FailureLeavesRowRetryable(t *testing.T) {
	e := newSweepEnv(t)
	row, keys := projectFixture(e)
	e.s3.failKey[keys[1]] = errBoom

	e.sweep()

	if e.db.projects["proj-1"] || len(e.db.assets) != 0 {
		t.Error("rows are deleted first and stay deleted")
	}
	if row.DeletedAt != nil || row.Attempts != 1 {
		t.Fatalf("partial S3 failure must leave the project retryable: %+v", row)
	}
	if e.db.byLocator(KindS3Object, keys[0]).DeletedAt == nil {
		t.Error("the key that did go is stamped")
	}
	if e.db.byLocator(KindS3Object, keys[1]).DeletedAt != nil {
		t.Error("the failed key must stay live in the ledger")
	}

	// Retry after backoff, S3 healthy now: the keys are found through the LEDGER
	// (the asset rows are gone) and the project completes.
	delete(e.s3.failKey, keys[1])
	sw := e.sweeper()
	sw.SetClock(func() time.Time { return sweepT0.Add(10 * time.Minute) })
	sw.Sweep(t.Context())
	if row.DeletedAt == nil {
		t.Fatalf("retry should finish the project: %+v", row)
	}
	if !e.s3.gone[keys[1]] {
		t.Error("retry must delete the remaining key")
	}
}

func TestSweepStudioProjectRowFailureDeletesNoS3(t *testing.T) {
	e := newSweepEnv(t)
	row, _ := projectFixture(e)
	e.db.failOn["project-rows"] = errBoom
	e.sweep()
	if len(e.s3.calls) != 0 {
		t.Error("if the row cascade fails, S3 must not be touched")
	}
	if row.Attempts != 1 || row.DeletedAt != nil {
		t.Errorf("row: %+v", row)
	}
}

// --- job_row / unknown / metadata ----------------------------------------------

func TestSweepJobRowAnonymizes(t *testing.T) {
	e := newSweepEnv(t)
	row := e.db.add(telemetry.LedgerRow{Kind: KindJobRow, Locator: "job-9", JobID: "job-9", OwnerSession: "s"})
	e.sweep()
	if row.DeletedAt == nil || len(e.db.anonymized) != 1 || e.db.anonymized[0] != "job-9" {
		t.Errorf("job_row: %+v anonymized=%v", row, e.db.anonymized)
	}
}

func TestSweepAnonymizesJobRowWhenLastFileRowGoes(t *testing.T) {
	e := newSweepEnv(t)
	e.jobDir(e.outputs, "job-1")
	e.jobDir(e.uploads, "job-1")
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-1", JobID: "job-1", OwnerSession: "s"})
	e.db.add(telemetry.LedgerRow{Kind: KindUploadDir, Locator: "job-1", JobID: "job-1", OwnerSession: "s",
		ExpiresAt: sweepT0.Add(time.Hour)}) // not due yet
	e.sweep()
	if len(e.db.anonymized) != 0 {
		t.Error("the job row must not be anonymized while one of its files is still live")
	}
	sw := e.sweeper()
	sw.SetClock(func() time.Time { return sweepT0.Add(2 * time.Hour) })
	sw.Sweep(t.Context())
	if len(e.db.anonymized) == 0 {
		t.Error("once the last file row is gone the job row is anonymized")
	}
}

func TestSweepUnknownKindIsLoggedAndLeft(t *testing.T) {
	e := newSweepEnv(t)
	row := e.db.add(telemetry.LedgerRow{Kind: "hologram", Locator: "x", OwnerSession: "s"})
	e.sweep()
	if row.DeletedAt != nil || row.Attempts != 0 {
		t.Errorf("unknown kinds are left untouched without burning attempts: %+v", row)
	}
	if row.NextAt == nil || !row.NextAt.After(sweepT0.Add(time.Hour)) {
		t.Error("unknown kinds are pushed out so they do not spin every tick")
	}
}

// --- metadata reports (ADR-0010) -----------------------------------------------

func TestSweepOutputDirKeepsMetadataDocumentsForAccountsThatKeepThem(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-m", "converted.mp4", "metadata.json", "metadata.result.json", "frames.zip")
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-m", JobID: "job-m", OwnerUserID: user1})

	e.sweep()

	if exists(filepath.Join(dir, "converted.mp4")) || exists(filepath.Join(dir, "frames.zip")) {
		t.Error("the media itself must go")
	}
	if !exists(filepath.Join(dir, "metadata.json")) || !exists(filepath.Join(dir, "metadata.result.json")) {
		t.Fatal("metadata reports must survive the file expiry (ADR-0010 default)")
	}
	if len(e.db.parked) != 1 {
		t.Fatal("a parked metadata_report ledger row must record that they are kept")
	}
	m := e.db.byLocator(KindMetadata, "job-m")
	if m == nil || !m.KeepWithAccount || !m.ExpiresAt.Equal(telemetry.ParkedExpiry) {
		t.Errorf("parked row: %+v", m)
	}
	if e.db.byLocator(KindOutputDir, "job-m").DeletedAt == nil {
		t.Error("the output_dir row itself is done")
	}
	// A later tick, years on, never touches the parked row.
	sw := e.sweeper()
	sw.SetClock(func() time.Time { return sweepT0.Add(5 * 365 * 24 * time.Hour) })
	sw.Sweep(t.Context())
	if !exists(filepath.Join(dir, "metadata.json")) {
		t.Error("parked reports survive until the account expires them")
	}
}

func TestSweepOutputDirDeletesMetadataWhenAccountOptedIn(t *testing.T) {
	e := newSweepEnv(t)
	e.db.keepMeta[user1] = false // delete_metadata_reports = true
	dir := e.jobDir(e.outputs, "job-m", "converted.mp4", "metadata.json")
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-m", JobID: "job-m", OwnerUserID: user1})
	e.sweep()
	if exists(dir) {
		t.Error("with delete_metadata_reports on the whole directory goes")
	}
	if len(e.db.parked) != 0 {
		t.Error("nothing is parked")
	}
}

func TestSweepOutputDirGuestMetadataGoesWithTheFiles(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-g", "converted.mp4", "metadata.json")
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-g", JobID: "job-g", OwnerSession: "sess"})
	e.sweep()
	if exists(dir) || len(e.db.parked) != 0 {
		t.Error("guests keep nothing: the 24 h promise covers metadata too")
	}
}

func TestSweepMetadataParkFailureLeavesDirectoryIntact(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-m", "converted.mp4", "metadata.json")
	e.db.failOn["park"] = errBoom
	row := e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-m", JobID: "job-m", OwnerUserID: user1})
	e.sweep()
	if !exists(filepath.Join(dir, "metadata.json")) || !exists(filepath.Join(dir, "converted.mp4")) {
		t.Error("park first, delete second: a failed park must leave everything in place")
	}
	if row.Attempts != 1 || row.DeletedAt != nil {
		t.Errorf("row must be retryable: %+v", row)
	}
}

func TestSweepSettingsErrorNeverDeletesMetadata(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-m", "converted.mp4", "metadata.json")
	e.db.failOn["settings"] = errBoom
	row := e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-m", JobID: "job-m", OwnerUserID: user1})
	e.sweep()
	if !exists(filepath.Join(dir, "metadata.json")) || row.Attempts != 1 {
		t.Error("if the setting cannot be read, nothing is deleted")
	}
}

func TestSweepParkedMetadataDeletedWhenAccountOptsIn(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-m", "metadata.json", "metadata.result.json")
	// The user turned delete_metadata_reports on; UnparkMetadataReports made the row due.
	row := e.db.add(telemetry.LedgerRow{Kind: KindMetadata, Locator: "job-m", JobID: "job-m", OwnerUserID: user1, KeepWithAccount: true})
	e.db.keepMeta[user1] = false
	e.sweep()
	if row.DeletedAt == nil || exists(filepath.Join(dir, "metadata.json")) {
		t.Errorf("due parked report must be deleted: %+v", row)
	}
	if exists(dir) {
		t.Error("the now-empty directory is removed too")
	}
}

func TestSweepDueMetadataRowForKeepingAccountIsRestampedNotDeleted(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-m", "metadata.json")
	row := e.db.add(telemetry.LedgerRow{Kind: KindMetadata, Locator: "job-m", JobID: "job-m", OwnerUserID: user1})
	e.sweep()
	if row.DeletedAt != nil || !row.KeepWithAccount || !row.ExpiresAt.Equal(telemetry.ParkedExpiry) {
		t.Errorf("must re-stamp to the account-lifetime sentinel: %+v", row)
	}
	if !exists(filepath.Join(dir, "metadata.json")) {
		t.Error("document kept")
	}
}

// --- dry run ---------------------------------------------------------------------

func TestDryRunChangesNothing(t *testing.T) {
	e := newSweepEnv(t)
	e.cfg.DryRun = true
	dir := e.jobDir(e.outputs, "job-d")
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-d", JobID: "job-d", OwnerSession: "s", SizeBytes: 7})
	e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: "results/20261001/s/x", OwnerSession: "s"})

	res := e.sweep()

	if !exists(dir) || len(e.disk.removed) != 0 || len(e.s3.calls) != 0 {
		t.Fatal("dry run must not touch disk or S3")
	}
	for _, r := range e.db.rows {
		if r.DeletedAt != nil || r.NextAt != nil || r.Attempts != 0 {
			t.Errorf("dry run must not modify ledger rows (no lease, no stamp): %+v", r)
		}
	}
	if e.db.claims != 0 {
		t.Error("dry run must not claim (lease) rows")
	}
	if !res.DryRun || res.PerKind[KindOutputDir].Would != 1 || res.PerKind[KindS3Object].Would != 1 || res.Total() != 2 {
		t.Errorf("dry-run report wrong: %+v", res.PerKind)
	}
	if e.meter.would[KindOutputDir] != 1 || len(e.meter.swept) != 0 {
		t.Errorf("dry run meters would_delete, not swept: %v %v", e.meter.would, e.meter.swept)
	}
	if res.BacklogDue != 2 {
		t.Errorf("backlog = %d", res.BacklogDue)
	}
}

func TestDryRunPagesThroughTheWholeBacklog(t *testing.T) {
	e := newSweepEnv(t)
	e.cfg.DryRun, e.cfg.Batch = true, 2
	for i := 0; i < 7; i++ {
		e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: fmt.Sprintf("j%d", i), OwnerSession: "s"})
	}
	if got := e.sweep().PerKind[KindOutputDir].Would; got != 7 {
		t.Errorf("dry run saw %d of 7", got)
	}
}

func TestSweepNoLedgerRowsMeansNothingIsDeleted(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "pre-mm22-job")
	res := e.sweep()
	if !exists(dir) || len(e.disk.removed) != 0 || len(e.s3.calls) != 0 || res.Total() != 0 {
		t.Error("the sweeper only ever acts on ledger rows")
	}
}

func TestSweepResultPropertiesAreAuditable(t *testing.T) {
	e := newSweepEnv(t)
	e.jobDir(e.outputs, "j1")
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "j1", JobID: "j1", OwnerSession: "s", SizeBytes: 2})
	res := e.sweep()
	p := res.Properties()
	kinds, _ := p["kinds"].(map[string]any)
	if p["sweeper"] != true || p["dryRun"] != false || kinds[KindOutputDir] == nil {
		t.Errorf("properties: %v", p)
	}
	if len(res.Paths) != 1 || res.Paths[0].PathType != "dir" || res.Paths[0].PathRedacted != "output_dir/j1" {
		t.Errorf("audit paths: %+v", res.Paths)
	}
}

func TestJobDirPathContainment(t *testing.T) {
	root := t.TempDir()
	for _, ok := range []string{"abc", "0d3f-uuid"} {
		if JobDirPath(root, ok) == "" {
			t.Errorf("%q should resolve", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", ".hidden", `a\b`, "/abs"} {
		if p := JobDirPath(root, bad); p != "" {
			t.Errorf("%q must not resolve, got %q", bad, p)
		}
	}
	if JobDirPath("", "abc") != "" {
		t.Error("unset root must not resolve")
	}
}
