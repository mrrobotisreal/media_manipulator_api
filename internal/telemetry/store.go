// Package telemetry persists operational/compliance records to Postgres.
//
// Every helper accepts pointer-style "request" structs so callers can omit
// fields without playing with the database driver. All writes are best-effort
// — telemetry must NEVER break a conversion request, so errors are logged and
// swallowed.
package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// Store wraps a pgxpool to write into mm_* tables. When pool is nil every
// method becomes a no-op (kept this way so handlers don't need branches).
type Store struct {
	Pool         *pgxpool.Pool
	WriteTimeout time.Duration
	Logger       *slog.Logger
}

// NewStore constructs a Store. pool may be nil — the store remains usable.
func NewStore(pool *pgxpool.Pool, cfg *config.Config, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	timeout := cfg.TelemetryWriteTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Store{Pool: pool, WriteTimeout: timeout, Logger: logger}
}

// Enabled reports whether the store has a live DB pool.
func (s *Store) Enabled() bool { return s != nil && s.Pool != nil }

func (s *Store) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, s.WriteTimeout)
}

// logErr logs telemetry write failures without bubbling them up.
func (s *Store) logErr(action string, err error, extra ...any) {
	if err == nil || s == nil || s.Logger == nil {
		return
	}
	attrs := append([]any{"action", action, "error", err.Error()}, extra...)
	s.Logger.Warn("telemetry write failed", attrs...)
}

// --- request log -----------------------------------------------------------

// RequestLog mirrors mm_api_requests.
type RequestLog struct {
	RequestID      string
	VisitorID      string
	SessionID      string
	JobID          string
	Method         string
	Route          string
	Path           string
	QueryHash      string
	StatusCode     int
	DurationMS     int
	RequestBytes   int64
	ResponseBytes  int64
	IP             string
	CFConnectingIP string
	XForwardedFor  string
	CFRay          string
	CFIPCountry    string
	UserAgent      string
	Origin         string
	Referer        string
	Tool           string
	Stage          string
	ErrorMessage   string
	Properties     map[string]any
	CreatedAt      time.Time
}

// InsertAPIRequest writes a row to mm_api_requests. Safe with a nil pool.
func (s *Store) InsertAPIRequest(ctx context.Context, r RequestLog) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	props := jsonBytes(r.Properties)
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_api_requests (
  request_id, visitor_id, session_id, job_id,
  method, route, path, query_hash, status_code, duration_ms,
  request_bytes, response_bytes,
  ip, cf_connecting_ip, x_forwarded_for, cf_ray, cf_ip_country,
  user_agent, origin, referer, tool, stage, error_message,
  created_at, properties
) VALUES (
  $1, $2, $3, $4,
  $5, $6, $7, $8, $9, $10,
  $11, $12,
  $13, $14, $15, $16, $17,
  $18, $19, $20, $21, $22, $23,
  $24, $25
)
ON CONFLICT (request_id) DO NOTHING
`,
		uuidOrNil(r.RequestID), uuidOrNil(r.VisitorID), uuidOrNil(r.SessionID), uuidOrNil(r.JobID),
		nullable(r.Method), nullable(r.Route), nullable(r.Path), nullable(r.QueryHash), r.StatusCode, r.DurationMS,
		r.RequestBytes, r.ResponseBytes,
		inetOrNil(r.IP), inetOrNil(r.CFConnectingIP), nullable(r.XForwardedFor), nullable(r.CFRay), nullable(r.CFIPCountry),
		nullable(r.UserAgent), nullable(r.Origin), nullable(r.Referer), nullable(r.Tool), nullable(r.Stage), nullable(r.ErrorMessage),
		coalesceTime(r.CreatedAt), props,
	)
	s.logErr("insert_api_request", err, "requestId", r.RequestID)
}

// --- visitors / sessions ---------------------------------------------------

// VisitorUpsert holds the fields needed to upsert mm_visitors on a request.
type VisitorUpsert struct {
	VisitorID   string
	UserAgent   string
	IP          string
	CountryCode string
}

// UpsertVisitor inserts or updates the visitor row.
func (s *Store) UpsertVisitor(ctx context.Context, v VisitorUpsert) {
	if !s.Enabled() || v.VisitorID == "" {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_visitors (visitor_id, first_seen_at, last_seen_at, visit_count, session_count,
                         first_user_agent, last_user_agent, first_ip, last_ip,
                         first_geo_country_code, last_geo_country_code)
VALUES ($1, now(), now(), 1, 0, $2, $2, $3, $3, $4, $4)
ON CONFLICT (visitor_id) DO UPDATE SET
  last_seen_at = now(),
  visit_count = mm_visitors.visit_count + 1,
  last_user_agent = COALESCE(NULLIF($2, ''), mm_visitors.last_user_agent),
  last_ip = COALESCE($3, mm_visitors.last_ip),
  last_geo_country_code = COALESCE(NULLIF($4, ''), mm_visitors.last_geo_country_code),
  updated_at = now()
`, v.VisitorID, v.UserAgent, inetOrNil(v.IP), v.CountryCode)
	s.logErr("upsert_visitor", err, "visitorId", v.VisitorID)
}

