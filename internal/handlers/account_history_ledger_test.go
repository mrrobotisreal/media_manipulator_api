package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// fakeHistory is Postgres + the ledger for the History route.
type fakeHistory struct {
	jobs   []models.ConversionHistoryEntry
	ledger []telemetry.JobLedgerEntry
	hours  int
	ledErr error
}

func (f *fakeHistory) ConversionHistoryForUser(context.Context, string, int, int) ([]models.ConversionHistoryEntry, int, error) {
	return f.jobs, len(f.jobs), nil
}
func (f *fakeHistory) LedgerForJobs(context.Context, []string) ([]telemetry.JobLedgerEntry, error) {
	return f.ledger, f.ledErr
}
func (f *fakeHistory) UserRetentionHours(context.Context, string) (int, bool, error) {
	return f.hours, f.hours > 0, nil
}

func historyEntries(t *testing.T, h *AccountHandler, src *fakeHistory, now time.Time) []historyEntry {
	t.Helper()
	h.presignFn = func(_ context.Context, key, _ string) string { return "https://signed.example/" + key }
	return h.buildHistoryEntries(context.Background(), &models.User{ID: "u", Tier: "free"}, src.jobs, now)
}

// Acceptance 5: after a simulated restart (the job map is EMPTY: the handler has
// no job lookup at all) a completed local job and a completed S3 job still list
// with deletesOn and a download link, and expired / deleted rows show expired.
func TestHistoryAfterRestartServedFromPostgresAndLedger(t *testing.T) {
	cfg := config.Load()
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil) // jobs == nil: nothing in memory
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	created := now.Add(-48 * time.Hour)
	src := &fakeHistory{hours: 4320,
		jobs: []models.ConversionHistoryEntry{
			{JobID: "local-live", Status: "completed", CreatedAt: created},
			{JobID: "s3-live", Status: "completed", CreatedAt: created, ResultS3Key: "results/20261021/s/s3-live/a.tar.gz"},
			{JobID: "local-swept", Status: "completed", CreatedAt: created},
			{JobID: "local-due", Status: "completed", CreatedAt: created},
			{JobID: "no-ledger", Status: "completed", CreatedAt: created},
		},
		ledger: []telemetry.JobLedgerEntry{
			{ID: "r1", Kind: "output_dir", Locator: "local-live", JobID: "local-live", CreatedAt: created, ExpiresAt: created.Add(4320 * time.Hour)},
			{ID: "r2", Kind: "s3_object", Locator: "results/20261021/s/s3-live/a.tar.gz", JobID: "s3-live", CreatedAt: created, ExpiresAt: created.Add(4320 * time.Hour)},
			{ID: "r3", Kind: "output_dir", Locator: "local-swept", JobID: "local-swept", CreatedAt: created, ExpiresAt: now.Add(-time.Hour), DeletedAt: &now},
			{ID: "r4", Kind: "output_dir", Locator: "local-due", JobID: "local-due", CreatedAt: created, ExpiresAt: now.Add(-time.Minute)},
		},
	}
	h.history = src
	got := map[string]historyEntry{}
	for _, e := range historyEntries(t, h, src, now) {
		got[e.JobID] = e
	}

	live := got["local-live"]
	if live.Expired || live.DownloadPath != "/download/local-live" || live.DeletesOn != created.Add(4320*time.Hour).Format(time.RFC3339) || live.RetainedObjectID != "r1" {
		t.Errorf("local live job after restart: %+v", live)
	}
	if live.ResultExpiresAt != live.DeletesOn {
		t.Error("resultExpiresAt keeps mirroring the ledger deadline for older clients")
	}
	s3 := got["s3-live"]
	if s3.Expired || s3.DownloadURL == "" || s3.DeletesOn == "" || s3.RetainedObjectID != "r2" {
		t.Errorf("S3 live job: %+v", s3)
	}
	for _, id := range []string{"local-swept", "local-due"} {
		e := got[id]
		if !e.Expired || e.DownloadPath != "" || e.DownloadURL != "" {
			t.Errorf("%s must be expired with no link: %+v", id, e)
		}
	}
	if !got["local-swept"].Expired || got["local-swept"].RetainedObjectID != "r3" {
		t.Error("a swept row reports expired")
	}
}

func TestHistoryS3ResultNotExpiredWhenPresignTTLPasses(t *testing.T) {
	cfg := config.Load()
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	created := now.Add(-30 * 24 * time.Hour)
	past := now.Add(-29 * 24 * time.Hour) // the long-dead presign expiry stored on the job row
	src := &fakeHistory{hours: 4320, jobs: []models.ConversionHistoryEntry{
		{JobID: "j", Status: "completed", CreatedAt: created, ResultS3Key: "results/k", ResultExpiresAt: &past},
	}, ledger: []telemetry.JobLedgerEntry{
		{ID: "r", Kind: "s3_object", Locator: "results/k", JobID: "j", CreatedAt: created, ExpiresAt: created.Add(4320 * time.Hour)},
	}}
	h.history = src
	e := historyEntries(t, h, src, now)[0]
	if e.Expired || e.DownloadURL == "" {
		t.Errorf("S3 results are re-presigned on demand, not expired by the presign TTL: %+v", e)
	}
}

func TestHistoryKeptLongerWhenDeadlineBeyondWindow(t *testing.T) {
	cfg := config.Load()
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	created := now.Add(-24 * time.Hour)
	src := &fakeHistory{hours: 24, jobs: []models.ConversionHistoryEntry{
		{JobID: "pinned", Status: "completed", CreatedAt: created},
		{JobID: "plain", Status: "completed", CreatedAt: created},
	}, ledger: []telemetry.JobLedgerEntry{
		{ID: "a", Kind: "output_dir", Locator: "pinned", JobID: "pinned", CreatedAt: created, ExpiresAt: created.Add(24*time.Hour + 30*24*time.Hour)},
		{ID: "b", Kind: "output_dir", Locator: "plain", JobID: "plain", CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour)},
	}}
	h.history = src
	got := historyEntries(t, h, src, now)
	if !got[0].KeptLonger || got[1].KeptLonger {
		t.Errorf("keptLonger: pinned=%v plain=%v", got[0].KeptLonger, got[1].KeptLonger)
	}
}

func TestHistoryUnledgeredRowFallsBackToTierEstimate(t *testing.T) {
	cfg := config.Load()
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	now := time.Now().UTC()
	src := &fakeHistory{jobs: []models.ConversionHistoryEntry{
		{JobID: "old", Status: "completed", OwnerTier: "anonymous", CreatedAt: now.Add(-72 * time.Hour)},
	}, ledErr: context.DeadlineExceeded}
	h.history = src
	e := historyEntries(t, h, src, now)[0]
	if !e.Expired || e.RetainedObjectID != "" || e.DeletesOn != "" {
		t.Errorf("pre-MM22 / ledger-down rows keep the tier estimate and add no ledger fields: %+v", e)
	}
}

func TestHistoryNewFieldsAreAdditiveJSON(t *testing.T) {
	b, _ := json.Marshal(historyEntry{JobID: "j", Status: "completed"})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"retainedObjectId", "deletesOn", "keptLonger"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s must be omitted when empty", k)
		}
	}
	for _, k := range []string{"jobId", "status", "createdAt", "expired"} {
		if _, ok := m[k]; !ok {
			t.Errorf("existing field %s must still be present", k)
		}
	}
}
