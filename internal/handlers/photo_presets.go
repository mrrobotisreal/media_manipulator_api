package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/photo"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// Aperture user presets (MM103) under /api/photo/presets. A preset is account
// settings (name, category, a partial doc of layers, an optional 96 px PNG
// thumbnail), so nothing here spends an allowance.
//
//	GET    /photo/presets                  list, newest first; layers only with ?full=1
//	POST   /photo/presets                  {name, category?, layers} -> 201; 403 photo_presets_limit
//	GET    /photo/presets/:id              full
//	PUT    /photo/presets/:id              If-Match revision; {name?, category?, layers?}
//	DELETE /photo/presets/:id              soft delete + ledger expire now
//	POST   /photo/presets/:id/thumbnail    multipart `file`, PNG <= 64 KB
//	GET    /photo/presets/:id/thumbnail    the PNG (private cache)
//
// Every route needs an account (anonymous -> 401 sign_in_required): guests keep
// their presets in the browser. Ownership is strict (owner only, no operator
// bypass); a stranger gets the same 404 a missing preset gets.

// PhotoPresetStore is the slice of *telemetry.Store the preset routes use.
type PhotoPresetStore interface {
	CreatePhotoPreset(ctx context.Context, in telemetry.PhotoPresetInsert, limit int) (*telemetry.PhotoPreset, error)
	ListPhotoPresets(ctx context.Context, ownerUID string, full bool, limit, offset int) ([]telemetry.PhotoPreset, error)
	GetPhotoPreset(ctx context.Context, id, ownerUID string) (*telemetry.PhotoPreset, error)
	SavePhotoPreset(ctx context.Context, id, ownerUID string, expectedRevision int, patch telemetry.PhotoPresetPatch) (*telemetry.PhotoPreset, error)
	SetPhotoPresetThumbnail(ctx context.Context, id, ownerUID, key string) (*telemetry.PhotoPreset, error)
	DeletePhotoPreset(ctx context.Context, id, ownerUID string) error
}

// SetPresetStore attaches the preset store (nil = the preset routes answer 503).
func (h *PhotoHandler) SetPresetStore(s PhotoPresetStore) {
	if h != nil {
		h.presets = s
	}
}

const (
	photoPresetNameMax    = 60
	photoPresetThumbMax   = 64 * 1024
	photoPresetThumbPx    = 256 // generous ceiling on the 96 px thumbnail
	photoPresetCategories = "color bw film fix custom"
)

func photoPresetCategoryOK(c string) bool {
	for _, k := range strings.Fields(photoPresetCategories) {
		if c == k {
			return true
		}
	}
	return false
}

func (h *PhotoHandler) presetsReady(c *gin.Context) bool {
	if h == nil || h.presets == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Aperture presets require a database, which is currently unavailable"})
		return false
	}
	if h.cfg != nil && !h.cfg.PhotoEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "photo_disabled", "message": "Aperture is switched off on this deployment"})
		return false
	}
	return true
}

func requirePhotoPresetUser(c *gin.Context) (string, bool) {
	userID, _ := callerIdentity(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "sign_in_required", "message": "Sign in to save Aperture presets to your account"})
		return "", false
	}
	return userID, true
}

// presetOwnerID is the identity gate plus a well-formed uuid check for :id.
func (h *PhotoHandler) presetOwnerID(c *gin.Context) (userID, id string, ok bool) {
	if !h.presetsReady(c) {
		return "", "", false
	}
	userID, ok = requirePhotoPresetUser(c)
	if !ok {
		return "", "", false
	}
	id = strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Preset not found"})
		return "", "", false
	}
	return userID, id, true
}

func denyPhotoPreset(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"error": "Preset not found"}) }

// presetThumbRel is the thumbnail path relative to PHOTO_DIR.
func presetThumbRel(ownerID, id string) string { return "presets/" + ownerID + "/" + id + ".png" }