// SessionUpsert mirrors mm_sessions writes for the session middleware/endpoint.
type SessionUpsert struct {
	SessionID   string
	VisitorID   string
	UserAgent   string
	IP          string
	CountryCode string
	Region      string
	City        string
	Lat         *float64
	Lon         *float64
	Timezone    string
	ASNNumber   uint
	ASNOrg      string
	Properties  map[string]any
}

// UpsertSession inserts/updates mm_sessions.
func (s *Store) UpsertSession(ctx context.Context, u SessionUpsert) {
	if !s.Enabled() || u.SessionID == "" {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	asnNum := int64(0)
	if u.ASNNumber > 0 {
		asnNum = int64(u.ASNNumber)
	}
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_sessions (
  session_id, visitor_id, started_at, last_seen_at,
  user_agent, ip,
  geo_country_code, geo_region, geo_city, geo_lat, geo_lon, geo_timezone,
  asn_number, asn_org, properties
) VALUES (
  $1, $2, now(), now(),
  $3, $4,
  $5, $6, $7, $8, $9, $10,
  $11, $12, $13
)
ON CONFLICT (session_id) DO UPDATE SET
  last_seen_at = now(),
  visitor_id = COALESCE(EXCLUDED.visitor_id, mm_sessions.visitor_id),
  user_agent = COALESCE(NULLIF(EXCLUDED.user_agent, ''), mm_sessions.user_agent),
  ip = COALESCE(EXCLUDED.ip, mm_sessions.ip),
  geo_country_code = COALESCE(NULLIF(EXCLUDED.geo_country_code, ''), mm_sessions.geo_country_code),
  geo_region = COALESCE(NULLIF(EXCLUDED.geo_region, ''), mm_sessions.geo_region),
  geo_city = COALESCE(NULLIF(EXCLUDED.geo_city, ''), mm_sessions.geo_city),
  geo_lat = COALESCE(EXCLUDED.geo_lat, mm_sessions.geo_lat),
  geo_lon = COALESCE(EXCLUDED.geo_lon, mm_sessions.geo_lon),
  geo_timezone = COALESCE(NULLIF(EXCLUDED.geo_timezone, ''), mm_sessions.geo_timezone),
  asn_number = COALESCE(NULLIF(EXCLUDED.asn_number, 0), mm_sessions.asn_number),
  asn_org = COALESCE(NULLIF(EXCLUDED.asn_org, ''), mm_sessions.asn_org),
  updated_at = now()
`,
		u.SessionID, uuidOrNil(u.VisitorID),
		nullable(u.UserAgent), inetOrNil(u.IP),
		nullable(u.CountryCode), nullable(u.Region), nullable(u.City), u.Lat, u.Lon, nullable(u.Timezone),
		asnNum, nullable(u.ASNOrg), jsonBytes(u.Properties),
	)
	s.logErr("upsert_session", err, "sessionId", u.SessionID)
}

// --- conversion jobs ------------------------------------------------------

// JobUpsert mirrors mm_conversion_jobs writes.
type JobUpsert struct {
	JobID          string
	VisitorID      string
	SessionID      string
	Status         string
	Mode           string
	Tool           string
	MediaKind      string
	SourceFormat   string
	TargetFormat   string
	Options        map[string]any
	StartedAt      *time.Time
	CompletedAt    *time.Time
	DurationMS     *int
	ResultS3Key    string
	ResultFileName string
	ResultExpires  *time.Time
	ErrorMessage   string
}

// UpsertJob is idempotent on (job_id). On conflict the row is updated.
func (s *Store) UpsertJob(ctx context.Context, j JobUpsert) {
	if !s.Enabled() || j.JobID == "" {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_conversion_jobs (
  job_id, visitor_id, session_id, status, mode, tool, media_kind,
  source_format, target_format, options,
  started_at, completed_at, duration_ms,
  result_s3_key, result_file_name, result_expires_at,
  error_message
) VALUES (
  $1, $2, $3, $4, $5, $6, $7,
  $8, $9, $10,
  $11, $12, $13,
  $14, $15, $16,
  $17
)
ON CONFLICT (job_id) DO UPDATE SET
  status = EXCLUDED.status,
  mode = COALESCE(NULLIF(EXCLUDED.mode, ''), mm_conversion_jobs.mode),
  tool = COALESCE(NULLIF(EXCLUDED.tool, ''), mm_conversion_jobs.tool),
  media_kind = COALESCE(EXCLUDED.media_kind, mm_conversion_jobs.media_kind),
  source_format = COALESCE(NULLIF(EXCLUDED.source_format, ''), mm_conversion_jobs.source_format),
  target_format = COALESCE(NULLIF(EXCLUDED.target_format, ''), mm_conversion_jobs.target_format),
  options = COALESCE(EXCLUDED.options, mm_conversion_jobs.options),
  started_at = COALESCE(EXCLUDED.started_at, mm_conversion_jobs.started_at),
  completed_at = COALESCE(EXCLUDED.completed_at, mm_conversion_jobs.completed_at),
  duration_ms = COALESCE(EXCLUDED.duration_ms, mm_conversion_jobs.duration_ms),
  result_s3_key = COALESCE(NULLIF(EXCLUDED.result_s3_key, ''), mm_conversion_jobs.result_s3_key),
  result_file_name = COALESCE(NULLIF(EXCLUDED.result_file_name, ''), mm_conversion_jobs.result_file_name),
  result_expires_at = COALESCE(EXCLUDED.result_expires_at, mm_conversion_jobs.result_expires_at),
  error_message = COALESCE(NULLIF(EXCLUDED.error_message, ''), mm_conversion_jobs.error_message),
  updated_at = now()
`,
		j.JobID, uuidOrNil(j.VisitorID), uuidOrNil(j.SessionID), j.Status, nullable(j.Mode), nullable(j.Tool), nullable(j.MediaKind),
		nullable(j.SourceFormat), nullable(j.TargetFormat), jsonBytes(j.Options),
		j.StartedAt, j.CompletedAt, intPtr(j.DurationMS),
		nullable(j.ResultS3Key), nullable(j.ResultFileName), j.ResultExpires,
		nullable(j.ErrorMessage),
	)
	s.logErr("upsert_job", err, "jobId", j.JobID)
}

