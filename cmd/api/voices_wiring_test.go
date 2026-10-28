package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// metricFor is the quota metric a POST to path spends, "" when ungated.
func metricFor(path string) string {
	for _, r := range quotaRoutes {
		if (r.matches != nil && r.matches(path)) || (r.path != "" && r.path == path) {
			return r.metric
		}
	}
	return ""
}

// MM77 acceptance 4: ai_ops for create/design/speak/preset-speak/convert, ops
// for shift; preview, rename, delete and reads are not gated.
func TestVoicesQuotaRoutes(t *testing.T) {
	cases := map[string]string{
		"/api/voices":        models.MetricAIOps,
		"/api/voices/design": models.MetricAIOps,
		"/api/voices/3f2b1c1e-0000-4000-8000-000000000001/speak": models.MetricAIOps,
		"/api/voices/presets/Vivian/speak":                       models.MetricAIOps,
		"/api/voices/convert":                                    models.MetricAIOps,
		"/api/voices/shift":                                      models.MetricOps,
		"/api/voices/design/preview":                             "",
		"/api/voices/capabilities":                               "",
		"/api/voices/3f2b1c1e-0000-4000-8000-000000000001":       "",
	}
	for path, want := range cases {
		if got := metricFor(path); got != want {
			t.Errorf("%s: metric %q want %q", path, got, want)
		}
	}
}

// voicesQuotaRouter mounts identity, tier, the voices pre-charge gate and the
// quota dispatcher exactly in setupRouter's order, then the real voices routes.
func voicesQuotaRouter(t *testing.T, store *stubQuotaStore, h *handlers.VoicesHandler, uid string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	registry := tiers.New(cfg)
	r := gin.New()
	api := r.Group("/api")
	api.Use(func(c *gin.Context) {
		if uid != "" {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: uid})
		}
		c.Next()
	})
	api.Use(middleware.TierContext(middleware.TierConfig{Store: store, Registry: registry}, true))
	api.Use(voicesPreCharge())
	api.Use(quotaDispatcher(middleware.QuotaConfig{Store: store, Registry: registry, Enabled: true, IPHashSecret: quotaIPSalt[:]}))
	handlers.RegisterVoicesRoutes(api, h)
	return r
}

type downBackend struct{}

func (downBackend) Enabled() bool { return true }
func (downBackend) Health(context.Context) (*services.VoicesHealth, error) {
	return nil, services.ErrVoicesUnavailable
}
func (downBackend) Presets(context.Context) ([]services.VoicePreset, error) {
	return nil, services.ErrVoicesUnavailable
}

func newDownVoicesHandler(t *testing.T) *handlers.VoicesHandler {
	cfg := config.Load()
	cfg.UploadDir, cfg.OutputDir = t.TempDir(), t.TempDir()
	cfg.Voices.Enabled = true
	jobs := services.NewJobManager()
	return handlers.NewVoicesHandler(handlers.VoicesDeps{Cfg: cfg, Registry: tiers.New(cfg), Jobs: jobs, Backend: downBackend{},
		Service: services.NewVoicesService(cfg, jobs, nil, nil), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// MM77 acceptance 4/5: with mm-voices down, a counted voices request answers
// 503 voices_unavailable and the MM73 dispatcher refunds the unit it spent, so
// the ledger is back to zero.
func TestVoicesServiceDownRefundsTheUnit(t *testing.T) {
	store := &stubQuotaStore{}
	r := voicesQuotaRouter(t, store, newDownVoicesHandler(t), "")
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/voices/presets/Vivian/speak", strings.NewReader(`{"text":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-MM-Session-ID", "sess-down")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "voices_unavailable") {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if n := store.counts["session|sess-down|ai_ops"]; n != 0 {
		t.Fatalf("a 503 from a down service must be refunded; ledger shows %d", n)
	}
}

// The sign-in gate runs before the charge: an anonymous create is 401 and
// never touches the ledger (not even charge-then-refund).
func TestVoicesSignInGateRunsBeforeCharge(t *testing.T) {
	store := &stubQuotaStore{}
	r := voicesQuotaRouter(t, store, newDownVoicesHandler(t), "")
	for _, path := range []string{"/api/voices", "/api/voices/design", "/api/voices/3f2b1c1e-0000-4000-8000-000000000001/speak", "/api/voices/convert"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("X-MM-Session-ID", "sess-anon")
		r.ServeHTTP(w, req)
		if w.Code != 401 && w.Code != 403 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if w.Header().Get(middleware.HeaderQuotaLimit) != "" {
			t.Fatalf("%s: refused before the charge, so no quota headers", path)
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.counts) != 0 {
		t.Fatalf("nothing may be incremented: %v", store.counts)
	}
}

// MM77 acceptance 8: the voices_create bucket covers POST /api/voices and
// /api/voices/design at 5/h/session and 20/h/IP; speak/convert/shift are not
// in it; operators are exempt (limiter.RouteExempt, the restore buckets' rule).
func TestVoicesCreateBucket(t *testing.T) {
	for _, k := range []string{"VOICES_RATE_LIMIT_PER_SESSION_PER_HOUR", "VOICES_RATE_LIMIT_PER_IP_PER_HOUR"} {
		t.Setenv(k, "")
	}
	cfg := config.Load()
	if cfg.Voices.RateLimitPerSessionPerHour != 5 || cfg.Voices.RateLimitPerIPPerHour != 20 {
		t.Fatalf("defaults: %+v", cfg.Voices)
	}
	type hit struct {
		key, tool        string
		session, ipLimit int
	}
	var mu sync.Mutex
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
	r.POST("/*any", func(c *gin.Context) { c.Status(200) })
	for path, want := range map[string]string{
		"/api/voices":                      "voices_create",
		"/api/voices/design":               "voices_create",
		"/api/voices/design/preview":       "",
		"/api/voices/presets/Vivian/speak": "",
		"/api/voices/convert":              "",
		"/api/voices/shift":                "",
	} {
		hits = nil
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		got := ""
		if len(hits) == 1 {
			got = hits[0].key
			if got == "voices_create" && (hits[0].session != 5 || hits[0].ipLimit != 20 || hits[0].tool != "voices") {
				t.Errorf("%s: bucket %+v", path, hits[0])
			}
		}
		if got != want {
			t.Errorf("%s: bucket %q want %q", path, got, want)
		}
	}
	// Operator exemption is the limiter's RouteExempt, which main wires to the
	// tier's RateLimitExempt; the operator row has it.
	op := tiers.New(&config.Config{OperatorEmails: []string{"o@x"}}).For(tiers.TierOperator)
	if !op.RateLimitExempt {
		t.Fatal("operator must be exempt from voices_create")
	}
}
