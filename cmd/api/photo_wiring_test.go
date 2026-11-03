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
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
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
	for _, path := range []string{"/api/photo/projects", "/api/photo/projects/x", "/api/photo/capabilities", "/api/photo/projects/x/proxy", "/api/photo/projects/x/files/bases/h.proxy.jpg"} {
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
		{http.MethodPost, "/api/photo/projects/abc/export", "photo_export"},
		{http.MethodPost, "/api/photo/batch", "photo_export"},
		{http.MethodPost, "/api/photo/export", "photo_export"},
		{http.MethodPost, "/api/photo/projects/abc/masks/subject", "photo_ai"},
		{http.MethodPost, "/api/photo/projects/abc/heal", "photo_ai"},
		{http.MethodPost, "/api/photo/masks/subject", "photo_ai"},
		{http.MethodPost, "/api/photo/heal", "photo_ai"},
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

// MM96: the quota routes for Aperture (export both forms and subject mask both
// forms spend ops, heal both forms spend ai_ops, batch is charged inside the
// handler) and the env split of the two new buckets.
func TestPhotoQuotaRoutesAndBuckets(t *testing.T) {
	cases := map[string]string{
		"/api/photo/projects/3f2b1c1e-0000-4000-8000-000000000001/export": models.MetricOps,
		"/api/photo/export": models.MetricOps,
		"/api/photo/projects/3f2b1c1e-0000-4000-8000-000000000001/masks/subject": models.MetricOps,
		"/api/photo/masks/subject":                                      models.MetricOps,
		"/api/photo/projects/3f2b1c1e-0000-4000-8000-000000000001/heal": models.MetricAIOps,
		"/api/photo/heal":                                               models.MetricAIOps,
		"/api/photo/batch":                                              quotaMetricByJob,
		"/api/photo/projects":                                           "",
		"/api/photo/projects/x/export/extra":                            "",
	}
	for path, want := range cases {
		if got := metricFor(path); got != want {
			t.Errorf("%s: metric %q want %q", path, got, want)
		}
	}
	for _, k := range []string{"RATE_LIMIT_PHOTO_EXPORT_PER_SESSION_PER_HOUR", "RATE_LIMIT_PHOTO_EXPORT_PER_IP_PER_HOUR",
		"RATE_LIMIT_PHOTO_AI_PER_SESSION_PER_HOUR", "RATE_LIMIT_PHOTO_AI_PER_IP_PER_HOUR",
		"RATE_LIMIT_UPLOADS_PER_SESSION_PER_HOUR", "RATE_LIMIT_UPLOADS_PER_IP_PER_HOUR"} {
		t.Setenv(k, "")
	}
	cfg := config.Load()
	if cfg.RateLimitPhotoAIPerSessionPerHour != 30 || cfg.RateLimitPhotoAIPerIPPerHour != 60 ||
		cfg.RateLimitPhotoExportPerSessionPerHour != cfg.RateLimitUploadsPerSessionPerHour {
		t.Fatalf("defaults: ai %d/%d export %d", cfg.RateLimitPhotoAIPerSessionPerHour, cfg.RateLimitPhotoAIPerIPPerHour, cfg.RateLimitPhotoExportPerSessionPerHour)
	}
	t.Setenv("RATE_LIMIT_PHOTO_AI_PER_SESSION_PER_HOUR", "5")
	if got := config.Load().RateLimitPhotoAIPerSessionPerHour; got != 5 {
		t.Fatalf("override = %d", got)
	}
}
