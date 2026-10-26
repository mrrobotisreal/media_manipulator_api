package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/vector"
)

// Motion (code name `vector`, MM43, ADR-0012) endpoints under /api/vector.
// "Motion" is the user-facing brand; every identifier here stays `vector`.
//
//	GET    /vector/projects                list (no document), signed-in only
//	POST   /vector/projects                create (sanitized doc, project cap)
//	GET    /vector/projects/:id            one project with its document
//	PUT    /vector/projects/:id            save; If-Match: <revision> (428 / 409)
//	DELETE /vector/projects/:id            delete row + thumbnail (ledger sweep)
//	POST   /vector/projects/:id/thumbnail  presigned PUT for the 320 px PNG
//	POST   /vector/exports                 count one export (optional auth)
//	POST   /vector/vectorize               raster -> SVG via the converter job
//
// Project routes need an account: anonymous Motion projects live only in the
// browser (IndexedDB). Ownership is strict and DB-backed (requireVectorOwner),
// and a stranger gets the same 404 a missing project gets.

// VectorStore is the slice of *telemetry.Store the Motion routes use.
type VectorStore interface {
	CreateVectorProject(ctx context.Context, in telemetry.VectorProjectInsert, limit int) (*telemetry.VectorProject, error)
	ListVectorProjects(ctx context.Context, ownerUID string, limit int) ([]telemetry.VectorProject, error)
	GetVectorProject(ctx context.Context, id string) (*telemetry.VectorProject, error)
	VectorProjectOwner(ctx context.Context, id string) (ownerUID, anonOwner string, found bool, err error)
	SaveVectorProject(ctx context.Context, in telemetry.VectorProjectUpdate) (*telemetry.VectorProject, error)
	DeleteVectorProjectRows(ctx context.Context, id string) ([]string, error)
	CountVectorExport(ctx context.Context, rec telemetry.VectorExportRecord, limit int64) (used int64, allowed bool, err error)
}

// ThumbnailPresigner signs the browser's PUT of a project thumbnail. The
// production implementation wraps s3.PresignClient; tests use a fake.
type ThumbnailPresigner interface {
	PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (url string, err error)
}

// VectorizeLauncher starts the raster -> SVG converter job for the image in
// the request's already-parsed multipart form (field `file`), through the
// existing /upload pipeline with options {format:"svg"}. It writes the HTTP
// response itself ({"jobId"} or an error) and is only called after the export
// allowance has been spent. *ConversionHandler implements it.
type VectorizeLauncher interface {
	LaunchVectorize(c *gin.Context, threshold, turdSize int)
}

// VectorHandler serves /api/vector/*.
type VectorHandler struct {
	store     VectorStore
	cfg       *config.Config
	tiers     *tiers.Registry
	presign   ThumbnailPresigner
	vectorize VectorizeLauncher
	expirer   Expirer
	logger    *slog.Logger
	now       func() time.Time
	// ipSalt keys the anonymous export counter on a hashed IP when there is no
	// session, exactly like RequireQuota.
	ipSalt   []byte
	onDelete func(scope string)
	// counted is the server_vector_export_counted seam (see emitCounted).
	counted func(c *gin.Context, format, tier string)
}

// VectorThumbnailMaxBytes caps the client-rendered 320 px PNG (proposed 512 KB).
const VectorThumbnailMaxBytes = 512 * 1024

// vectorThumbnailContentType is the only accepted thumbnail type.
const vectorThumbnailContentType = "image/png"

// vectorListLimit bounds GET /vector/projects (Premium's cap is 25).
const vectorListLimit = 200

// vectorNameMax caps a project name like Studio's clampProjectName.
const vectorNameMax = 200

// NewVectorHandler builds the handler. store may be nil (no database): every
// route then answers 503. presign and vectorize may be nil: those routes 503.
// registry is the same tier table the /api group resolves against; it is used
// only to name the lowest tier that unlocks a format.
func NewVectorHandler(store VectorStore, cfg *config.Config, registry *tiers.Registry, presign ThumbnailPresigner, vectorize VectorizeLauncher, logger *slog.Logger) *VectorHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &VectorHandler{store: store, cfg: cfg, tiers: registry, presign: presign, vectorize: vectorize, logger: logger, now: time.Now}
}

