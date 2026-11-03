package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/photo"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// Aperture (code name `photo`, MM95, ADR-0017) endpoints under /api/photo.
// "Aperture" is the user-facing brand; every identifier here stays `photo`.
//
//	POST   /photo/projects                 create: multipart file (+ name, doc); ingest
//	GET    /photo/projects                 list (no doc), newest first, limit/offset
//	GET    /photo/projects/:id             {project, doc}
//	PUT    /photo/projects/:id             save; If-Match: <revision> (428 / 409)
//	DELETE /photo/projects/:id             soft delete + ledger expire now
//	GET    /photo/projects/:id/original    inline bytes (real MIME, Range, private cache)
//	GET    /photo/projects/:id/proxy       1920 px proxy
//	GET    /photo/projects/:id/thumb       320 px thumbnail
//	GET    /photo/projects/:id/files/*rel  masks/<id>.png and bases/<hash>.png only
//	GET    /photo/capabilities             what this caller's tier can do
//	/photo/presets...                      MM103 user presets (photo_presets.go)
//
// Every project route needs an account: anonymous Aperture projects live only in
// the browser (IndexedDB) and use MM96's stateless routes. Ownership is strict
// and DB-backed (requirePhotoOwner): the owner, or an operator; a stranger gets
// the same 404 a missing project gets. The MM96 routes (export, batch, heal,
// subject mask, and their stateless twins) live in photo_render.go.

// PhotoStore is the slice of *telemetry.Store the Aperture routes use.
type PhotoStore interface {
	CreatePhotoProject(ctx context.Context, in telemetry.PhotoProjectInsert, limit int) (*telemetry.PhotoProject, error)
	ListPhotoProjects(ctx context.Context, ownerUID string, limit, offset int) ([]telemetry.PhotoProject, error)
	GetPhotoProject(ctx context.Context, id string) (*telemetry.PhotoProject, error)
	PhotoProjectOwner(ctx context.Context, id string) (ownerUID string, found bool, err error)
	SavePhotoProject(ctx context.Context, id, ownerUID string, expectedRevision int, name *string, doc []byte) (*telemetry.PhotoProject, error)
	CompletePhotoIngest(ctx context.Context, id string, width, height int, hasAlpha bool, doc []byte) (*telemetry.PhotoProject, error)
	SetPhotoProjectStatus(ctx context.Context, id, status, errMsg string) (*telemetry.PhotoProject, error)
	DeletePhotoProject(ctx context.Context, id, ownerUID string) error
	UpsertPhotoDerivative(ctx context.Context, d telemetry.PhotoDerivative) (*telemetry.PhotoDerivative, error)
	FindPhotoDerivative(ctx context.Context, projectID, kind, hash string) (*telemetry.PhotoDerivative, error)
}

// PhotoHandler serves /api/photo/*.
type PhotoHandler struct {
	store    PhotoStore
	presets  PhotoPresetStore // MM103 (photo_presets.go)
	cfg      *config.Config
	ingest   services.PhotoIngester
	expirer  Expirer
	logger   *slog.Logger
	onDelete func(scope string)
	pipe     *PhotoPipeline
	capsOnce sync.Once
	caps     PhotoCaps
	// counted is the server_photo_export_counted seam (see emitCounted).
	counted func(c *gin.Context, format, tier, mpBucket string, batch bool)
}

const (
	photoListDefault = 50
	photoListMax     = 200
	photoNameMax     = 200
	// photoFormMemory is the multipart in-memory budget; larger files spill to disk.
	photoFormMemory = 32 << 20
	photoCacheCtl   = "private, max-age=3600"
)

// photoAcceptedExts is the accepted source set: the /upload image set plus
// HEIC/HEIF/TIFF/BMP/AVIF/WebP/GIF (first frame). SVG is refused.
var photoAcceptedExts = map[string]string{
	"jpg": "jpg", "jpeg": "jpg", "png": "png", "webp": "webp", "avif": "avif",
	"tif": "tiff", "tiff": "tiff", "bmp": "bmp", "gif": "gif", "heic": "heic", "heif": "heif",
}

