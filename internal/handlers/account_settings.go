package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// SettingsStore is the slice of *telemetry.Store the settings routes need.
// Tests pass a fake: no database in this repo's unit suite.
type SettingsStore interface {
	Enabled() bool
	UserByFirebaseUID(ctx context.Context, firebaseUID string) (*models.User, error)
	GetOrCreateUserSettings(ctx context.Context, userID string, defaultHours int) (*telemetry.UserSettings, error)
	ApplyUserSettings(ctx context.Context, userID string, defaultHours int, patch telemetry.SettingsPatch, now time.Time) (*telemetry.UserSettings, int64, error)
	PreviewRetentionShrink(ctx context.Context, userID string, hours int, now time.Time) (telemetry.RetentionPreview, error)
}

// SettingsHandler serves GET/PATCH /api/account/settings and the shrink preview.
//
// Retention is NOT tier-gated: every signed-in user may choose 24 h to 2 years.
type SettingsHandler struct {
	store  SettingsStore
	logger *slog.Logger
	now    func() time.Time
}

// NewSettingsHandler builds the handler. store may be a nil interface only in
// misconfigured boots; the routes then answer 503.
func NewSettingsHandler(store SettingsStore, logger *slog.Logger) *SettingsHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &SettingsHandler{store: store, logger: logger, now: time.Now}
}

type settingsLimits struct {
	MinHours     int `json:"minHours"`
	MaxHours     int `json:"maxHours"`
	DefaultHours int `json:"defaultHours"`
}

type settingsResponse struct {
	RetentionHours        int    `json:"retentionHours"`
	RetentionScope        string `json:"retentionScope"`
	DeleteMetadataReports bool   `json:"deleteMetadataReports"`
	HistoryVisibility     string `json:"historyVisibility"`
	// MM31: additive email preferences. Welcome, payment-failed and account-deleted
	// mail are transactional and have no toggle.
	EmailAIJobFinished bool           `json:"emailAiJobFinished"`
	EmailExpiryDigest  bool           `json:"emailExpiryDigest"`
	UpdatedAt          time.Time      `json:"updatedAt"`
	Limits             settingsLimits `json:"limits"`
	// Restamped is the number of live items whose expiry moved. PATCH only.
	Restamped *int64 `json:"restamped,omitempty"`
}

type previewCounts struct {
	Results  int64 `json:"results"`
	Projects int64 `json:"projects"`
	Other    int64 `json:"other"`
	Bytes    int64 `json:"bytes"`
}

type previewResponse struct {
	WouldDelete        previewCounts `json:"wouldDelete"`
	EarliestDeletionAt *time.Time    `json:"earliestDeletionAt"`
}

type settingsError struct {
	Error string `json:"error"`
	Field string `json:"field,omitempty"`
}

func limitsFor(defaultHours int) settingsLimits {
	return settingsLimits{MinHours: retention.MinHours, MaxHours: retention.MaxHours, DefaultHours: defaultHours}
}

func toSettingsResponse(s *telemetry.UserSettings, defaultHours int) settingsResponse {
	return settingsResponse{
		RetentionHours:        s.RetentionHours,
		RetentionScope:        s.RetentionScope,
		DeleteMetadataReports: s.DeleteMetadataReports,
		HistoryVisibility:     s.HistoryVisibility,
		EmailAIJobFinished:    s.EmailAIJobFinished,
		EmailExpiryDigest:     s.EmailExpiryDigest,
		UpdatedAt:             s.UpdatedAt,
		Limits:                limitsFor(defaultHours),
	}
}

// caller resolves the signed-in account. It writes the error response itself.
func (h *SettingsHandler) caller(c *gin.Context) (*models.User, bool) {
	principal, ok := middleware.PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, settingsError{Error: "Authentication required"})
		return nil, false
	}
	if h.store == nil || !h.store.Enabled() {
		c.JSON(http.StatusServiceUnavailable, settingsError{Error: "Settings are temporarily unavailable"})
		return nil, false
	}
	user, err := h.store.UserByFirebaseUID(c.Request.Context(), principal.UID)
	if err != nil || user == nil {
		if err != nil {
			h.logger.Warn("account settings: user lookup failed", "error", err.Error())
		}
		// No mm_users row yet means /sync has not landed; nothing to configure.
		c.JSON(http.StatusConflict, settingsError{Error: "Account is not set up yet; sign in again"})
		return nil, false
	}
	return user, true
}

