package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// ---------------------------------------------------------------------------
// B3 — per-route rate limits were dead code.
//
// `router.Use(...)` used to be called at the END of setupRouter, after every
// route had been registered. Gin snapshots a group's middleware slice into each
// route at registration time, so an engine-level Use() afterwards only affects
// the 404/405 chains: all sixteen buckets never executed, and nothing failed
// loudly. These tests are the only thing that keeps that fixed.
// ---------------------------------------------------------------------------

// TestGinUseAfterRegistrationDoesNotReachRoutes pins the gin semantic that
// caused the bug. If a future gin release changes this, the second assertion
// starts failing and we learn about it here rather than in production.
func TestGinUseAfterRegistrationDoesNotReachRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newEngine := func(useBeforeRegister bool) (*gin.Engine, *bool) {
		ran := false
		marker := func(c *gin.Context) { ran = true; c.Next() }
		r := gin.New()
		if useBeforeRegister {
			r.Use(marker)
		}
		g := r.Group("/api")
		g.POST("/upload", func(c *gin.Context) { c.Status(http.StatusOK) })
		if !useBeforeRegister {
			r.Use(marker)
		}
		return r, &ran
	}

	before, ranBefore := newEngine(true)
	after, ranAfter := newEngine(false)

	for _, r := range []*gin.Engine{before, after} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/upload", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected the route to respond 200, got %d", w.Code)
		}
	}

	if !*ranBefore {
		t.Error("middleware installed BEFORE registration must run on the route")
	}
	if *ranAfter {
		t.Error("gin semantics changed: middleware installed after registration now reaches routes")
	}
}

// TestRouteLimitDispatcherMatchesGatedRoutes checks the rule table itself: the
// dispatcher has to recognise both the literal paths and the parameterized
// Content Studio ones, and must leave everything else alone.
func TestRouteLimitDispatcherMatchesGatedRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	limiter := limits.New(nil, cfg, nil, nil)

	var mu sync.Mutex
	var hits []string
	spy := func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
		return func(c *gin.Context) {
			mu.Lock()
			hits = append(hits, routeKey)
			mu.Unlock()
			c.Next()
		}
	}

	r := gin.New()
	r.Use(routeLimitDispatcher(cfg, limiter, spy))
	r.POST("/api/upload", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/api/studio/projects/abc-123/export", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/api/job/xyz/cancel", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/api/upload", func(c *gin.Context) { c.Status(http.StatusOK) })

	cases := []struct {
		method, path string
		wantKey      string
	}{
		{http.MethodPost, "/api/upload", "upload"},
		{http.MethodPost, "/api/studio/projects/abc-123/export", "studio_project_export"},
		// Not a gated route.
		{http.MethodPost, "/api/job/xyz/cancel", ""},
		// GET never spends a bucket.
		{http.MethodGet, "/api/upload", ""},
	}

	for _, tc := range cases {
		mu.Lock()
		hits = nil
		mu.Unlock()

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: expected 200, got %d", tc.method, tc.path, w.Code)
		}

		mu.Lock()
		got := strings.Join(hits, ",")
		mu.Unlock()
		if got != tc.wantKey {
			t.Errorf("%s %s: dispatcher hit %q, want %q", tc.method, tc.path, got, tc.wantKey)
		}
	}
}

// TestSetupRouterAppliesRateLimitsToRegisteredRoutes is the actual B3
// regression test: it builds the real router and asserts that a POST to
// /api/upload passes through routeLimitDispatcher. Before the fix this test
// fails — the request is served, but the guard never runs.
func TestSetupRouterAppliesRateLimitsToRegisteredRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var hits []string
	original := newRouteGuard
	newRouteGuard = func(_ *limits.Limiter) func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
		return func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
			return func(c *gin.Context) {
				mu.Lock()
				hits = append(hits, routeKey)
				mu.Unlock()
				c.Next()
			}
		}
	}
	t.Cleanup(func() { newRouteGuard = original })

	cfg := config.Load()
	// Everything the upload route touches beyond the middleware chain is nil
	// here; the request will fail somewhere in the handler, and that is fine.
	// What is under test is whether the guard ran at all.
	router := setupRouter(
		cfg, nil, nil, nil, nil, nil, nil, tiers.New(cfg), nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, limits.New(nil, cfg, nil, nil), metrics.New(),
		// nil analytics emitter: an unconfigured emitter is a supported state and
		// every emission is a no-op, which is exactly what this test wants.
		nil,
	)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/upload", strings.NewReader(""))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	func() {
		// The handler itself is nil-dependency and may panic; gin.New() has no
		// Recovery middleware in TestMode for our purposes, so contain it.
		defer func() { _ = recover() }()
		router.ServeHTTP(w, req)
	}()

	mu.Lock()
	defer mu.Unlock()
	if len(hits) == 0 {
		t.Fatal("routeLimitDispatcher never ran for POST /api/upload — per-route rate limits are dead code again (B3)")
	}
	if hits[0] != "upload" {
		t.Fatalf("expected the 'upload' bucket, got %q", hits[0])
	}
}

// ---------------------------------------------------------------------------
// Quota enforcement
// ---------------------------------------------------------------------------

