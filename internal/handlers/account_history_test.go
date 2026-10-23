package handlers

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

func TestClampedIntBoundsThePage(t *testing.T) {
	cases := []struct {
		raw  string
		def  int
		max  int
		want int
	}{
		{"", 20, 50, 20},
		{"10", 20, 50, 10},
		{"500", 20, 50, 50},  // capped, not rejected
		{"-1", 20, 50, 20},   // nonsense falls back to the default
		{"abc", 20, 50, 20},  // ditto: page one, not an error page
		{"0", 20, 50, 0},     // an explicit zero offset is meaningful
		{"1200", 0, 0, 1200}, // no cap requested
	}
	for _, tc := range cases {
		if got := clampedInt(tc.raw, tc.def, tc.max); got != tc.want {
			t.Errorf("clampedInt(%q, %d, %d): got %d want %d", tc.raw, tc.def, tc.max, got, tc.want)
		}
	}
}

// historyTestHandler builds the handler against config defaults, so the
// retention windows these tests assert on are the published matrix (24h / 7d /
// 30d) rather than fixtures.
func historyTestHandler() *AccountHandler {
	cfg := config.Load()
	return NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
}

func TestResultExpiryPrefersTheRecordedExpiry(t *testing.T) {
	h := historyTestHandler()
	recorded := time.Now().UTC().Add(3 * time.Hour)
	got := h.resultExpiry(models.ConversionHistoryEntry{
		OwnerTier:       string(tiers.TierAnonymous),
		CreatedAt:       time.Now().UTC(),
		ResultExpiresAt: &recorded,
	})
	if got == nil || !got.Equal(recorded) {
		t.Fatalf("an S3 result's own expiry must win: got %v want %v", got, recorded)
	}
}

func TestResultExpiryFallsBackToTheTierRetentionWindow(t *testing.T) {
	h := historyTestHandler()
	created := time.Now().UTC().Add(-48 * time.Hour)

	// Anonymous keeps results 24h, so a job from two days ago is gone...
	anon := h.resultExpiry(models.ConversionHistoryEntry{OwnerTier: string(tiers.TierAnonymous), CreatedAt: created})
	if anon == nil {
		t.Fatal("expected a computed expiry for an anonymous job")
	}
	if !anon.Before(time.Now().UTC()) {
		t.Errorf("a 48h-old anonymous result should be expired, expiry %v", anon)
	}

	// ...while premium keeps it 30 days.
	premium := h.resultExpiry(models.ConversionHistoryEntry{OwnerTier: string(tiers.TierPremium), CreatedAt: created})
	if premium == nil {
		t.Fatal("expected a computed expiry for a premium job")
	}
	if premium.Before(time.Now().UTC()) {
		t.Errorf("a 48h-old premium result should still be live, expiry %v", premium)
	}
}

func TestResultExpiryIsUnknownWithoutAnOwnerTier(t *testing.T) {
	// Rows written while the tier system was switched off carry no owner tier.
	// Guessing a window would risk showing a live result as expired.
	h := historyTestHandler()
	if got := h.resultExpiry(models.ConversionHistoryEntry{CreatedAt: time.Now().UTC().Add(-time.Hour)}); got != nil {
		t.Errorf("expected an unknown expiry, got %v", got)
	}
}

// stubJobLookup stands in for the in-memory job manager.
type stubJobLookup struct {
	job *models.ConversionJob
	err error
}

func (s *stubJobLookup) GetJob(string) (*models.ConversionJob, error) { return s.job, s.err }

func TestLiveLocalResultOnlyClaimsALinkThatWouldWork(t *testing.T) {
	cfg := config.Load()
	cases := []struct {
		name string
		jobs JobLookup
		want bool
	}{
		{"no job manager wired", nil, false},
		{"job gone from memory", &stubJobLookup{err: errors.New("job not found")}, false},
		{"job still running", &stubJobLookup{job: &models.ConversionJob{Status: models.StatusProcessing}}, false},
		{"job failed", &stubJobLookup{job: &models.ConversionJob{Status: models.StatusFailed}}, false},
		{"completed and live", &stubJobLookup{job: &models.ConversionJob{Status: models.StatusCompleted}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, tc.jobs)
			if got := h.liveLocalResult("job-1"); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestPresignResultWithoutAnS3ClientYieldsNoLink(t *testing.T) {
	// No S3 configured: history still lists rows, they just carry no link.
	h := historyTestHandler()
	if url := h.presignResult(t.Context(), "results/2026/a.tar.gz", "a.tar.gz"); url != "" {
		t.Errorf("expected no download url, got %q", url)
	}
}

func TestHistoryMetadataAvailableStatsCachedDocuments(t *testing.T) {
	out := t.TempDir()
	write := func(job, name string) {
		dir := filepath.Join(out, job)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("both", originalMetadataFile)
	write("both", resultMetadataFile)
	write("orig-only", originalMetadataFile)
	write("result-only", resultMetadataFile)
	if err := os.MkdirAll(filepath.Join(out, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		job  string
		want *models.MetadataAvailability
	}{
		{"both", &models.MetadataAvailability{Original: true, Result: true}},
		{"orig-only", &models.MetadataAvailability{Original: true}},
		{"result-only", &models.MetadataAvailability{Result: true}},
		{"empty", nil},
		{"swept-away", nil},
		{"../both", nil},
		{"", nil},
	}
	for _, tc := range cases {
		got := historyMetadataAvailable(out, tc.job)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("job %q: got %+v want %+v", tc.job, got, tc.want)
		}
	}
	if historyMetadataAvailable("", "both") != nil {
		t.Error("no output dir configured must yield nil")
	}
}

func TestHistoryEntryMetadataAvailableIsAdditive(t *testing.T) {
	b, _ := json.Marshal(historyEntry{JobID: "j", Status: "completed"})
	if strings.Contains(string(b), "metadataAvailable") {
		t.Errorf("field must be omitted when nil: %s", b)
	}
	b, _ = json.Marshal(historyEntry{JobID: "j", Expired: true, MetadataAvailable: &models.MetadataAvailability{Result: true}})
	if !strings.Contains(string(b), `"metadataAvailable":{"original":false,"result":true}`) {
		t.Errorf("unexpected JSON: %s", b)
	}
}