// SetDeletion attaches the inline sweeper used by DELETE (nil = 503).
func (h *VectorHandler) SetDeletion(e Expirer) {
	if h != nil {
		h.expirer = e
	}
}

// SetDeletionMetric attaches the data_deleted counter hook.
func (h *VectorHandler) SetDeletionMetric(f func(scope string)) {
	if h != nil {
		h.onDelete = f
	}
}

// SetIPHashSecret sets the salt for the hashed-IP export subject.
func (h *VectorHandler) SetIPHashSecret(salt []byte) {
	if h != nil {
		h.ipSalt = salt
	}
}

// RegisterVectorRoutes mounts the Motion routes on r (the /api group), modelled
// on RegisterStudioRoutes.
func RegisterVectorRoutes(r gin.IRouter, h *VectorHandler) {
	v := r.Group("/vector")
	v.GET("/projects", h.ListProjects)
	v.POST("/projects", h.CreateProject)
	v.GET("/projects/:id", h.GetProject)
	v.PUT("/projects/:id", h.SaveProject)
	v.DELETE("/projects/:id", h.DeleteProject)
	v.POST("/projects/:id/thumbnail", h.PresignThumbnail)
	v.POST("/exports", h.CountExport)
	v.POST("/vectorize", h.Vectorize)
}

// --- shared helpers ----------------------------------------------------------

func (h *VectorHandler) ready(c *gin.Context) bool {
	if h == nil || h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Motion requires a database, which is currently unavailable"})
		return false
	}
	return true
}

func (h *VectorHandler) maxDocBytes() int {
	if h.cfg != nil && h.cfg.VectorMaxDocBytes > 0 {
		return h.cfg.VectorMaxDocBytes
	}
	return vector.DefaultLimits().MaxDocBytes
}

// requireVectorUser is the project routes' identity gate: a signed-in caller
// with a resolved account id. Anonymous Motion projects are browser-only, so
// there is no session fallback here (unlike Studio). 401 otherwise.
func requireVectorUser(c *gin.Context) (string, bool) {
	userID, _ := callerIdentity(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return "", false
	}
	return userID, true
}

// requireVectorOwner is the Motion sibling of requireJobOwnerRec (MM24): the
// strict, database-backed owner check every :id route runs. Rules:
//
//   - the caller must be signed in (401 otherwise);
//   - the row must exist and carry owner_uid == the caller's account id;
//   - an ownerless row (no owner_uid, e.g. a future anon_owner import that was
//     never claimed) is never served, to anyone;
//   - there is no operator bypass;
//   - every miss is the same 404 a missing project gets (no probing);
//   - a lookup error fails CLOSED (503).
func requireVectorOwner(c *gin.Context, store VectorStore, logger *slog.Logger, id string) (userID string, ok bool) {
	userID, ok = requireVectorUser(c)
	if !ok {
		return "", false
	}
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 64 {
		denyVectorProject(c)
		return "", false
	}
	ownerUID, _, found, err := store.VectorProjectOwner(c.Request.Context(), id)
	if err != nil {
		logger.Warn("vector: owner lookup failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Project lookup is temporarily unavailable"})
		return "", false
	}
	if !found || strings.TrimSpace(ownerUID) == "" || ownerUID != userID {
		denyVectorProject(c)
		return "", false
	}
	return userID, true
}

func denyVectorProject(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
}

// vectorProjectBody is the JSON shape of one project. Document is omitted on
// list responses.
type vectorProjectBody struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Revision      int             `json:"revision"`
	ThumbnailKey  string          `json:"thumbnailKey,omitempty"`
	DocumentBytes int             `json:"documentBytes"`
	CreatedAt     string          `json:"createdAt"`
	UpdatedAt     string          `json:"updatedAt"`
	Document      json.RawMessage `json:"document,omitempty"`
}

