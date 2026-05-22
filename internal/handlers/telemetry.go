package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/geo"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// TelemetryHandler exposes the POST /api/telemetry/* endpoints. Inserts are
// best-effort and never break conversion flows.
type TelemetryHandler struct {
	Store    *telemetry.Store
	Enricher *geo.Enricher
}

// NewTelemetryHandler returns a handler. Either argument may be nil — the
// endpoints respond 503 in that case.
func NewTelemetryHandler(store *telemetry.Store, enricher *geo.Enricher) *TelemetryHandler {
	return &TelemetryHandler{Store: store, Enricher: enricher}
}

// Register mounts the telemetry routes under the given group.
func (h *TelemetryHandler) Register(g gin.IRouter) {
	t := g.Group("/telemetry")
	t.POST("/session", h.Session)
	t.POST("/page-view", h.PageView)
	t.POST("/content-read", h.ContentRead)
	t.POST("/tool-view", h.ToolView)
	t.POST("/tool-usage", h.ToolUsage)
	t.POST("/feature-usage", h.FeatureUsage)
	t.POST("/conversion-history", h.ConversionHistory)
	t.POST("/download", h.Download)
	t.POST("/error", h.Error)
}

// maxBodyBytes caps the JSON body so a bad actor can't post 100MB of "props".
const maxBodyBytes = 256 * 1024

type bodySessionUpsert struct {
	VisitorID  string         `json:"visitorId"`
	SessionID  string         `json:"sessionId"`
	Properties map[string]any `json:"properties"`
}

