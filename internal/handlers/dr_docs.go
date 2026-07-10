package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// DrDocsHandler serves the Double Raven partner portal document endpoints
// (GET /api/dr/docs, GET /api/dr/docs/:slug) plus the in-portal "Create Doc"
// editor endpoints (create/update/publish drafts + presign/complete/delete
// media assets). It is Postgres-backed by the dr_documents / dr_document_assets
// tables (see the init_double_raven_docs + add_dr_document_assets migrations)
// and, like the Content Studio handler, degrades to 503 rather than panicking
// when no DB pool or S3 client is configured (the repo's opt-in pattern — see
// db.New).
//
// The S3 client + presign client + cfg are held so document media assets use
// Media Manipulator's standard S3 handshake — presign -> client PUT -> complete
// — reusing the shared client/bucket/config (mirrors NewDrCommentsHandler). No
// second S3 client, no new environment variables.
//
// Authorization is handled entirely upstream by
// middleware.RequireDoubleRavenAuth on the /dr group (see setupRouter); this
// handler assumes the caller is already an allowlisted, verified user, and
// reads authorship (created_by/updated_by/author_uid) only from the verified
// DRClaims in the gin context, never from the request body.
type DrDocsHandler struct {
	pool      *pgxpool.Pool
	cfg       *config.Config
	s3Client  *s3.Client
	s3Presign *s3.PresignClient
}

// NewDrDocsHandler wires the handler. The constructor signature mirrors
// NewDrCommentsHandler(pool, cfg, s3Client); the presign client is derived from
// s3Client (nil-safe so a nil S3 client leaves asset endpoints returning 503
// while the read endpoints keep working).
func NewDrDocsHandler(pool *pgxpool.Pool, cfg *config.Config, s3Client *s3.Client) *DrDocsHandler {
	var presign *s3.PresignClient
	if s3Client != nil {
		presign = s3.NewPresignClient(s3Client)
	}
	return &DrDocsHandler{pool: pool, cfg: cfg, s3Client: s3Client, s3Presign: presign}
}

// RegisterDrDocsRoutes wires the document endpoints onto a group that is
// ALREADY prefixed /dr and gated by RequireDoubleRavenAuth (see setupRouter),
// so the concrete paths resolve to /api/dr/docs[/…].
//
// NOTE on the ":slug" param name: gin keys its route tree per HTTP method, and
// within a method every wildcard at the same path position must share one name.
// The comments handler already registers POST /docs/:slug/comments, so the
// wildcard directly under /docs/ is pinned to ":slug" for the whole group. Some
// endpoints address a document by its UUID rather than its slug, but they must
// still use the ":slug" param name to avoid a wildcard-name conflict panic. The
// interpretation per route:
//
//   READ (slug, drSlugPattern-validated):  GET /docs/:slug, GET /docs/:slug/revisions,
//                                           GET /docs/:slug/revisions/:rev
//   MUTATION (UUID, drDocIDParam):          PUT/DELETE /docs/:slug, POST /docs/:slug/publish,
//                                           POST|PUT|DELETE /docs/:slug/edit,
//                                           POST /docs/:slug/edit/publish,
//                                           POST /docs/:slug/assets[/:assetId/complete],
//                                           DELETE /docs/:slug/assets/:assetId
func RegisterDrDocsRoutes(r gin.IRouter, h *DrDocsHandler) {
	r.GET("/docs", h.ListDocs)
	r.POST("/docs", h.CreateDoc)
	r.GET("/docs/:slug", h.GetDoc)                            // slug
	r.PUT("/docs/:slug", h.UpdateDoc)                         // UUID — draft autosave
	r.DELETE("/docs/:slug", h.DeleteDoc)                      // UUID — creator-only soft delete
	r.POST("/docs/:slug/publish", h.PublishDoc)               // UUID — draft publish
	r.POST("/docs/:slug/edit", h.StartOrResumeEdit)           // UUID — start/resume edit session
	r.PUT("/docs/:slug/edit", h.UpdateEdit)                   // UUID — edit-session autosave
	r.POST("/docs/:slug/edit/publish", h.PublishEdit)         // UUID — publish edit changes
	r.DELETE("/docs/:slug/edit", h.DiscardEdit)               // UUID — discard edit session
	r.GET("/docs/:slug/revisions", h.ListRevisions)           // slug — version history
	r.GET("/docs/:slug/revisions/:rev", h.GetRevision)        // slug + positive int
	r.POST("/docs/:slug/assets", h.PresignAsset)              // UUID
	r.POST("/docs/:slug/assets/:assetId/complete", h.CompleteAsset) // UUID
	r.DELETE("/docs/:slug/assets/:assetId", h.DeleteAsset)    // UUID

	// Documentation filesystem (dr_doc_folders.go). Folder CRUD lives under
	// the DISTINCT /doc-folders prefix — registering /docs/folders would
	// conflict with the pinned :slug wildcard above. The per-document
	// move/rename operations join the :slug subtree (UUID-interpreted).
	r.GET("/doc-folders", h.ListDocFolders)
	r.POST("/doc-folders", h.CreateDocFolder)
	r.PUT("/doc-folders/:folderId", h.UpdateDocFolder)
	r.DELETE("/doc-folders/:folderId", h.DeleteDocFolder)
	r.PUT("/docs/:slug/move", h.MoveDoc)     // UUID
	r.PUT("/docs/:slug/rename", h.RenameDoc) // UUID

	// Per-document edit sharing (dr_docs_sharing.go): the creator's
	// "Partner can edit" toggle.
	r.PUT("/docs/:slug/sharing", h.UpdateDocSharing) // UUID — creator-only
}