type photoPresetBody struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Category     *string         `json:"category"`
	Revision     int             `json:"revision"`
	HasThumbnail bool            `json:"hasThumbnail"`
	ThumbnailURL string          `json:"thumbnailUrl,omitempty"`
	CreatedAt    string          `json:"createdAt"`
	UpdatedAt    string          `json:"updatedAt"`
	Layers       json.RawMessage `json:"layers,omitempty"`
}

func presetBody(p *telemetry.PhotoPreset, withLayers bool) photoPresetBody {
	b := photoPresetBody{
		ID: p.ID, Name: p.Name, Revision: p.Revision, HasThumbnail: p.ThumbnailKey != "",
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if p.Category != "" {
		cat := p.Category
		b.Category = &cat
	}
	if b.HasThumbnail {
		b.ThumbnailURL = "/api/photo/presets/" + p.ID + "/thumbnail"
	}
	if withLayers && len(p.Layers) > 0 {
		b.Layers = json.RawMessage(p.Layers)
	}
	return b
}

// sanitizePresetLayers runs the preset layer rules and answers the 4xx itself.
func (h *PhotoHandler) sanitizePresetLayers(c *gin.Context, raw []byte) ([]byte, bool) {
	l := photo.PresetLimits(photo.Limits{})
	out, err := photo.SanitizeLayers(raw, l)
	if err == nil && string(out) == "[]" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "photo_preset_invalid", "code": photo.CodeInvalidDoc, "message": "A preset needs at least one layer"})
		return nil, false
	}
	if err != nil {
		var ve *photo.ValidationError
		if errors.As(err, &ve) {
			status := http.StatusUnprocessableEntity
			if ve.Code == photo.CodeTooLarge {
				status = http.StatusRequestEntityTooLarge
			}
			c.JSON(status, gin.H{"error": "photo_preset_invalid", "code": ve.Code, "path": ve.Path, "message": ve.Message})
			return nil, false
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "photo_preset_invalid"})
		return nil, false
	}
	return out, true
}

func clampPresetName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > photoPresetNameMax {
		return "", false
	}
	return name, true
}

func readPresetBody(c *gin.Context) ([]byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, photo.PresetMaxBytes+64*1024)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_preset_too_large", "maxBytes": photo.PresetMaxBytes})
			return nil, false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return nil, false
	}
	return raw, true
}

// ListPresets handles GET /photo/presets?full=1&limit=&offset=.
//
//	200 {"presets":[{id,name,category,revision,hasThumbnail,thumbnailUrl,createdAt,updatedAt,layers?}],"limit":N,"used":N}
//
// limit is the caller's tier cap (-1 unlimited); used the number of live presets.
func (h *PhotoHandler) ListPresets(c *gin.Context) {
	if !h.presetsReady(c) {
		return
	}
	userID, ok := requirePhotoPresetUser(c)
	if !ok {
		return
	}
	limit, offset := photoListMax, 0
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 && v < limit {
		limit = v
	}
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v > 0 {
		offset = v
	}
	full := c.Query("full") == "1" || c.Query("full") == "true"
	rows, err := h.presets.ListPhotoPresets(c.Request.Context(), userID, full, limit, offset)
	if err != nil {
		h.logger.Warn("photo: list presets failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Presets are temporarily unavailable"})
		return
	}
	out := make([]photoPresetBody, 0, len(rows))
	for i := range rows {
		out = append(out, presetBody(&rows[i], full))
	}
	capLimit := tiers.Unlimited
	if res, resolved := middleware.ResolveTier(c); resolved {
		capLimit = res.Limits.PhotoPresets
	}
	c.JSON(http.StatusOK, gin.H{"presets": out, "limit": capLimit, "used": len(rows) + offset})
}

type photoPresetCreateRequest struct {
	Name     string          `json:"name"`
	Category *string         `json:"category"`
	Layers   json.RawMessage `json:"layers"`
}