// JobEvent mirrors mm_job_events.
type JobEvent struct {
	JobID        string
	RequestID    string
	EventName    string
	Stage        string
	Status       string
	Progress     int
	Message      string
	ErrorMessage string
	Properties   map[string]any
}

// InsertJobEvent appends an event to mm_job_events.
func (s *Store) InsertJobEvent(ctx context.Context, e JobEvent) {
	if !s.Enabled() || e.JobID == "" {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	progressPtr := (*int)(nil)
	if e.Progress >= 0 && e.Progress <= 100 {
		p := e.Progress
		progressPtr = &p
	}
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_job_events (job_id, request_id, event_name, stage, status, progress, message, error_message, properties)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`,
		e.JobID, uuidOrNil(e.RequestID), e.EventName, nullable(e.Stage), nullable(e.Status), progressPtr,
		nullable(e.Message), nullable(e.ErrorMessage), jsonBytes(e.Properties),
	)
	s.logErr("insert_job_event", err, "jobId", e.JobID, "event", e.EventName)
}

// --- tool / feature / errors / scans ---------------------------------------

// ToolUsage is a row for mm_tool_usage_events.
type ToolUsage struct {
	VisitorID    string
	SessionID    string
	RequestID    string
	JobID        string
	Tool         string
	MediaKind    string
	Action       string
	SourceFormat string
	TargetFormat string
	Options      map[string]any
	Success      *bool
	DurationMS   int
	InputBytes   int64
	OutputBytes  int64
	Properties   map[string]any
}

// InsertToolUsage writes mm_tool_usage_events.
func (s *Store) InsertToolUsage(ctx context.Context, u ToolUsage) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_tool_usage_events (
  visitor_id, session_id, request_id, job_id, tool, media_kind, action,
  source_format, target_format, options, success, duration_ms,
  input_size_bytes, output_size_bytes, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
`,
		uuidOrNil(u.VisitorID), uuidOrNil(u.SessionID), uuidOrNil(u.RequestID), uuidOrNil(u.JobID),
		u.Tool, nullable(u.MediaKind), u.Action,
		nullable(u.SourceFormat), nullable(u.TargetFormat), jsonBytes(u.Options), u.Success, u.DurationMS,
		u.InputBytes, u.OutputBytes, jsonBytes(u.Properties),
	)
	s.logErr("insert_tool_usage", err, "tool", u.Tool, "action", u.Action)
}

// ToolError mirrors mm_tool_errors.
type ToolError struct {
	VisitorID            string
	SessionID            string
	RequestID            string
	JobID                string
	Source               string
	Tool                 string
	Stage                string
	ErrorType            string
	ErrorMessage         string
	RedactedErrorMessage string
	StackTail            string
	CommandAuditID       string
	MediaKind            string
	Severity             string
	Properties           map[string]any
}

// InsertToolError writes a row to mm_tool_errors.
func (s *Store) InsertToolError(ctx context.Context, e ToolError) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	if e.Severity == "" {
		e.Severity = "error"
	}
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_tool_errors (
  visitor_id, session_id, request_id, job_id,
  source, tool, stage, error_type, error_message, redacted_error_message,
  stack_or_trace_tail, command_audit_id, media_kind, severity, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
`,
		uuidOrNil(e.VisitorID), uuidOrNil(e.SessionID), uuidOrNil(e.RequestID), uuidOrNil(e.JobID),
		e.Source, nullable(e.Tool), nullable(e.Stage), nullable(e.ErrorType),
		nullable(e.ErrorMessage), nullable(e.RedactedErrorMessage),
		nullable(e.StackTail), uuidOrNil(e.CommandAuditID), nullable(e.MediaKind), e.Severity,
		jsonBytes(e.Properties),
	)
	s.logErr("insert_tool_error", err, "tool", e.Tool, "stage", e.Stage)
}

// FeatureUsage mirrors mm_feature_usage_events.
type FeatureUsage struct {
	VisitorID       string
	SessionID       string
	RequestID       string
	JobID           string
	FeatureName     string
	FeatureCategory string
	Action          string
	Value           string
	MediaKind       string
	Success         *bool
	Properties      map[string]any
}

// InsertFeatureUsage writes mm_feature_usage_events.
func (s *Store) InsertFeatureUsage(ctx context.Context, f FeatureUsage) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_feature_usage_events (
  visitor_id, session_id, request_id, job_id,
  feature_name, feature_category, action, value, media_kind, success, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
`,
		uuidOrNil(f.VisitorID), uuidOrNil(f.SessionID), uuidOrNil(f.RequestID), uuidOrNil(f.JobID),
		f.FeatureName, nullable(f.FeatureCategory), f.Action, nullable(f.Value), nullable(f.MediaKind), f.Success, jsonBytes(f.Properties),
	)
	s.logErr("insert_feature_usage", err)
}