// drSlugPattern mirrors the kebab-case slug shape the seed + UI use. Validated
// before hitting the DB so obviously bad input is a cheap 400 rather than a
// query.
var drSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (h *DrDocsHandler) dbReady(c *gin.Context) bool {
	if h.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Document storage is unavailable"})
		return false
	}
	return true
}

func (h *DrDocsHandler) s3Ready(c *gin.Context) bool {
	if h.s3Client == nil || h.s3Presign == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Asset storage is unavailable"})
		return false
	}
	return true
}

// presignGet returns a presigned GET URL for key. When downloadName is
// non-empty the URL forces a browser download (Content-Disposition: attachment)
// with that filename. Returns "" (best-effort) if presigning is unavailable or
// fails — hydration then leaves the block's src untouched. Mirrors the comments
// handler's presignGet.
func (h *DrDocsHandler) presignGet(ctx context.Context, key, downloadName string) string {
	if h.s3Presign == nil || key == "" || h.cfg == nil {
		return ""
	}
	ttl := h.cfg.S3ResultPresignTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	in := &s3.GetObjectInput{Bucket: aws.String(h.cfg.S3Bucket), Key: aws.String(key)}
	if downloadName != "" {
		in.ResponseContentDisposition = aws.String(fmt.Sprintf(`attachment; filename="%s"`, downloadName))
	}
	out, err := h.s3Presign.PresignGetObject(ctx, in, func(o *s3.PresignOptions) { o.Expires = ttl })
	if err != nil {
		log.Printf("dr docs: presign get %s: %v", key, err)
		return ""
	}
	return out.URL
}

func (h *DrDocsHandler) deleteObject(ctx context.Context, key string) {
	if h.s3Client == nil || key == "" || h.cfg == nil {
		return
	}
	if _, err := h.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	}); err != nil {
		log.Printf("dr docs: best-effort delete %s failed: %v", key, err)
	}
}

// ListDocs returns published, live (non-soft-deleted) documents ordered
// most-recently-updated first. It selects METADATA COLUMNS ONLY — the content
// JSONB is deliberately excluded so a listing never ships document bodies over
// the wire. Drafts never appear (status = 'published'); soft-deleted docs never
// appear (deleted_at IS NULL, served by the partial dr_documents_live_idx).
// CanDelete/HasEditSession are computed per row from the verified caller.
func (h *DrDocsHandler) ListDocs(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	rows, err := h.pool.Query(ctx, `
SELECT d.id, d.slug, d.title, d.summary, d.status, COALESCE(d.created_by, ''), d.folder_id, d.allow_partner_edits, d.created_at, d.updated_at,
       EXISTS(SELECT 1 FROM dr_document_edit_sessions s WHERE s.document_id = d.id) AS has_edit_session
FROM dr_documents d
WHERE d.status = 'published' AND d.deleted_at IS NULL
ORDER BY d.updated_at DESC
`)
	if err != nil {
		log.Printf("dr docs: list query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list documents"})
		return
	}
	defer rows.Close()

	docs := make([]models.DrDocSummary, 0)
	for rows.Next() {
		var d models.DrDocSummary
		if err := rows.Scan(&d.ID, &d.Slug, &d.Title, &d.Summary, &d.Status, &d.CreatedBy, &d.FolderID, &d.AllowPartnerEdits, &d.CreatedAt, &d.UpdatedAt, &d.HasEditSession); err != nil {
			log.Printf("dr docs: list scan failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list documents"})
			return
		}
		d.CanDelete = drCanDelete(d.CreatedBy, claims.Email)
		d.CanEdit = drCanEdit(d.CreatedBy, d.AllowPartnerEdits, claims.Email)
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		log.Printf("dr docs: list rows error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list documents"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"docs": docs})
}