// CreatePreset handles POST /photo/presets.
//
//	201 the preset (with layers)
//	401 sign_in_required | 403 {"error":"photo_presets_limit","limit","used","upgradePath"}
//	413 photo_preset_too_large | 422 photo_preset_invalid
func (h *PhotoHandler) CreatePreset(c *gin.Context) {
	if !h.presetsReady(c) {
		return
	}
	userID, ok := requirePhotoPresetUser(c)
	if !ok {
		return
	}
	raw, ok := readPresetBody(c)
	if !ok {
		return
	}
	var req photoPresetCreateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	name, ok := clampPresetName(req.Name)
	if !ok {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "photo_preset_invalid", "code": photo.CodeInvalidDoc, "message": "name must be 1 to 60 characters"})
		return
	}
	category := ""
	if req.Category != nil && *req.Category != "" {
		if !photoPresetCategoryOK(*req.Category) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "photo_preset_invalid", "code": photo.CodeInvalidDoc, "message": "category must be color, bw, film, fix or custom"})
			return
		}
		category = *req.Category
	}
	layers, ok := h.sanitizePresetLayers(c, req.Layers)
	if !ok {
		return
	}
	limit, tier := tiers.Unlimited, tiers.TierFree
	if res, resolved := middleware.ResolveTier(c); resolved {
		limit, tier = res.Limits.PhotoPresets, res.Tier
	}
	id := uuid.NewString()
	p, err := h.presets.CreatePhotoPreset(c.Request.Context(), telemetry.PhotoPresetInsert{
		ID: id, OwnerUserID: userID, Name: name, Category: category, Layers: layers,
	}, limit)
	var capErr *telemetry.PhotoPresetLimitError
	switch {
	case errors.As(err, &capErr):
		c.JSON(http.StatusForbidden, gin.H{
			"error": "photo_presets_limit", "message": "You have reached the saved Aperture preset limit for your plan.",
			"limit": capErr.Limit, "used": capErr.Used, "upgradePath": vectorUpgradePath(tier),
		})
		return
	case err != nil:
		h.logger.Warn("photo: create preset failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to save preset"})
		return
	}
	// Presets are settings: the ledger row exists for the delete and account-delete
	// paths only, so it never comes due by time.
	retention.Default().Register(detachedCtx(c), retention.RegisterInput{
		Kind: retention.KindPhotoPreset, Locator: p.ID, ProjectID: p.ID, Owner: retentionOwnerFor(c), KeepWithAccount: true,
	})
	c.JSON(http.StatusCreated, presetBody(p, true))
}

// GetPreset handles GET /photo/presets/:id (with layers).
func (h *PhotoHandler) GetPreset(c *gin.Context) {
	userID, id, ok := h.presetOwnerID(c)
	if !ok {
		return
	}
	p, err := h.presets.GetPhotoPreset(c.Request.Context(), id, userID)
	if errors.Is(err, telemetry.ErrPhotoPresetNotFound) {
		denyPhotoPreset(c)
		return
	}
	if err != nil {
		h.logger.Warn("photo: get preset failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Presets are temporarily unavailable"})
		return
	}
	c.JSON(http.StatusOK, presetBody(p, true))
}

type photoPresetSaveRequest struct {
	Name     *string         `json:"name"`
	Category json.RawMessage `json:"category"`
	Layers   json.RawMessage `json:"layers"`
}

