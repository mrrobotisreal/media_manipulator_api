package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM90: the AI allowance (`ai_ops`) and the everyday allowance (`ops`) are two
// separate daily counters. A job spends exactly one of them. Model-run AI jobs
// submitted through /api/upload (and /api/video-upload/complete) spend
// `ai_ops`; everything else there spends `ops`.

// chargeByJob stands in for the real UploadFile's first steps: parse the
// `options` form field, then spend the one allowance the job calls for. The
// real handler is exercised in internal/handlers/quota_metric_test.go.
func chargeByJob(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		options := map[string]interface{}{}
		if raw := c.Request.FormValue("options"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &options)
		}
		if !middleware.SpendDeferredQuota(c, handlers.QuotaMetricForJob(options)) {
			return
		}
		next(c)
	}
}

// uploadRequest builds a multipart POST shaped like the UI's: file, then options.
func uploadRequest(t *testing.T, path, session string, options map[string]interface{}) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", "input.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("payload"))
	if options != nil {
		raw, _ := json.Marshal(options)
		_ = w.WriteField("options", string(raw))
	}
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if session != "" {
		req.Header.Set("X-MM-Session-ID", session)
	}
	return req
}

func aiOpts(op string) map[string]interface{} {
	return map[string]interface{}{"format": "png", "ai": map[string]interface{}{"enabled": true, "operation": op}}
}

// decisionTwoUploads are ticket decision 2's upload jobs: each spends `ai_ops`.
var decisionTwoUploads = map[string]map[string]interface{}{
	"ai_upscale":          aiOpts("ai_upscale"),
	"face_privacy":        aiOpts("face_privacy"),
	"redact_text":         aiOpts("redact_text"),
	"remove_object":       aiOpts("remove_object"),
	"isolate_vocals":      aiOpts("isolate_vocals"),
	"remove_vocals":       aiOpts("remove_vocals"),
	"frame_interpolation": aiOpts("frame_interpolation"),
	"transcribe":          {"mode": "transcribe", "format": "srt"},
}

// decisionThreeUploads stay on `ops` on purpose (decision 3), with a plain
// conversion and an AI block that is switched off.
var decisionThreeUploads = map[string]map[string]interface{}{
	"remove_background":       aiOpts("remove_background"),
	"clean_voice":             aiOpts("clean_voice"),
	"remove_background_noise": aiOpts("remove_background_noise"),
	"plain_conversion":        {"format": "webp", "quality": 80},
	"no_options":              nil,
	"ai_disabled":             {"format": "png", "ai": map[string]interface{}{"enabled": false, "operation": "ai_upscale"}},
	"ai_none":                 {"format": "png", "ai": map[string]interface{}{"enabled": true, "operation": "none"}},
	"specialized_tool":        {"mode": "extract_audio", "format": "mp3"},
}

func uploadQuotaRouter(store *stubQuotaStore, registry *tiers.Registry, principal *middleware.Principal) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	if principal != nil {
		api.Use(func(c *gin.Context) { c.Set(middleware.CtxPrincipal, principal); c.Next() })
		api.Use(middleware.TierContext(middleware.TierConfig{Store: store, Registry: registry}, true))
	}
	api.Use(quotaDispatcher(middleware.QuotaConfig{Store: store, Registry: registry, Enabled: true, IPHashSecret: quotaIPSalt[:]}))
	ok := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) }
	api.POST("/upload", chargeByJob(ok))
	api.POST("/video-upload/complete", chargeByJob(ok))
	api.POST("/tools/caption-translator", ok)
	api.POST("/tools/stitch-audio-to-video", ok)
	return r
}

func (s *stubQuotaStore) count(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[key]
}

// The routing table: the upload routes are decided per job, caption
// translation is an AI job, and the other conversion routes stay everyday.
func TestMM90QuotaRouteTable(t *testing.T) {
	cases := map[string]string{
		"/api/upload":                      quotaMetricByJob,
		"/api/video-upload/complete":       quotaMetricByJob,
		"/api/tools/caption-translator":    models.MetricAIOps,
		"/api/tools/stitch-audio-to-video": models.MetricOps,
		"/api/video-transcode/start":       models.MetricOps,
		"/api/image-restore/start":         models.MetricAIOps,
		"/api/document-scan/start":         models.MetricAIOps,
	}
	for path, want := range cases {
		if got := metricFor(path); got != want {
			t.Errorf("%s: metric %q, want %q", path, got, want)
		}
	}
}

