package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
)

// MM81 "Download as MP3": POST /api/download/:jobId/mp3 spends no quota (never
// ai_ops) and has its own result_mp3 bucket (20/h/session, 40/h/IP by default);
// the status GET, /download and /link are not in it.
func TestResultMP3BucketAndNoQuota(t *testing.T) {
	for _, k := range []string{"RATE_LIMIT_RESULT_MP3_PER_SESSION_PER_HOUR", "RATE_LIMIT_RESULT_MP3_PER_IP_PER_HOUR"} {
		t.Setenv(k, "")
	}
	cfg := config.Load()
	if cfg.RateLimitResultMP3PerSessionPerHour != 20 || cfg.RateLimitResultMP3PerIPPerHour != 40 {
		t.Fatalf("defaults %d/%d", cfg.RateLimitResultMP3PerSessionPerHour, cfg.RateLimitResultMP3PerIPPerHour)
	}
	for _, path := range []string{"/api/download/j1/mp3", "/api/download/j1/link", "/api/download/j1"} {
		if got := metricFor(path); got != "" {
			t.Errorf("%s spends %q; downloads must be free", path, got)
		}
	}

	type hit struct {
		key              string
		session, ipLimit int
	}
	var mu sync.Mutex
	var hits []hit
	spy := func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
		return func(c *gin.Context) {
			mu.Lock()
			hits = append(hits, hit{routeKey, sessionLimit, ipLimit})
			mu.Unlock()
			c.Next()
		}
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(routeLimitDispatcher(cfg, limits.New(nil, cfg, nil, nil), spy))
	r.Any("/*any", func(c *gin.Context) { c.Status(200) })
	for _, tc := range []struct{ method, path, want string }{
		{http.MethodPost, "/api/download/j1/mp3", "result_mp3"},
		{http.MethodGet, "/api/download/j1/mp3", ""},
		{http.MethodPost, "/api/download/j1/link", ""},
		{http.MethodPost, "/api/download/a/b/mp3", ""},
		{http.MethodPost, "/api/download//mp3", ""},
	} {
		hits = nil
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
		got := ""
		if len(hits) == 1 {
			got = hits[0].key
			if got == "result_mp3" && (hits[0].session != 20 || hits[0].ipLimit != 40) {
				t.Errorf("bucket limits %+v", hits[0])
			}
		}
		if got != tc.want {
			t.Errorf("%s %s: bucket %q want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
