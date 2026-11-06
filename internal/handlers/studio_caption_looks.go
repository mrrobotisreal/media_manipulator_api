package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// Darkroom saved caption looks (MM122 decision 4) under
// /api/studio/caption-looks. A look is account settings (a name and a complete
// caption style), so nothing here spends an allowance. Modelled on the Aperture
// presets (photo_presets.go).
//
//	GET    /studio/caption-looks       list, newest first: {looks, limit, used}
//	POST   /studio/caption-looks       {name, style} -> 201; 403 caption_looks_limit
//	GET    /studio/caption-looks/:id   one look
//	PUT    /studio/caption-looks/:id   If-Match revision; {name?, style?}
//	DELETE /studio/caption-looks/:id   soft delete + ledger expire now
//
// Every route needs an account (anonymous -> 401 sign_in_required). The cap is
// Limits.CaptionLooksSaved (TIER_*_CAPTION_LOOKS 0/0/10/-1): a free account
// may list (always empty) but every create is the 403 with upgradePath
// premium; Premium's 11th is the 403 with upgradePath pro. Ownership is strict
// (owner only); a stranger gets the same 404 a missing look gets.

// CaptionLookStore is the slice of *telemetry.Store the caption-look routes use.
type CaptionLookStore interface {
	CreateStudioCaptionLook(ctx context.Context, in telemetry.StudioCaptionLookInsert, limit int) (*telemetry.StudioCaptionLook, int, error)
	ListStudioCaptionLooks(ctx context.Context, ownerUID string, limit, offset int) ([]telemetry.StudioCaptionLook, error)
	CountStudioCaptionLooks(ctx context.Context, ownerUID string) (int, error)
	GetStudioCaptionLook(ctx context.Context, id, ownerUID string) (*telemetry.StudioCaptionLook, error)
	SaveStudioCaptionLook(ctx context.Context, id, ownerUID string, expectedRevision int, patch telemetry.StudioCaptionLookPatch) (*telemetry.StudioCaptionLook, error)
	DeleteStudioCaptionLook(ctx context.Context, id, ownerUID string) error
}

// SetCaptionLookStore attaches the caption-look store (nil = the routes answer 503).
func (h *StudioHandler) SetCaptionLookStore(s CaptionLookStore) {
	if h != nil {
		h.captionLooks = s
	}
}

const (
	captionLookNameMax  = 60
	captionLookBodyMax  = 16 * 1024 // a style is ~600 bytes
	captionLookListMax  = 200
	captionLooksLimitMs = "You have reached the saved caption look limit for your plan."
)

func (h *StudioHandler) captionLooksReady(c *gin.Context) bool {
	if h == nil || h.captionLooks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Saved caption looks require a database, which is currently unavailable"})
		return false
	}
	return true
}

func requireCaptionLookUser(c *gin.Context) (string, bool) {
	userID, _ := callerIdentity(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "sign_in_required", "message": "Sign in to save caption looks to your account"})
		return "", false
	}
	return userID, true
}

// captionLookOwnerID is the identity gate plus a well-formed uuid check for :id.
func (h *StudioHandler) captionLookOwnerID(c *gin.Context) (userID, id string, ok bool) {
	if !h.captionLooksReady(c) {
		return "", "", false
	}
	userID, ok = requireCaptionLookUser(c)
	if !ok {
		return "", "", false
	}
	id = strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		denyCaptionLook(c)
		return "", "", false
	}
	return userID, id, true
}

func denyCaptionLook(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": "Caption look not found"})
}

type captionLookBody struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Style     json.RawMessage `json:"style"`
	Revision  int             `json:"revision"`
	CreatedAt string          `json:"createdAt"`
	UpdatedAt string          `json:"updatedAt"`
}

func captionLookJSON(l *telemetry.StudioCaptionLook) captionLookBody {
	return captionLookBody{
		ID: l.ID, Name: l.Name, Style: json.RawMessage(l.Style), Revision: l.Revision,
		CreatedAt: l.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: l.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// sanitizeCaptionLookStyle runs the caption-style sanitizer (the same one a
// project save uses) and returns its canonical JSON. A saved look never keeps
// a lookId (it is the user's own look, not a catalog one).
func sanitizeCaptionLookStyle(c *gin.Context, raw json.RawMessage) ([]byte, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "caption_look_invalid", "message": "style is required"})
		return nil, false
	}
	var st models.StudioCaptionStyle
	if err := json.Unmarshal(raw, &st); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "caption_look_invalid", "message": "style must be a caption style object"})
		return nil, false
	}
	clean := models.SanitizeCaptionStyle(&st)
	clean.LookID = ""
	out, err := json.Marshal(clean)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "caption_look_invalid"})
		return nil, false
	}
	return out, true
}