// DownloadResult mirrors mm_download_results.
type DownloadResult struct {
	VisitorID         string
	SessionID         string
	RequestID         string
	JobID             string
	Tool              string
	MediaKind         string
	FileName          string
	SafeFileExtension string
	OutputFormat      string
	SizeBytes         int64
	ContentType       string
	ResultS3Key       string
	ResultURLExpires  *time.Time
	SHA256            string
	DownloadedAt      *time.Time
	Success           bool
	FailureReason     string
	Properties        map[string]any
}

// InsertDownloadResult writes mm_download_results.
func (s *Store) InsertDownloadResult(ctx context.Context, d DownloadResult) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_download_results (
  visitor_id, session_id, request_id, job_id,
  tool, media_kind, file_name, safe_file_extension, output_format,
  size_bytes, content_type, result_s3_key, result_url_expires_at,
  sha256, downloaded_at, success, failure_reason, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
`,
		uuidOrNil(d.VisitorID), uuidOrNil(d.SessionID), uuidOrNil(d.RequestID), uuidOrNil(d.JobID),
		nullable(d.Tool), nullable(d.MediaKind), nullable(d.FileName), nullable(d.SafeFileExtension), nullable(d.OutputFormat),
		d.SizeBytes, nullable(d.ContentType), nullable(d.ResultS3Key), d.ResultURLExpires,
		nullable(d.SHA256), d.DownloadedAt, d.Success, nullable(d.FailureReason), jsonBytes(d.Properties),
	)
	s.logErr("insert_download_result", err)
}

// PageView mirrors mm_page_views.
type PageView struct {
	VisitorID            string
	SessionID            string
	VisitID              string
	PageType             string
	PageSlug             string
	PageTitle            string
	Pathname             string
	CurrentURL           string
	Referrer             string
	EnteredAt            *time.Time
	ExitedAt             *time.Time
	TotalVisibleMS       int64
	TotalActiveMS        int64
	MaxScrollPercent     float64
	CompletedRead        *bool
	QuickScrollToBottom  *bool
	LikelyRealRead       *bool
	WordCount            int
	EstimatedReadSeconds int
	Properties           map[string]any
}

// InsertPageView writes mm_page_views.
func (s *Store) InsertPageView(ctx context.Context, p PageView) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_page_views (
  visitor_id, session_id, visit_id, page_type, page_slug, page_title,
  pathname, current_url, referrer,
  entered_at, exited_at, total_visible_ms, total_active_ms,
  max_scroll_percent, completed_read, quick_scroll_to_bottom, likely_real_read,
  word_count, estimated_read_seconds, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
`,
		uuidOrNil(p.VisitorID), uuidOrNil(p.SessionID), uuidOrNil(p.VisitID), nullable(p.PageType), nullable(p.PageSlug), nullable(p.PageTitle),
		nullable(p.Pathname), nullable(p.CurrentURL), nullable(p.Referrer),
		p.EnteredAt, p.ExitedAt, p.TotalVisibleMS, p.TotalActiveMS,
		p.MaxScrollPercent, p.CompletedRead, p.QuickScrollToBottom, p.LikelyRealRead,
		p.WordCount, p.EstimatedReadSeconds, jsonBytes(p.Properties),
	)
	s.logErr("insert_page_view", err, "pageType", p.PageType)
}

// ContentReadEvent mirrors mm_content_read_events.
type ContentReadEvent struct {
	PageViewID         string
	VisitorID          string
	SessionID          string
	PageType           string
	PageSlug           string
	EventName          string
	ScrollPercent      float64
	ActiveMSSinceLast  int64
	VisibleMSSinceLast int64
	TotalActiveMS      int64
	TotalVisibleMS     int64
	ViewportHeight     int
	DocumentHeight     int
	WordsVisible       int
	QuickScroll        *bool
	Properties         map[string]any
}