func vectorBody(p *telemetry.VectorProject, withDoc bool) vectorProjectBody {
	b := vectorProjectBody{
		ID: p.ID, Name: p.Name, Revision: p.Revision, ThumbnailKey: p.ThumbnailKey,
		DocumentBytes: p.DocumentBytes,
		CreatedAt:     p.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:     p.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if withDoc && len(p.Document) > 0 {
		b.Document = json.RawMessage(p.Document)
	}
	return b
}

// vectorRevisionConflictBody is the 409 payload. The keys are copied from
// studioRevisionConflictBody so the client's save-state machine is shared.
func vectorRevisionConflictBody(e *telemetry.VectorRevisionConflictError) gin.H {
	return gin.H{
		"error":           "revision_conflict",
		"currentRevision": e.CurrentRevision,
		"updatedAt":       e.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func clampVectorName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Untitled animation"
	}
	if len(name) > vectorNameMax {
		// Trim on a rune boundary.
		cut := vectorNameMax
		for cut > 0 && !utf8Start(name[cut]) {
			cut--
		}
		return name[:cut]
	}
	return name
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// readVectorBody reads a JSON body capped at the document limit plus envelope
// slack. 413 when larger.
func (h *VectorHandler) readVectorBody(c *gin.Context) ([]byte, bool) {
	limit := int64(h.maxDocBytes()) + 64*1024
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "vector_doc_too_large", "maxBytes": h.maxDocBytes()})
			return nil, false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return nil, false
	}
	return raw, true
}

// sanitizeDoc runs the VectorDoc sanitizer and writes the 422 on failure.
func (h *VectorHandler) sanitizeDoc(c *gin.Context, raw json.RawMessage) ([]byte, bool) {
	l := vector.DefaultLimits()
	l.MaxDocBytes = h.maxDocBytes()
	doc, err := vector.SanitizeWith(raw, l)
	if err != nil {
		var ve *vector.ValidationError
		if errors.As(err, &ve) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error": "vector_doc_invalid", "code": ve.Code, "path": ve.Path, "message": ve.Message,
			})
			return nil, false
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "vector_doc_invalid"})
		return nil, false
	}
	return doc.Canonical(), true
}

// vectorUpgradePath is what to offer a caller refused on Motion grounds, per
// the single ladder (ADR-0011): sign up, then Premium, then Pro; nothing above
// Pro. MM49 finalises routing; this keeps the existing vocabulary.
func vectorUpgradePath(t tiers.Tier) string {
	switch t {
	case tiers.TierAnonymous:
		return "signup"
	case tiers.TierFree:
		return "premium"
	case tiers.TierPremium:
		return "pro"
	}
	return ""
}

// vectorFormatUpgradePath is the lowest public tier whose format set includes
// format, expressed as an upgrade path ("signup" for free, else the tier name).
// "" when no public tier has it (e.g. MP4 before MM49 gives Pro raster video).
func vectorFormatUpgradePath(r *tiers.Registry, current tiers.Tier, format string) string {
	if r == nil {
		return vectorUpgradePath(current)
	}
	for _, t := range tiers.All() {
		if tiers.Rank(t) <= tiers.Rank(current) {
			continue
		}
		if r.For(t).VectorFormats.Has(format) {
			if t == tiers.TierFree {
				return "signup"
			}
			return string(t)
		}
	}
	return ""
}

// --- projects ----------------------------------------------------------------

// ListProjects handles GET /vector/projects. Never selects `document`.
//
//	200 {"projects":[{id,name,revision,thumbnailKey?,documentBytes,createdAt,updatedAt}]}
func (h *VectorHandler) ListProjects(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	userID, ok := requireVectorUser(c)
	if !ok {
		return
	}
	projects, err := h.store.ListVectorProjects(c.Request.Context(), userID, vectorListLimit)
	if err != nil {
		h.logger.Warn("vector: list projects failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Projects are temporarily unavailable"})
		return
	}
	out := make([]vectorProjectBody, 0, len(projects))
	for i := range projects {
		out = append(out, vectorBody(&projects[i], false))
	}
	c.JSON(http.StatusOK, gin.H{"projects": out})
}