func clampCaptionLookName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > captionLookNameMax {
		return "", false
	}
	return name, true
}

func readCaptionLookBody(c *gin.Context) ([]byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, captionLookBodyMax)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "caption_look_too_large", "maxBytes": captionLookBodyMax})
			return nil, false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return nil, false
	}
	return raw, true
}

// captionLookCap is the caller's tier cap and tier (cap unlimited when no tier
// system resolves, like the preset routes).
func captionLookCap(c *gin.Context) (int, tiers.Tier) {
	if res, ok := middleware.ResolveTier(c); ok {
		return res.Limits.CaptionLooksSaved, res.Tier
	}
	return tiers.Unlimited, tiers.TierFree
}

// ListCaptionLooks handles GET /studio/caption-looks?limit=&offset=.
//
//	200 {"looks":[{id,name,style,revision,createdAt,updatedAt}],"limit":N,"used":N}
//
// limit is the caller's tier cap (-1 unlimited); used the live look count.
func (h *StudioHandler) ListCaptionLooks(c *gin.Context) {
	if !h.captionLooksReady(c) {
		return
	}
	userID, ok := requireCaptionLookUser(c)
	if !ok {
		return
	}
	limit, offset := captionLookListMax, 0
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 && v < limit {
		limit = v
	}
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v > 0 {
		offset = v
	}
	rows, err := h.captionLooks.ListStudioCaptionLooks(c.Request.Context(), userID, limit, offset)
	if err != nil {
		log.Printf("studio: list caption looks failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Caption looks are temporarily unavailable"})
		return
	}
	used := len(rows) + offset
	if len(rows) == limit {
		if n, err := h.captionLooks.CountStudioCaptionLooks(c.Request.Context(), userID); err == nil {
			used = n
		}
	}
	out := make([]captionLookBody, 0, len(rows))
	for i := range rows {
		out = append(out, captionLookJSON(&rows[i]))
	}
	capLimit, _ := captionLookCap(c)
	c.JSON(http.StatusOK, gin.H{"looks": out, "limit": capLimit, "used": used})
}

type captionLookCreateRequest struct {
	Name  string          `json:"name"`
	Style json.RawMessage `json:"style"`
}

// CreateCaptionLook handles POST /studio/caption-looks.
//
//	201 the look (+ "used": the live count after the save)
//	401 sign_in_required | 403 {"error":"caption_looks_limit","limit","used","upgradePath"}
//	413 caption_look_too_large | 422 caption_look_invalid
func (h *StudioHandler) CreateCaptionLook(c *gin.Context) {
	if !h.captionLooksReady(c) {
		return
	}
	userID, ok := requireCaptionLookUser(c)
	if !ok {
		return
	}
	raw, ok := readCaptionLookBody(c)
	if !ok {
		return
	}
	var req captionLookCreateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	name, ok := clampCaptionLookName(req.Name)
	if !ok {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "caption_look_invalid", "message": "name must be 1 to 60 characters"})
		return
	}
	style, ok := sanitizeCaptionLookStyle(c, req.Style)
	if !ok {
		return
	}
	limit, tier := captionLookCap(c)
	l, used, err := h.captionLooks.CreateStudioCaptionLook(c.Request.Context(), telemetry.StudioCaptionLookInsert{
		ID: uuid.NewString(), OwnerUserID: userID, Name: name, Style: style,
	}, limit)
	var capErr *telemetry.StudioCaptionLookLimitError
	switch {
	case errors.As(err, &capErr):
		c.JSON(http.StatusForbidden, gin.H{
			"error": "caption_looks_limit", "message": captionLooksLimitMs,
			"limit": capErr.Limit, "used": capErr.Used, "upgradePath": vectorUpgradePath(tier),
		})
		return
	case err != nil:
		log.Printf("studio: create caption look failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to save caption look"})
		return
	}
	// Looks are settings: the ledger row exists for the delete and
	// account-delete paths only, so it never comes due by time.
	retention.Default().Register(detachedCtx(c), retention.RegisterInput{
		Kind: retention.KindStudioCaptionLook, Locator: l.ID, ProjectID: l.ID, Owner: retentionOwnerFor(c), KeepWithAccount: true,
	})
	body := captionLookJSON(l)
	c.JSON(http.StatusCreated, gin.H{
		"id": body.ID, "name": body.Name, "style": body.Style, "revision": body.Revision,
		"createdAt": body.CreatedAt, "updatedAt": body.UpdatedAt, "used": used,
	})
}