// SavePreset handles PUT /photo/presets/:id.
//
//	header If-Match: <revision>   required (quotes and W/ tolerated)
//	body   {"name"?, "category"? (null clears), "layers"?}
//	200    the preset with revision+1 | 409 revision_conflict | 428 if_match_required
func (h *PhotoHandler) SavePreset(c *gin.Context) {
	userID, id, ok := h.presetOwnerID(c)
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
	raw, ok := readPresetBody(c)
	if !ok {
		return
	}
	var req photoPresetSaveRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	var patch telemetry.PhotoPresetPatch
	if req.Name != nil {
		n, ok := clampPresetName(*req.Name)
		if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "photo_preset_invalid", "code": photo.CodeInvalidDoc, "message": "name must be 1 to 60 characters"})
			return
		}
		patch.Name = &n
	}
	if len(req.Category) > 0 {
		if string(req.Category) == "null" {
			patch.ClearCategory = true
		} else {
			var cat string
			if err := json.Unmarshal(req.Category, &cat); err != nil || !photoPresetCategoryOK(cat) {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "photo_preset_invalid", "code": photo.CodeInvalidDoc, "message": "category must be color, bw, film, fix or custom"})
				return
			}
			patch.Category = &cat
		}
	}
	if len(req.Layers) > 0 && string(req.Layers) != "null" {
		layers, ok := h.sanitizePresetLayers(c, req.Layers)
		if !ok {
			return
		}
		patch.Layers = layers
	}
	if patch.Name == nil && patch.Category == nil && !patch.ClearCategory && patch.Layers == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nothing to save: send name, category or layers"})
		return
	}
	p, err := h.presets.SavePhotoPreset(c.Request.Context(), id, userID, expected, patch)
	var conflict *telemetry.PhotoPresetRevisionConflictError
	switch {
	case errors.As(err, &conflict):
		c.JSON(http.StatusConflict, gin.H{"error": "revision_conflict", "revision": conflict.Revision})
		return
	case errors.Is(err, telemetry.ErrPhotoPresetNotFound):
		denyPhotoPreset(c)
		return
	case err != nil:
		h.logger.Warn("photo: save preset failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to save preset"})
		return
	}
	c.JSON(http.StatusOK, presetBody(p, true))
}

