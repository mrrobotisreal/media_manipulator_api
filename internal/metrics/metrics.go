// Package metrics owns the Prometheus registry exposed at /metrics.
//
// All metrics are registered against a private Registry so tests can build
// fresh instances without panicking on duplicate registrations.
package metrics

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

// Registry bundles every metric the API exposes.
type Registry struct {
	Reg *prometheus.Registry

	httpDuration            *prometheus.HistogramVec
	httpStatus              *prometheus.CounterVec
	conversionJobs          *prometheus.CounterVec
	toolUsage               *prometheus.CounterVec
	commandDuration         *prometheus.HistogramVec
	commandFailures         *prometheus.CounterVec
	ffmpegValidationRejects *prometheus.CounterVec
	rateLimitEvents         *prometheus.CounterVec
	gpuWait                 *prometheus.HistogramVec
	gpuRun                  *prometheus.HistogramVec
	cleanupDeletedFiles     prometheus.Counter
	cleanupDeletedBytes     prometheus.Counter
	cleanupErrors           prometheus.Counter
	activeJobs              prometheus.Gauge
	sseSubscribers          prometheus.Gauge
}

// New creates a fresh Registry with all metrics registered.
func New() *Registry {
	reg := prometheus.NewRegistry()
	r := &Registry{Reg: reg}

	r.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mm_http_request_duration_seconds",
		Help:    "HTTP request duration by route, method, and status class.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method", "status_class"})

	r.httpStatus = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_http_requests_total",
		Help: "HTTP requests served by route, method, and status code.",
	}, []string{"route", "method", "status"})

	r.conversionJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_conversion_jobs_total",
		Help: "Conversion jobs by tool, media_kind, and outcome.",
	}, []string{"tool", "media_kind", "outcome"})

	r.toolUsage = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_tool_usage_total",
		Help: "Tool usage events by tool, media_kind, action, and status.",
	}, []string{"tool", "media_kind", "action", "status"})

	r.commandDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mm_command_duration_seconds",
		Help:    "Subprocess duration by executable, tool, stage, and status.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800, 7200},
	}, []string{"executable", "tool", "stage", "status"})

	r.commandFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_command_failures_total",
		Help: "Subprocess failures by executable, tool, and stage.",
	}, []string{"executable", "tool", "stage"})

	r.ffmpegValidationRejects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_ffmpeg_validation_rejects_total",
		Help: "FFmpeg pre-flight validation rejections by reason.",
	}, []string{"reason"})

	r.rateLimitEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_rate_limit_events_total",
		Help: "Rate limit decisions by scope, route, and outcome.",
	}, []string{"scope", "route", "outcome"})

	r.gpuWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mm_gpu_wait_seconds",
		Help:    "GPU scheduler wait time by task type and device.",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300},
	}, []string{"task", "device"})

	r.gpuRun = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mm_gpu_run_seconds",
		Help:    "GPU job run time by task type and device.",
		Buckets: []float64{1, 5, 10, 30, 60, 300, 600, 1800, 7200},
	}, []string{"task", "device"})

	r.cleanupDeletedFiles = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mm_cleanup_deleted_files_total",
		Help: "Files deleted by the cleanup worker.",
	})
	r.cleanupDeletedBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mm_cleanup_deleted_bytes_total",
		Help: "Bytes deleted by the cleanup worker.",
	})
	r.cleanupErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mm_cleanup_errors_total",
		Help: "Cleanup worker errors.",
	})

	r.activeJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mm_active_jobs",
		Help: "Active conversion jobs in flight.",
	})
	r.sseSubscribers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mm_sse_subscribers",
		Help: "Connected SSE subscribers across all jobs.",
	})

	reg.MustRegister(
		r.httpDuration, r.httpStatus, r.conversionJobs, r.toolUsage,
		r.commandDuration, r.commandFailures, r.ffmpegValidationRejects,
		r.rateLimitEvents, r.gpuWait, r.gpuRun,
		r.cleanupDeletedFiles, r.cleanupDeletedBytes, r.cleanupErrors,
		r.activeJobs, r.sseSubscribers,
	)
	return r
}

