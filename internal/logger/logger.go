// Package logger centralises slog setup for the API.
//
// We use stdlib slog (JSON by default, text in dev) so every package can
// produce consistent structured logs without taking on a new dependency.
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// CtxKey is the type used for context keys exported by this package.
type CtxKey string

const (
	CtxRequestID CtxKey = "mm.request_id"
	CtxJobID     CtxKey = "mm.job_id"
	CtxVisitorID CtxKey = "mm.visitor_id"
	CtxSessionID CtxKey = "mm.session_id"
	CtxTool      CtxKey = "mm.tool"
	CtxStage     CtxKey = "mm.stage"

	// GinKey is the gin context key under which we attach a *Fields struct
	// describing the current request — mirrors CtxKey-set fields for
	// handlers that work via gin contexts directly.
	GinKey = "mm.log_fields"
)

// Fields is the per-request bundle our middleware attaches to gin contexts.
type Fields struct {
	RequestID string
	VisitorID string
	SessionID string
	JobID     string
	Tool      string
	Stage     string
	Route     string
	MediaKind string
}

// New returns the process-wide slog.Logger configured per cfg.LogLevel /
// cfg.LogFormat. Callers should call slog.SetDefault on the returned logger
// at startup.
func New(cfg *config.Config) *slog.Logger {
	level := parseLevel(cfg.LogLevel)
	opts := &slog.HandlerOptions{Level: level, AddSource: false}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.LogFormat)) {
	case "text", "console", "dev":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// FromContext returns a logger enriched with any of our standard fields that
// are present on ctx. Always returns a non-nil logger (falls back to the
// default).
func FromContext(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if ctx == nil {
		return l
	}
	attrs := contextAttrs(ctx)
	if len(attrs) == 0 {
		return l
	}
	return l.With(attrs...)
}

func contextAttrs(ctx context.Context) []any {
	var attrs []any
	if v := getString(ctx, CtxRequestID); v != "" {
		attrs = append(attrs, "requestId", v)
	}
	if v := getString(ctx, CtxJobID); v != "" {
		attrs = append(attrs, "jobId", v)
	}
	if v := getString(ctx, CtxVisitorID); v != "" {
		attrs = append(attrs, "visitorId", v)
	}
	if v := getString(ctx, CtxSessionID); v != "" {
		attrs = append(attrs, "sessionId", v)
	}
	if v := getString(ctx, CtxTool); v != "" {
		attrs = append(attrs, "tool", v)
	}
	if v := getString(ctx, CtxStage); v != "" {
		attrs = append(attrs, "stage", v)
	}
	return attrs
}

func getString(ctx context.Context, key CtxKey) string {
	if v, _ := ctx.Value(key).(string); v != "" {
		return v
	}
	return ""
}

// WithRequest returns a logger enriched with fields from the gin context
// (request_id, session_id, visitor_id, tool, stage, route).
func WithRequest(c *gin.Context) *slog.Logger {
	if c == nil {
		return slog.Default()
	}
	if fields, ok := c.Get(GinKey); ok {
		if f, ok := fields.(*Fields); ok && f != nil {
			return appendFields(slog.Default(), f)
		}
	}
	return slog.Default()
}

// WithFields enriches a logger with a Fields struct.
func WithFields(l *slog.Logger, f *Fields) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	if f == nil {
		return l
	}
	return appendFields(l, f)
}

func appendFields(l *slog.Logger, f *Fields) *slog.Logger {
	attrs := make([]any, 0, 16)
	if f.RequestID != "" {
		attrs = append(attrs, "requestId", f.RequestID)
	}
	if f.JobID != "" {
		attrs = append(attrs, "jobId", f.JobID)
	}
	if f.VisitorID != "" {
		attrs = append(attrs, "visitorId", f.VisitorID)
	}
	if f.SessionID != "" {
		attrs = append(attrs, "sessionId", f.SessionID)
	}
	if f.Tool != "" {
		attrs = append(attrs, "tool", f.Tool)
	}
	if f.Stage != "" {
		attrs = append(attrs, "stage", f.Stage)
	}
	if f.Route != "" {
		attrs = append(attrs, "route", f.Route)
	}
	if f.MediaKind != "" {
		attrs = append(attrs, "mediaKind", f.MediaKind)
	}
	if len(attrs) == 0 {
		return l
	}
	return l.With(attrs...)
}

// ContextFromGin returns a context.Context that carries the request fields
// from the gin context. Used by services that don't take a *gin.Context.
func ContextFromGin(c *gin.Context) context.Context {
	if c == nil {
		return context.Background()
	}
	ctx := c.Request.Context()
	if v, ok := c.Get(GinKey); ok {
		if f, ok := v.(*Fields); ok && f != nil {
			if f.RequestID != "" {
				ctx = context.WithValue(ctx, CtxRequestID, f.RequestID)
			}
			if f.JobID != "" {
				ctx = context.WithValue(ctx, CtxJobID, f.JobID)
			}
			if f.VisitorID != "" {
				ctx = context.WithValue(ctx, CtxVisitorID, f.VisitorID)
			}
			if f.SessionID != "" {
				ctx = context.WithValue(ctx, CtxSessionID, f.SessionID)
			}
			if f.Tool != "" {
				ctx = context.WithValue(ctx, CtxTool, f.Tool)
			}
			if f.Stage != "" {
				ctx = context.WithValue(ctx, CtxStage, f.Stage)
			}
		}
	}
	return ctx
}

// WithJob returns a child context carrying the job ID.
func WithJob(ctx context.Context, jobID string) context.Context {
	if jobID == "" {
		return ctx
	}
	return context.WithValue(ctx, CtxJobID, jobID)
}

// WithTool returns a child context carrying tool/stage labels.
func WithTool(ctx context.Context, tool, stage string) context.Context {
	if tool != "" {
		ctx = context.WithValue(ctx, CtxTool, tool)
	}
	if stage != "" {
		ctx = context.WithValue(ctx, CtxStage, stage)
	}
	return ctx
}