// vectorCreateRequest is POST /vector/projects.
type vectorCreateRequest struct {
	Name     string          `json:"name"`
	Document json.RawMessage `json:"document"`
}

// CreateProject handles POST /vector/projects.
//
//	body {"name":"...","document":<VectorDoc v1>}
//	201  project (with document, revision 1)
//	401  not signed in (anonymous projects are browser-only)
//	403  {"error":"vector_projects_limit","limit":n,"used":n,"upgradePath":"..."}
//	422  {"error":"vector_doc_invalid","code","path","message"}
func (h *VectorHandler) CreateProject(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	userID, ok := requireVectorUser(c)
	if !ok {
		return
	}
	raw, ok := h.readVectorBody(c)
	if !ok {
		return
	}
	var req vectorCreateRequest
	if err := json.Unmarshal(raw, &req); err != nil || len(req.Document) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	doc, ok := h.sanitizeDoc(c, req.Document)
	if !ok {
		return
	}
	limit := tiers.Unlimited
	tier := tiers.TierFree
	if res, resolved := middleware.ResolveTier(c); resolved {
		limit, tier = res.Limits.VectorProjects, res.Tier
	}
	p, err := h.store.CreateVectorProject(c.Request.Context(), telemetry.VectorProjectInsert{
		ID: uuid.NewString(), OwnerUID: userID, Name: clampVectorName(req.Name), Document: doc,
	}, limit)
	var capErr *telemetry.VectorProjectLimitError
	switch {
	case errors.As(err, &capErr):
		c.JSON(http.StatusForbidden, gin.H{
			"error":       "vector_projects_limit",
			"message":     "You have reached the saved Motion project limit for your plan.",
			"limit":       capErr.Limit,
			"used":        capErr.Used,
			"upgradePath": vectorUpgradePath(tier),
		})
		return
	case err != nil:
		h.logger.Warn("vector: create project failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to create project"})
		return
	}
	// Ledger (ADR-0010): the project row is a retained object of the account.
	retention.Default().Register(detachedCtx(c), retention.RegisterInput{
		Kind: retention.KindVectorProject, Locator: p.ID, Owner: retentionOwnerFor(c), ProjectID: p.ID,
		SizeBytes: int64(len(doc)),
	})
	c.JSON(http.StatusCreated, vectorBody(p, true))
}

// GetProject handles GET /vector/projects/:id.
func (h *VectorHandler) GetProject(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, ok := requireVectorOwner(c, h.store, h.logger, id); !ok {
		return
	}
	p, err := h.store.GetVectorProject(c.Request.Context(), id)
	if errors.Is(err, telemetry.ErrVectorNotFound) {
		denyVectorProject(c)
		return
	}
	if err != nil {
		h.logger.Warn("vector: get project failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Project lookup is temporarily unavailable"})
		return
	}
	c.JSON(http.StatusOK, vectorBody(p, true))
}

// vectorSaveRequest is PUT /vector/projects/:id. Absent fields are unchanged;
// thumbnailKey "" clears the thumbnail.
type vectorSaveRequest struct {
	Name         *string         `json:"name"`
	Document     json.RawMessage `json:"document"`
	ThumbnailKey *string         `json:"thumbnailKey"`
}

