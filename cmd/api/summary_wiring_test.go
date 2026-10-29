package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM74: POST /api/summarize/start spends exactly one ai_ops.
func TestSummarizeStartIsAnAIOpsQuotaRoute(t *testing.T) {
	found := 0
	for _, r := range quotaRoutes {
		hit := r.path == summarizeStartPath || (r.matches != nil && r.matches(summarizeStartPath))
		if hit {
			found++
			if r.metric != models.MetricAIOps {
				t.Fatalf("/api/summarize/start spends %q, want ai_ops", r.metric)
			}
		}
		for _, free := range []string{"/api/summarize/capabilities", "/api/summarize/j1/result"} {
			if r.path == free || (r.matches != nil && r.matches(free)) {
				t.Fatalf("%s must never be charged", free)
			}
		}
	}
	if found != 1 {
		t.Fatalf("/api/summarize/start must be in quotaRoutes exactly once, found %d", found)
	}
}

// summaryDispatcherRouter mounts the real quotaDispatcher with the real gate
// map entry, exactly as setupRouter does.
func summaryDispatcherRouter(store *stubQuotaStore, registry *tiers.Registry, signedIn bool, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(func(c *gin.Context) {
		if signedIn {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "uid-1"})
		}
		c.Next()
	})
	api.Use(middleware.TierContext(middleware.TierConfig{Store: store, Registry: registry}, true))
	api.Use(quotaDispatcher(middleware.QuotaConfig{
		Store: store, Registry: registry, Enabled: true, IPHashSecret: quotaIPSalt[:],
	}, map[string]gin.HandlerFunc{
		models.MetricAIOps: summarizeQuotaGate(),
	}))
	api.POST("/summarize/start", handler)
	api.POST("/image-restore/start", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

// An anonymous request gets 403 signin_required from the gate, which runs
// BEFORE RequireQuota: zero usage is charged and the handler never runs.
func TestSummarizeSignInGateRunsBeforeQuotaCharge(t *testing.T) {
	cfg := config.Load()
	store := &stubQuotaStore{}
	handlerRan := false
	r := summaryDispatcherRouter(store, tiers.New(cfg), false, func(c *gin.Context) {
		handlerRan = true
		c.JSON(http.StatusAccepted, gin.H{"jobId": "j"})
	})
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, summarizeStartPath, nil)
		req.Header.Set("X-MM-Session-ID", "anon-1")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error":"signin_required"`) {
			t.Fatalf("anonymous: want 403 signin_required, got %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get(middleware.HeaderQuotaRemaining) != "" {
			t.Fatal("RequireQuota ran before the gate")
		}
	}
	if handlerRan {
		t.Fatal("the handler must not run for an anonymous caller")
	}
	store.mu.Lock()
	n := len(store.counts)
	store.mu.Unlock()
	if n != 0 {
		t.Fatalf("anonymous summarize charged usage: %v", store.counts)
	}

	// The gate is path-scoped: other ai_ops routes still charge anonymous callers.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image-restore/start", nil)
	req.Header.Set("X-MM-Session-ID", "anon-1")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || store.counts["session|anon-1|ai_ops"] != 1 {
		t.Fatalf("image-restore must be unaffected: %d %v", w.Code, store.counts)
	}
}

// A signed-in caller passes the gate and spends one ai_ops; a refused input
// (the handler's 4xx) gives it back.
func TestSummarizeSignedInChargesAiOpsAndRefundsRefusal(t *testing.T) {
	cfg := config.Load()
	store := &stubQuotaStore{user: &models.User{ID: "u-1", Tier: "free"}}
	refuse := false
	r := summaryDispatcherRouter(store, tiers.New(cfg), true, func(c *gin.Context) {
		if refuse {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "no_audio_or_video"})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"jobId": "j"})
	})
	post := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, summarizeStartPath, nil))
		return w
	}
	if w := post(); w.Code != http.StatusAccepted || w.Header().Get(middleware.HeaderQuotaLimit) != "10" {
		t.Fatalf("free summary: %d limit=%q", w.Code, w.Header().Get(middleware.HeaderQuotaLimit))
	}
	if store.counts["user|u-1|ai_ops"] != 1 {
		t.Fatalf("one ai_ops expected: %v", store.counts)
	}
	refuse = true
	if w := post(); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refusal: %d", w.Code)
	}
	if store.counts["user|u-1|ai_ops"] != 1 {
		t.Fatalf("the refused request must be refunded: %v", store.counts)
	}
}

func TestSummarizeStartRouteLimitBucket(t *testing.T) {
	cfg := config.Load()
	if cfg.VideoSummary.RateLimitPerSessionPerHour != 6 || cfg.VideoSummary.RateLimitPerIPPerHour != 12 {
		t.Fatalf("defaults: %+v", cfg.VideoSummary)
	}
	var mu sync.Mutex
	type hit struct {
		key, tool        string
		session, ipLimit int
	}
	var hits []hit
	spy := func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
		return func(c *gin.Context) {
			mu.Lock()
			hits = append(hits, hit{routeKey, tool, sessionLimit, ipLimit})
			mu.Unlock()
			c.Next()
		}
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(routeLimitDispatcher(cfg, limits.New(nil, cfg, nil, nil), spy))
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	r.POST(summarizeStartPath, ok)
	r.GET("/api/summarize/capabilities", ok)
	r.GET("/api/summarize/:jobId/result", ok)

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, summarizeStartPath, nil))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/summarize/capabilities", nil))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/summarize/j/result", nil))
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 || hits[0] != (hit{"summarize_start", "video_summary", 6, 12}) {
		t.Fatalf("want one summarize_start/video_summary 6/12 hit, got %+v", hits)
	}
}

// The routes are mounted by setupRouter through the conversion handler, and
// GETs never reach the quota dispatcher.
func TestSummarizeRoutesMountedBySetupRouter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.VideoSummary.Enabled = false
	cfg.OutputDir, cfg.UploadDir = t.TempDir(), t.TempDir()
	jm := services.NewJobManager()
	registry := tiers.New(cfg)
	conv := handlers.NewConversionHandler(jm, services.NewConverter(cfg), cfg, services.NewMediaInspector(cfg.CommandTimeout), nil, nil, nil, nil, registry)
	conv.SetVideoSummaryHandler(handlers.NewVideoSummaryHandler(jm, cfg, registry, nil, nil))
	router := setupRouter(
		cfg, conv, nil, nil, nil, nil, nil, nil, registry, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, limits.New(nil, cfg, nil, nil), metrics.New(), nil,
	)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/summarize/capabilities", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("capabilities not mounted: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get(middleware.HeaderQuotaLimit) != "" {
		t.Fatal("capabilities must never be charged")
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/summarize/unknown/result", nil))
	if w.Code != http.StatusNotFound || w.Body.String() != `{"error":"Job not found"}` {
		t.Fatalf("result route: %d %s", w.Code, w.Body.String())
	}
}
