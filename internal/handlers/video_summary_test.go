package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// summaryQuotaStore counts ai_ops in memory and resolves one user.
type summaryQuotaStore struct {
	mu     sync.Mutex
	counts map[string]int64
	user   *models.User
}

func (s *summaryQuotaStore) Enabled() bool { return true }
func (s *summaryQuotaStore) UserByFirebaseUID(context.Context, string) (*models.User, error) {
	return s.user, nil
}
func (s *summaryQuotaStore) IncrementUsage(_ context.Context, k models.UsageSubjectKind, key, metric string, _ time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = map[string]int64{}
	}
	s.counts[string(k)+"|"+key+"|"+metric]++
	return s.counts[string(k)+"|"+key+"|"+metric], nil
}
func (s *summaryQuotaStore) DecrementUsage(_ context.Context, k models.UsageSubjectKind, key, metric string, _ time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts[string(k)+"|"+key+"|"+metric] > 0 {
		s.counts[string(k)+"|"+key+"|"+metric]--
	}
	return s.counts[string(k)+"|"+key+"|"+metric], nil
}
func (s *summaryQuotaStore) total() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, v := range s.counts {
		n += v
	}
	return n
}

type summaryFixture struct {
	cfg      *config.Config
	jm       *services.JobManager
	h        *VideoSummaryHandler
	store    *summaryQuotaStore
	router   *gin.Engine
	enqueued []services.VideoSummaryRequest
	probe    *models.VideoProbeResponse
	signedIn bool
}

func newSummaryFixture(t *testing.T, tier string, signedIn bool) *summaryFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.UploadDir, cfg.OutputDir = t.TempDir(), t.TempDir()
	cfg.VideoSummary.Enabled = true
	f := &summaryFixture{cfg: cfg, jm: services.NewJobManager(), signedIn: signedIn,
		store: &summaryQuotaStore{user: &models.User{ID: "user-" + tier, FirebaseUID: "uid", Tier: tier}},
		probe: &models.VideoProbeResponse{DurationSeconds: 120, Width: 1280, Height: 720, VideoCodec: "h264", HasAudio: true}}
	registry := tiers.New(cfg)
	f.h = NewVideoSummaryHandler(f.jm, cfg, registry, nil, nil)
	f.h.probe = func(context.Context, string) (*models.VideoProbeResponse, error) { return f.probe, nil }
	f.h.enqueue = func(r services.VideoSummaryRequest) { f.enqueued = append(f.enqueued, r) }
	f.h.reachable = func(context.Context, string) bool { return true }

	r := gin.New()
	api := r.Group("/api")
	api.Use(func(c *gin.Context) {
		if f.signedIn {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "uid"})
		}
		c.Next()
	})
	api.Use(middleware.TierContext(middleware.TierConfig{Store: f.store, Registry: registry}, true))
	// The chain cmd/api builds for this path: the sign-in gate, then the ai_ops
	// charge, then the handler (refund on 4xx is RequireQuota's).
	quota := middleware.RequireQuota(middleware.QuotaConfig{Store: f.store, Registry: registry, Enabled: true}, models.MetricAIOps)
	gate := SummarizeSignInGate()
	api.Use(func(c *gin.Context) {
		if c.Request.Method == http.MethodPost && c.Request.URL.Path == "/api/summarize/start" {
			gate(c)
			if c.IsAborted() {
				return
			}
			quota(c)
			return
		}
		c.Next()
	})
	RegisterVideoSummaryRoutes(api, f.h)
	f.router = r
	return f
}