// SaveProject handles PUT /vector/projects/:id.
//
//	header If-Match: <revision>          required (quotes and W/ tolerated)
//	body   {"name"?, "document"?, "thumbnailKey"?}
//	200    project with revision+1
//	400    thumbnailKey not under the caller's own prefix for this project
//	409    {"error":"revision_conflict","currentRevision":n,"updatedAt":"RFC3339Nano"}
//	422    document invalid
//	428    {"error":"if_match_required"}
func (h *VectorHandler) SaveProject(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	userID, ok := requireVectorOwner(c, h.store, h.logger, id)
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
	raw, ok := h.readVectorBody(c)
	if !ok {
		return
	}
	var req vectorSaveRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	upd := telemetry.VectorProjectUpdate{ID: id, OwnerUID: userID, ExpectedRevision: expected}
	if req.Name != nil {
		n := clampVectorName(*req.Name)
		upd.Name = &n
	}
	if len(req.Document) > 0 && string(req.Document) != "null" {
		doc, ok := h.sanitizeDoc(c, req.Document)
		if !ok {
			return
		}
		upd.Document = doc
	}
	if req.ThumbnailKey != nil {
		key := strings.TrimSpace(*req.ThumbnailKey)
		if key != "" && key != vectorThumbnailKey(userID, id) {
			// Only the caller's own key for THIS project: anything else could
			// point a project at another user's object (or overwrite one).
			c.JSON(http.StatusBadRequest, gin.H{"error": "thumbnail_key_invalid"})
			return
		}
		upd.ThumbnailKey = &key
	}
	p, err := h.store.SaveVectorProject(c.Request.Context(), upd)
	var conflict *telemetry.VectorRevisionConflictError
	switch {
	case errors.As(err, &conflict):
		c.JSON(http.StatusConflict, vectorRevisionConflictBody(conflict))
		return
	case errors.Is(err, telemetry.ErrVectorNotFound):
		denyVectorProject(c)
		return
	case err != nil:
		h.logger.Warn("vector: save project failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to save project"})
		return
	}
	if upd.Document != nil {
		// Grow the ledger row's size; created_at/expires_at never move on save.
		retention.Default().Register(detachedCtx(c), retention.RegisterInput{
			Kind: retention.KindVectorProject, Locator: p.ID, Owner: retentionOwnerFor(c), ProjectID: p.ID,
			SizeBytes: int64(len(upd.Document)),
		})
	}
	c.JSON(http.StatusOK, vectorBody(p, true))
}

// parseIfMatch reads `If-Match: 3`, `"3"` or `W/"3"`. present=false when the
// header is missing or empty; valid=false when it is not a positive integer.
func parseIfMatch(v string) (rev int, present, valid bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false, false
	}
	v = strings.TrimPrefix(v, "W/")
	v = strings.Trim(v, `"`)
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, true, false
	}
	return n, true, true
}