// InsertContentReadEvent writes mm_content_read_events.
func (s *Store) InsertContentReadEvent(ctx context.Context, e ContentReadEvent) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_content_read_events (
  page_view_id, visitor_id, session_id, page_type, page_slug, event_name,
  scroll_percent, active_ms_since_last, visible_ms_since_last,
  total_active_ms, total_visible_ms, viewport_height, document_height,
  words_visible_estimate, quick_scroll_flag, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
`,
		uuidOrNil(e.PageViewID), uuidOrNil(e.VisitorID), uuidOrNil(e.SessionID),
		nullable(e.PageType), nullable(e.PageSlug), e.EventName,
		e.ScrollPercent, e.ActiveMSSinceLast, e.VisibleMSSinceLast,
		e.TotalActiveMS, e.TotalVisibleMS, e.ViewportHeight, e.DocumentHeight,
		e.WordsVisible, e.QuickScroll, jsonBytes(e.Properties),
	)
	s.logErr("insert_content_read_event", err, "event", e.EventName)
}

// ToolView mirrors mm_tool_views.
type ToolView struct {
	VisitorID        string
	SessionID        string
	VisitID          string
	Tool             string
	MediaKind        string
	Pathname         string
	CurrentURL       string
	Referrer         string
	EnteredAt        *time.Time
	ExitedAt         *time.Time
	TotalVisibleMS   int64
	TotalActiveMS    int64
	MaxScrollPercent float64
	Properties       map[string]any
}

// InsertToolView writes mm_tool_views.
func (s *Store) InsertToolView(ctx context.Context, v ToolView) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_tool_views (
  visitor_id, session_id, visit_id, tool, media_kind,
  pathname, current_url, referrer, entered_at, exited_at,
  total_visible_ms, total_active_ms, max_scroll_percent, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
`,
		uuidOrNil(v.VisitorID), uuidOrNil(v.SessionID), uuidOrNil(v.VisitID), v.Tool, nullable(v.MediaKind),
		nullable(v.Pathname), nullable(v.CurrentURL), nullable(v.Referrer), v.EnteredAt, v.ExitedAt,
		v.TotalVisibleMS, v.TotalActiveMS, v.MaxScrollPercent, jsonBytes(v.Properties),
	)
	s.logErr("insert_tool_view", err, "tool", v.Tool)
}

// HistoryEvent mirrors mm_conversion_history_events.
type HistoryEvent struct {
	VisitorID       string
	SessionID       string
	JobID           string
	EventName       string
	Tool            string
	MediaKind       string
	SourceFormat    string
	TargetFormat    string
	ResultAvailable *bool
	ResultExpired   *bool
	AgeSeconds      int
	Properties      map[string]any
}