func summaryMultipartBody(t *testing.T, name string, content []byte, options string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(content)
	if options != "" {
		_ = w.WriteField("options", options)
	}
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func (f *summaryFixture) start(t *testing.T, options string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := summaryMultipartBody(t, "talk.mp4", []byte("not really a video"), options)
	req := httptest.NewRequest(http.MethodPost, "/api/summarize/start", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-MM-Session-ID", "sess-1")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func summaryDecode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return m
}

// An anonymous caller is refused with 403 signin_required by the gate, before
// RequireQuota runs: nothing is charged, so nothing needs a refund.
func TestSummarizeAnonymousIsRefusedBeforeCharge(t *testing.T) {
	f := newSummaryFixture(t, "free", false)
	w := f.start(t, "")
	if w.Code != http.StatusForbidden || decode(t, w)["error"] != "signin_required" {
		t.Fatalf("want 403 signin_required, got %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get(middleware.HeaderQuotaLimit) != "" || w.Header().Get(middleware.HeaderTier) != "" {
		t.Fatal("the quota middleware must not have run")
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if len(f.store.counts) != 0 {
		t.Fatalf("anonymous request touched the ledger: %v", f.store.counts)
	}
	if len(f.enqueued) != 0 {
		t.Fatal("no job for an anonymous caller")
	}
}

func TestSummarizeSignedInStartsJobAndChargesOnce(t *testing.T) {
	f := newSummaryFixture(t, "free", true)
	w := f.start(t, `{"outputLanguage":"de","depth":"standard"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d %s", w.Code, w.Body.String())
	}
	jobID, _ := decode(t, w)["jobId"].(string)
	if jobID == "" || len(f.enqueued) != 1 {
		t.Fatalf("job not created/enqueued: %s %v", w.Body.String(), f.enqueued)
	}
	if f.store.total() != 1 {
		t.Fatalf("one ai_ops spent, got %d", f.store.total())
	}
	req := f.enqueued[0]
	if req.JobID != jobID || req.MaxFrames != 24 || req.OutputLanguage != "de-DE" || req.Depth != "standard" || req.Probe == nil {
		t.Fatalf("request: %+v", req)
	}
	if _, err := os.Stat(req.InputPath); err != nil {
		t.Fatalf("upload not staged: %v", err)
	}
	if !strings.HasPrefix(req.InputPath, filepath.Join(f.cfg.UploadDir, jobID)) || req.UploadDir != filepath.Join(f.cfg.UploadDir, jobID) {
		t.Fatalf("upload must live in its job directory: %s", req.InputPath)
	}
	job, _ := f.jm.GetJob(jobID)
	if job.Mode != models.VideoSummaryMode || len(job.Stages) != 6 || job.Stages[0].Key != "probe" || job.Options["depth"] != "standard" {
		t.Fatalf("job: mode=%q stages=%v options=%v", job.Mode, job.Stages, job.Options)
	}
	if owner, ok := f.jm.JobOwnerOf(jobID); !ok || owner.UserID != "user-free" {
		t.Fatalf("owner not recorded: %+v", owner)
	}
}

// A refused input (neither audio nor a usable video stream) is a 4xx after the
// charge, and the unit comes back.
func TestSummarizeRefusedInputIsRefunded(t *testing.T) {
	f := newSummaryFixture(t, "free", true)
	f.probe = &models.VideoProbeResponse{DurationSeconds: 3, Width: 600, Height: 600, VideoCodec: "png"}
	w := f.start(t, "")
	if w.Code != http.StatusUnprocessableEntity || decode(t, w)["error"] != "no_audio_or_video" {
		t.Fatalf("want 422 no_audio_or_video, got %d %s", w.Code, w.Body.String())
	}
	if f.store.total() != 0 {
		t.Fatalf("refused input must be refunded, ledger = %d", f.store.total())
	}
	if len(f.enqueued) != 0 {
		t.Fatal("no job for a refused input")
	}
	entries, _ := os.ReadDir(f.cfg.UploadDir)
	if len(entries) != 0 {
		t.Fatalf("staged upload not cleaned up: %v", entries)
	}
}

func TestSummarizeOverDurationIsRefusedAndRefunded(t *testing.T) {
	f := newSummaryFixture(t, "free", true)
	f.probe.DurationSeconds = 5000 // free allows 20 minutes
	w := f.start(t, "")
	if w.Code != http.StatusRequestEntityTooLarge || decode(t, w)["reason"] != "video_duration" {
		t.Fatalf("want 413 video_duration, got %d %s", w.Code, w.Body.String())
	}
	if f.store.total() != 0 {
		t.Fatal("over-duration refusal must be refunded")
	}
}

// depth "deep" for a non-Pro tier silently falls back to standard and the job
// records the effective depth; Pro gets deep only when it is configured.
func TestSummarizeDeepFallsBackToStandardForNonPro(t *testing.T) {
	f := newSummaryFixture(t, "premium", true)
	f.h.svc = services.NewVideoSummaryService(services.VideoSummarySettings{DeepModel: "big", DeepURL: "http://x"}, f.jm, nil)
	w := f.start(t, `{"depth":"deep"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("deep must not be refused: %d %s", w.Code, w.Body.String())
	}
	if f.enqueued[0].Depth != "standard" {
		t.Fatalf("premium deep must fall back to standard: %q", f.enqueued[0].Depth)
	}
	job, _ := f.jm.GetJob(f.enqueued[0].JobID)
	if job.Options["depth"] != "standard" || job.Options["requestedDepth"] != "deep" {
		t.Fatalf("options must record the effective depth: %v", job.Options)
	}
	if f.enqueued[0].MaxFrames != 48 {
		t.Fatalf("premium budget 48: %d", f.enqueued[0].MaxFrames)
	}
}

func TestSummarizeDeepForProOnlyWhenConfigured(t *testing.T) {
	f := newSummaryFixture(t, "pro", true)
	f.start(t, `{"depth":"deep"}`)
	if f.enqueued[0].Depth != "standard" {
		t.Fatalf("deep not configured: pro falls back to standard, got %q", f.enqueued[0].Depth)
	}
	f.h.svc = services.NewVideoSummaryService(services.VideoSummarySettings{DeepModel: "big", DeepURL: "http://x"}, f.jm, nil)
	f.start(t, `{"depth":"deep"}`)
	if f.enqueued[1].Depth != "deep" || f.enqueued[1].MaxFrames != 96 {
		t.Fatalf("pro with deep configured: %+v", f.enqueued[1])
	}
}

func TestEffectiveSummaryDepth(t *testing.T) {
	cases := []struct {
		req        string
		tier, conf bool
		want       string
		wantErr    bool
	}{
		{"", true, true, "standard", false},
		{"standard", true, true, "standard", false},
		{"deep", true, true, "deep", false},
		{"DEEP", true, true, "deep", false},
		{"deep", false, true, "standard", false},
		{"deep", true, false, "standard", false},
		{"turbo", true, true, "", true},
	}
	for _, tc := range cases {
		got, err := EffectiveSummaryDepth(tc.req, tc.tier, tc.conf)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("EffectiveSummaryDepth(%q,%v,%v) = %q,%v", tc.req, tc.tier, tc.conf, got, err)
		}
	}
}

func TestSummarizeValidationErrorsAreRefunded(t *testing.T) {
	f := newSummaryFixture(t, "free", true)
	for options, want := range map[string]string{
		`{"outputLanguage":"klingon"}`: "invalid_output_language",
		`{"depth":"turbo"}`:            "invalid_depth",
		`not json`:                     "invalid_options",
	} {
		w := f.start(t, options)
		if w.Code != http.StatusBadRequest || decode(t, w)["error"] != want {
			t.Errorf("%s: got %d %s", options, w.Code, w.Body.String())
		}
	}
	if f.store.total() != 0 {
		t.Fatalf("validation errors must be refunded: %d", f.store.total())
	}
}

func TestSummarizeDisabledIs503(t *testing.T) {
	f := newSummaryFixture(t, "free", true)
	f.cfg.VideoSummary.Enabled = false
	w := f.start(t, "")
	if w.Code != http.StatusServiceUnavailable || decode(t, w)["error"] != "summary_unavailable" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if f.store.total() != 0 {
		t.Fatal("a disabled feature must not keep the charge")
	}
}

func TestSummarizeJSONBodyUsesPresignedUpload(t *testing.T) {
	f := newSummaryFixture(t, "free", true)
	var gotKey string
	var gotMax int64
	f.h.fetchS3 = func(_ context.Context, key, path string, maxBytes int64) (int64, error) {
		gotKey, gotMax = key, maxBytes
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		return 1234, os.WriteFile(path, []byte("video"), 0o644)
	}
	body := `{"s3Key":"videos/sess-1/abc123/clip.mp4","fileName":"clip.mp4","fileSizeBytes":1234,"options":{"outputLanguage":"auto"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/summarize/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if gotKey != "videos/sess-1/abc123/clip.mp4" || gotMax != 1024*1024*1024 {
		t.Fatalf("key %q max %d (free tier cap 1 GiB)", gotKey, gotMax)
	}
	if f.enqueued[0].OutputLanguage != "auto" {
		t.Fatalf("language: %q", f.enqueued[0].OutputLanguage)
	}

	// A bad key and an oversized object are 4xx and refunded.
	for _, tc := range []struct {
		body string
		err  error
		code int
	}{
		{`{"s3Key":"../etc/passwd"}`, nil, http.StatusBadRequest},
		{`{"s3Key":"videos/s/a/b.mp4"}`, errTooLarge{size: 9}, http.StatusRequestEntityTooLarge},
		{`{"s3Key":"videos/s/a/b.mp4"}`, errors.New("NoSuchKey"), http.StatusBadRequest},
	} {
		f.h.fetchS3 = func(context.Context, string, string, int64) (int64, error) { return 0, tc.err }
		req := httptest.NewRequest(http.MethodPost, "/api/summarize/start", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Errorf("%s: got %d %s", tc.body, w.Code, w.Body.String())
		}
	}
	if f.store.total() != 1 {
		t.Fatalf("only the accepted request keeps its unit: %d", f.store.total())
	}
}

func TestSummarizeCapabilities(t *testing.T) {
	f := newSummaryFixture(t, "free", false)
	get := func() map[string]any {
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/summarize/capabilities", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		return decode(t, w)
	}
	m := get()
	if m["enabled"] != true || m["vlmModel"] != "qwen3-vl:8b-instruct-q8_0" || m["summaryModel"] != "qwen3.5:9b-q8_0" || m["deep"] != false {
		t.Fatalf("capabilities: %v", m)
	}
	if _, has := m["deepModel"]; has {
		t.Fatal("deepModel is omitted when deep is not configured")
	}
	frames := m["maxFramesByTier"].(map[string]any)
	if frames["anonymous"] != 0.0 || frames["free"] != 24.0 || frames["premium"] != 48.0 || frames["pro"] != 96.0 {
		t.Fatalf("maxFramesByTier: %v", frames)
	}
	if tiers := m["deepTiers"].([]any); len(tiers) != 1 || tiers[0] != "pro" {
		t.Fatalf("deepTiers: %v", tiers)
	}
	if langs := m["outputLanguages"].([]any); len(langs) != 19 || langs[0] != "auto" {
		t.Fatalf("outputLanguages: %v", langs)
	}
	// Ollama down: enabled false. Deep configured and up: deep true + model.
	f.h.reachable = func(_ context.Context, url string) bool { return url == "http://deep" }
	f.h.svc = services.NewVideoSummaryService(services.VideoSummarySettings{DeepModel: "qwen3.8:27b", DeepURL: "http://deep"}, f.jm, nil)
	m = get()
	if m["enabled"] != false || m["deep"] != true || m["deepModel"] != "qwen3.8:27b" {
		t.Fatalf("probe results: %v", m)
	}
	f.cfg.VideoSummary.Enabled = false
	if m = get(); m["enabled"] != false || m["deep"] != false {
		t.Fatalf("SUMMARY_ENABLED=false: %v", m)
	}
}

// completedSummaryJob makes a finished summary job owned by session sess-owner.
func completedSummaryJob(t *testing.T, f *summaryFixture) *models.ConversionJob {
	t.Helper()
	job := f.jm.CreateJob(models.OriginalFileInfo{Name: "talk.mp4", Type: "video/mp4"}, map[string]interface{}{"mode": "summarize"})
	_ = f.jm.SetMode(job.ID, models.VideoSummaryMode)
	f.jm.SetJobOwner(job.ID, services.JobOwner{SessionID: "sess-owner"})
	dir := filepath.Join(f.cfg.OutputDir, job.ID)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, services.SummaryMarkdownFile), []byte("# Title\n\nBody.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, services.SummaryJSONFile), []byte(`{"title":"Title"}`), 0o644)
	_ = f.jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	return job
}

func (f *summaryFixture) get(path, session string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if session != "" {
		req.Header.Set("X-MM-Session-ID", session)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func TestSummarizeResultFormats(t *testing.T) {
	SetResultOwnershipEnforce(true)
	f := newSummaryFixture(t, "free", false)
	job := completedSummaryJob(t, f)
	for _, tc := range []struct{ q, ct, body, name string }{
		{"", "text/markdown; charset=utf-8", "# Title\n\nBody.\n", "talk_summary.md"},
		{"?format=md", "text/markdown; charset=utf-8", "# Title\n\nBody.\n", "talk_summary.md"},
		{"?format=json", "application/json; charset=utf-8", `{"title":"Title"}`, "talk_summary.json"},
	} {
		w := f.get("/api/summarize/"+job.ID+"/result"+tc.q, "sess-owner")
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != tc.ct || w.Body.String() != tc.body {
			t.Fatalf("%q: %d %q %q", tc.q, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Content-Disposition"), tc.name) || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%q headers: %v", tc.q, w.Header())
		}
	}
	if w := f.get("/api/summarize/"+job.ID+"/result?format=pdf", "sess-owner"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown format: %d", w.Code)
	}
}

// A non-owner gets exactly the response /transcript/:jobId gives a non-owner
// (same helper: authorizeJobAccess + denyJobAccess).
func TestSummarizeResultNonOwnerMatchesTranscriptRoute(t *testing.T) {
	SetResultOwnershipEnforce(true)
	f := newSummaryFixture(t, "free", false)
	job := completedSummaryJob(t, f)
	conv := &ConversionHandler{jobManager: f.jm, cfg: f.cfg}
	tr := gin.New()
	tr.GET("/api/transcript/:jobId", conv.GetTranscriptResult)

	for _, session := range []string{"stranger", ""} {
		got := f.get("/api/summarize/"+job.ID+"/result?format=json", session)
		req := httptest.NewRequest(http.MethodGet, "/api/transcript/"+job.ID, nil)
		if session != "" {
			req.Header.Set("X-MM-Session-ID", session)
		}
		want := httptest.NewRecorder()
		tr.ServeHTTP(want, req)
		if got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Fatalf("session %q: summarize %d %s, transcript %d %s", session, got.Code, got.Body.String(), want.Code, want.Body.String())
		}
		if got.Code != http.StatusNotFound || got.Body.String() != `{"error":"Job not found"}` {
			t.Fatalf("stranger response: %d %s", got.Code, got.Body.String())
		}
	}
	// An unknown job id answers the same body.
	if w := f.get("/api/summarize/nope/result", "stranger"); w.Code != http.StatusNotFound || w.Body.String() != `{"error":"Job not found"}` {
		t.Fatalf("unknown id: %d %s", w.Code, w.Body.String())
	}
}

func TestSummarizeResultNotReadyAndWrongMode(t *testing.T) {
	f := newSummaryFixture(t, "free", false)
	job := f.jm.CreateJob(models.OriginalFileInfo{Name: "a.mp4"}, nil)
	_ = f.jm.SetMode(job.ID, models.VideoSummaryMode)
	if w := f.get("/api/summarize/"+job.ID+"/result", ""); w.Code != http.StatusConflict || decode(t, w)["error"] != "not_ready" {
		t.Fatalf("pending job: %d %s", w.Code, w.Body.String())
	}
	other := f.jm.CreateJob(models.OriginalFileInfo{Name: "a.mp4"}, nil)
	_ = f.jm.SetMode(other.ID, "transcode")
	_ = f.jm.UpdateJobStatus(other.ID, models.StatusCompleted)
	if w := f.get("/api/summarize/"+other.ID+"/result", ""); w.Code != http.StatusNotFound {
		t.Fatalf("a non-summary job is not served here: %d", w.Code)
	}
}

func TestSummarizeSignInGateWithoutTierSystemStaysOut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/summarize/start", nil)
	SummarizeSignInGate()(c)
	if c.IsAborted() {
		t.Fatal("with no tier resolution the gate must not refuse (the handler decides)")
	}
}

// /api/download/:jobId (the link account History lists) serves summary.md
// for a finished summary job, owner only.
func TestSummaryJobDownloadServesMarkdown(t *testing.T) {
	SetResultOwnershipEnforce(true)
	f := newSummaryFixture(t, "free", false)
	job := completedSummaryJob(t, f)
	conv := &ConversionHandler{jobManager: f.jm, cfg: f.cfg}
	r := gin.New()
	r.GET("/api/download/:jobId", conv.DownloadFile)
	req := httptest.NewRequest(http.MethodGet, "/api/download/"+job.ID, nil)
	req.Header.Set("X-MM-Session-ID", "sess-owner")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "# Title\n\nBody.\n" || !strings.Contains(w.Header().Get("Content-Disposition"), "talk_summary.md") {
		t.Fatalf("download: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if got := findResultFile(f.cfg.OutputDir, job.ID); filepath.Base(got) != "summary.md" {
		t.Fatalf("restart fallback must find summary.md, got %q", got)
	}
}

func TestSummarizeGateZeroBudgetSignedInIsNotInPlan(t *testing.T) {
	// The fixture builds its registry from config.Load(), so the env applies.
	t.Setenv("TIER_FREE_SUMMARY_MAX_FRAMES", "0")
	f := newSummaryFixture(t, "free", true)
	w := f.start(t, "")
	if w.Code != http.StatusForbidden || summaryDecode(t, w)["error"] != "summary_not_in_plan" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if f.store.total() != 0 || len(f.enqueued) != 0 {
		t.Fatal("refused before the charge, no job")
	}
}
