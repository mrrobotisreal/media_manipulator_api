package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
)

// MM95: only POST /api/photo/projects is rate-limited (the photo_projects
// bucket, uploads numbers by default), no photo route is quota-charged, and
// env overrides split the bucket from uploads.
func TestPhotoProjectsBucketWiring(t *testing.T) {
	for _, k := range []string{"RATE_LIMIT_PHOTO_PROJECTS_PER_SESSION_PER_HOUR", "RATE_LIMIT_PHOTO_PROJECTS_PER_IP_PER_HOUR",
		"RATE_LIMIT_UPLOADS_PER_SESSION_PER_HOUR", "RATE_LIMIT_UPLOADS_PER_IP_PER_HOUR"} {
		t.Setenv(k, "")
	}
	cfg := config.Load()
	if cfg.RateLimitPhotoProjectsPerSessionPerHour != cfg.RateLimitUploadsPerSessionPerHour ||
		cfg.RateLimitPhotoProjectsPerIPPerHour != cfg.RateLimitUploadsPerIPPerHour {
		t.Fatalf("defaults must equal the uploads numbers: %d/%d vs %d/%d",
			cfg.RateLimitPhotoProjectsPerSessionPerHour, cfg.RateLimitPhotoProjectsPerIPPerHour,
			cfg.RateLimitUploadsPerSessionPerHour, cfg.RateLimitUploadsPerIPPerHour)
	}
	for _, path := range []string{"/api/photo/projects", "/api/photo/projects/x", "/api/photo/capabilities", "/api/photo/projects/x/proxy"} {
		if got := metricFor(path); got != "" {
			t.Errorf("%s spends %q; project CRUD must be free of charge", path, got)
		}
	}

	t.Setenv("RATE_LIMIT_PHOTO_PROJECTS_PER_SESSION_PER_HOUR", "7")
	t.Setenv("RATE_LIMIT_PHOTO_PROJECTS_PER_IP_PER_HOUR", "9")
	cfg = config.Load()
	if cfg.RateLimitPhotoProjectsPerSessionPerHour != 7 || cfg.RateLimitPhotoProjectsPerIPPerHour != 9 {
		t.Fatalf("overrides %d/%d", cfg.RateLimitPhotoProjectsPerSessionPerHour, cfg.RateLimitPhotoProjectsPerIPPerHour)
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
		{http.MethodPost, "/api/photo/projects", "photo_projects"},
		{http.MethodGet, "/api/photo/projects", ""},
		{http.MethodPut, "/api/photo/projects/abc", ""},
		{http.MethodGet, "/api/photo/capabilities", ""},
		{http.MethodPost, "/api/photo/projects/abc/export", ""}, // MM96's bucket
	} {
		hits = nil
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, strings.NewReader("")))
		got := ""
		if len(hits) == 1 {
			got = hits[0].key
			if got == "photo_projects" && (hits[0].session != 7 || hits[0].ipLimit != 9) {
				t.Errorf("bucket limits %+v", hits[0])
			}
		}
		if got != tc.want {
			t.Errorf("%s %s: bucket %q want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
