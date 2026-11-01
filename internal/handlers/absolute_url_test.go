package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

func TestDownloadLinkAbsoluteURL(t *testing.T) {
	SetResultOwnershipEnforce(true)
	owner := map[string]string{"X-MM-Session-ID": "sess-owner"}
	for _, tc := range []struct{ name, env, wantBase string }{
		{"default", "", "https://api.media-manipulator.com"},
		{"explicit", "https://api.example.com", "https://api.example.com"},
		{"trailing slash", "https://api.example.com/", "https://api.example.com"},
		{"api suffix stripped", "https://api.example.com/api", "https://api.example.com"},
		{"api suffix and slash", "https://api.example.com/api/", "https://api.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, h, job := resultFixtureWithHandler(t, &services.JobOwner{SessionID: "sess-owner"}, pngBody())
			h.cfg.PublicAPIBaseURL = config.NormalizePublicAPIBaseURL(tc.env)
			w := doLinkReq(r, http.MethodPost, "/download/"+job.ID+"/link", "", owner)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			var lr struct{ URL, AbsoluteURL string }
			lr2 := map[string]any{}
			_ = json.Unmarshal(w.Body.Bytes(), &lr2)
			lr.URL, _ = lr2["url"].(string)
			lr.AbsoluteURL, _ = lr2["absoluteUrl"].(string)
			if !strings.HasPrefix(lr.URL, "/api/download/") {
				t.Fatalf("relative url changed: %q", lr.URL)
			}
			if lr.AbsoluteURL != tc.wantBase+lr.URL {
				t.Fatalf("absoluteUrl %q want %q", lr.AbsoluteURL, tc.wantBase+lr.URL)
			}
		})
	}
}

func TestDownloadLinkAbsoluteURLOnMP3Ready(t *testing.T) {
	SetResultOwnershipEnforce(true)
	fakeTranscode(t, false)
	installFakeLedger(t, nil)
	r, job, _ := wavStemFixture(t, "wav")
	owner := map[string]string{"X-MM-Session-ID": "sess-owner"}
	doLinkReq(r, http.MethodPost, "/download/"+job.ID+"/mp3", "", owner)
	waitMP3Idle(t, job.ID)
	w := doLinkReq(r, http.MethodPost, "/download/"+job.ID+"/mp3", "", owner)
	m := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	rel, _ := m["url"].(string)
	abs, _ := m["absoluteUrl"].(string)
	if m["status"] != "ready" || rel == "" || abs != "https://api.media-manipulator.com"+rel {
		t.Fatalf("mp3 ready body: %s", w.Body.String())
	}
}

func TestHistoryDownloadAbsoluteURL(t *testing.T) {
	cfg := config.Load()
	cfg.PublicAPIBaseURL = "https://api.example.com"
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	created := now.Add(-time.Hour)
	src := &fakeHistory{hours: 4320,
		jobs: []models.ConversionHistoryEntry{
			{JobID: "local", Status: "completed", CreatedAt: created},
			{JobID: "s3", Status: "completed", CreatedAt: created, ResultS3Key: "results/k"},
		},
		ledger: []telemetry.JobLedgerEntry{
			{ID: "r1", Kind: "output_dir", Locator: "local", JobID: "local", CreatedAt: created, ExpiresAt: created.Add(4320 * time.Hour)},
			{ID: "r2", Kind: "s3_object", Locator: "results/k", JobID: "s3", CreatedAt: created, ExpiresAt: created.Add(4320 * time.Hour)},
		},
	}
	h.history = src
	h.presignFn = func(_ context.Context, key, _ string) string { return "https://signed.example/" + key }
	got := map[string]historyEntry{}
	for _, e := range h.buildHistoryEntries(context.Background(), &models.User{ID: "u", Tier: "free"}, src.jobs, now) {
		got[e.JobID] = e
	}
	if l := got["local"]; l.DownloadPath != "/download/local" || l.DownloadAbsoluteURL != "https://api.example.com/api/download/local" {
		t.Errorf("local row: %+v", l)
	}
	if s := got["s3"]; s.DownloadURL != "https://signed.example/results/k" || s.DownloadAbsoluteURL != "" || s.DownloadPath != "" {
		t.Errorf("presigned row must be unchanged: %+v", s)
	}
}
