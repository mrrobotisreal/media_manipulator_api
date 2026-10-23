package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// pngBody is a 1000-byte PNG-signature payload (the sniffer needs only the head).
func pngBody() []byte {
	b := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{7}, 1000)...)
	return b[:1000]
}

// resultFixture creates a completed png job whose output exists on disk and
// returns a router with the conversion routes mounted.
func resultFixture(t *testing.T, owner *services.JobOwner, name string, body []byte) (*gin.Engine, *services.JobManager, *models.ConversionJob) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	jm := services.NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "photo.png", Type: "image/png"}, map[string]interface{}{"format": "png"})
	h := &ConversionHandler{cfg: &config.Config{OutputDir: dir}, jobManager: jm}
	out := h.outputPath(job, filepath.Join(dir, job.ID))
	if name != "" {
		out = filepath.Join(dir, job.ID, name)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, body, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"transcribe_result.json", "analysis.json"} {
		if err := os.WriteFile(filepath.Join(dir, job.ID, side), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if owner != nil {
		jm.SetJobOwner(job.ID, *owner)
	}
	_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	r := gin.New()
	r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{}, Registry: tiers.New(&config.Config{})}, true))
	RegisterConversionRoutes(r, h)
	return r, jm, job
}

func doGet(r *gin.Engine, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestInlineResultRangeAndHeaders(t *testing.T) {
	SetResultOwnershipEnforce(true)
	body := pngBody()
	r, _, job := resultFixture(t, nil, "", body)

	t.Run("no range gives 200 inline with Accept-Ranges", func(t *testing.T) {
		w := doGet(r, "/result/"+job.ID, nil)
		if w.Code != 200 {
			t.Fatalf("code %d", w.Code)
		}
		if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
			t.Errorf("Accept-Ranges %q", got)
		}
		if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
			t.Errorf("disposition %q", got)
		}
		if got := w.Header().Get("Content-Type"); got != "image/png" {
			t.Errorf("content-type %q", got)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("nosniff %q", got)
		}
		if got := w.Header().Get("Cache-Control"); got != "private, max-age=300" {
			t.Errorf("cache %q", got)
		}
		if !bytes.Equal(w.Body.Bytes(), body) {
			t.Error("body mismatch")
		}
	})
	t.Run("range 0-99 gives 206", func(t *testing.T) {
		w := doGet(r, "/result/"+job.ID, map[string]string{"Range": "bytes=0-99"})
		if w.Code != http.StatusPartialContent {
			t.Fatalf("code %d", w.Code)
		}
		if got := w.Header().Get("Content-Range"); got != "bytes 0-99/1000" {
			t.Errorf("Content-Range %q", got)
		}
		if got := w.Header().Get("Content-Length"); got != "100" {
			t.Errorf("Content-Length %q", got)
		}
		if !bytes.Equal(w.Body.Bytes(), body[:100]) {
			t.Error("range body mismatch")
		}
	})
	t.Run("unsatisfiable range gives 416", func(t *testing.T) {
		w := doGet(r, "/result/"+job.ID, map[string]string{"Range": "bytes=5000-6000"})
		if w.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("download stays attachment octet-stream", func(t *testing.T) {
		w := doGet(r, "/download/"+job.ID, nil)
		if w.Code != 200 {
			t.Fatalf("code %d", w.Code)
		}
		if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
			t.Errorf("disposition %q", got)
		}
		if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("content-type %q", got)
		}
	})
}

func TestInlineResultUnsafeMimeIsAttachment(t *testing.T) {
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><script>alert(1)</script></svg>`)
	r, jm, job := resultFixture(t, nil, "", svg)
	w := doGet(r, "/result/"+job.ID, nil)
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("content-type %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("disposition %q", got)
	}
	if j, _ := jm.GetJob(job.ID); j.ResultMime != "image/svg+xml" {
		t.Errorf("stored mime %q", j.ResultMime)
	}
}

func TestResultAndDownloadOwnership(t *testing.T) {
	SetResultOwnershipEnforce(true)
	owner := &services.JobOwner{SessionID: "sess-owner"}
	r, _, job := resultFixture(t, owner, "", pngBody())
	missing := doGet(r, "/result/does-not-exist", nil)

	for _, route := range []string{"/result/", "/download/"} {
		t.Run(route, func(t *testing.T) {
			if w := doGet(r, route+job.ID, map[string]string{"X-MM-Session-ID": "sess-owner"}); w.Code != 200 {
				t.Errorf("owner header: %d", w.Code)
			}
			w := doGet(r, route+job.ID, map[string]string{"X-MM-Session-ID": "stranger"})
			if w.Code != 404 || w.Body.String() != missing.Body.String() {
				t.Errorf("stranger: %d %q want 404 %q", w.Code, w.Body.String(), missing.Body.String())
			}
			if w := doGet(r, route+job.ID, nil); w.Code != 404 {
				t.Errorf("no credentials: %d", w.Code)
			}
		})
	}
	// ?sid only on the inline route.
	if w := doGet(r, "/result/"+job.ID+"?sid=sess-owner", nil); w.Code != 200 {
		t.Errorf("/result ?sid: %d", w.Code)
	}
	if w := doGet(r, "/download/"+job.ID+"?sid=sess-owner", nil); w.Code != 404 {
		t.Errorf("/download ?sid must be rejected: %d", w.Code)
	}
}

func TestJobStatusJSONResultMimeOmittedUntilResult(t *testing.T) {
	jm := services.NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "a.png"}, map[string]interface{}{})
	before := mustJSON(t, job)
	if strings.Contains(before, "resultMime") {
		t.Errorf("resultMime must be omitted before a result: %s", before)
	}
	_ = jm.SetResultMime(job.ID, "image/png")
	got, _ := jm.GetJob(job.ID)
	if after := mustJSON(t, got); !strings.Contains(after, `"resultMime":"image/png"`) {
		t.Errorf("resultMime missing: %s", after)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTranscriptAndAnalysisOwnership(t *testing.T) {
	SetResultOwnershipEnforce(true)
	owner := &services.JobOwner{SessionID: "sess-owner"}
	r, jm, job := resultFixture(t, owner, "", pngBody())
	_ = jm
	missing := doGet(r, "/transcript/does-not-exist", nil)
	for _, route := range []string{"/transcript/", "/analysis/"} {
		if w := doGet(r, route+job.ID, map[string]string{"X-MM-Session-ID": "sess-owner"}); w.Code != 200 {
			t.Errorf("%s owner: %d", route, w.Code)
		}
		w := doGet(r, route+job.ID, map[string]string{"X-MM-Session-ID": "stranger"})
		if w.Code != 404 || w.Body.String() != missing.Body.String() {
			t.Errorf("%s stranger: %d %q", route, w.Code, w.Body.String())
		}
		if w := doGet(r, route+job.ID+"?sid=sess-owner", nil); w.Code != 404 {
			t.Errorf("%s ?sid must be rejected: %d", route, w.Code)
		}
	}
}
