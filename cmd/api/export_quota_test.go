package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// Export has its own allowance: with free exports=3 and ops=30, the 4th export
// of a UTC day is a 429 for metric export while ops remain available.
func TestExportHasItsOwnAllowance(t *testing.T) {
	t.Setenv("TIER_ANON_OPS_PER_DAY", "30")
	t.Setenv("TIER_ANON_EXPORTS_PER_DAY", "3")
	cfg := config.Load()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(quotaDispatcher(middleware.QuotaConfig{
		Store: &stubQuotaStore{}, Registry: tiers.New(cfg), Enabled: true, IPHashSecret: quotaIPSalt[:],
	}))
	ok := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) }
	api.POST("/studio/projects/:id/export", ok)
	api.POST("/upload", ok)

	do := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("X-MM-Session-ID", "session-export")
		r.ServeHTTP(w, req)
		return w
	}
	for i := 1; i <= 3; i++ {
		if w := do("/api/studio/projects/p1/export"); w.Code != http.StatusOK {
			t.Fatalf("export %d: want 200, got %d", i, w.Code)
		}
	}
	w := do("/api/studio/projects/p1/export")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("4th export: want 429, got %d", w.Code)
	}
	for _, want := range []string{`"error":"quota_exceeded"`, `"metric":"export"`, `"limit":3`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("429 body missing %s: %s", want, w.Body.String())
		}
	}
	if got := w.Header().Get(middleware.HeaderQuotaRemaining); got != "0" {
		t.Errorf("remaining = %q, want 0", got)
	}
	if w := do("/api/upload"); w.Code != http.StatusOK {
		t.Fatalf("ops must remain available after exports are spent, got %d", w.Code)
	}
}

// A refused (over-height) export is a 403 output_height_limited and does NOT
// spend the export allowance; an allowed one does.
func TestRefusedExportDoesNotSpendQuota(t *testing.T) {
	cfg := config.Load()
	store := &stubQuotaStore{}
	height := 2160 // anonymous cap is 1080
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	reg := tiers.New(cfg)
	api.Use(middleware.TierContext(middleware.TierConfig{Store: store, Registry: reg}, true))
	api.Use(quotaDispatcher(middleware.QuotaConfig{
		Store: store, Registry: reg, Enabled: true, IPHashSecret: quotaIPSalt[:],
	}, map[string]gin.HandlerFunc{
		"export": handlers.ExportHeightGate(func(*gin.Context) (int, bool) { return height, true }),
	}))
	api.POST("/studio/projects/:id/export", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	post := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/studio/projects/p1/export", nil)
		req.Header.Set("X-MM-Session-ID", "s-gate")
		r.ServeHTTP(w, req)
		return w
	}
	w := post()
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error":"output_height_limited"`) ||
		!strings.Contains(w.Body.String(), `"upgradePath":"signup"`) {
		t.Fatalf("want 403 output_height_limited/signup, got %d %s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	n := len(store.counts)
	store.mu.Unlock()
	if n != 0 {
		t.Fatalf("refused export spent quota: %v", store.counts)
	}
	height = 1080
	if w := post(); w.Code != http.StatusOK {
		t.Fatalf("allowed export: got %d", w.Code)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.counts) != 1 {
		t.Fatalf("allowed export must count once: %v", store.counts)
	}
}

// Part 20: render-again starts a render, so it spends the export allowance and
// rides the export rate bucket; the history list and frame export do not.
func TestIsStudioExportPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/studio/projects/p1/export":              true,
		"/api/studio/projects/p1/exports/e1/again":    true,
		"/api/studio/projects/p1/exports":             false,
		"/api/studio/projects/p1/frame":               false,
		"/api/studio/projects/p1/captions/generate":   false,
		"/api/studio/assets/a1/export":                false,
		"/api/studio/projects/p1/versions/v1/restore": false,
	} {
		if got := isStudioExportPath(path); got != want {
			t.Errorf("%s: got %v want %v", path, got, want)
		}
	}
}
