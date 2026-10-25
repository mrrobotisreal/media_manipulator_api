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
	analyticsEmitEvents     *prometheus.CounterVec
	analyticsEmitBatches    *prometheus.CounterVec
	retentionRegisterFailed prometheus.Counter
	retentionSwept          *prometheus.CounterVec
	retentionWouldDelete    *prometheus.CounterVec
	retentionSweepErrors    *prometheus.CounterVec
	retentionGaveUp         *prometheus.CounterVec
	retentionBacklog        *prometheus.GaugeVec
	accountDeleted          *prometheus.CounterVec
	dataDeleted             *prometheus.CounterVec
	billingUnknownPrice     prometheus.Counter
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

	// Product-analytics emission (internal/analyticsemit). These are the only
	// visibility into a feed that is deliberately silent on failure: nothing in
	// that package ever returns an error to a caller, so `failed` and `dropped`
	// climbing is the sole signal that server-truth events are not arriving.
	r.analyticsEmitEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_analytics_emit_events_total",
		Help: "Server-truth analytics events by outcome (sent, failed, dropped, skipped_no_identity).",
	}, []string{"result"})
	r.analyticsEmitBatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_analytics_emit_batches_total",
		Help: "Server-truth analytics batches POSTed to the analytics service, by status.",
	}, []string{"status"})

	r.retentionRegisterFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mm_retention_register_failed_total",
		Help: "Retained-object ledger writes that failed (MM22); the request still succeeded.",
	})

	r.retentionSwept = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_retention_swept_total",
		Help: "Ledger rows the retention sweeper deleted, by kind (MM23).",
	}, []string{"kind"})
	r.retentionWouldDelete = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_retention_would_delete_total",
		Help: "Ledger rows the sweeper WOULD have deleted in dry-run mode, by kind (MM23).",
	}, []string{"kind"})
	r.retentionSweepErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_retention_sweep_errors_total",
		Help: "Failed attempts to delete a ledger row, by kind (MM23).",
	}, []string{"kind"})
	r.retentionGaveUp = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_retention_gave_up_total",
		Help: "Ledger rows the sweeper stopped retrying after the max attempts, by kind; they need a human (MM23).",
	}, []string{"kind"})
	r.retentionBacklog = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mm_retention_backlog",
		Help: "Overdue, undeleted ledger rows: state=due (retryable) or state=gave_up (stuck) (MM23).",
	}, []string{"state"})

	r.accountDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_account_deleted_total",
		Help: "DELETE /api/account outcomes: completed, incomplete (a step failed, retryable), files_pending (S3 not clean, nothing else touched) (MM24).",
	}, []string{"result"})
	r.dataDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mm_data_deleted_total",
		Help: "User-initiated deletions by scope: item (a job), project, all (MM24).",
	}, []string{"scope"})

	r.billingUnknownPrice = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mm_billing_unknown_price_total",
		Help: "Webhook entitlement decisions whose Stripe price id is not configured; granted premium, never pro (MM39).",
	})

	reg.MustRegister(
		r.billingUnknownPrice,
		r.accountDeleted, r.dataDeleted,
		r.retentionSwept, r.retentionWouldDelete, r.retentionSweepErrors, r.retentionGaveUp, r.retentionBacklog,
		r.httpDuration, r.httpStatus, r.conversionJobs, r.toolUsage,
		r.commandDuration, r.commandFailures, r.ffmpegValidationRejects,
		r.rateLimitEvents, r.gpuWait, r.gpuRun,
		r.cleanupDeletedFiles, r.cleanupDeletedBytes, r.cleanupErrors,
		r.activeJobs, r.sseSubscribers,
		r.analyticsEmitEvents, r.analyticsEmitBatches,
		r.retentionRegisterFailed,
	)
	return r
}

// AnalyticsEmitEvents records n server-truth analytics events with one outcome.
func (r *Registry) AnalyticsEmitEvents(result string, n int) {
	if r == nil || n <= 0 {
		return
	}
	r.analyticsEmitEvents.WithLabelValues(result).Add(float64(n))
}

// AnalyticsEmitBatch records one POSTed batch.
func (r *Registry) AnalyticsEmitBatch(status string) {
	if r == nil {
		return
	}
	r.analyticsEmitBatches.WithLabelValues(status).Inc()
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

// RetentionRegisterFailed counts a failed retained-objects ledger write.
func (r *Registry) RetentionRegisterFailed() {
	if r == nil {
		return
	}
	r.retentionRegisterFailed.Inc()
}

// RetentionSwept counts ledger rows the sweeper deleted.
func (r *Registry) RetentionSwept(kind string, n int) {
	if r == nil || n <= 0 {
		return
	}
	r.retentionSwept.WithLabelValues(kind).Add(float64(n))
}

// RetentionWouldDelete counts rows a dry-run sweep would have deleted.
func (r *Registry) RetentionWouldDelete(kind string, n int) {
	if r == nil || n <= 0 {
		return
	}
	r.retentionWouldDelete.WithLabelValues(kind).Add(float64(n))
}

// RetentionSweepError counts one failed delete attempt.
func (r *Registry) RetentionSweepError(kind string) {
	if r == nil {
		return
	}
	r.retentionSweepErrors.WithLabelValues(kind).Inc()
}

// RetentionGaveUp counts one row that hit the attempts ceiling.
func (r *Registry) RetentionGaveUp(kind string) {
	if r == nil {
		return
	}
	r.retentionGaveUp.WithLabelValues(kind).Inc()
}

// SetRetentionBacklog sets the retention_backlog gauge.
func (r *Registry) SetRetentionBacklog(due, gaveUp int64) {
	if r == nil {
		return
	}
	r.retentionBacklog.WithLabelValues("due").Set(float64(due))
	r.retentionBacklog.WithLabelValues("gave_up").Set(float64(gaveUp))
}

// AccountDeleted counts a finished DELETE /api/account by result
// (completed, incomplete, files_pending).
func (r *Registry) AccountDeleted(result string) {
	if r == nil {
		return
	}
	r.accountDeleted.WithLabelValues(result).Inc()
}

// DataDeleted counts a user-initiated deletion by scope (item, project, all).
func (r *Registry) DataDeleted(scope string) {
	if r == nil {
		return
	}
	r.dataDeleted.WithLabelValues(scope).Inc()
}

// BillingUnknownPrice counts a subscription whose price id is not in the
// configured price -> tier table (granted premium) (MM39).
func (r *Registry) BillingUnknownPrice() {
	if r == nil {
		return
	}
	r.billingUnknownPrice.Inc()
}
