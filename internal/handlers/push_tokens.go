package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/email"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MA23: FCM device-token registration. The token is never logged or echoed.

const (
	maxPushTokenLen  = 4096
	maxPushVersionLn = 64
)

// PushTokenStore is the slice of *telemetry.Store the push-token routes need.
type PushTokenStore interface {
	Enabled() bool
	UserByFirebaseUID(ctx context.Context, firebaseUID string) (*models.User, error)
	UpsertPushToken(ctx context.Context, userID, token, platform, appVersion, locale string, now time.Time) error
	DeletePushToken(ctx context.Context, userID, token string) (bool, error)
}

// PushTokenHandler serves POST|DELETE /api/account/push-tokens.
type PushTokenHandler struct {
	store  PushTokenStore
	logger *slog.Logger
	now    func() time.Time
}

// NewPushTokenHandler builds the handler.
func NewPushTokenHandler(store PushTokenStore, logger *slog.Logger) *PushTokenHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &PushTokenHandler{store: store, logger: logger, now: time.Now}
}

type pushTokenBody struct {
	Token      string `json:"token"`
	Platform   string `json:"platform"`
	AppVersion string `json:"appVersion"`
	Locale     string `json:"locale"`
}

func (h *PushTokenHandler) caller(c *gin.Context) (*models.User, bool) {
	principal, ok := middleware.PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return nil, false
	}
	if h.store == nil || !h.store.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "push_unavailable"})
		return nil, false
	}
	user, err := h.store.UserByFirebaseUID(c.Request.Context(), principal.UID)
	if err != nil {
		h.logger.Warn("push tokens: user lookup failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup_failed"})
		return nil, false
	}
	if user == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "account_not_synced", "reason": "account_not_synced"})
		return nil, false
	}
	return user, true
}

// Register is POST /api/account/push-tokens.
func (h *PushTokenHandler) Register(c *gin.Context) {
	var body pushTokenBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	if body.Token == "" || utf8.RuneCountInString(body.Token) > maxPushTokenLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_token"})
		return
	}
	if body.Platform != "ios" && body.Platform != "android" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_platform"})
		return
	}
	body.AppVersion = strings.TrimSpace(body.AppVersion)
	if utf8.RuneCountInString(body.AppVersion) > maxPushVersionLn {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_app_version"})
		return
	}
	user, ok := h.caller(c)
	if !ok {
		return
	}
	locale := email.NormalizeLocale(body.Locale)
	if err := h.store.UpsertPushToken(c.Request.Context(), user.ID, body.Token, body.Platform, body.AppVersion, locale, h.now().UTC()); err != nil {
		h.logger.Error("push tokens: upsert failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save_failed"})
		return
	}
	c.Status(http.StatusNoContent)
}

// Unregister is DELETE /api/account/push-tokens. Idempotent: 204 whether or not
// the caller had that token, so it never reveals another user's tokens.
func (h *PushTokenHandler) Unregister(c *gin.Context) {
	var body pushTokenBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	if body.Token == "" || utf8.RuneCountInString(body.Token) > maxPushTokenLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_token"})
		return
	}
	user, ok := h.caller(c)
	if !ok {
		return
	}
	if _, err := h.store.DeletePushToken(c.Request.Context(), user.ID, body.Token); err != nil {
		h.logger.Error("push tokens: delete failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete_failed"})
		return
	}
	c.Status(http.StatusNoContent)
}