// GetDoc returns a single document including its content block JSON. The slug is
// shape-validated first (cheap 400), then looked up; a missing row is a 404.
// Drafts are intentionally served here (the editor reloads a draft by its
// placeholder slug on refresh) even though ListDocs never lists them.
//
// Before responding, the content is HYDRATED: any image/video/file block whose
// src is a canonical "dr-asset://<uuid>" reference is rewritten to a presigned
// GET URL (see hydrateContent). Hydration is a READ-TIME transform only — the
// stored JSONB is never modified — and a document with zero dr-asset://
// references passes through byte-for-byte unchanged.
func (h *DrDocsHandler) GetDoc(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	slug := strings.TrimSpace(c.Param("slug"))
	if !drSlugPattern.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid document slug"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var d models.DrDoc
	// jsonb → []byte returns the raw stored JSON. Soft-deleted docs (deleted_at
	// NOT NULL) are excluded → 404 for everyone.
	var content []byte
	err := h.pool.QueryRow(ctx, `
SELECT d.id, d.slug, d.title, d.summary, d.status, COALESCE(d.created_by, ''), d.folder_id, d.allow_partner_edits, d.content_format, d.content, d.created_at, d.updated_at,
       EXISTS(SELECT 1 FROM dr_document_edit_sessions s WHERE s.document_id = d.id)
FROM dr_documents d
WHERE d.slug = $1 AND d.deleted_at IS NULL
`, slug).Scan(&d.ID, &d.Slug, &d.Title, &d.Summary, &d.Status, &d.CreatedBy, &d.FolderID, &d.AllowPartnerEdits, &d.ContentFormat, &content, &d.CreatedAt, &d.UpdatedAt, &d.HasEditSession)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		log.Printf("dr docs: get query failed for slug %q: %v", slug, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load document"})
		return
	}
	d.CanDelete = drCanDelete(d.CreatedBy, claims.Email)
	d.CanEdit = drCanEdit(d.CreatedBy, d.AllowPartnerEdits, claims.Email)
	// Read-time hydration of dr-asset:// media references → presigned URLs.
	d.Content = h.hydrateContent(ctx, d.ID, content)
	c.JSON(http.StatusOK, d)
}

// drAssetInfo is the minimal asset row needed to hydrate a media block.
type drAssetInfo struct {
	s3Key     string
	fileName  string
	sizeBytes int64
}

// hydrateContent rewrites canonical dr-asset:// media references in the content
// JSON to presigned GET URLs. It returns the ORIGINAL bytes unchanged when the
// content contains no dr-asset:// reference at all (so asset-free documents —
// including the seeded ADD — are byte-for-byte identical to what was stored and
// there are no marshaling side effects). This never writes to the database.
func (h *DrDocsHandler) hydrateContent(ctx context.Context, docID string, raw []byte) []byte {
	// Cheap short-circuit: no canonical reference anywhere → nothing to do.
	if !bytes.Contains(raw, []byte("dr-asset://")) {
		return raw
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	blocksAny, ok := doc["blocks"].([]any)
	if !ok {
		return raw
	}

	type target struct {
		block map[string]any
		kind  string
		ref   string
	}
	var targets []target
	idSet := map[string]struct{}{}
	for _, ba := range blocksAny {
		block, ok := ba.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := block["type"].(string)
		if kind != "image" && kind != "video" && kind != "file" {
			continue
		}
		src, _ := block["src"].(string)
		ref, ok := parseDrAssetRef(src)
		if !ok {
			continue // plain external https:// src — pass through untouched
		}
		if _, err := uuid.Parse(ref); err != nil {
			log.Printf("dr docs: hydrate: malformed asset ref %q in doc %s", ref, docID)
			continue
		}
		targets = append(targets, target{block: block, kind: kind, ref: ref})
		idSet[ref] = struct{}{}
	}
	if len(targets) == 0 {
		return raw
	}

	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	assets := h.loadAssetsByID(ctx, docID, ids)

	changed := false
	for _, tg := range targets {
		info, ok := assets[tg.ref]
		if !ok {
			// Not uploaded / not owned by this doc — leave src as-is and log.
			log.Printf("dr docs: hydrate: asset %s not found or not uploaded for doc %s", tg.ref, docID)
			continue
		}
		var url string
		if tg.kind == "file" {
			url = h.presignGet(ctx, info.s3Key, info.fileName)
		} else {
			url = h.presignGet(ctx, info.s3Key, "")
		}
		if url == "" {
			continue
		}
		tg.block["src"] = url
		tg.block["assetRef"] = "dr-asset://" + tg.ref
		if tg.kind == "file" {
			if name, has := tg.block["name"].(string); !has || name == "" {
				tg.block["name"] = info.fileName
			}
			if _, has := tg.block["sizeBytes"]; !has && info.sizeBytes > 0 {
				tg.block["sizeBytes"] = info.sizeBytes
			}
		}
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(doc)
	if err != nil {
		log.Printf("dr docs: hydrate: re-marshal doc %s: %v", docID, err)
		return raw
	}
	return out
}

// loadAssetsByID batches the asset lookups for hydration into a single query:
// only 'uploaded' assets belonging to docID are returned, keyed by asset id.
func (h *DrDocsHandler) loadAssetsByID(ctx context.Context, docID string, ids []string) map[string]drAssetInfo {
	out := map[string]drAssetInfo{}
	if len(ids) == 0 || h.pool == nil {
		return out
	}
	rows, err := h.pool.Query(ctx, `
SELECT id, s3_key, file_name, size_bytes
FROM dr_document_assets
WHERE document_id = $1 AND status = 'uploaded' AND id = ANY($2)`, docID, ids)
	if err != nil {
		log.Printf("dr docs: hydrate: load assets for doc %s: %v", docID, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var info drAssetInfo
		if err := rows.Scan(&id, &info.s3Key, &info.fileName, &info.sizeBytes); err != nil {
			log.Printf("dr docs: hydrate: scan asset: %v", err)
			continue
		}
		out[id] = info
	}
	return out
}