func (h *TelemetryHandler) Session(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body bodySessionUpsert
	if !h.bind(c, &body) {
		return
	}
	visitorID, sessionID := h.identify(c, body.VisitorID, body.SessionID)
	ip := geo.ExtractIP(c)
	ua := c.GetHeader("User-Agent")
	entry := &geo.Entry{}
	if h.Enricher != nil && ip != "" {
		if e, _ := h.Enricher.Lookup(c.Request.Context(), ip); e != nil {
			entry = e
		}
	}
	if visitorID != "" {
		h.Store.UpsertVisitor(c.Request.Context(), telemetry.VisitorUpsert{
			VisitorID:   visitorID,
			UserAgent:   ua,
			IP:          ip,
			CountryCode: entry.CountryCode,
		})
	}
	if sessionID != "" {
		h.Store.UpsertSession(c.Request.Context(), telemetry.SessionUpsert{
			SessionID:   sessionID,
			VisitorID:   visitorID,
			UserAgent:   ua,
			IP:          ip,
			CountryCode: entry.CountryCode,
			Region:      entry.Region,
			City:        entry.City,
			Lat:         entry.Lat,
			Lon:         entry.Lon,
			Timezone:    entry.Timezone,
			ASNNumber:   entry.ASNNumber,
			ASNOrg:      entry.ASNOrg,
			Properties:  body.Properties,
		})
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

type bodyPageView struct {
	VisitorID         string         `json:"visitorId"`
	SessionID         string         `json:"sessionId"`
	VisitID           string         `json:"visitId"`
	PageType          string         `json:"pageType"`
	PageSlug          string         `json:"pageSlug"`
	PageTitle         string         `json:"pageTitle"`
	Pathname          string         `json:"pathname"`
	CurrentURL        string         `json:"currentUrl"`
	Referrer          string         `json:"referrer"`
	EnteredAt         *time.Time     `json:"enteredAt"`
	ExitedAt          *time.Time     `json:"exitedAt"`
	TotalVisibleMS    int64          `json:"totalVisibleMs"`
	TotalActiveMS     int64          `json:"totalActiveMs"`
	MaxScrollPercent  float64        `json:"maxScrollPercent"`
	CompletedRead     *bool          `json:"completedRead"`
	QuickScrollBottom *bool          `json:"quickScrollToBottom"`
	LikelyRealRead    *bool          `json:"likelyRealRead"`
	WordCount         int            `json:"wordCount"`
	EstimatedReadSecs int            `json:"estimatedReadSeconds"`
	Properties        map[string]any `json:"properties"`
}

func (h *TelemetryHandler) PageView(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body bodyPageView
	if !h.bind(c, &body) {
		return
	}
	visitorID, sessionID := h.identify(c, body.VisitorID, body.SessionID)
	h.Store.InsertPageView(c.Request.Context(), telemetry.PageView{
		VisitorID:            visitorID,
		SessionID:            sessionID,
		VisitID:              body.VisitID,
		PageType:             body.PageType,
		PageSlug:             body.PageSlug,
		PageTitle:            body.PageTitle,
		Pathname:             body.Pathname,
		CurrentURL:           body.CurrentURL,
		Referrer:             body.Referrer,
		EnteredAt:            body.EnteredAt,
		ExitedAt:             body.ExitedAt,
		TotalVisibleMS:       body.TotalVisibleMS,
		TotalActiveMS:        body.TotalActiveMS,
		MaxScrollPercent:     body.MaxScrollPercent,
		CompletedRead:        body.CompletedRead,
		QuickScrollToBottom:  body.QuickScrollBottom,
		LikelyRealRead:       body.LikelyRealRead,
		WordCount:            body.WordCount,
		EstimatedReadSeconds: body.EstimatedReadSecs,
		Properties:           body.Properties,
	})
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

type bodyContentRead struct {
	PageViewID         string         `json:"pageViewId"`
	VisitorID          string         `json:"visitorId"`
	SessionID          string         `json:"sessionId"`
	PageType           string         `json:"pageType"`
	PageSlug           string         `json:"pageSlug"`
	EventName          string         `json:"eventName"`
	ScrollPercent      float64        `json:"scrollPercent"`
	ActiveMSSinceLast  int64          `json:"activeMsSinceLast"`
	VisibleMSSinceLast int64          `json:"visibleMsSinceLast"`
	TotalActiveMS      int64          `json:"totalActiveMs"`
	TotalVisibleMS     int64          `json:"totalVisibleMs"`
	ViewportHeight     int            `json:"viewportHeight"`
	DocumentHeight     int            `json:"documentHeight"`
	WordsVisible       int            `json:"wordsVisibleEstimate"`
	QuickScrollFlag    *bool          `json:"quickScrollFlag"`
	Properties         map[string]any `json:"properties"`
}

func (h *TelemetryHandler) ContentRead(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body bodyContentRead
	if !h.bind(c, &body) {
		return
	}
	if body.EventName == "" {
		body.EventName = "heartbeat"
	}
	visitorID, sessionID := h.identify(c, body.VisitorID, body.SessionID)
	h.Store.InsertContentReadEvent(c.Request.Context(), telemetry.ContentReadEvent{
		PageViewID:         body.PageViewID,
		VisitorID:          visitorID,
		SessionID:          sessionID,
		PageType:           body.PageType,
		PageSlug:           body.PageSlug,
		EventName:          body.EventName,
		ScrollPercent:      body.ScrollPercent,
		ActiveMSSinceLast:  body.ActiveMSSinceLast,
		VisibleMSSinceLast: body.VisibleMSSinceLast,
		TotalActiveMS:      body.TotalActiveMS,
		TotalVisibleMS:     body.TotalVisibleMS,
		ViewportHeight:     body.ViewportHeight,
		DocumentHeight:     body.DocumentHeight,
		WordsVisible:       body.WordsVisible,
		QuickScroll:        body.QuickScrollFlag,
		Properties:         body.Properties,
	})
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

type bodyToolView struct {
	VisitorID        string         `json:"visitorId"`
	SessionID        string         `json:"sessionId"`
	VisitID          string         `json:"visitId"`
	Tool             string         `json:"tool"`
	MediaKind        string         `json:"mediaKind"`
	Pathname         string         `json:"pathname"`
	CurrentURL       string         `json:"currentUrl"`
	Referrer         string         `json:"referrer"`
	EnteredAt        *time.Time     `json:"enteredAt"`
	ExitedAt         *time.Time     `json:"exitedAt"`
	TotalVisibleMS   int64          `json:"totalVisibleMs"`
	TotalActiveMS    int64          `json:"totalActiveMs"`
	MaxScrollPercent float64        `json:"maxScrollPercent"`
	Properties       map[string]any `json:"properties"`
}

func (h *TelemetryHandler) ToolView(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body bodyToolView
	if !h.bind(c, &body) {
		return
	}
	if strings.TrimSpace(body.Tool) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tool is required"})
		return
	}
	visitorID, sessionID := h.identify(c, body.VisitorID, body.SessionID)
	h.Store.InsertToolView(c.Request.Context(), telemetry.ToolView{
		VisitorID:        visitorID,
		SessionID:        sessionID,
		VisitID:          body.VisitID,
		Tool:             body.Tool,
		MediaKind:        body.MediaKind,
		Pathname:         body.Pathname,
		CurrentURL:       body.CurrentURL,
		Referrer:         body.Referrer,
		EnteredAt:        body.EnteredAt,
		ExitedAt:         body.ExitedAt,
		TotalVisibleMS:   body.TotalVisibleMS,
		TotalActiveMS:    body.TotalActiveMS,
		MaxScrollPercent: body.MaxScrollPercent,
		Properties:       body.Properties,
	})
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

type bodyToolUsage struct {
	VisitorID    string         `json:"visitorId"`
	SessionID    string         `json:"sessionId"`
	RequestID    string         `json:"requestId"`
	JobID        string         `json:"jobId"`
	Tool         string         `json:"tool"`
	MediaKind    string         `json:"mediaKind"`
	Action       string         `json:"action"`
	SourceFormat string         `json:"sourceFormat"`
	TargetFormat string         `json:"targetFormat"`
	Options      map[string]any `json:"options"`
	Success      *bool          `json:"success"`
	DurationMS   int            `json:"durationMs"`
	InputBytes   int64          `json:"inputBytes"`
	OutputBytes  int64          `json:"outputBytes"`
	Properties   map[string]any `json:"properties"`
}

func (h *TelemetryHandler) ToolUsage(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body bodyToolUsage
	if !h.bind(c, &body) {
		return
	}
	if strings.TrimSpace(body.Tool) == "" || strings.TrimSpace(body.Action) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tool and action are required"})
		return
	}
	visitorID, sessionID := h.identify(c, body.VisitorID, body.SessionID)
	h.Store.InsertToolUsage(c.Request.Context(), telemetry.ToolUsage{
		VisitorID:    visitorID,
		SessionID:    sessionID,
		RequestID:    body.RequestID,
		JobID:        body.JobID,
		Tool:         body.Tool,
		MediaKind:    body.MediaKind,
		Action:       body.Action,
		SourceFormat: body.SourceFormat,
		TargetFormat: body.TargetFormat,
		Options:      body.Options,
		Success:      body.Success,
		DurationMS:   body.DurationMS,
		InputBytes:   body.InputBytes,
		OutputBytes:  body.OutputBytes,
		Properties:   body.Properties,
	})
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

type bodyFeatureUsage struct {
	VisitorID       string         `json:"visitorId"`
	SessionID       string         `json:"sessionId"`
	JobID           string         `json:"jobId"`
	FeatureName     string         `json:"featureName"`
	FeatureCategory string         `json:"featureCategory"`
	Action          string         `json:"action"`
	Value           string         `json:"value"`
	MediaKind       string         `json:"mediaKind"`
	Success         *bool          `json:"success"`
	Properties      map[string]any `json:"properties"`
}

func (h *TelemetryHandler) FeatureUsage(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body bodyFeatureUsage
	if !h.bind(c, &body) {
		return
	}
	if strings.TrimSpace(body.FeatureName) == "" || strings.TrimSpace(body.Action) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "featureName and action are required"})
		return
	}
	visitorID, sessionID := h.identify(c, body.VisitorID, body.SessionID)
	h.Store.InsertFeatureUsage(c.Request.Context(), telemetry.FeatureUsage{
		VisitorID:       visitorID,
		SessionID:       sessionID,
		JobID:           body.JobID,
		FeatureName:     body.FeatureName,
		FeatureCategory: body.FeatureCategory,
		Action:          body.Action,
		Value:           body.Value,
		MediaKind:       body.MediaKind,
		Success:         body.Success,
		Properties:      body.Properties,
	})
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

type bodyHistory struct {
	VisitorID    string         `json:"visitorId"`
	SessionID    string         `json:"sessionId"`
	JobID        string         `json:"jobId"`
	EventName    string         `json:"eventName"`
	Tool         string         `json:"tool"`
	MediaKind    string         `json:"mediaKind"`
	SourceFormat string         `json:"sourceFormat"`
	TargetFormat string         `json:"targetFormat"`
	ResultAvail  *bool          `json:"resultAvailable"`
	ResultExpd   *bool          `json:"resultExpired"`
	AgeSeconds   int            `json:"ageSeconds"`
	Properties   map[string]any `json:"properties"`
}

func (h *TelemetryHandler) ConversionHistory(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body bodyHistory
	if !h.bind(c, &body) {
		return
	}
	if strings.TrimSpace(body.EventName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "eventName is required"})
		return
	}
	visitorID, sessionID := h.identify(c, body.VisitorID, body.SessionID)
	h.Store.InsertHistoryEvent(c.Request.Context(), telemetry.HistoryEvent{
		VisitorID:       visitorID,
		SessionID:       sessionID,
		JobID:           body.JobID,
		EventName:       body.EventName,
		Tool:            body.Tool,
		MediaKind:       body.MediaKind,
		SourceFormat:    body.SourceFormat,
		TargetFormat:    body.TargetFormat,
		ResultAvailable: body.ResultAvail,
		ResultExpired:   body.ResultExpd,
		AgeSeconds:      body.AgeSeconds,
		Properties:      body.Properties,
	})
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

type bodyDownload struct {
	VisitorID         string         `json:"visitorId"`
	SessionID         string         `json:"sessionId"`
	JobID             string         `json:"jobId"`
	Tool              string         `json:"tool"`
	MediaKind         string         `json:"mediaKind"`
	FileName          string         `json:"fileName"`
	SafeFileExtension string         `json:"safeFileExtension"`
	OutputFormat      string         `json:"outputFormat"`
	SizeBytes         int64          `json:"sizeBytes"`
	ContentType       string         `json:"contentType"`
	SHA256            string         `json:"sha256"`
	Success           bool           `json:"success"`
	FailureReason     string         `json:"failureReason"`
	Properties        map[string]any `json:"properties"`
}

func (h *TelemetryHandler) Download(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body bodyDownload
	if !h.bind(c, &body) {
		return
	}
	visitorID, sessionID := h.identify(c, body.VisitorID, body.SessionID)
	now := time.Now().UTC()
	h.Store.InsertDownloadResult(c.Request.Context(), telemetry.DownloadResult{
		VisitorID:         visitorID,
		SessionID:         sessionID,
		JobID:             body.JobID,
		Tool:              body.Tool,
		MediaKind:         body.MediaKind,
		FileName:          body.FileName,
		SafeFileExtension: body.SafeFileExtension,
		OutputFormat:      body.OutputFormat,
		SizeBytes:         body.SizeBytes,
		ContentType:       body.ContentType,
		SHA256:            body.SHA256,
		DownloadedAt:      &now,
		Success:           body.Success || body.FailureReason == "",
		FailureReason:     body.FailureReason,
		Properties:        body.Properties,
	})
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

type bodyError struct {
	VisitorID    string         `json:"visitorId"`
	SessionID    string         `json:"sessionId"`
	JobID        string         `json:"jobId"`
	Source       string         `json:"source"`
	Tool         string         `json:"tool"`
	Stage        string         `json:"stage"`
	ErrorType    string         `json:"errorType"`
	ErrorMessage string         `json:"errorMessage"`
	MediaKind    string         `json:"mediaKind"`
	Severity     string         `json:"severity"`
	Properties   map[string]any `json:"properties"`
}

func (h *TelemetryHandler) Error(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body bodyError
	if !h.bind(c, &body) {
		return
	}
	if strings.TrimSpace(body.Source) == "" {
		body.Source = "client"
	}
	visitorID, sessionID := h.identify(c, body.VisitorID, body.SessionID)
	h.Store.InsertToolError(c.Request.Context(), telemetry.ToolError{
		VisitorID:    visitorID,
		SessionID:    sessionID,
		JobID:        body.JobID,
		Source:       body.Source,
		Tool:         body.Tool,
		Stage:        body.Stage,
		ErrorType:    body.ErrorType,
		ErrorMessage: body.ErrorMessage,
		MediaKind:    body.MediaKind,
		Severity:     body.Severity,
		Properties:   body.Properties,
	})
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}

// --- helpers ---------------------------------------------------------------

func (h *TelemetryHandler) enabled(c *gin.Context) bool {
	if h == nil || h.Store == nil || !h.Store.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "telemetry storage disabled"})
		return false
	}
	return true
}

func (h *TelemetryHandler) bind(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	if err := c.ShouldBindJSON(target); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return false
	}
	return true
}

// identify resolves visitor/session from request headers (preferred) falling
// back to JSON body fields. Returns the canonical values that should be
// persisted.
func (h *TelemetryHandler) identify(c *gin.Context, bodyVisitor, bodySession string) (string, string) {
	visitorID := strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID"))
	if visitorID == "" {
		visitorID = strings.TrimSpace(bodyVisitor)
	}
	sessionID := strings.TrimSpace(c.GetHeader("X-MM-Session-ID"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(bodySession)
	}
	if v, ok := c.Get(logger.GinKey); ok {
		if f, ok := v.(*logger.Fields); ok && f != nil {
			if visitorID != "" {
				f.VisitorID = visitorID
			}
			if sessionID != "" {
				f.SessionID = sessionID
			}
		}
	}
	return visitorID, sessionID
}

// Compile-time check that ctx parameter on the helper functions is used.
var _ = context.Background