var photoExtMime = map[string]string{
	"jpg": "image/jpeg", "png": "image/png", "webp": "image/webp", "avif": "image/avif",
	"tiff": "image/tiff", "bmp": "image/bmp", "gif": "image/gif", "heic": "image/heic", "heif": "image/heif",
}

// photoFileRe matches the only relpaths /files/*relpath serves: two safe
// segments, masks/<id>.png or bases/<hash>.png.
var photoFileRe = regexp.MustCompile(`^(masks/[A-Za-z0-9_-]{1,40}\.png|bases/[A-Za-z0-9_-]{1,64}\.png|bases/[A-Za-z0-9_-]{1,64}\.proxy\.(jpg|png))$`)

// photoCapabilityFormats are the export formats the editor will offer (MM96).
var photoCapabilityFormats = []string{"jpg", "png", "webp", "avif", "tiff"}

// NewPhotoHandler builds the handler. store may be nil (no database): project
// routes then answer 503. ingest may be nil: create answers 503.
func NewPhotoHandler(store PhotoStore, cfg *config.Config, ingest services.PhotoIngester, logger *slog.Logger) *PhotoHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &PhotoHandler{store: store, cfg: cfg, ingest: ingest, logger: logger}
}

// SetDeletion attaches the inline sweeper used by DELETE (nil = 503).
func (h *PhotoHandler) SetDeletion(e Expirer) {
	if h != nil {
		h.expirer = e
	}
}

// SetDeletionMetric attaches the data_deleted counter hook.
func (h *PhotoHandler) SetDeletionMetric(f func(scope string)) {
	if h != nil {
		h.onDelete = f
	}
}

// RegisterPhotoRoutes mounts the Aperture routes on r (the /api group).
func RegisterPhotoRoutes(r gin.IRouter, h *PhotoHandler) {
	p := r.Group("/photo")
	p.GET("/capabilities", h.Capabilities)
	// MM103: the signed-in user's presets (photo_presets.go).
	p.GET("/presets", h.ListPresets)
	p.POST("/presets", h.CreatePreset)
	p.GET("/presets/:id", h.GetPreset)
	p.PUT("/presets/:id", h.SavePreset)
	p.DELETE("/presets/:id", h.DeletePreset)
	p.POST("/presets/:id/thumbnail", h.UploadPresetThumbnail)
	p.GET("/presets/:id/thumbnail", h.ServePresetThumbnail)
	p.GET("/projects", h.ListProjects)
	p.POST("/projects", h.CreateProject)
	p.GET("/projects/:id", h.GetProject)
	p.PUT("/projects/:id", h.SaveProject)
	p.DELETE("/projects/:id", h.DeleteProject)
	p.GET("/projects/:id/original", h.ServeOriginal)
	p.GET("/projects/:id/proxy", h.ServeProxy)
	p.GET("/projects/:id/thumb", h.ServeThumb)
	p.GET("/projects/:id/files/*relpath", h.ServeFile)

	// MM96: export, batch, subject mask, heal and their stateless twins
	// (photo_render.go).
	p.POST("/projects/:id/masks/subject", h.ProjectSubjectMask)
	p.POST("/projects/:id/heal", h.ProjectHeal)
	p.POST("/projects/:id/export", h.ProjectExport)
	p.POST("/batch", h.Batch)
	p.POST("/export", h.StatelessExport)
	p.POST("/masks/subject", h.StatelessSubjectMask)
	p.POST("/heal", h.StatelessHeal)
}

// --- helpers -------------------------------------------------------------

func (h *PhotoHandler) ready(c *gin.Context) bool {
	if h == nil || h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Aperture requires a database, which is currently unavailable"})
		return false
	}
	if h.cfg != nil && !h.cfg.PhotoEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "photo_disabled", "message": "Aperture is switched off on this deployment"})
		return false
	}
	return true
}

func (h *PhotoHandler) maxDocBytes() int {
	if h.cfg != nil && h.cfg.PhotoMaxDocBytes > 0 {
		return h.cfg.PhotoMaxDocBytes
	}
	return photo.DefaultLimits().MaxDocBytes
}