// InsertHistoryEvent writes mm_conversion_history_events.
func (s *Store) InsertHistoryEvent(ctx context.Context, e HistoryEvent) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_conversion_history_events (
  visitor_id, session_id, job_id, event_name,
  tool, media_kind, source_format, target_format,
  result_available, result_expired, age_seconds, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
`,
		uuidOrNil(e.VisitorID), uuidOrNil(e.SessionID), uuidOrNil(e.JobID), e.EventName,
		nullable(e.Tool), nullable(e.MediaKind), nullable(e.SourceFormat), nullable(e.TargetFormat),
		e.ResultAvailable, e.ResultExpired, e.AgeSeconds, jsonBytes(e.Properties),
	)
	s.logErr("insert_history_event", err, "event", e.EventName)
}

// --- media / scans / safety ------------------------------------------------

// MediaAsset mirrors mm_media_assets.
type MediaAsset struct {
	MediaAssetID             string
	VisitorID                string
	SessionID                string
	JobID                    string
	OriginalFilenameRedacted string
	OriginalExtension        string
	MediaKind                string
	MIMEType                 string
	SizeBytes                int64
	Width                    int
	Height                   int
	DurationSeconds          float64
	FPS                      float64
	VideoCodec               string
	AudioCodec               string
	HasAudio                 *bool
	SHA256                   string
	PerceptualHash           string
	Metadata                 map[string]any
	ExifSummary              map[string]any
	GPSPresent               *bool
	GPSStripped              *bool
}

// InsertMediaAsset writes a row to mm_media_assets.
func (s *Store) InsertMediaAsset(ctx context.Context, a MediaAsset) string {
	if !s.Enabled() {
		return a.MediaAssetID
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var id string
	row := s.Pool.QueryRow(ctx, `
INSERT INTO mm_media_assets (
  visitor_id, session_id, job_id,
  original_filename_redacted, original_extension, media_kind, mime_type,
  size_bytes, width, height, duration_seconds, fps,
  video_codec, audio_codec, has_audio, sha256, perceptual_hash,
  metadata, exif_summary, gps_present, gps_stripped
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
RETURNING media_asset_id
`,
		uuidOrNil(a.VisitorID), uuidOrNil(a.SessionID), uuidOrNil(a.JobID),
		nullable(a.OriginalFilenameRedacted), nullable(a.OriginalExtension), a.MediaKind, nullable(a.MIMEType),
		a.SizeBytes, a.Width, a.Height, a.DurationSeconds, a.FPS,
		nullable(a.VideoCodec), nullable(a.AudioCodec), a.HasAudio, nullable(a.SHA256), nullable(a.PerceptualHash),
		jsonBytes(a.Metadata), jsonBytes(a.ExifSummary), a.GPSPresent, a.GPSStripped,
	)
	if err := row.Scan(&id); err != nil {
		s.logErr("insert_media_asset", err)
		return a.MediaAssetID
	}
	return id
}

// ToolScan mirrors mm_tool_scans.
type ToolScan struct {
	VisitorID             string
	SessionID             string
	RequestID             string
	JobID                 string
	MediaAssetID          string
	Tool                  string
	ScannerName           string
	ScannerVersion        string
	ModelName             string
	ModelVersion          string
	ScanType              string
	Summary               string
	Description           string
	DetectedLanguage      string
	Labels                []any
	SafetyRating          string
	SafetyScore           *float64
	HarmfulContent        bool
	HarmfulContentReasons []any
	TOSViolation          bool
	TOSCategories         []any
	Warnings              []any
	RawResult             map[string]any
	StartedAt             *time.Time
	CompletedAt           *time.Time
	DurationMS            int
}

// InsertToolScan writes mm_tool_scans and returns scan_id.
func (s *Store) InsertToolScan(ctx context.Context, sc ToolScan) string {
	if !s.Enabled() {
		return ""
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var id string
	row := s.Pool.QueryRow(ctx, `
INSERT INTO mm_tool_scans (
  visitor_id, session_id, request_id, job_id, media_asset_id,
  tool, scanner_name, scanner_version, model_name, model_version, scan_type,
  summary, description, detected_language,
  labels, safety_rating, safety_score, harmful_content, harmful_content_reasons,
  tos_violation, tos_categories, warnings, raw_result,
  started_at, completed_at, duration_ms
) VALUES (
  $1, $2, $3, $4, $5,
  $6, $7, $8, $9, $10, $11,
  $12, $13, $14,
  $15, $16, $17, $18, $19,
  $20, $21, $22, $23,
  $24, $25, $26
)
RETURNING scan_id
`,
		uuidOrNil(sc.VisitorID), uuidOrNil(sc.SessionID), uuidOrNil(sc.RequestID), uuidOrNil(sc.JobID), uuidOrNil(sc.MediaAssetID),
		nullable(sc.Tool), nullable(sc.ScannerName), nullable(sc.ScannerVersion), nullable(sc.ModelName), nullable(sc.ModelVersion), nullable(sc.ScanType),
		nullable(sc.Summary), nullable(sc.Description), nullable(sc.DetectedLanguage),
		jsonArray(sc.Labels), nullable(sc.SafetyRating), sc.SafetyScore, sc.HarmfulContent, jsonArray(sc.HarmfulContentReasons),
		sc.TOSViolation, jsonArray(sc.TOSCategories), jsonArray(sc.Warnings), jsonBytes(sc.RawResult),
		sc.StartedAt, sc.CompletedAt, sc.DurationMS,
	)
	if err := row.Scan(&id); err != nil {
		s.logErr("insert_tool_scan", err)
		return ""
	}
	return id
}

// SafetyIncident mirrors mm_safety_incidents.
type SafetyIncident struct {
	VisitorID             string
	SessionID             string
	RequestID             string
	JobID                 string
	MediaAssetID          string
	ScanID                string
	IncidentStatus        string
	Severity              string
	Tool                  string
	MediaKind             string
	SafetyRating          string
	TOSViolation          bool
	TOSCategories         []any
	SafetyLabels          []any
	HarmfulContentReasons []any
	Summary               string
	EvidenceReference     map[string]any
	FileSHA256            string
	InputSizeBytes        int64
	OriginalExtension     string
	MIMEType              string
	IP                    string
	CFConnectingIP        string
	XForwardedFor         string
	CFRay                 string
	UserAgent             string
	Origin                string
	Referer               string
	GeoCountryCode        string
	GeoRegion             string
	GeoCity               string
	GeoLat                *float64
	GeoLon                *float64
	GeoTimezone           string
	ASNNumber             uint
	ASNOrg                string
	RetentionUntil        *time.Time
	LegalHold             bool
	Properties            map[string]any
}

// InsertSafetyIncident writes mm_safety_incidents.
func (s *Store) InsertSafetyIncident(ctx context.Context, in SafetyIncident) string {
	if !s.Enabled() {
		return ""
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	if in.IncidentStatus == "" {
		in.IncidentStatus = "open"
	}
	if in.Severity == "" {
		in.Severity = "low"
	}
	asnNum := int64(0)
	if in.ASNNumber > 0 {
		asnNum = int64(in.ASNNumber)
	}
	var id string
	row := s.Pool.QueryRow(ctx, `
INSERT INTO mm_safety_incidents (
  visitor_id, session_id, request_id, job_id, media_asset_id, scan_id,
  incident_status, severity, tool, media_kind, safety_rating,
  tos_violation, tos_categories, safety_labels, harmful_content_reasons,
  summary, evidence_reference, file_sha256, input_size_bytes, original_extension, mime_type,
  ip, cf_connecting_ip, x_forwarded_for, cf_ray, user_agent, origin, referer,
  geo_country_code, geo_region, geo_city, geo_lat, geo_lon, geo_timezone, asn_number, asn_org,
  retention_until, legal_hold, properties
) VALUES (
  $1, $2, $3, $4, $5, $6,
  $7, $8, $9, $10, $11,
  $12, $13, $14, $15,
  $16, $17, $18, $19, $20, $21,
  $22, $23, $24, $25, $26, $27, $28,
  $29, $30, $31, $32, $33, $34, $35, $36,
  $37, $38, $39
)
RETURNING safety_incident_id
`,
		uuidOrNil(in.VisitorID), uuidOrNil(in.SessionID), uuidOrNil(in.RequestID), uuidOrNil(in.JobID),
		uuidOrNil(in.MediaAssetID), uuidOrNil(in.ScanID),
		in.IncidentStatus, in.Severity, nullable(in.Tool), nullable(in.MediaKind), nullable(in.SafetyRating),
		in.TOSViolation, jsonArray(in.TOSCategories), jsonArray(in.SafetyLabels), jsonArray(in.HarmfulContentReasons),
		nullable(in.Summary), jsonBytes(in.EvidenceReference), nullable(in.FileSHA256), in.InputSizeBytes, nullable(in.OriginalExtension), nullable(in.MIMEType),
		inetOrNil(in.IP), inetOrNil(in.CFConnectingIP), nullable(in.XForwardedFor), nullable(in.CFRay), nullable(in.UserAgent), nullable(in.Origin), nullable(in.Referer),
		nullable(in.GeoCountryCode), nullable(in.GeoRegion), nullable(in.GeoCity), in.GeoLat, in.GeoLon, nullable(in.GeoTimezone), asnNum, nullable(in.ASNOrg),
		in.RetentionUntil, in.LegalHold, jsonBytes(in.Properties),
	)
	if err := row.Scan(&id); err != nil {
		s.logErr("insert_safety_incident", err)
		return ""
	}
	return id
}

// --- command audit / rate limit / cleanup ---------------------------------

// InsertCommandAudit writes a row to mm_command_audit_logs. It implements
// cmdaudit.AuditSink so the runner can call it directly.
func (s *Store) InsertCommandAudit(ctx context.Context, rec cmdaudit.Record) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	args, _ := json.Marshal(rec.ArgsRedacted)
	env, _ := json.Marshal(rec.EnvRedacted)
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_command_audit_logs (
  command_audit_id, request_id, job_id, tool, stage,
  executable, args_redacted, env_redacted, working_dir_redacted,
  started_at, completed_at, duration_ms,
  exit_code, timed_out, success, stdout_tail, stderr_tail, error_message
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
ON CONFLICT (command_audit_id) DO NOTHING
`,
		rec.AuditID, uuidOrNil(rec.RequestID), uuidOrNil(rec.JobID), nullable(rec.Tool), nullable(rec.Stage),
		rec.Executable, args, env, nullable(rec.WorkingDirRed),
		rec.StartedAt, rec.CompletedAt, rec.DurationMS,
		rec.ExitCode, rec.TimedOut, rec.Success, nullable(rec.StdoutTail), nullable(rec.StderrTail), nullable(rec.ErrorMessage),
	)
	s.logErr("insert_command_audit", err, "auditId", rec.AuditID, "tool", rec.Tool)
}