// DeleteProject handles DELETE /vector/projects/:id.
//
//	200 {"deleted":{"objects":N,"bytes":N},"pending":N}
//
// The project's ledger rows (the vector_project row and its thumbnail
// s3_object) expire and sweep inline; the row is then removed directly in case
// it was never ledgered. Idempotent.
func (h *VectorHandler) DeleteProject(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	if h.expirer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, ok := requireVectorOwner(c, h.store, h.logger, id); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	res, err := h.expirer.ExpireNow(ctx, ledgerFilterForProject(id))
	if err != nil && !errors.Is(err, retention.ErrNoExpire) {
		h.logger.Error("vector: delete project failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	if _, err := h.store.DeleteVectorProjectRows(ctx, id); err != nil {
		h.logger.Warn("vector: deleting project row failed; a repeat will retry", "error", err.Error())
		res.Pending++
	}
	if h.onDelete != nil {
		h.onDelete("project")
	}
	c.JSON(http.StatusOK, newDeletedBody(res))
}

// --- thumbnail ---------------------------------------------------------------

// vectorThumbnailPrefix is the first S3 key segment of every Motion thumbnail;
// the sweeper's AllowedPrefixes must include it.
const vectorThumbnailPrefix = "vector"

// vectorThumbnailKey is the one key a project's thumbnail may have:
// vector/<ownerUid>/<projectId>/thumb.png. Both segments are server ids, never
// client input, so a client cannot aim the upload anywhere else.
func vectorThumbnailKey(ownerUID, projectID string) string {
	return vectorThumbnailPrefix + "/" + sanitizeS3PathSegment(ownerUID) + "/" + sanitizeS3PathSegment(projectID) + "/thumb.png"
}

// PresignThumbnail handles POST /vector/projects/:id/thumbnail.
//
//	body {"sizeBytes":N, "contentType":"image/png"}   sizeBytes required, 1..524288
//	201  {"uploadUrl","thumbnailKey","contentType":"image/png","maxBytes":524288,"expiresAt"}
//	400  missing/invalid sizeBytes, or a contentType other than image/png
//	413  sizeBytes over 512 KB
//
// The URL is signed for exactly sizeBytes and image/png, so S3 itself refuses
// a larger or different upload. The client PUTs the PNG to uploadUrl with
// Content-Type image/png, then sends thumbnailKey with its next save. The key
// is fixed per project, so a new thumbnail overwrites the old one.
func (h *VectorHandler) PresignThumbnail(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	userID, ok := requireVectorOwner(c, h.store, h.logger, id)
	if !ok {
		return
	}
	if h.presign == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 uploads are not configured"})
		return
	}
	var body struct {
		ContentType string `json:"contentType"`
		SizeBytes   int64  `json:"sizeBytes"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := json.NewDecoder(c.Request.Body).Decode(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if ct := strings.ToLower(strings.TrimSpace(body.ContentType)); ct != "" && ct != vectorThumbnailContentType {
		c.JSON(http.StatusBadRequest, gin.H{"error": "thumbnail_content_type", "contentType": vectorThumbnailContentType})
		return
	}
	if body.SizeBytes > VectorThumbnailMaxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "thumbnail_too_large", "maxBytes": VectorThumbnailMaxBytes})
		return
	}
	if body.SizeBytes <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "thumbnail_size_required", "maxBytes": VectorThumbnailMaxBytes})
		return
	}
	ttl := 15 * time.Minute
	if h.cfg != nil && h.cfg.S3PresignTTL > 0 {
		ttl = h.cfg.S3PresignTTL
	}
	key := vectorThumbnailKey(userID, id)
	url, err := h.presign.PresignPut(c.Request.Context(), key, vectorThumbnailContentType, body.SizeBytes, ttl)
	if err != nil {
		h.logger.Warn("vector: presign thumbnail failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload URL"})
		return
	}
	// Ledger: the thumbnail is an S3 child of the project, so a project delete,
	// the sweeper and account deletion all reach it. Registered before the PUT
	// so an abandoned upload is still reaped.
	retention.Default().RegisterS3(detachedCtx(c), retentionOwnerFor(c), key, "", "", id, body.SizeBytes)
	c.JSON(http.StatusCreated, gin.H{
		"uploadUrl":    url,
		"thumbnailKey": key,
		"contentType":  vectorThumbnailContentType,
		"maxBytes":     VectorThumbnailMaxBytes,
		"expiresAt":    h.now().UTC().Add(ttl).Format(time.RFC3339),
	})
}

// --- export counting -----------------------------------------------------------

// countOutcome is the result of spending one Motion export unit.
type countOutcome struct {
	tier      tiers.Tier
	limit     int64
	remaining int64 // -1 = unlimited
	resetsAt  time.Time
}

// spendExport runs CheckExport and then the transactional counter. It writes
// the refusal response itself and returns ok=false on any refusal or failure.
// It is shared by POST /vector/exports and POST /vector/vectorize so both spend
// the same allowance through the same rules.
//
// Unlike RequireQuota it FAILS CLOSED: the export runs in the browser after a
// 200, so a counter that cannot be written must not hand out free exports.
func (h *VectorHandler) spendExport(c *gin.Context, req vector.ExportRequest) (countOutcome, bool) {
	res, resolved := middleware.ResolveTier(c)
	if !resolved {
		// Tier system disabled (TIER_QUOTA_ENABLED=false): the deployment's
		// pre-tier behaviour is "no tier-derived limit", so count nothing and
		// allow every countable format.
		if !tiers.IsVectorFormat(req.Format) {
			vectorNotCountable(c, req.Format)
			return countOutcome{}, false
		}
		return countOutcome{tier: tiers.TierAnonymous, remaining: -1, resetsAt: middleware.NextUTCMidnight(h.now())}, true
	}
	now := h.now().UTC()
	reset := middleware.NextUTCMidnight(now)
	out := countOutcome{tier: res.Tier, resetsAt: reset}

	d := vector.CheckExport(res.Limits, req)
	switch d.Outcome {
	case vector.DecisionNotCountable:
		vectorNotCountable(c, req.Format)
		return out, false
	case vector.DecisionLocked:
		c.JSON(http.StatusForbidden, gin.H{
			"error":       "vector_format_locked",
			"format":      req.Format,
			"tier":        string(res.Tier),
			"upgradePath": vectorFormatUpgradePath(h.tiers, res.Tier, req.Format),
		})
		return out, false
	case vector.DecisionCap:
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "vector_cap_exceeded",
			"cap":   d.Cap,
			"max":   d.Max,
			"tier":  string(res.Tier),
		})
		return out, false
	}

	limit := int64(res.Limits.VectorExportsPerDay)
	out.limit = limit
	kind, key := middleware.QuotaSubjectFor(c, res.User, h.ipSalt)
	used, allowed, err := h.store.CountVectorExport(c.Request.Context(), telemetry.VectorExportRecord{
		ID: uuid.NewString(), ProjectID: strings.TrimSpace(req.ProjectID),
		SubjectKind: string(kind), SubjectKey: key, Tier: string(res.Tier),
		Format: req.Format, Height: req.Height, Fps: req.Fps, DurationMs: req.DurationMs, At: now,
	}, limit)
	if err != nil {
		h.logger.Warn("vector: counting export failed; refusing", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Exports are temporarily unavailable"})
		return out, false
	}
	c.Header(middleware.HeaderTier, string(res.Tier))
	c.Header(middleware.HeaderQuotaReset, reset.Format(time.RFC3339))
	if limit > 0 {
		out.remaining = limit - used
		if out.remaining < 0 {
			out.remaining = 0
		}
		c.Header(middleware.HeaderQuotaLimit, strconv.FormatInt(limit, 10))
		c.Header(middleware.HeaderQuotaRemaining, strconv.FormatInt(out.remaining, 10))
	} else {
		out.remaining = -1
	}
	if !allowed {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":       "vector_export_limit",
			"tier":        string(res.Tier),
			"metric":      "vector_export",
			"limit":       limit,
			"used":        used,
			"resetsAt":    reset.Format(time.RFC3339),
			"upgradePath": vectorUpgradePath(res.Tier),
		})
		return out, false
	}
	h.emitCounted(c, req.Format, string(res.Tier))
	return out, true
}

func vectorNotCountable(c *gin.Context, format string) {
	c.JSON(http.StatusBadRequest, gin.H{
		"error":   "vector_format_not_countable",
		"format":  format,
		"message": "Static SVG export is free and is never counted; other formats must be one of the Motion export formats.",
	})
}

// emitCounted sends server_vector_export_counted {format, tier} (MM49) once
// the unit is spent. Fire-and-forget through the analytics emitter; tests
// replace it through the `counted` seam. The vector_exports row written in the
// same transaction stays the ground truth: the event needs the caller's
// visitor and session ids, and the editor sends X-MM-Visitor-ID only when the
// visitor has granted analytics consent.
func (h *VectorHandler) emitCounted(c *gin.Context, format, tier string) {
	if h.counted != nil {
		h.counted(c, format, tier)
	}
}

// SetAnalyticsEmitter attaches the server-truth analytics feed (MM49). A nil
// or disabled emitter sends nothing.
func (h *VectorHandler) SetAnalyticsEmitter(e *analyticsemit.Emitter) {
	if h == nil || !e.Enabled() {
		return
	}
	h.counted = func(c *gin.Context, format, tier string) {
		e.Emit(vectorCountedEvent(c, format, tier, time.Now().UTC()))
	}
}

// vectorCountedEvent builds the server_vector_export_counted event. The
// properties are the counted format and the tier, both closed vocabularies;
// the tier also travels in the context block like every server event.
func vectorCountedEvent(c *gin.Context, format, tier string, at time.Time) analyticsemit.Event {
	return analyticsemit.Event{
		EventName: analyticsemit.ServerVectorExportCounted,
		EventTS:   at,
		VisitorID: strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")),
		SessionID: strings.TrimSpace(c.GetHeader("X-MM-Session-ID")),
		Context:   analyticsemit.Context("", "", "", tier),
		Properties: map[string]any{
			"format": format,
			"tier":   tier,
		},
	}
}

// CountExport handles POST /vector/exports (OptionalFirebaseAuth + session id).
//
//	body {"projectId"?, "format", "durationMs", "height", "fps"}
//	200  {"ok":true,"format","tier","remaining":n|-1,"limit":n,"resetsAt":"RFC3339"}
//	400  {"error":"vector_format_not_countable","format"}   svg_static / unknown
//	403  {"error":"vector_format_locked","format","tier","upgradePath"}
//	422  {"error":"vector_cap_exceeded","cap":"duration"|"height"|"fps","max":n,"tier"}
//	429  {"error":"vector_export_limit","tier","metric":"vector_export","limit","used","resetsAt","upgradePath"}
//
// Call it BEFORE running the browser exporter: a 200 has already spent the
// unit, and a cancelled export is not refunded.
func (h *VectorHandler) CountExport(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	var req vector.ExportRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	req.Format = strings.ToLower(strings.TrimSpace(req.Format))
	if req.DurationMs < 0 || req.Height < 0 || req.Fps < 0 || len(req.ProjectID) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	out, ok := h.spendExport(c, req)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":        true,
		"format":    req.Format,
		"tier":      string(out.tier),
		"limit":     out.limit,
		"remaining": out.remaining,
		"resetsAt":  out.resetsAt.Format(time.RFC3339),
	})
}

// --- vectorize ---------------------------------------------------------------

// Vectorize handles POST /vector/vectorize: multipart `file` (an image, at most
// the tier's MaxFileBytes) plus optional form fields `threshold` (1..99) and
// `turd` (potrace -t). It is counted as import_svg_vectorize through the SAME
// CheckExport + counter BEFORE any job exists, then runs the existing potrace
// path through the converter job (JobManager, output format svg) and answers
// the usual {"jobId"}. The SVG is fetched with /api/job/:id + /api/download/:id.
// Missing potrace surfaces as the job's existing "vectorization is unavailable"
// error.
func (h *VectorHandler) Vectorize(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	if h.vectorize == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Vectorization is temporarily unavailable"})
		return
	}
	// Parse and pre-check the upload BEFORE spending the allowance, so a request
	// that could never run (no file, not a raster image, too large) costs
	// nothing. The tier's MaxFileBytes caps the body exactly like /upload.
	uploadCap := maxUploadBytes(c, h.cfg)
	if uploadCap > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, uploadCap)
	}
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "File exceeds the upload limit for your plan", "maxBytes": uploadCap})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse form"})
		return
	}
	_, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no file provided"})
		return
	}
	if !vectorizableUpload(header.Filename, header.Header.Get("Content-Type")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vectorize_unsupported_file", "message": "Vectorize takes a raster image (PNG, JPG, WebP, GIF, BMP, TIFF). SVG files import directly."})
		return
	}
	threshold, terr := optionalFormInt(c.Request.FormValue("threshold"))
	turd, uerr := optionalFormInt(c.Request.FormValue("turd"))
	if terr != nil || uerr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "threshold and turd must be integers"})
		return
	}
	if _, ok := h.spendExport(c, vector.ExportRequest{Format: tiers.VectorFormatImportSVGVectorize}); !ok {
		return
	}
	h.vectorize.LaunchVectorize(c, threshold, turd)
}

// vectorizableUpload is the cheap pre-count check: a raster image by declared
// type or extension, and never an SVG (already a vector; the UI imports it).
func vectorizableUpload(name, contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	ext := strings.ToLower(strings.TrimPrefix(filepathExt(name), "."))
	if ct == "image/svg+xml" || ext == "svg" || ext == "svgz" {
		return false
	}
	if strings.HasPrefix(ct, "image/") {
		return true
	}
	switch ext {
	case "png", "jpg", "jpeg", "webp", "gif", "bmp", "tif", "tiff", "avif", "heic", "heif":
		return true
	}
	return false
}

func filepathExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 && !strings.ContainsAny(name[i:], `/\`) {
		return name[i:]
	}
	return ""
}

func optionalFormInt(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("not an integer: %q", v)
	}
	return n, nil
}