func (h *PhotoHandler) maxMegapixels() int {
	if h.cfg != nil && h.cfg.PhotoMaxMegapixels > 0 {
		return h.cfg.PhotoMaxMegapixels
	}
	return 120
}

func (h *PhotoHandler) photoRoot() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.PhotoDir) != "" {
		return h.cfg.PhotoDir
	}
	return "uploads/photo"
}

// projectDir is <PHOTO_DIR>/<id>; id is always a validated uuid.
func (h *PhotoHandler) projectDir(id string) string { return filepath.Join(h.photoRoot(), id) }

// requirePhotoUser is the identity gate: a signed-in caller with a resolved
// account id. Anonymous Aperture projects are browser-only, so there is no
// session fallback. 401 sign_in_required otherwise.
func requirePhotoUser(c *gin.Context) (string, bool) {
	userID, _ := callerIdentity(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "sign_in_required", "message": "Sign in to save Aperture projects"})
		return "", false
	}
	return userID, true
}

func isPhotoOperator(c *gin.Context) bool {
	res, ok := middleware.ResolveTier(c)
	return ok && res.Tier == tiers.TierOperator
}

func denyPhotoProject(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
}

// requirePhotoOwner is the strict, database-backed owner check every :id route
// runs: signed in (401), a well-formed uuid, a live row owned by the caller or
// the caller is an operator. Every miss is the same 404 (no probing); a lookup
// error fails closed (503).
func requirePhotoOwner(c *gin.Context, store PhotoStore, logger *slog.Logger, id string) (userID string, ok bool) {
	userID, ok = requirePhotoUser(c)
	if !ok {
		return "", false
	}
	if _, err := uuid.Parse(id); err != nil {
		denyPhotoProject(c)
		return "", false
	}
	ownerUID, found, err := store.PhotoProjectOwner(c.Request.Context(), id)
	if err != nil {
		logger.Warn("photo: owner lookup failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Project lookup is temporarily unavailable"})
		return "", false
	}
	if !found || strings.TrimSpace(ownerUID) == "" || (ownerUID != userID && !isPhotoOperator(c)) {
		denyPhotoProject(c)
		return "", false
	}
	return userID, true
}

type photoProjectBody struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Revision    int    `json:"revision"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	HasAlpha    bool   `json:"hasAlpha"`
	Bytes       int64  `json:"originalBytes"`
	OriginalExt string `json:"originalExt"`
	OriginalURL string `json:"originalUrl"`
	ProxyURL    string `json:"proxyUrl"`
	ThumbURL    string `json:"thumbUrl"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func photoBody(p *telemetry.PhotoProject) photoProjectBody {
	base := "/api/photo/projects/" + p.ID
	return photoProjectBody{
		ID: p.ID, Name: p.Name, Revision: p.Revision, Status: p.Status, Error: p.Error,
		Width: p.OriginalWidth, Height: p.OriginalHeight, HasAlpha: p.HasAlpha, Bytes: p.OriginalBytes,
		OriginalExt: p.OriginalExt, OriginalURL: base + "/original", ProxyURL: base + "/proxy", ThumbURL: base + "/thumb",
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// photoDocResponse is {project, doc}; doc is the stored canonical JSON.
func photoDocResponse(p *telemetry.PhotoProject) gin.H {
	out := gin.H{"project": photoBody(p)}
	if len(p.Doc) > 0 {
		out["doc"] = json.RawMessage(p.Doc)
	}
	return out
}

func clampPhotoName(name, fallback string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(fallback)
	}
	if name == "" {
		return "Untitled photo"
	}
	if len(name) > photoNameMax {
		cut := photoNameMax
		for cut > 0 && !utf8Start(name[cut]) {
			cut--
		}
		return name[:cut]
	}
	return name
}

func (h *PhotoHandler) sanitizeDoc(c *gin.Context, raw []byte) ([]byte, bool) {
	l := photo.DefaultLimits()
	l.MaxDocBytes = h.maxDocBytes()
	out, err := photo.Sanitize(raw, l)
	if err != nil {
		var ve *photo.ValidationError
		if errors.As(err, &ve) {
			status := http.StatusUnprocessableEntity
			if ve.Code == photo.CodeTooLarge {
				status = http.StatusRequestEntityTooLarge
			}
			c.JSON(status, gin.H{"error": "photo_doc_invalid", "code": ve.Code, "path": ve.Path, "message": ve.Message})
			return nil, false
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "photo_doc_invalid"})
		return nil, false
	}
	return out, true
}

