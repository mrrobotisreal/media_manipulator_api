package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func okProbe(context.Context) error { return nil }
func failProbe(context.Context) error {
	return errors.New("dial tcp 10.1.2.3:5432: secret-host password authentication failed /var/data/bucket-name")
}

func healthGet(t *testing.T, h *SystemHealthHandler) (int, string, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r.Group("/api"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/system/health", nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v %s", err, w.Body.String())
	}
	return w.Code, w.Body.String(), body
}

func TestSystemHealth_StatusRules(t *testing.T) {
	cases := []struct {
		name       string
		db, redis  func(context.Context) error
		storage    func(context.Context) error
		wantStatus string
		wantCode   int
		wantRedis  bool
	}{
		{"all ok", okProbe, okProbe, okProbe, "healthy", 200, true},
		{"no redis configured", okProbe, nil, okProbe, "healthy", 200, false},
		{"redis fails", okProbe, failProbe, okProbe, "degraded", 200, true},
		{"storage fails", okProbe, okProbe, failProbe, "degraded", 200, true},
		{"db fails", failProbe, okProbe, okProbe, "down", 503, true},
		{"db and redis fail", failProbe, failProbe, okProbe, "down", 503, true},
		{"db nil probe is down", nil, nil, okProbe, "down", 503, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewSystemHealthHandler(SystemHealthProbes{Database: tc.db, Redis: tc.redis, Storage: tc.storage})
			code, raw, body := healthGet(t, h)
			if code != tc.wantCode || body["status"] != tc.wantStatus {
				t.Fatalf("got %d %v, want %d %s", code, body["status"], tc.wantCode, tc.wantStatus)
			}
			checks := body["checks"].(map[string]any)
			if _, has := checks["redis"]; has != tc.wantRedis {
				t.Fatalf("redis present=%v want %v: %s", has, tc.wantRedis, raw)
			}
			for _, leak := range []string{"secret-host", "10.1.2.3", "/var/data", "bucket-name", "password", "dial tcp"} {
				if strings.Contains(raw, leak) {
					t.Fatalf("response leaks %q: %s", leak, raw)
				}
			}
			if body["service"] != "media_manipulator_api" || body["version"] == "" {
				t.Fatalf("service/version: %s", raw)
			}
		})
	}
}

func TestSystemHealth_ShapeAndQueue(t *testing.T) {
	h := NewSystemHealthHandler(SystemHealthProbes{
		Database: okProbe, Storage: okProbe,
		Queue: func() HealthQueue { return HealthQueue{GPUEnabled: true, Running: 1, Waiting: 3} },
	})
	_, _, body := healthGet(t, h)
	q := body["queue"].(map[string]any)
	if q["gpuEnabled"] != true || q["running"] != float64(1) || q["waiting"] != float64(3) {
		t.Fatalf("queue: %v", q)
	}
	if _, ok := body["uptimeSeconds"].(float64); !ok {
		t.Fatalf("uptimeSeconds missing: %v", body)
	}
	checks := body["checks"].(map[string]any)
	if checks["database"] != "ok" || checks["storage"] != "ok" {
		t.Fatalf("checks: %v", checks)
	}

	// No queue probe: GPU off, zeros, keys still present.
	h2 := NewSystemHealthHandler(SystemHealthProbes{Database: okProbe, Storage: okProbe})
	_, raw, _ := healthGet(t, h2)
	if !strings.Contains(raw, `"queue":{"gpuEnabled":false,"running":0,"waiting":0}`) {
		t.Fatalf("disabled queue shape: %s", raw)
	}
}

func TestSystemHealth_CachesForFiveSeconds(t *testing.T) {
	var calls atomic.Int32
	h := NewSystemHealthHandler(SystemHealthProbes{
		Database: func(context.Context) error { calls.Add(1); return nil },
		Storage:  okProbe,
	})
	clock := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return clock }
	h.started = clock.Add(-time.Minute)

	_, _, b1 := healthGet(t, h)
	clock = clock.Add(4 * time.Second)
	_, _, b2 := healthGet(t, h)
	if calls.Load() != 1 {
		t.Fatalf("probes ran %d times within the TTL, want 1", calls.Load())
	}
	if b1["uptimeSeconds"] != float64(60) || b2["uptimeSeconds"] != float64(64) {
		t.Fatalf("uptime should keep advancing from cache: %v %v", b1["uptimeSeconds"], b2["uptimeSeconds"])
	}
	clock = clock.Add(2 * time.Second) // 6 s since first probe
	healthGet(t, h)
	if calls.Load() != 2 {
		t.Fatalf("probes ran %d times after the TTL, want 2", calls.Load())
	}
}

func TestSystemHealth_HungProbeIsBoundedAndFails(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := NewSystemHealthHandler(SystemHealthProbes{
		Database: okProbe,
		Redis:    func(context.Context) error { <-release; return nil }, // ignores its ctx
		Storage:  okProbe,
	})
	start := time.Now()
	code, _, body := healthGet(t, h)
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("took %s, probe timeout not enforced", el)
	}
	if code != 200 || body["status"] != "degraded" || body["checks"].(map[string]any)["redis"] != "fail" {
		t.Fatalf("hung redis: %d %v", code, body)
	}
}

func TestSystemHealth_PanickingProbeFails(t *testing.T) {
	h := NewSystemHealthHandler(SystemHealthProbes{
		Database: func(context.Context) error { panic("boom") },
		Storage:  okProbe,
	})
	code, raw, _ := healthGet(t, h)
	if code != 503 || strings.Contains(raw, "boom") {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestStorageWritableProbe(t *testing.T) {
	if err := StorageWritableProbe(t.TempDir())(context.Background()); err != nil {
		t.Fatalf("writable dir: %v", err)
	}
	if err := StorageWritableProbe(t.TempDir() + "/missing")(context.Background()); err == nil {
		t.Fatal("missing dir must fail")
	}
}

func TestBuildVersionNeverEmpty(t *testing.T) {
	if BuildVersion() == "" {
		t.Fatal("empty version")
	}
}
