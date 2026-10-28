package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM43: the Motion routes are mounted on /api, a PUT carrying If-Match passes
// CORS preflight, and the generic quota dispatcher never charges /api/vector/*
// (Motion counts its own allowance, transactionally, inside the handler).
func TestVectorRoutesMountedAndNotQuotaCharged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	reg := tiers.New(cfg)
	vh := handlers.NewVectorHandler(nil, cfg, reg, nil, nil, nil) // no DB: every route 503s
	router := setupRouter(
		cfg, nil, nil, vh, nil, nil, nil, nil, reg, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, limits.New(nil, cfg, nil, nil), metrics.New(), nil,
	)
	for _, rq := range []struct{ method, path string }{
		{"GET", "/api/vector/projects"},
		{"POST", "/api/vector/projects"},
		{"PUT", "/api/vector/projects/abc"},
		{"POST", "/api/vector/exports"},
		{"POST", "/api/vector/vectorize"},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(rq.method, rq.path, strings.NewReader(`{}`)))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want the handler's 503 (route not mounted?)", rq.method, rq.path, w.Code)
		}
		if w.Header().Get("X-MM-Quota-Limit") != "" {
			t.Errorf("%s %s was charged by the generic quota dispatcher", rq.method, rq.path)
		}
	}

	req := httptest.NewRequest(http.MethodOptions, "/api/vector/projects/abc", nil)
	req.Header.Set("Origin", "https://media-manipulator.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	req.Header.Set("Access-Control-Request-Headers", "if-match,content-type")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if allowed := strings.ToLower(w.Header().Get("Access-Control-Allow-Headers")); !strings.Contains(allowed, "if-match") {
		t.Fatalf("CORS preflight must allow If-Match, got %q (status %d)", allowed, w.Code)
	}
}

func TestRetentionPrefixesIncludeVector(t *testing.T) {
	got := retentionS3Prefixes(config.Load())
	// MM77: voices/ follows vector/; both must be on the allowlist.
	has := map[string]bool{}
	for _, p := range got {
		has[p] = true
	}
	if !has["vector"] {
		t.Fatalf("sweeper prefixes %v must include the Motion thumbnail prefix", got)
	}
	if !has["voices"] {
		t.Fatalf("sweeper prefixes %v must include the voices prefix (MM77)", got)
	}
}