// Get returns the caller's settings, creating the default row on first read.
func (h *SettingsHandler) Get(c *gin.Context) {
	user, ok := h.caller(c)
	if !ok {
		return
	}
	def := retention.DefaultHours(user.Tier)
	s, err := h.store.GetOrCreateUserSettings(c.Request.Context(), user.ID, def)
	if err != nil {
		h.logger.Error("account settings: read failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, settingsError{Error: "Could not load your settings"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, toSettingsResponse(s, def))
}

type settingsPatchBody struct {
	RetentionHours        *int    `json:"retentionHours"`
	RetentionScope        *string `json:"retentionScope"`
	DeleteMetadataReports *bool   `json:"deleteMetadataReports"`
	HistoryVisibility     *string `json:"historyVisibility"`
	EmailAIJobFinished    *bool   `json:"emailAiJobFinished"`
	EmailExpiryDigest     *bool   `json:"emailExpiryDigest"`
}

// validate returns the first field-level problem, or nil.
func (b settingsPatchBody) validate() *settingsError {
	if b.RetentionHours == nil && b.RetentionScope == nil && b.DeleteMetadataReports == nil && b.HistoryVisibility == nil &&
		b.EmailAIJobFinished == nil && b.EmailExpiryDigest == nil {
		return &settingsError{Error: "No settings provided"}
	}
	if b.RetentionHours != nil && !retention.ValidHours(*b.RetentionHours) {
		return &settingsError{
			Error: "retentionHours must be between " + strconv.Itoa(retention.MinHours) + " and " + strconv.Itoa(retention.MaxHours),
			Field: "retentionHours",
		}
	}
	if b.RetentionScope != nil && *b.RetentionScope != telemetry.RetentionScopeAll {
		return &settingsError{Error: "retentionScope must be \"all\"", Field: "retentionScope"}
	}
	if b.HistoryVisibility != nil &&
		*b.HistoryVisibility != telemetry.HistoryVisibilityFull && *b.HistoryVisibility != telemetry.HistoryVisibilityNames {
		return &settingsError{Error: "historyVisibility must be \"full\" or \"names_only\"", Field: "historyVisibility"}
	}
	return nil
}

func (h *SettingsHandler) previewFor(c *gin.Context, user *models.User, hours int) (previewResponse, bool) {
	now := h.now().UTC()
	p, err := h.store.PreviewRetentionShrink(c.Request.Context(), user.ID, hours, now)
	if err != nil {
		h.logger.Error("account settings: preview failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, settingsError{Error: "Could not compute the preview"})
		return previewResponse{}, false
	}
	out := previewResponse{WouldDelete: previewCounts{Results: p.Results, Projects: p.Projects, Other: p.Other, Bytes: p.Bytes}}
	if p.Results+p.Projects+p.Other > 0 {
		// Doomed rows are floored to now+1h on re-stamp, so that is the earliest sweep.
		at := now.Add(retention.RestampFloor)
		out.EarliestDeletionAt = &at
	}
	return out, true
}

// Preview is GET /api/account/settings/preview?retentionHours=N. Read-only.
func (h *SettingsHandler) Preview(c *gin.Context) {
	user, ok := h.caller(c)
	if !ok {
		return
	}
	hours, err := strconv.Atoi(c.Query("retentionHours"))
	if err != nil || !retention.ValidHours(hours) {
		c.JSON(http.StatusBadRequest, settingsError{
			Error: "retentionHours must be between " + strconv.Itoa(retention.MinHours) + " and " + strconv.Itoa(retention.MaxHours),
			Field: "retentionHours",
		})
		return
	}
	out, ok := h.previewFor(c, user, hours)
	if !ok {
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, out)
}

// Patch is PATCH /api/account/settings[?dryRun=1].
func (h *SettingsHandler) Patch(c *gin.Context) {
	user, ok := h.caller(c)
	if !ok {
		return
	}
	var body settingsPatchBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, settingsError{Error: "Invalid request body"})
		return
	}
	if verr := body.validate(); verr != nil {
		c.JSON(http.StatusBadRequest, verr)
		return
	}
	def := retention.DefaultHours(user.Tier)

	if q := c.Query("dryRun"); q == "1" || q == "true" {
		hours := 0
		if body.RetentionHours != nil {
			hours = *body.RetentionHours
		} else {
			cur, err := h.store.GetOrCreateUserSettings(c.Request.Context(), user.ID, def)
			if err != nil {
				c.JSON(http.StatusInternalServerError, settingsError{Error: "Could not load your settings"})
				return
			}
			hours = cur.RetentionHours
		}
		out, ok := h.previewFor(c, user, hours)
		if !ok {
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, out)
		return
	}

	updated, restamped, err := h.store.ApplyUserSettings(c.Request.Context(), user.ID, def, telemetry.SettingsPatch{
		RetentionHours:        body.RetentionHours,
		DeleteMetadataReports: body.DeleteMetadataReports,
		HistoryVisibility:     body.HistoryVisibility,
		EmailAIJobFinished:    body.EmailAIJobFinished,
		EmailExpiryDigest:     body.EmailExpiryDigest,
	}, h.now().UTC())
	if err != nil {
		h.logger.Error("account settings: update failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, settingsError{Error: "Could not save your settings"})
		return
	}
	resp := toSettingsResponse(updated, def)
	resp.Restamped = &restamped
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, resp)
}