// withSource returns doc (canonical) with source.width/height set to the
// ingested original's EXIF-upright size.
func (h *PhotoHandler) withSource(doc []byte, w, hgt int) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	src, _ := m["source"].(map[string]any)
	if src == nil {
		src = map[string]any{}
	}
	src["width"], src["height"] = float64(w), float64(hgt)
	m["source"] = src
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	l := photo.DefaultLimits()
	l.MaxDocBytes = h.maxDocBytes()
	return photo.Sanitize(raw, l)
}

// --- capabilities ---------------------------------------------------------

// Capabilities handles GET /photo/capabilities (anyone).
//
//	200 {enabled, render, formats, maxMegapixels, photoProjects, photoBatch, photoPresets, aiMasks, heal, tier}
//
// render, aiMasks and heal report whether the renderer venv, rembg and LaMa are
// installed (checked once, cached; MM96).
func (h *PhotoHandler) Capabilities(c *gin.Context) {
	tier := tiers.TierAnonymous
	limits := tiers.Limits{PhotoMaxMegapixels: 12}
	if res, ok := middleware.ResolveTier(c); ok {
		tier, limits = res.Tier, res.Limits
	}
	enabled := h != nil && (h.cfg == nil || h.cfg.PhotoEnabled)
	caps := h.capabilities()
	c.JSON(http.StatusOK, gin.H{
		"enabled":       enabled,
		"render":        caps.Render,
		"formats":       photoCapabilityFormats,
		"maxMegapixels": limits.PhotoMaxMegapixels,
		"photoProjects": limits.PhotoProjects,
		"photoBatch":    limits.PhotoBatch,
		"photoPresets":  limits.PhotoPresets,
		"aiMasks":       caps.AIMasks,
		"heal":          caps.Heal,
		"tier":          string(tier),
	})
}

// --- projects ---------------------------------------------------------------

// ListProjects handles GET /photo/projects?limit=&offset=. Never selects the doc.
//
//	200 {"projects":[{id,name,revision,status,width,height,thumbUrl,updatedAt,...}]}
func (h *PhotoHandler) ListProjects(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	userID, ok := requirePhotoUser(c)
	if !ok {
		return
	}
	limit, offset := photoListDefault, 0
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > photoListMax {
		limit = photoListMax
	}
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v > 0 {
		offset = v
	}
	projects, err := h.store.ListPhotoProjects(c.Request.Context(), userID, limit, offset)
	if err != nil {
		h.logger.Warn("photo: list projects failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Projects are temporarily unavailable"})
		return
	}
	out := make([]photoProjectBody, 0, len(projects))
	for i := range projects {
		out = append(out, photoBody(&projects[i]))
	}
	c.JSON(http.StatusOK, gin.H{"projects": out})
}

