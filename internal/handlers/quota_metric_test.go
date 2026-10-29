package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

// MM90 decisions 2 and 3, per operation: which allowance a job spends.
func TestQuotaMetricForJob(t *testing.T) {
	ai := func(op string) map[string]interface{} {
		return map[string]interface{}{"ai": map[string]interface{}{"enabled": true, "operation": op}}
	}
	cases := []struct {
		name string
		opts map[string]interface{}
		want string
	}{
		// Decision 2: model-run jobs spend the AI allowance.
		{"ai_upscale", ai(services.AIImageOpUpscale), models.MetricAIOps},
		{"face_privacy", ai(services.AIImageOpFacePrivacy), models.MetricAIOps},
		{"redact_text", ai(services.AIImageOpRedactText), models.MetricAIOps},
		{"remove_object", ai(services.AIImageOpRemoveObject), models.MetricAIOps},
		{"isolate_vocals", ai(services.AIAudioOpIsolateVocals), models.MetricAIOps},
		{"remove_vocals", ai(services.AIAudioOpRemoveVocals), models.MetricAIOps},
		{"frame_interpolation", ai(services.AIVideoOpFrameInterpolation), models.MetricAIOps},
		{"transcribe", map[string]interface{}{"mode": "transcribe"}, models.MetricAIOps},
		{"transcribe_any_case", map[string]interface{}{"mode": " Transcribe "}, models.MetricAIOps},
		{"op_case_and_space", ai("  AI_Upscale "), models.MetricAIOps},
		// Decision 3: these stay on the everyday allowance on purpose.
		{"remove_background", ai(services.AIImageOpRemoveBackground), models.MetricOps},
		{"clean_voice", ai(services.AIAudioOpCleanVoice), models.MetricOps},
		{"remove_background_noise", ai(services.AIAudioOpRemoveNoise), models.MetricOps},
		// Non-AI conversions.
		{"plain_conversion", map[string]interface{}{"format": "webp"}, models.MetricOps},
		{"nil_options", nil, models.MetricOps},
		{"specialized_tool", map[string]interface{}{"mode": services.SpecializedModeExtractFrames}, models.MetricOps},
		{"ai_disabled", map[string]interface{}{"ai": map[string]interface{}{"enabled": false, "operation": services.AIImageOpUpscale}}, models.MetricOps},
		{"ai_none", ai("none"), models.MetricOps},
		{"unknown_ai_op", ai("teleport"), models.MetricOps},
		{"ai_not_an_object", map[string]interface{}{"ai": "ai_upscale"}, models.MetricOps},
	}
	for _, tc := range cases {
		if got := QuotaMetricForJob(tc.opts); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ledgerStore is an in-memory middleware.QuotaStore.
type ledgerStore struct {
	mu     sync.Mutex
	counts map[string]int64
}

func (s *ledgerStore) Enabled() bool { return true }
func (s *ledgerStore) UserByFirebaseUID(context.Context, string) (*models.User, error) {
	return nil, nil
}
func (s *ledgerStore) IncrementUsage(_ context.Context, _ models.UsageSubjectKind, key, metric string, _ time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = map[string]int64{}
	}
	s.counts[key+"|"+metric]++
	return s.counts[key+"|"+metric], nil
}
func (s *ledgerStore) DecrementUsage(_ context.Context, _ models.UsageSubjectKind, key, metric string, _ time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts[key+"|"+metric] > 0 {
		s.counts[key+"|"+metric]--
	}
	return s.counts[key+"|"+metric], nil
}
func (s *ledgerStore) get(k string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[k]
}

// The real UploadFile spends the allowance its job calls for, through
// DeferQuota: an over-limit AI upload is the 429 naming `ai_ops` and nothing
// is saved; a refused upload (unsupported file) gives its unit back; the
// everyday allowance is never touched by an AI upload.
func TestUploadFileSpendsTheJobsAllowance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.UploadDir, cfg.OutputDir, cfg.TempDir = t.TempDir(), t.TempDir(), t.TempDir()
	cfg.MaxFileSize = 1 << 20
	store := &ledgerStore{}
	h := &ConversionHandler{cfg: cfg, inspector: services.NewMediaInspector(5 * time.Second), jobManager: services.NewJobManager()}
	r := gin.New()
	api := r.Group("/api")
	api.Use(func(c *gin.Context) {
		if c.Request.URL.Path == "/api/upload" {
			middleware.DeferQuota(middleware.QuotaConfig{Store: store, Registry: tiers.New(cfg), Enabled: true, IPHashSecret: []byte("salt")})(c)
			return
		}
		c.Next()
	})
	api.POST("/upload", h.UploadFile)

	post := func(opts map[string]interface{}) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "notes.unknownext")
		_, _ = fw.Write([]byte("not a media file"))
		raw, _ := json.Marshal(opts)
		_ = mw.WriteField("options", string(raw))
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/upload", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-MM-Session-ID", "s-real")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	upscale := map[string]interface{}{"format": "png", "ai": map[string]interface{}{"enabled": true, "operation": "ai_upscale"}}

	// Refused after the charge (unsupported type): spent on ai_ops, refunded.
	w := post(upscale)
	if w.Code != http.StatusBadRequest || w.Header().Get(middleware.HeaderQuotaMetric) != models.MetricAIOps {
		t.Fatalf("want 400 with X-MM-Quota-Metric ai_ops, got %d %q %s", w.Code, w.Header().Get(middleware.HeaderQuotaMetric), w.Body.String())
	}
	if ai, ops := store.get("s-real|ai_ops"), store.get("s-real|ops"); ai != 0 || ops != 0 {
		t.Fatalf("after the refusal ai_ops=%d ops=%d, want 0 / 0", ai, ops)
	}

	// Spend the AI allowance by hand, then the real handler refuses the next
	// AI upload with 429 ai_ops before saving anything.
	for i := 0; i < cfg.TierAnonAIOpsPerDay; i++ {
		_, _ = store.IncrementUsage(context.Background(), models.UsageSubjectSession, "s-real", models.MetricAIOps, time.Now())
	}
	w = post(upscale)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"metric":"ai_ops"`) {
		t.Fatalf("want 429 ai_ops, got %d %s", w.Code, w.Body.String())
	}
	if store.get("s-real|ops") != 0 {
		t.Fatal("an AI upload touched ops")
	}

	// An everyday upload is still charged to ops (and refunded on the same refusal).
	w = post(map[string]interface{}{"format": "jpg"})
	if w.Code != http.StatusBadRequest || w.Header().Get(middleware.HeaderQuotaMetric) != models.MetricOps {
		t.Fatalf("everyday upload: %d %q", w.Code, w.Header().Get(middleware.HeaderQuotaMetric))
	}
}

// Outside DeferQuota (quota off, or a route that is not gated) spending is a
// no-op that lets the request through.
func TestSpendDeferredQuotaWithoutDeferQuotaIsANoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	if !middleware.SpendDeferredQuota(c, models.MetricAIOps) || !middleware.SpendDeferredQuota(nil, models.MetricOps) {
		t.Fatal("want true")
	}
}

// MM90 acceptance 2: with no env override, GET /api/tiers reports 3 / 10 /
// 100 / 200 AI jobs a day, and the everyday numbers are unchanged.
func TestTiersReportsMM90AIAllowances(t *testing.T) {
	for _, k := range []string{"TIER_ANON_AI_OPS_PER_DAY", "TIER_FREE_AI_OPS_PER_DAY", "TIER_PREMIUM_AI_OPS_PER_DAY", "TIER_PRO_AI_OPS_PER_DAY",
		"TIER_ANON_OPS_PER_DAY", "TIER_FREE_OPS_PER_DAY", "TIER_PREMIUM_OPS_PER_DAY", "TIER_PRO_OPS_PER_DAY"} {
		t.Setenv(k, "")
	}
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	r := gin.New()
	r.GET("/api/tiers", h.Tiers)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tiers", nil))
	var body struct {
		Tiers []struct {
			Tier   string `json:"tier"`
			Limits struct {
				OpsPerDay   int `json:"opsPerDay"`
				AIOpsPerDay int `json:"aiOpsPerDay"`
			} `json:"limits"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int{"anonymous": {5, 3}, "free": {30, 10}, "premium": {300, 100}, "pro": {700, 200}}
	got := map[string][2]int{}
	for _, row := range body.Tiers {
		got[row.Tier] = [2]int{row.Limits.OpsPerDay, row.Limits.AIOpsPerDay}
		t.Logf("/api/tiers %s: opsPerDay=%d aiOpsPerDay=%d", row.Tier, row.Limits.OpsPerDay, row.Limits.AIOpsPerDay)
	}
	for tier, w := range want {
		if got[tier] != w {
			t.Errorf("%s: ops/ai = %v, want %v", tier, got[tier], w)
		}
	}
}
