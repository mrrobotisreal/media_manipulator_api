package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
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