// stubQuotaStore counts in memory so the middleware can be exercised without
// Postgres.
type stubQuotaStore struct {
	mu      sync.Mutex
	counts  map[string]int64
	user    *models.User
	userErr error
	incErr  error
}

func (s *stubQuotaStore) Enabled() bool { return true }

func (s *stubQuotaStore) UserByFirebaseUID(_ context.Context, _ string) (*models.User, error) {
	return s.user, s.userErr
}

func (s *stubQuotaStore) IncrementUsage(_ context.Context, kind models.UsageSubjectKind, key, metric string, _ time.Time) (int64, error) {
	if s.incErr != nil {
		return 0, s.incErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = map[string]int64{}
	}
	k := string(kind) + "|" + key + "|" + metric
	s.counts[k]++
	return s.counts[k], nil
}

// quotaTestRouter mounts the dispatcher exactly the way setupRouter does:
// on the group, before any route is registered.
func quotaTestRouter(store middleware.QuotaStore, registry *tiers.Registry) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(quotaDispatcher(middleware.QuotaConfig{
		Store:        store,
		Registry:     registry,
		Enabled:      true,
		IPHashSecret: quotaIPSalt[:],
	}))
	api.POST("/upload", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	api.POST("/image-restore/start", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	api.GET("/job/:id", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

// The anonymous allowance is 5 operations a day: five succeed, the sixth is a
// 429 that tells the UI exactly what to offer next.
func TestQuotaExhaustsAnonymousAllowance(t *testing.T) {
	cfg := config.Load()
	r := quotaTestRouter(&stubQuotaStore{}, tiers.New(cfg))

	for i := 1; i <= cfg.TierAnonOpsPerDay; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
		req.Header.Set("X-MM-Session-ID", "session-a")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d (%s)", i, w.Code, w.Body.String())
		}
		if got := w.Header().Get(middleware.HeaderTier); got != string(tiers.TierAnonymous) {
			t.Errorf("request %d: X-MM-Tier = %q, want %q", i, got, tiers.TierAnonymous)
		}
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	req.Header.Set("X-MM-Session-ID", "session-a")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the allowance is spent, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"error":"quota_exceeded"`, `"tier":"anonymous"`, `"upgradePath":"signup"`, `"resetsAt"`} {
		if !strings.Contains(body, want) {
			t.Errorf("429 body is missing %s: %s", want, body)
		}
	}
	if w.Header().Get(middleware.HeaderQuotaRemaining) != "0" {
		t.Errorf("X-MM-Quota-Remaining = %q, want 0", w.Header().Get(middleware.HeaderQuotaRemaining))
	}
}

// Separate sessions have separate allowances — one visitor exhausting theirs
// must not lock anyone else out.
func TestQuotaIsPerSubject(t *testing.T) {
	cfg := config.Load()
	r := quotaTestRouter(&stubQuotaStore{}, tiers.New(cfg))

	for i := 0; i <= cfg.TierAnonOpsPerDay; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
		req.Header.Set("X-MM-Session-ID", "session-a")
		r.ServeHTTP(w, req)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	req.Header.Set("X-MM-Session-ID", "session-b")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("a second session must have its own allowance, got %d", w.Code)
	}
}

// ai_ops is a separate, much tighter budget — spending it must not consume the
// general ops allowance, and vice versa.
func TestQuotaMetricsAreIndependent(t *testing.T) {
	cfg := config.Load()
	r := quotaTestRouter(&stubQuotaStore{}, tiers.New(cfg))

	// Anonymous gets 1 AI op a day.
	for i := 0; i < cfg.TierAnonAIOpsPerDay; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/image-restore/start", nil)
		req.Header.Set("X-MM-Session-ID", "session-c")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("AI op %d: expected 200, got %d", i+1, w.Code)
		}
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image-restore/start", nil)
	req.Header.Set("X-MM-Session-ID", "session-c")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the AI allowance to be spent, got %d", w.Code)
	}

	// The ordinary ops budget is untouched.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	req.Header.Set("X-MM-Session-ID", "session-c")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("spending ai_ops must not spend ops, got %d", w.Code)
	}
}

// A GET is a status poll or a download of work already paid for; it must never
// spend an operation.
func TestQuotaIgnoresReads(t *testing.T) {
	cfg := config.Load()
	store := &stubQuotaStore{}
	r := quotaTestRouter(store, tiers.New(cfg))

	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/job/abc", nil)
		req.Header.Set("X-MM-Session-ID", "session-d")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("poll %d: expected 200, got %d", i, w.Code)
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.counts) != 0 {
		t.Fatalf("reads spent quota: %v", store.counts)
	}
}

// A dead database must never break the free product. An increment error lets
// the request through.
func TestQuotaFailsOpenOnStoreError(t *testing.T) {
	cfg := config.Load()
	r := quotaTestRouter(&stubQuotaStore{incErr: context.DeadlineExceeded}, tiers.New(cfg))

	for i := 0; i < cfg.TierAnonOpsPerDay+3; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
		req.Header.Set("X-MM-Session-ID", "session-e")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: quota must fail open, got %d", i, w.Code)
		}
	}
}