// Middleware records http duration + status per request.
func (r *Registry) Middleware() gin.HandlerFunc {
	if r == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "unknown"
		}
		status := c.Writer.Status()
		statusClass := classifyStatus(status)
		r.httpDuration.WithLabelValues(route, c.Request.Method, statusClass).Observe(time.Since(start).Seconds())
		r.httpStatus.WithLabelValues(route, c.Request.Method, strconv.Itoa(status)).Inc()
	}
}

// Helpers --------------------------------------------------------------------

// ConversionJob records a finished conversion outcome.
func (r *Registry) ConversionJob(tool, mediaKind, outcome string) {
	if r == nil {
		return
	}
	r.conversionJobs.WithLabelValues(tool, mediaKind, outcome).Inc()
}

// ToolUsage records a tool usage attempt.
func (r *Registry) ToolUsage(tool, mediaKind, action, status string) {
	if r == nil {
		return
	}
	r.toolUsage.WithLabelValues(tool, mediaKind, action, status).Inc()
}

// CommandDuration records a subprocess run.
func (r *Registry) CommandDuration(executable, tool, stage string, success bool, d time.Duration) {
	if r == nil {
		return
	}
	status := "ok"
	if !success {
		status = "fail"
		r.commandFailures.WithLabelValues(executable, tool, stage).Inc()
	}
	r.commandDuration.WithLabelValues(executable, tool, stage, status).Observe(d.Seconds())
}

// FFmpegValidationReject records a validation rejection.
func (r *Registry) FFmpegValidationReject(reason string) {
	if r == nil {
		return
	}
	r.ffmpegValidationRejects.WithLabelValues(reason).Inc()
}

// RateLimitAllowed records an allowed rate-limit decision.
func (r *Registry) RateLimitAllowed(scope, route string) {
	if r == nil {
		return
	}
	r.rateLimitEvents.WithLabelValues(scope, route, "allowed").Inc()
}

// RateLimitBlocked records a blocked rate-limit decision.
func (r *Registry) RateLimitBlocked(scope, route string) {
	if r == nil {
		return
	}
	r.rateLimitEvents.WithLabelValues(scope, route, "blocked").Inc()
}

// GPUWait records GPU scheduler wait time.
func (r *Registry) GPUWait(task, device string, d time.Duration) {
	if r == nil {
		return
	}
	r.gpuWait.WithLabelValues(task, device).Observe(d.Seconds())
}

// GPURun records GPU job run time.
func (r *Registry) GPURun(task, device string, d time.Duration) {
	if r == nil {
		return
	}
	r.gpuRun.WithLabelValues(task, device).Observe(d.Seconds())
}

// CleanupDeleted records cleanup output.
func (r *Registry) CleanupDeleted(files int64, bytes int64) {
	if r == nil {
		return
	}
	if files > 0 {
		r.cleanupDeletedFiles.Add(float64(files))
	}
	if bytes > 0 {
		r.cleanupDeletedBytes.Add(float64(bytes))
	}
}

// CleanupError increments the cleanup error counter.
func (r *Registry) CleanupError() {
	if r == nil {
		return
	}
	r.cleanupErrors.Inc()
}

// SetActiveJobs sets the active-jobs gauge.
func (r *Registry) SetActiveJobs(n int) {
	if r == nil {
		return
	}
	r.activeJobs.Set(float64(n))
}

// IncSSE / DecSSE adjust the subscriber count.
func (r *Registry) IncSSE() {
	if r == nil {
		return
	}
	r.sseSubscribers.Inc()
}

// DecSSE decrements the SSE subscriber gauge.
func (r *Registry) DecSSE() {
	if r == nil {
		return
	}
	r.sseSubscribers.Dec()
}

func classifyStatus(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}