// Each decision-2 job spends exactly one `ai_ops` and no `ops`; each
// decision-3 job and every non-AI conversion spends exactly one `ops` and no
// `ai_ops`. Both upload routes behave the same.
func TestMM90UploadJobsSpendExactlyOneAllowance(t *testing.T) {
	cfg := config.Load()
	for _, path := range []string{"/api/upload", "/api/video-upload/complete"} {
		for name, opts := range decisionTwoUploads {
			t.Run(path+"/ai_ops/"+name, func(t *testing.T) {
				store := &stubQuotaStore{}
				r := uploadQuotaRouter(store, tiers.New(cfg), nil)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, uploadRequest(t, path, "s-"+name, opts))
				if w.Code != http.StatusOK {
					t.Fatalf("got %d %s", w.Code, w.Body.String())
				}
				if ai, ops := store.count("session|s-"+name+"|ai_ops"), store.count("session|s-"+name+"|ops"); ai != 1 || ops != 0 {
					t.Fatalf("ai_ops=%d ops=%d, want 1 / 0", ai, ops)
				}
				if got := w.Header().Get(middleware.HeaderQuotaMetric); got != models.MetricAIOps {
					t.Fatalf("X-MM-Quota-Metric = %q, want ai_ops", got)
				}
				if got := w.Header().Get(middleware.HeaderQuotaLimit); got != "3" {
					t.Fatalf("X-MM-Quota-Limit = %q, want the anonymous AI allowance 3", got)
				}
			})
		}
		for name, opts := range decisionThreeUploads {
			t.Run(path+"/ops/"+name, func(t *testing.T) {
				store := &stubQuotaStore{}
				r := uploadQuotaRouter(store, tiers.New(cfg), nil)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, uploadRequest(t, path, "s-"+name, opts))
				if w.Code != http.StatusOK {
					t.Fatalf("got %d %s", w.Code, w.Body.String())
				}
				if ai, ops := store.count("session|s-"+name+"|ai_ops"), store.count("session|s-"+name+"|ops"); ai != 0 || ops != 1 {
					t.Fatalf("ai_ops=%d ops=%d, want 0 / 1", ai, ops)
				}
				if got := w.Header().Get(middleware.HeaderQuotaMetric); got != models.MetricOps {
					t.Fatalf("X-MM-Quota-Metric = %q, want ops", got)
				}
			})
		}
	}
}

// Acceptance 1: exhaust `ai_ops`, then an everyday job still succeeds, with
// `ops` untouched by the AI jobs. The over-limit AI job is a 429 naming
// `ai_ops`, and it does not touch `ops` either.
func TestMM90ExhaustingAIOpsLeavesEveryEverydayJob(t *testing.T) {
	cfg := config.Load()
	store := &stubQuotaStore{}
	r := uploadQuotaRouter(store, tiers.New(cfg), nil)
	const s = "s-exhaust"
	aiJobs := []map[string]interface{}{aiOpts("ai_upscale"), {"mode": "transcribe"}, aiOpts("isolate_vocals")}
	if len(aiJobs) != cfg.TierAnonAIOpsPerDay {
		t.Fatalf("test assumes the anonymous AI allowance is %d, got %d", len(aiJobs), cfg.TierAnonAIOpsPerDay)
	}
	for i, opts := range aiJobs {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, uploadRequest(t, "/api/upload", s, opts))
		if w.Code != http.StatusOK {
			t.Fatalf("AI job %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	// Caption translation draws on the same, now-empty AI allowance.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/tools/caption-translator", nil))
	_ = w
	over := httptest.NewRecorder()
	r.ServeHTTP(over, uploadRequest(t, "/api/upload", s, aiOpts("face_privacy")))
	if over.Code != http.StatusTooManyRequests {
		t.Fatalf("4th AI job: want 429, got %d %s", over.Code, over.Body.String())
	}
	for _, want := range []string{`"error":"quota_exceeded"`, `"metric":"ai_ops"`, `"limit":3`} {
		if !strings.Contains(over.Body.String(), want) {
			t.Errorf("429 body missing %s: %s", want, over.Body.String())
		}
	}
	if n := store.count("session|" + s + "|ops"); n != 0 {
		t.Fatalf("AI jobs touched ops: %d", n)
	}

	// Every everyday job is still there.
	for i := 1; i <= cfg.TierAnonOpsPerDay; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, uploadRequest(t, "/api/upload", s, map[string]interface{}{"format": "jpg"}))
		if w.Code != http.StatusOK {
			t.Fatalf("everyday job %d after the AI allowance ran out: %d %s", i, w.Code, w.Body.String())
		}
		if got := w.Header().Get(middleware.HeaderQuotaRemaining); got != itoa(cfg.TierAnonOpsPerDay-i) {
			t.Fatalf("everyday job %d: remaining %s, want %d", i, got, cfg.TierAnonOpsPerDay-i)
		}
	}
	if n := store.count("session|" + s + "|ops"); n != int64(cfg.TierAnonOpsPerDay) {
		t.Fatalf("ops ledger = %d, want exactly the %d everyday jobs", n, cfg.TierAnonOpsPerDay)
	}
	if n := store.count("session|" + s + "|ai_ops"); n != int64(cfg.TierAnonAIOpsPerDay)+1 {
		t.Fatalf("ai_ops ledger = %d, want %d (the 429 is not refunded)", n, cfg.TierAnonAIOpsPerDay+1)
	}
}