// DeletePreset handles DELETE /photo/presets/:id.
//
//	200 {"deleted":{"objects":N,"bytes":N},"pending":N}
func (h *PhotoHandler) DeletePreset(c *gin.Context) {
	userID, id, ok := h.presetOwnerID(c)
	if !ok {
		return
	}
	if h.expirer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	// Ownership first: another user's id is the same 404 a missing preset gets and
	// never reaches the ledger.
	if _, err := h.presets.GetPhotoPreset(ctx, id, userID); errors.Is(err, telemetry.ErrPhotoPresetNotFound) {
		denyPhotoPreset(c)
		return
	} else if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Presets are temporarily unavailable"})
		return
	}
	if err := h.presets.DeletePhotoPreset(ctx, id, userID); err != nil {
		h.logger.Error("photo: soft delete preset failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	res, err := h.expirer.ExpireNow(ctx, ledgerFilterForProject(id))
	if err != nil && !errors.Is(err, retention.ErrNoExpire) {
		h.logger.Error("photo: delete preset failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	if h.onDelete != nil {
		h.onDelete("preset")
	}
	c.JSON(http.StatusOK, newDeletedBody(res))
}

// pngSize reads the IHDR of a PNG: ok only for a well-formed signature + IHDR.
func pngSize(b []byte) (w, hgt int, ok bool) {
	sig := []byte("\x89PNG\r\n\x1a\n")
	if len(b) < 33 || !bytes.HasPrefix(b, sig) || string(b[12:16]) != "IHDR" {
		return 0, 0, false
	}
	return int(binary.BigEndian.Uint32(b[16:20])), int(binary.BigEndian.Uint32(b[20:24])), true
}

// UploadPresetThumbnail handles POST /photo/presets/:id/thumbnail (multipart
// `file`, a PNG of at most 64 KB and 256 px on the long side).
//
//	200 the preset (hasThumbnail true) | 413 photo_preset_thumbnail_too_large | 415 photo_preset_thumbnail_invalid
func (h *PhotoHandler) UploadPresetThumbnail(c *gin.Context) {
	userID, id, ok := h.presetOwnerID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, photoPresetThumbMax+16*1024)
	if err := c.Request.ParseMultipartForm(photoPresetThumbMax + 16*1024); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_preset_thumbnail_too_large", "maxBytes": photoPresetThumbMax})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid multipart request"})
		return
	}
	defer func() {
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
	}()
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A multipart `file` field is required"})
		return
	}
	if fh.Size > photoPresetThumbMax {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_preset_thumbnail_too_large", "maxBytes": photoPresetThumbMax})
		return
	}
	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid multipart request"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, photoPresetThumbMax+1))
	f.Close()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid multipart request"})
		return
	}
	if len(data) > photoPresetThumbMax {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_preset_thumbnail_too_large", "maxBytes": photoPresetThumbMax})
		return
	}
	w, hgt, isPNG := pngSize(data)
	if !isPNG || w < 1 || hgt < 1 || w > photoPresetThumbPx || hgt > photoPresetThumbPx {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "photo_preset_thumbnail_invalid", "message": "The thumbnail must be a PNG of at most 256 px"})
		return
	}
	if _, err := h.presets.GetPhotoPreset(c.Request.Context(), id, userID); errors.Is(err, telemetry.ErrPhotoPresetNotFound) {
		denyPhotoPreset(c)
		return
	} else if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Presets are temporarily unavailable"})
		return
	}
	rel := presetThumbRel(userID, id)
	path := retention.PhotoPresetThumbPath(h.photoRoot(), userID, id)
	if path == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Preset thumbnails are unavailable"})
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("photo: preset thumbnail dir failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Preset thumbnails are unavailable"})
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Preset thumbnails are unavailable"})
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Preset thumbnails are unavailable"})
		return
	}
	// The ledger row (created with the preset) learns the file size; a retry only
	// ever grows it.
	retention.Default().Register(detachedCtx(c), retention.RegisterInput{
		Kind: retention.KindPhotoPreset, Locator: id, ProjectID: id, Owner: retentionOwnerFor(c), SizeBytes: int64(len(data)), KeepWithAccount: true,
	})
	if _, err := h.presets.SetPhotoPresetThumbnail(c.Request.Context(), id, userID, rel); err != nil {
		if errors.Is(err, telemetry.ErrPhotoPresetNotFound) {
			_ = os.Remove(path)
			denyPhotoPreset(c)
			return
		}
		h.logger.Warn("photo: recording preset thumbnail failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to save thumbnail"})
		return
	}
	p, err := h.presets.GetPhotoPreset(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Presets are temporarily unavailable"})
		return
	}
	c.JSON(http.StatusOK, presetBody(p, false))
}

// ServePresetThumbnail handles GET /photo/presets/:id/thumbnail.
func (h *PhotoHandler) ServePresetThumbnail(c *gin.Context) {
	userID, id, ok := h.presetOwnerID(c)
	if !ok {
		return
	}
	p, err := h.presets.GetPhotoPreset(c.Request.Context(), id, userID)
	if errors.Is(err, telemetry.ErrPhotoPresetNotFound) {
		denyPhotoPreset(c)
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Presets are temporarily unavailable"})
		return
	}
	path := retention.PhotoPresetThumbPath(h.photoRoot(), userID, id)
	if p.ThumbnailKey == "" || path == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	}
	file, err := os.Open(path)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	}
	defer file.Close()
	serveResultFile(c, file, info, "image/png", id+".png", true, "private, max-age=300")
}

// IsPhotoPresetWritePath matches the two POSTs the photo_presets rate bucket
// guards: POST /api/photo/presets and POST /api/photo/presets/<id>/thumbnail.
func IsPhotoPresetWritePath(path string) bool {
	if path == "/api/photo/presets" {
		return true
	}
	rest, ok := strings.CutPrefix(path, "/api/photo/presets/")
	if !ok {
		return false
	}
	id, tail, found := strings.Cut(rest, "/")
	return found && id != "" && tail == "thumbnail"
}