// Insert implements cmdaudit.AuditSink.
func (s *Store) Insert(ctx context.Context, rec cmdaudit.Record) {
	s.InsertCommandAudit(ctx, rec)
}

// RateLimitEvent mirrors mm_rate_limit_events.
type RateLimitEvent struct {
	VisitorID         string
	SessionID         string
	RequestID         string
	LimiterKeyHash    string
	LimiterScope      string
	Route             string
	Tool              string
	Allowed           bool
	LimitCount        int
	Remaining         int
	RetryAfterSeconds int
	IP                string
	Properties        map[string]any
}

// InsertRateLimitEvent writes mm_rate_limit_events.
func (s *Store) InsertRateLimitEvent(ctx context.Context, e RateLimitEvent) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_rate_limit_events (
  visitor_id, session_id, request_id, limiter_key_hash, limiter_scope,
  route, tool, allowed, limit_count, remaining, retry_after_seconds, ip, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
`,
		uuidOrNil(e.VisitorID), uuidOrNil(e.SessionID), uuidOrNil(e.RequestID), nullable(e.LimiterKeyHash), nullable(e.LimiterScope),
		nullable(e.Route), nullable(e.Tool), e.Allowed, e.LimitCount, e.Remaining, e.RetryAfterSeconds, inetOrNil(e.IP), jsonBytes(e.Properties),
	)
	s.logErr("insert_rate_limit_event", err)
}

// CleanupRun mirrors mm_cleanup_runs.
type CleanupRun struct {
	StartedAt        time.Time
	CompletedAt      *time.Time
	Status           string
	UploadDir        string
	OutputDir        string
	TempDir          string
	RetentionSeconds int
	DeletedFiles     int64
	DeletedDirs      int64
	DeletedBytes     int64
	ErrorMessage     string
	Properties       map[string]any
}

// InsertCleanupRun writes mm_cleanup_runs and returns its id.
func (s *Store) InsertCleanupRun(ctx context.Context, r CleanupRun) string {
	if !s.Enabled() {
		return ""
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	if r.Status == "" {
		r.Status = "ok"
	}
	var id string
	row := s.Pool.QueryRow(ctx, `
INSERT INTO mm_cleanup_runs (
  started_at, completed_at, status, upload_dir, output_dir, temp_dir,
  retention_seconds, deleted_files, deleted_dirs, deleted_bytes, error_message, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING cleanup_run_id
`,
		r.StartedAt, r.CompletedAt, r.Status, r.UploadDir, r.OutputDir, r.TempDir,
		r.RetentionSeconds, r.DeletedFiles, r.DeletedDirs, r.DeletedBytes, nullable(r.ErrorMessage), jsonBytes(r.Properties),
	)
	if err := row.Scan(&id); err != nil {
		s.logErr("insert_cleanup_run", err)
		return ""
	}
	return id
}

// CleanupPath mirrors mm_cleanup_deleted_paths.
type CleanupPath struct {
	CleanupRunID string
	PathRedacted string
	PathType     string
	AgeSeconds   int
	SizeBytes    int64
	DeletedAt    time.Time
	ErrorMessage string
}

// InsertCleanupPaths writes a batch of mm_cleanup_deleted_paths rows.
func (s *Store) InsertCleanupPaths(ctx context.Context, items []CleanupPath) {
	if !s.Enabled() || len(items) == 0 {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	batch := &pgx.Batch{}
	for _, it := range items {
		batch.Queue(`
INSERT INTO mm_cleanup_deleted_paths (cleanup_run_id, path_redacted, path_type, age_seconds, size_bytes, deleted_at, error_message)
VALUES ($1, $2, $3, $4, $5, $6, $7)
`, it.CleanupRunID, nullable(it.PathRedacted), nullable(it.PathType), it.AgeSeconds, it.SizeBytes, it.DeletedAt, nullable(it.ErrorMessage))
	}
	br := s.Pool.SendBatch(ctx, batch)
	defer br.Close()
	for range items {
		if _, err := br.Exec(); err != nil {
			s.logErr("insert_cleanup_path", err)
			return
		}
	}
}

// GPUDeviceUpsert mirrors mm_gpu_devices.
type GPUDeviceUpsert struct {
	SchedulerKey  string
	Backend       string
	DeviceIndex   int
	PCIBusID      string
	Name          string
	TotalMemoryMB int64
	FreeMemoryMB  int64
	Capabilities  map[string]any
}

// UpsertGPUDevice writes mm_gpu_devices.
func (s *Store) UpsertGPUDevice(ctx context.Context, d GPUDeviceUpsert) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
INSERT INTO mm_gpu_devices (
  scheduler_device_key, backend, device_index, pci_bus_id, name,
  total_memory_mb, free_memory_mb, capabilities, last_seen_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (scheduler_device_key) DO UPDATE SET
  backend = EXCLUDED.backend,
  device_index = EXCLUDED.device_index,
  pci_bus_id = COALESCE(NULLIF(EXCLUDED.pci_bus_id, ''), mm_gpu_devices.pci_bus_id),
  name = COALESCE(NULLIF(EXCLUDED.name, ''), mm_gpu_devices.name),
  total_memory_mb = EXCLUDED.total_memory_mb,
  free_memory_mb = EXCLUDED.free_memory_mb,
  capabilities = EXCLUDED.capabilities,
  last_seen_at = now(),
  updated_at = now()
`,
		d.SchedulerKey, nullable(d.Backend), d.DeviceIndex, nullable(d.PCIBusID), nullable(d.Name),
		d.TotalMemoryMB, d.FreeMemoryMB, jsonBytes(d.Capabilities),
	)
	s.logErr("upsert_gpu_device", err, "key", d.SchedulerKey)
}