// Mitch's example, binding: Premium with 300 everyday + 100 AI is 400 jobs a
// day; using all 100 AI jobs still leaves all 300 everyday jobs.
func TestMM90PremiumGets400JobsADay(t *testing.T) {
	cfg := config.Load()
	registry := tiers.New(cfg)
	store := &stubQuotaStore{user: &models.User{ID: "u-prem", Tier: "premium"}}
	r := uploadQuotaRouter(store, registry, &middleware.Principal{UID: "uid-prem", Email: "p@example.com", EmailVerified: true})
	premium := registry.For(tiers.TierPremium)
	if premium.OpsPerDay != 300 || premium.AIOpsPerDay != 100 {
		t.Fatalf("premium = %d everyday / %d AI, want 300 / 100", premium.OpsPerDay, premium.AIOpsPerDay)
	}
	succeeded := 0
	for i := 0; i < premium.AIOpsPerDay; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, uploadRequest(t, "/api/upload", "", aiOpts("remove_object")))
		if w.Code != http.StatusOK {
			t.Fatalf("AI job %d: %d", i+1, w.Code)
		}
		succeeded++
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, uploadRequest(t, "/api/upload", "", aiOpts("remove_object")))
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"metric":"ai_ops"`) {
		t.Fatalf("101st AI job: want 429 ai_ops, got %d %s", w.Code, w.Body.String())
	}
	if n := store.count("user|u-prem|ops"); n != 0 {
		t.Fatalf("100 AI jobs spent %d everyday jobs", n)
	}
	for i := 0; i < premium.OpsPerDay; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, uploadRequest(t, "/api/upload", "", map[string]interface{}{"format": "png"}))
		if w.Code != http.StatusOK {
			t.Fatalf("everyday job %d after 100 AI jobs: %d %s", i+1, w.Code, w.Body.String())
		}
		succeeded++
	}
	if succeeded != 400 {
		t.Fatalf("%d jobs succeeded, want 400", succeeded)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, uploadRequest(t, "/api/upload", "", map[string]interface{}{"format": "png"}))
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"metric":"ops"`) {
		t.Fatalf("301st everyday job: want 429 ops, got %d %s", w.Code, w.Body.String())
	}
}

// And the other way round: spending every everyday job leaves the AI allowance.
func TestMM90ExhaustingOpsLeavesTheAIAllowance(t *testing.T) {
	cfg := config.Load()
	store := &stubQuotaStore{}
	r := uploadQuotaRouter(store, tiers.New(cfg), nil)
	const s = "s-ops-first"
	for i := 0; i <= cfg.TierAnonOpsPerDay; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, uploadRequest(t, "/api/upload", s, aiOpts("clean_voice")))
		if i == cfg.TierAnonOpsPerDay && (w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"metric":"ops"`)) {
			t.Fatalf("6th everyday job: want 429 ops, got %d %s", w.Code, w.Body.String())
		}
	}
	for i := 0; i < cfg.TierAnonAIOpsPerDay; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, uploadRequest(t, "/api/video-upload/complete", s, aiOpts("frame_interpolation")))
		if w.Code != http.StatusOK {
			t.Fatalf("AI job %d after the everyday allowance ran out: %d", i+1, w.Code)
		}
	}
	if n := store.count("session|" + s + "|ai_ops"); n != int64(cfg.TierAnonAIOpsPerDay) {
		t.Fatalf("ai_ops = %d", n)
	}
}

// Caption translation is an AI job: it spends `ai_ops` and answers the 429
// with `metric: ai_ops` once the AI allowance is spent, leaving `ops` alone.
func TestMM90CaptionTranslationSpendsAIOps(t *testing.T) {
	cfg := config.Load()
	store := &stubQuotaStore{}
	r := uploadQuotaRouter(store, tiers.New(cfg), nil)
	post := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/caption-translator", nil)
		req.Header.Set("X-MM-Session-ID", "s-cap")
		r.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < cfg.TierAnonAIOpsPerDay; i++ {
		if w := post(); w.Code != http.StatusOK {
			t.Fatalf("translation %d: %d", i+1, w.Code)
		}
	}
	w := post()
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"metric":"ai_ops"`) {
		t.Fatalf("want 429 ai_ops, got %d %s", w.Code, w.Body.String())
	}
	if n := store.count("session|s-cap|ops"); n != 0 {
		t.Fatalf("caption translation spent ops: %d", n)
	}
}

// A refused upload gives back the unit of whichever allowance it spent.
func TestMM90RefusedAIUploadRefundsAIOps(t *testing.T) {
	cfg := config.Load()
	store := &stubQuotaStore{}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(quotaDispatcher(middleware.QuotaConfig{Store: store, Registry: tiers.New(cfg), Enabled: true, IPHashSecret: quotaIPSalt[:]}))
	api.POST("/upload", chargeByJob(func(c *gin.Context) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported file type"})
	}))
	for i := 0; i < cfg.TierAnonAIOpsPerDay*3; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, uploadRequest(t, "/api/upload", "s-ref", aiOpts("ai_upscale")))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("refusal %d: %d", i+1, w.Code)
		}
	}
	if ai, ops := store.count("session|s-ref|ai_ops"), store.count("session|s-ref|ops"); ai != 0 || ops != 0 {
		t.Fatalf("refused uploads left ai_ops=%d ops=%d, want 0 / 0", ai, ops)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