// CreateProject handles POST /photo/projects (multipart: file, name?, doc?).
//
//	201  {project, doc} with status ready
//	401  sign_in_required
//	403  {"error":"photo_projects_limit","limit","used","upgradePath"}
//	413  photo_file_too_large {maxBytes} | photo_too_many_pixels {maxMegapixels}
//	415  photo_unsupported_source (SVG and non-image types)
//	422  photo_doc_invalid
//	502  photo_ingest_failed (the row is kept with status failed)
func (h *PhotoHandler) CreateProject(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	userID, ok := requirePhotoUser(c)
	if !ok {
		return
	}
	if h.ingest == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Photo ingest is not configured"})
		return
	}
	limit, tier := tiers.Unlimited, tiers.TierFree
	maxBytes := int64(0)
	if h.cfg != nil {
		maxBytes = h.cfg.MaxFileSize
	}
	if res, resolved := middleware.ResolveTier(c); resolved {
		limit, tier = res.Limits.PhotoProjects, res.Tier
		if res.Limits.MaxFileBytes > 0 {
			maxBytes = res.Limits.MaxFileBytes
		}
	}
	if maxBytes > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+1<<20)
	}
	if err := c.Request.ParseMultipartForm(photoFormMemory); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_file_too_large", "maxBytes": maxBytes})
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
	if maxBytes > 0 && fh.Size > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_file_too_large", "maxBytes": maxBytes})
		return
	}
	ext, accepted := photoAcceptedExts[strings.ToLower(strings.TrimPrefix(filepath.Ext(fh.Filename), "."))]
	if !accepted || photoLooksLikeSVG(fh) {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{
			"error": "photo_unsupported_source", "message": "Aperture opens JPG, PNG, WebP, AVIF, TIFF, BMP, GIF, HEIC and HEIF photos",
		})
		return
	}
	var docRaw []byte
	if d := strings.TrimSpace(c.PostForm("doc")); d != "" {
		docRaw = []byte(d)
	} else {
		docRaw = []byte(`{"version":1,"source":{"width":1,"height":1}}`)
	}
	doc, ok := h.sanitizeDoc(c, docRaw)
	if !ok {
		return
	}
	name := clampPhotoName(c.PostForm("name"), strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename)))

	id := uuid.NewString()
	_, err = h.store.CreatePhotoProject(c.Request.Context(), telemetry.PhotoProjectInsert{
		ID: id, OwnerUserID: userID, Name: name, Doc: doc, OriginalExt: ext, OriginalBytes: fh.Size,
		OriginalWidth: 1, OriginalHeight: 1, Status: telemetry.PhotoStatusIngesting,
	}, limit)
	var capErr *telemetry.PhotoProjectLimitError
	switch {
	case errors.As(err, &capErr):
		c.JSON(http.StatusForbidden, gin.H{
			"error": "photo_projects_limit", "message": "You have reached the saved Aperture project limit for your plan.",
			"limit": capErr.Limit, "used": capErr.Used, "upgradePath": vectorUpgradePath(tier),
		})
		return
	case err != nil:
		h.logger.Warn("photo: create project failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to create project"})
		return
	}

	// From here the project directory exists on disk: ledger it first so a crash
	// mid-ingest leaves a tracked, sweepable directory.
	dir := h.projectDir(id)
	original := filepath.Join(dir, "original."+ext)
	if err := os.MkdirAll(dir, 0o755); err == nil {
		err = c.SaveUploadedFile(fh, original)
		if err != nil {
			h.logger.Warn("photo: saving the upload failed", "error", err.Error())
		}
		retention.Default().Register(detachedCtx(c), retention.RegisterInput{
			Kind: retention.KindPhotoProject, Locator: id, Owner: retentionOwnerFor(c), ProjectID: id, SizeBytes: fh.Size,
		})
		if err != nil {
			h.failIngest(c, id, "could not store the upload")
			c.JSON(http.StatusBadGateway, gin.H{"error": "photo_ingest_failed", "projectId": id})
			return
		}
	} else {
		h.failIngest(c, id, "could not create the project directory")
		c.JSON(http.StatusBadGateway, gin.H{"error": "photo_ingest_failed", "projectId": id})
		return
	}

	res, ierr := h.ingest.Ingest(detachedCtx(c), dir, original, h.maxMegapixels())
	var tooMany *services.PhotoTooManyPixelsError
	switch {
	case errors.As(ierr, &tooMany):
		// A refusal, not a failure: the project never existed as far as the caller
		// is concerned. Drop the row (soft) and expire the ledger row + directory.
		h.discard(c, id, userID)
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_too_many_pixels", "maxMegapixels": tooMany.MaxMegapixels})
		return
	case ierr != nil:
		h.logger.Warn("photo: ingest failed", "project", id, "error", ierr.Error())
		h.failIngest(c, id, ierr.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "photo_ingest_failed", "projectId": id})
		return
	}
	final, err := h.withSource(doc, res.Width, res.Height)
	if err != nil {
		h.failIngest(c, id, "document rebuild failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "photo_ingest_failed", "projectId": id})
		return
	}
	done, err := h.store.CompletePhotoIngest(detachedCtx(c), id, res.Width, res.Height, res.HasAlpha, final)
	if err != nil {
		h.logger.Warn("photo: completing ingest failed", "project", id, "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to create project"})
		return
	}
	c.JSON(http.StatusCreated, photoDocResponse(done))
}

