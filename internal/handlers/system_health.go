package handlers

import (
	"context"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// System health (MM34). GET /api/system/health is public, unauthenticated and
// feeds the /status page. It deliberately reports only coarse, fixed values:
// "ok" or "fail" per dependency, never an error string, hostname, path or
// bucket name. The body is trusted by the UI even on a 503.

const (
	healthProbeTimeout = time.Second
	healthCacheTTL     = 5 * time.Second
	healthServiceName  = "media_manipulator_api"

	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthDown     = "down"
	checkOK        = "ok"
	checkFail      = "fail"
)

// HealthQueue is the GPU queue view the status page shows.
type HealthQueue struct {
	GPUEnabled bool `json:"gpuEnabled"`
	Running    int  `json:"running"`
	Waiting    int  `json:"waiting"`
}

// HealthChecks is the per-dependency result. Redis is omitted when it is not
// configured.
type HealthChecks struct {
	Database string `json:"database"`
	Redis    string `json:"redis,omitempty"`
	Storage  string `json:"storage"`
}

// SystemHealthResponse is the exact JSON the status page consumes.
type SystemHealthResponse struct {
	Status        string       `json:"status"`
	Service       string       `json:"service"`
	Version       string       `json:"version"`
	UptimeSeconds int64        `json:"uptimeSeconds"`
	Checks        HealthChecks `json:"checks"`
	Queue         HealthQueue  `json:"queue"`
}

// SystemHealthProbes are the injectable dependency checks. A nil Database or
// Storage probe counts as a failure; a nil Redis probe means Redis is not
// configured (the check is omitted); a nil Queue reports a disabled GPU queue.
type SystemHealthProbes struct {
	Database func(ctx context.Context) error
	Redis    func(ctx context.Context) error
	Storage  func(ctx context.Context) error
	Queue    func() HealthQueue
}

// SystemHealthHandler serves GET /api/system/health with a 5 s result cache.
type SystemHealthHandler struct {
	probes  SystemHealthProbes
	version string
	started time.Time
	now     func() time.Time

	mu       sync.Mutex
	cachedAt time.Time
	cached   *SystemHealthResponse
}

// NewSystemHealthHandler builds the handler; uptime counts from now.
func NewSystemHealthHandler(p SystemHealthProbes) *SystemHealthHandler {
	return &SystemHealthHandler{probes: p, version: BuildVersion(), started: time.Now(), now: time.Now}
}

// BuildVersion is the short VCS revision stamped by `go build`, else "dev".
func BuildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				if len(s.Value) > 7 {
					return s.Value[:7]
				}
				return s.Value
			}
		}
	}
	return "dev"
}

// StorageWritableProbe returns a probe that creates and removes a temp file in
// dir: local output storage that is full or read-only is a real outage. No
// path ever reaches the response.
func StorageWritableProbe(dir string) func(context.Context) error {
	return func(context.Context) error {
		f, err := os.CreateTemp(dir, ".health-*")
		if err != nil {
			return err
		}
		name := f.Name()
		_ = f.Close()
		return os.Remove(name)
	}
}

// Register mounts the route on r.
func (h *SystemHealthHandler) Register(r gin.IRoutes) { r.GET("/system/health", h.Get) }

// Get answers 200 for healthy/degraded and 503 for down.
func (h *SystemHealthHandler) Get(c *gin.Context) {
	resp := h.snapshot()
	code := http.StatusOK
	if resp.Status == HealthDown {
		code = http.StatusServiceUnavailable
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(code, resp)
}

func (h *SystemHealthHandler) snapshot() SystemHealthResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if h.cached != nil && now.Sub(h.cachedAt) < healthCacheTTL {
		out := *h.cached
		out.UptimeSeconds = h.uptime(now)
		return out
	}
	// Probes run detached from the request: a client that hangs up must not
	// poison the cached result for everyone else.
	resp := h.compute()
	h.cached, h.cachedAt = &resp, now
	return resp
}

func (h *SystemHealthHandler) uptime(now time.Time) int64 {
	if s := int64(now.Sub(h.started) / time.Second); s > 0 {
		return s
	}
	return 0
}

func (h *SystemHealthHandler) compute() SystemHealthResponse {
	var db, redis, storage string
	var queue HealthQueue
	var wg sync.WaitGroup
	run := func(dst *string, probe func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			*dst = runProbe(probe)
		}()
	}
	run(&db, h.probes.Database)
	if h.probes.Redis != nil {
		run(&redis, h.probes.Redis)
	}
	run(&storage, h.probes.Storage)
	if h.probes.Queue != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := make(chan HealthQueue, 1)
			go func() { ch <- h.probes.Queue() }()
			select {
			case queue = <-ch:
			case <-time.After(healthProbeTimeout):
			}
		}()
	}
	wg.Wait()

	status := HealthHealthy
	switch {
	case db != checkOK:
		status = HealthDown
	case storage != checkOK, h.probes.Redis != nil && redis != checkOK:
		status = HealthDegraded
	}
	return SystemHealthResponse{
		Status:        status,
		Service:       healthServiceName,
		Version:       h.version,
		UptimeSeconds: h.uptime(h.now()),
		Checks:        HealthChecks{Database: db, Redis: redis, Storage: storage},
		Queue:         queue,
	}
}

// runProbe runs one check with the 1 s ceiling. A probe that ignores its
// context is abandoned (its goroutine finishes on its own) rather than waited
// on, so a hung dependency cannot hold the endpoint open.
func runProbe(probe func(context.Context) error) string {
	if probe == nil {
		return checkFail
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- context.Canceled
			}
		}()
		done <- probe(ctx)
	}()
	select {
	case err := <-done:
		if err == nil {
			return checkOK
		}
	case <-ctx.Done():
	}
	return checkFail
}
