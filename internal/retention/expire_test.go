package retention

import (
	"context"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// ExpireStore on the in-memory ledger, with the same semantics as the SQL.

func (f *fakeLedgerDB) matches(r *fakeRow, fl telemetry.LedgerFilter) bool {
	if r.DeletedAt != nil {
		return false
	}
	switch {
	case fl.UserID != "":
		if r.OwnerUserID != fl.UserID {
			return false
		}
	case fl.SessionID != "" && fl.JobID == "" && fl.ProjectID == "":
		if r.OwnerUserID != "" || r.OwnerSession != fl.SessionID {
			return false
		}
	}
	if fl.JobID != "" && r.JobID != fl.JobID {
		return false
	}
	if fl.ProjectID != "" && r.ProjectID != fl.ProjectID {
		return false
	}
	return true
}

func (f *fakeLedgerDB) ExpireLedger(_ context.Context, fl telemetry.LedgerFilter, now time.Time) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !fl.Valid() {
		return nil, context.Canceled
	}
	var ids []string
	for _, id := range f.order {
		r := f.rows[id]
		if !f.matches(r, fl) {
			continue
		}
		if e := now.Add(-time.Second); r.ExpiresAt.After(e) {
			r.ExpiresAt, r.LedgerRow.ExpiresAt = e, e
		}
		r.NextAt, r.Attempts, r.LedgerRow.Attempts, r.Err = nil, 0, 0, ""
		ids = append(ids, id)
	}
	return ids, nil
}

func (f *fakeLedgerDB) ClaimByIDs(_ context.Context, ids []string, now time.Time, limit int, lease time.Duration) ([]telemetry.LedgerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []telemetry.LedgerRow
	for _, id := range ids {
		r := f.rows[id]
		if r == nil || r.DeletedAt != nil || (r.NextAt != nil && !r.NextAt.Before(now)) || len(out) >= limit {
			continue
		}
		l := now.Add(lease)
		r.NextAt = &l
		out = append(out, r.LedgerRow)
	}
	return out, nil
}

func (f *fakeLedgerDB) PendingByIDs(_ context.Context, ids []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, id := range ids {
		if r := f.rows[id]; r != nil && r.DeletedAt == nil {
			n++
		}
	}
	return n, nil
}

func TestExpireNowDeletesJobRowsWithDerivedKeysAndLeavesOthers(t *testing.T) {
	e := newSweepEnv(t)
	e.cfg.DryRun = true // an inline user delete ignores dry run
	out := e.jobDir(e.outputs, "job-1")
	up := e.jobDir(e.uploads, "job-1", "incoming_a.mp4")
	other := e.jobDir(e.outputs, "job-2")
	future := sweepT0.Add(24 * time.Hour)
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-1", JobID: "job-1", OwnerUserID: user1, ExpiresAt: future})
	e.db.add(telemetry.LedgerRow{Kind: KindUploadDir, Locator: "job-1", JobID: "job-1", OwnerUserID: user1, ExpiresAt: future})
	e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: "results/j1/out.mp4", JobID: "job-1", OwnerUserID: user1, SizeBytes: 9, ExpiresAt: future})
	e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: "results/j1/out.proxy.mp4", ParentLocator: "results/j1/out.mp4", JobID: "job-1", OwnerUserID: user1, ExpiresAt: future})
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-2", JobID: "job-2", OwnerUserID: user1, ExpiresAt: future})

	res, err := e.sweeper().ExpireNow(context.Background(), telemetry.LedgerFilter{JobID: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pending != 0 {
		t.Errorf("pending %d", res.Pending)
	}
	if exists(out) || exists(up) {
		t.Error("job directories must be gone")
	}
	if !exists(other) {
		t.Error("another job must be untouched")
	}
	keys := e.s3.allKeys()
	if !has(keys, "results/j1/out.mp4") || !has(keys, "results/j1/out.proxy.mp4") {
		t.Errorf("DeleteObjects must see parent and derived keys: %v", keys)
	}
	if e.db.byLocator(KindOutputDir, "job-2").DeletedAt != nil {
		t.Error("other job's ledger row must stay live")
	}
	if res.Objects != 4 {
		t.Errorf("objects %d (output, upload, s3 parent, s3 derived)", res.Objects)
	}
	// Idempotent.
	again, _ := e.sweeper().ExpireNow(context.Background(), telemetry.LedgerFilter{JobID: "job-1"})
	if again.Matched != 0 || again.Pending != 0 || again.Objects != 0 {
		t.Errorf("repeat should match nothing: %+v", again)
	}
}

func TestExpireNowS3FailureReportsPendingAndStaysRetryable(t *testing.T) {
	e := newSweepEnv(t)
	e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: "results/a.bin", JobID: "job-1", OwnerUserID: user1,
		ExpiresAt: sweepT0.Add(time.Hour)})
	e.s3.failKey["results/a.bin"] = errBoom

	res, err := e.sweeper().ExpireNow(context.Background(), telemetry.LedgerFilter{JobID: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pending != 1 || res.Objects != 0 {
		t.Fatalf("want pending 1: %+v", res)
	}
	row := e.db.byLocator(KindS3Object, "results/a.bin")
	if row.DeletedAt != nil || row.Attempts != 1 {
		t.Errorf("row must stay live with one recorded attempt: %+v", row)
	}
	// S3 recovers; the next ordinary sweep (after the backoff) finishes it.
	delete(e.s3.failKey, "results/a.bin")
	sw := e.sweeper()
	sw.SetClock(func() time.Time { return sweepT0.Add(time.Hour) })
	sw.Sweep(context.Background())
	if e.db.byLocator(KindS3Object, "results/a.bin").DeletedAt == nil {
		t.Error("the ticker must finish a row an inline sweep left behind")
	}
}

func TestExpireNowRefusesEmptyFilterAndExpiresParkedMetadata(t *testing.T) {
	e := newSweepEnv(t)
	if _, err := e.sweeper().ExpireNow(context.Background(), telemetry.LedgerFilter{}); err == nil {
		t.Error("an empty filter would mean every row and must be refused")
	}
	dir := e.jobDir(e.outputs, "job-m", "metadata.json", "metadata.result.json")
	row := e.db.add(telemetry.LedgerRow{Kind: KindMetadata, Locator: "job-m", JobID: "job-m", OwnerUserID: user1,
		ExpiresAt: telemetry.ParkedExpiry, KeepWithAccount: true})
	res, err := e.sweeper().ExpireNow(context.Background(), telemetry.LedgerFilter{UserID: user1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pending != 0 || row.DeletedAt == nil {
		t.Errorf("parked report must be deleted by an account-wide expire: %+v", res)
	}
	if exists(dir + "/metadata.json") {
		t.Error("parked metadata file must be removed")
	}
}

func TestExpireNowDeletesMetadataReportsEvenWhenOwnerKeepsThem(t *testing.T) {
	e := newSweepEnv(t)
	e.db.keepMeta[user1] = true // the default: keep reports past file expiry
	dir := e.jobDir(e.outputs, "job-k", "converted.png", "metadata.json")
	e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-k", JobID: "job-k", OwnerUserID: user1, ExpiresAt: sweepT0.Add(time.Hour)})

	res, err := e.sweeper().ExpireNow(context.Background(), telemetry.LedgerFilter{JobID: "job-k"})
	if err != nil || res.Pending != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if exists(dir) {
		t.Error("a user delete removes the whole directory, metadata report included")
	}
	if len(e.db.parked) != 0 {
		t.Errorf("nothing may be parked by a user delete: %v", e.db.parked)
	}
}