func (h *PhotoHandler) failIngest(c *gin.Context, id, msg string) {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if _, err := h.store.SetPhotoProjectStatus(detachedCtx(c), id, telemetry.PhotoStatusFailed, msg); err != nil {
		h.logger.Warn("photo: marking ingest failed did not stick", "project", id, "error", err.Error())
	}
}

// discard removes a project that never became usable (refused at ingest).
func (h *PhotoHandler) discard(c *gin.Context, id, userID string) {
	ctx := detachedCtx(c)
	if err := h.store.DeletePhotoProject(ctx, id, userID); err != nil {
		h.logger.Warn("photo: discarding refused project failed", "project", id, "error", err.Error())
	}
	if h.expirer != nil {
		if _, err := h.expirer.ExpireNow(ctx, ledgerFilterForProject(id)); err != nil && !errors.Is(err, retention.ErrNoExpire) {
			h.logger.Warn("photo: expiring refused project failed", "project", id, "error", err.Error())
		}
	}
	_ = os.RemoveAll(h.projectDir(id))
}

// photoLooksLikeSVG sniffs the head of the upload: an SVG renamed to .png must
// not reach ImageMagick's SVG delegate.
func photoLooksLikeSVG(fh *multipart.FileHeader) bool {
	f, err := fh.Open()
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 1024)
	n, _ := io.ReadFull(f, head)
	h := strings.ToLower(string(head[:n]))
	return strings.Contains(h, "<svg") || strings.Contains(h, "<!doctype svg")
}

// GetProject handles GET /photo/projects/:id.
func (h *PhotoHandler) GetProject(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, ok := requirePhotoOwner(c, h.store, h.logger, id); !ok {
		return
	}
	p, err := h.store.GetPhotoProject(c.Request.Context(), id)
	if errors.Is(err, telemetry.ErrPhotoNotFound) {
		denyPhotoProject(c)
		return
	}
	if err != nil {
		h.logger.Warn("photo: get project failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Project lookup is temporarily unavailable"})
		return
	}
	c.JSON(http.StatusOK, photoDocResponse(p))
}

type photoSaveRequest struct {
	Name *string         `json:"name"`
	Doc  json.RawMessage `json:"doc"`
}

// SaveProject handles PUT /photo/projects/:id.
//
//	header If-Match: <revision>   required (quotes and W/ tolerated)
//	body   {"name"?, "doc"?}
//	200    {project, doc} with revision+1
//	409    {"error":"revision_conflict","revision":n}
//	413    photo_doc_too_large | 422 photo_doc_invalid | 428 if_match_required
func (h *PhotoHandler) SaveProject(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	userID, ok := requirePhotoOwner(c, h.store, h.logger, id)
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
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(h.maxDocBytes())+64*1024)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_doc_too_large", "maxBytes": h.maxDocBytes()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	var req photoSaveRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	var name *string
	if req.Name != nil {
		n := clampPhotoName(*req.Name, "")
		name = &n
	}
	var doc []byte
	if len(req.Doc) > 0 && string(req.Doc) != "null" {
		d, ok := h.sanitizeDoc(c, req.Doc)
		if !ok {
			return
		}
		doc = d
	}
	// The stored project owner is the access check; the CAS predicates on the
	// row's own owner, which for an operator is not the caller.
	owner := userID
	if o, found, oerr := h.store.PhotoProjectOwner(c.Request.Context(), id); oerr == nil && found {
		owner = o
	}
	p, err := h.store.SavePhotoProject(c.Request.Context(), id, owner, expected, name, doc)
	var conflict *telemetry.PhotoRevisionConflictError
	switch {
	case errors.As(err, &conflict):
		c.JSON(http.StatusConflict, gin.H{"error": "revision_conflict", "revision": conflict.Revision})
		return
	case errors.Is(err, telemetry.ErrPhotoNotFound):
		denyPhotoProject(c)
		return
	case err != nil:
		h.logger.Warn("photo: save project failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to save project"})
		return
	}
	c.JSON(http.StatusOK, photoDocResponse(p))
}

