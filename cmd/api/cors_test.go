package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM92: the per-locale translator deployments (`<prefix>.translate.media-manipulator.com`)
// call this API from the browser. Until 2026-10-31 they were missing from the CORS
// allow-list, so every request from them failed and their /status page read "Down".
func TestCORSAllowsTranslatorDeployments(t *testing.T) {
	cfg := config.Load()
	router := setupRouter(
		cfg, nil, nil, nil, nil, nil, nil, nil, tiers.New(cfg), nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, limits.New(nil, cfg, nil, nil), metrics.New(),
		nil,
	)

	cases := []struct {
		origin  string
		allowed bool
	}{
		{"https://www.media-manipulator.com", true},
		{"https://media-manipulator.com", true},
		{"https://de.translate.media-manipulator.com", true},
		{"https://uk.translate.media-manipulator.com", true},
		{"https://pt-br.translate.media-manipulator.com", true},
		{"https://zh-cn.translate.media-manipulator.com", true},
		// Not ours: the scheme, host or suffix differs.
		{"http://de.translate.media-manipulator.com", false},
		{"https://translate.media-manipulator.com", false},
		{"https://de.translate.media-manipulator.com.evil.example", false},
		{"https://evil.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.origin, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
			req.Header.Set("Origin", tc.origin)
			router.ServeHTTP(w, req)
			got := w.Header().Get("Access-Control-Allow-Origin")
			if tc.allowed && got != tc.origin {
				t.Fatalf("origin %q: want Access-Control-Allow-Origin=%q, got %q (status %d)", tc.origin, tc.origin, got, w.Code)
			}
			if !tc.allowed && got != "" {
				t.Fatalf("origin %q must be refused, but Access-Control-Allow-Origin=%q", tc.origin, got)
			}
		})
	}
}