// GetCaptionLook handles GET /studio/caption-looks/:id.
func (h *StudioHandler) GetCaptionLook(c *gin.Context) {
	userID, id, ok := h.captionLookOwnerID(c)
	if !ok {
		return
	}
	l, err := h.captionLooks.GetStudioCaptionLook(c.Request.Context(), id, userID)
	if errors.Is(err, telemetry.ErrStudioCaptionLookNotFound) {
		denyCaptionLook(c)
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Caption looks are temporarily unavailable"})
		return
	}
	c.JSON(http.StatusOK, captionLookJSON(l))
}

type captionLookSaveRequest struct {
	Name  *string         `json:"name"`
	Style json.RawMessage `json:"style"`
}

// SaveCaptionLook handles PUT /studio/caption-looks/:id.
//
//	header If-Match: <revision>   required (quotes and W/ tolerated)
//	body   {"name"?, "style"?}
//	200    the look with revision+1 | 409 revision_conflict | 428 if_match_required
func (h *StudioHandler) SaveCaptionLook(c *gin.Context) {
	userID, id, ok := h.captionLookOwnerID(c)
	if !ok {
		return
	}
	expected, present, valid := parseIfMatch(c.GetHeader("If-Match"))
	if !present {
		c.JSON(http.StatusPreconditionRequired, gin.H{"error": "if_match_required", "message": "Send If-Match: <revision> with every save"})
		return
	}
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "if_match_invalid", "message": "If-Match must be the integer revision"})
		return
	}
	raw, ok := readCaptionLookBody(c)
	if !ok {
		return
	}
	var req captionLookSaveRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	var patch telemetry.StudioCaptionLookPatch
	if req.Name != nil {
		n, ok := clampCaptionLookName(*req.Name)
		if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "caption_look_invalid", "message": "name must be 1 to 60 characters"})
			return
		}
		patch.Name = &n
	}
	if len(req.Style) > 0 && string(req.Style) != "null" {
		style, ok := sanitizeCaptionLookStyle(c, req.Style)
		if !ok {
			return
		}
		patch.Style = style
	}
	if patch.Name == nil && patch.Style == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nothing to save: send name or style"})
		return
	}
	l, err := h.captionLooks.SaveStudioCaptionLook(c.Request.Context(), id, userID, expected, patch)
	var conflict *telemetry.StudioCaptionLookRevisionConflictError
	switch {
	case errors.As(err, &conflict):
		c.JSON(http.StatusConflict, gin.H{"error": "revision_conflict", "revision": conflict.Revision})
		return
	case errors.Is(err, telemetry.ErrStudioCaptionLookNotFound):
		denyCaptionLook(c)
		return
	case err != nil:
		log.Printf("studio: save caption look failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to save caption look"})
		return
	}
	c.JSON(http.StatusOK, captionLookJSON(l))
}

// DeleteCaptionLook handles DELETE /studio/caption-looks/:id.
//
//	200 {"deleted":{"objects":N,"bytes":N},"pending":N}
func (h *StudioHandler) DeleteCaptionLook(c *gin.Context) {
	userID, id, ok := h.captionLookOwnerID(c)
	if !ok {
		return
	}
	if h.expirer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	// Ownership first: another user's id is the same 404 a missing look gets
	// and never reaches the ledger.
	if _, err := h.captionLooks.GetStudioCaptionLook(ctx, id, userID); errors.Is(err, telemetry.ErrStudioCaptionLookNotFound) {
		denyCaptionLook(c)
		return
	} else if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Caption looks are temporarily unavailable"})
		return
	}
	if err := h.captionLooks.DeleteStudioCaptionLook(ctx, id, userID); err != nil {
		log.Printf("studio: soft delete caption look failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	res, err := h.expirer.ExpireNow(ctx, ledgerFilterForProject(id))
	if err != nil && !errors.Is(err, retention.ErrNoExpire) {
		log.Printf("studio: delete caption look failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	if h.onDelete != nil {
		h.onDelete("caption_look")
	}
	c.JSON(http.StatusOK, newDeletedBody(res))
}

// IsCaptionLookWritePath matches the create the caption_looks rate bucket
// guards: POST /api/studio/caption-looks (reads, saves, deletes are not limited).
func IsCaptionLookWritePath(path string) bool { return path == "/api/studio/caption-looks" }