// DeleteProject handles DELETE /photo/projects/:id.
//
//	200 {"deleted":{"objects":N,"bytes":N},"pending":N}
//
// The row is soft-deleted at once (it vanishes from every read), then the ledger
// row expires and the inline sweep removes the directory and the row itself.
func (h *PhotoHandler) DeleteProject(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	if h.expirer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, ok := requirePhotoOwner(c, h.store, h.logger, id); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	owner, found, _ := h.store.PhotoProjectOwner(ctx, id)
	if found {
		if err := h.store.DeletePhotoProject(ctx, id, owner); err != nil {
			h.logger.Error("photo: soft delete failed", "error", err.Error())
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
			return
		}
	}
	res, err := h.expirer.ExpireNow(ctx, ledgerFilterForProject(id))
	if err != nil && !errors.Is(err, retention.ErrNoExpire) {
		h.logger.Error("photo: delete project failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	if h.onDelete != nil {
		h.onDelete("project")
	}
	c.JSON(http.StatusOK, newDeletedBody(res))
}

// --- files ---------------------------------------------------------------

// ServeOriginal handles GET /photo/projects/:id/original.
func (h *PhotoHandler) ServeOriginal(c *gin.Context) {
	h.serveProjectFile(c, func(p *telemetry.PhotoProject) []string {
		return []string{"original." + p.OriginalExt}
	})
}

// ServeProxy handles GET /photo/projects/:id/proxy.
func (h *PhotoHandler) ServeProxy(c *gin.Context) {
	h.serveProjectFile(c, func(*telemetry.PhotoProject) []string { return []string{"proxy.jpg", "proxy.png"} })
}

// ServeThumb handles GET /photo/projects/:id/thumb.
func (h *PhotoHandler) ServeThumb(c *gin.Context) {
	h.serveProjectFile(c, func(*telemetry.PhotoProject) []string { return []string{"thumb.jpg", "thumb.png"} })
}

// ServeFile handles GET /photo/projects/:id/files/*relpath: only masks/<id>.png
// and bases/<hash>.png, anything else is a plain 404.
func (h *PhotoHandler) ServeFile(c *gin.Context) {
	rel := strings.TrimPrefix(c.Param("relpath"), "/")
	if !photoFileRe.MatchString(rel) {
		// Still run the ownership check first so a stranger learns nothing, but a
		// non-matching path is a 404 either way.
		if h.ready(c) {
			id := strings.TrimSpace(c.Param("id"))
			if _, ok := requirePhotoOwner(c, h.store, h.logger, id); !ok {
				return
			}
		}
		if !c.Writer.Written() {
			c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		}
		return
	}
	h.serveProjectFile(c, func(*telemetry.PhotoProject) []string { return []string{rel} })
}

// serveProjectFile checks ownership, then serves the first existing candidate
// inline with its real MIME, Range support and a private cache header.
func (h *PhotoHandler) serveProjectFile(c *gin.Context, candidates func(*telemetry.PhotoProject) []string) {
	if !h.ready(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, ok := requirePhotoOwner(c, h.store, h.logger, id); !ok {
		return
	}
	p, err := h.store.GetPhotoProject(c.Request.Context(), id)
	if errors.Is(err, telemetry.ErrPhotoNotFound) {
		denyPhotoProject(c)
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Project lookup is temporarily unavailable"})
		return
	}
	dir := h.projectDir(id)
	for _, rel := range candidates(p) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if !retention.IsWithin(path, dir) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		serveResultFile(c, f, info, photoMimeFor(rel), filepath.Base(rel), true, photoCacheCtl)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
}

func photoMimeFor(rel string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(rel), "."))
	if ext == "jpeg" {
		ext = "jpg"
	}
	if m, ok := photoExtMime[ext]; ok {
		return m
	}
	return "application/octet-stream"
}