// GPUJobInsert mirrors mm_gpu_jobs writes.
type GPUJobInsert struct {
	JobID        string
	RequestID    string
	Tool         string
	TaskType     string
	SchedulerKey string
	AcquiredAt   *time.Time
	ReleasedAt   *time.Time
	WaitMS       int
	RunMS        int
	Status       string
	ErrorMessage string
	Properties   map[string]any
}

// InsertGPUJob writes mm_gpu_jobs.
func (s *Store) InsertGPUJob(ctx context.Context, g GPUJobInsert) string {
	if !s.Enabled() {
		return ""
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	if g.Status == "" {
		g.Status = "completed"
	}
	var id string
	row := s.Pool.QueryRow(ctx, `
INSERT INTO mm_gpu_jobs (
  job_id, request_id, tool, task_type, scheduler_device_key,
  acquired_at, released_at, wait_ms, run_ms, status, error_message, properties
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING gpu_job_id
`,
		uuidOrNil(g.JobID), uuidOrNil(g.RequestID), nullable(g.Tool), nullable(g.TaskType), nullable(g.SchedulerKey),
		g.AcquiredAt, g.ReleasedAt, g.WaitMS, g.RunMS, g.Status, nullable(g.ErrorMessage), jsonBytes(g.Properties),
	)
	if err := row.Scan(&id); err != nil {
		s.logErr("insert_gpu_job", err)
		return ""
	}
	return id
}

// --- helpers ----------------------------------------------------------------

// nullable returns nil for "" so columns become NULL rather than empty strings.
func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// uuidOrNil returns nil when the input is empty so optional uuid columns stay NULL.
func uuidOrNil(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// inetOrNil parses an IP. Invalid/empty input becomes NULL.
func inetOrNil(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if ip := net.ParseIP(s); ip != nil {
		return s
	}
	return nil
}

func coalesceTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

func intPtr(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func jsonBytes(v any) []byte {
	if v == nil {
		return []byte("{}")
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return []byte("{}")
	}
	body, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return body
}

func jsonArray(v []any) []byte {
	if len(v) == 0 {
		return []byte("[]")
	}
	body, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return body
}
