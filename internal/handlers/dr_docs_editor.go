package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// ----------------------------------------------------------------------- //
// Constants + pure helpers (unit-tested in dr_docs_editor_test.go)
// ----------------------------------------------------------------------- //

const (
	drMaxImageAssetBytes int64 = 10 << 20  // 10 MiB
	drMaxVideoAssetBytes int64 = 200 << 20 // 200 MiB
	drMaxFileAssetBytes  int64 = 25 << 20  // 25 MiB
	drMaxDocAssets             = 50
	drMaxContentBytes    int64 = 2 << 20 // 2 MiB
	drMaxBlocks                = 2000
	drMaxTitleChars            = 300
	drMaxSummaryChars          = 1000
	drMaxFileNameChars         = 255
	drDerivedSummaryChars      = 200

	drEmptyContent = `{"format":"dr-blocks/v1","blocks":[]}`
)

// drKnownBlockTypes is the tripwire set for structural validation: any block
// whose "type" is not one of these is rejected, catching a client/server
// contract drift early. Full schema validation stays in the UI's Zod layer.
var drKnownBlockTypes = map[string]bool{
	"heading":    true,
	"paragraph":  true,
	"blockquote": true,
	"callout":    true,
	"list":       true,
	"table":      true,
	"code":       true,
	"divider":    true,
	"image":      true,
	"video":      true,
	"file":       true,
}

// docAssetExt allowlists a (kind, contentType) pair and returns the file
// extension used in the S3 key plus that kind's max upload size. ok=false → the
// pair is unsupported (400 at the call site). This EXTENDS, and does not modify,
// the comments handler's attachmentExt (images only).
func docAssetExt(kind, contentType string) (ext string, maxBytes int64, ok bool) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	switch kind {
	case "image":
		switch ct {
		case "image/png":
			return "png", drMaxImageAssetBytes, true
		case "image/jpeg":
			return "jpg", drMaxImageAssetBytes, true
		case "image/webp":
			return "webp", drMaxImageAssetBytes, true
		case "image/gif":
			return "gif", drMaxImageAssetBytes, true
		}
	case "video":
		switch ct {
		case "video/mp4":
			return "mp4", drMaxVideoAssetBytes, true
		case "video/webm":
			return "webm", drMaxVideoAssetBytes, true
		case "video/quicktime":
			return "mov", drMaxVideoAssetBytes, true
		}
	case "file":
		switch ct {
		case "application/pdf":
			return "pdf", drMaxFileAssetBytes, true
		case "application/zip":
			return "zip", drMaxFileAssetBytes, true
		case "text/plain":
			return "txt", drMaxFileAssetBytes, true
		case "text/csv":
			return "csv", drMaxFileAssetBytes, true
		case "application/json":
			return "json", drMaxFileAssetBytes, true
		case "text/markdown":
			return "md", drMaxFileAssetBytes, true
		case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
			return "docx", drMaxFileAssetBytes, true
		case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
			return "xlsx", drMaxFileAssetBytes, true
		}
	}
	return "", 0, false
}

// sanitizeDrFileName reduces an arbitrary uploaded name to a safe display /
// download basename: no path separators or control chars, bounded length,
// never empty. The S3 key never uses this (it's asset-UUID + ext); this is
// only the human-facing file_name stored + used for the download disposition.
func sanitizeDrFileName(name string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	if utf8.RuneCountInString(name) > drMaxFileNameChars {
		name = string([]rune(name)[:drMaxFileNameChars])
	}
	return name
}

// slugifyDrTitle turns a document title into a kebab-case slug matching
// drSlugPattern: lowercase, every run of non-[a-z0-9] characters becomes a
// single '-', leading/trailing '-' trimmed, capped at 120 chars before any
// uniqueness suffix, and "doc" when the result would be empty.
func slugifyDrTitle(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 120 {
		slug = strings.Trim(slug[:120], "-")
	}
	if slug == "" {
		return "doc"
	}
	return slug
}

// nextAvailableSlug returns base if it is free, else base-2, base-3, … using the
// taken predicate. Pure (the DB lookup is injected) so the suffix logic is
// unit-testable without a database. Bounded so a persistently-failing predicate
// can never loop forever.
func nextAvailableSlug(base string, taken func(string) bool) string {
	if base == "" {
		base = "doc"
	}
	if !taken(base) {
		return base
	}
	for i := 2; i < 10000; i++ {
		cand := base + "-" + strconv.Itoa(i)
		if !taken(cand) {
			return cand
		}
	}
	return base + "-" + strconv.Itoa(10000)
}

// validateDrBlocksJSON does cheap structural validation of stored content so
// garbage can't be persisted: bounded size, a JSON object with
// format == "dr-blocks/v1" and a "blocks" array (≤ drMaxBlocks) of objects,
// each carrying a string "type" from drKnownBlockTypes.
func validateDrBlocksJSON(raw []byte) error {
	if len(raw) == 0 {
		return errors.New("content is empty")
	}
	if int64(len(raw)) > drMaxContentBytes {
		return errors.New("content is too large")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return errors.New("content must be a JSON object")
	}
	var format string
	if err := json.Unmarshal(top["format"], &format); err != nil || format != "dr-blocks/v1" {
		return errors.New(`content format must be "dr-blocks/v1"`)
	}
	blocksRaw, ok := top["blocks"]
	if !ok || len(blocksRaw) == 0 || strings.TrimSpace(string(blocksRaw)) == "null" {
		return errors.New("content.blocks must be an array")
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(blocksRaw, &blocks); err != nil {
		return errors.New("content.blocks must be an array")
	}
	if len(blocks) > drMaxBlocks {
		return fmt.Errorf("content has too many blocks (max %d)", drMaxBlocks)
	}
	for i, b := range blocks {
		var bm map[string]json.RawMessage
		if err := json.Unmarshal(b, &bm); err != nil {
			return fmt.Errorf("block %d must be an object", i)
		}
		var t string
		if err := json.Unmarshal(bm["type"], &t); err != nil {
			return fmt.Errorf("block %d is missing a string type", i)
		}
		if !drKnownBlockTypes[t] {
			return fmt.Errorf("block %d has unknown type %q", i, t)
		}
	}
	return nil
}

// parseDrAssetRef extracts the asset id from a canonical "dr-asset://<id>" src.
// ok=false for plain external URLs (which pass through hydration untouched).
func parseDrAssetRef(src string) (string, bool) {
	const prefix = "dr-asset://"
	if strings.HasPrefix(src, prefix) {
		if ref := src[len(prefix):]; ref != "" {
			return ref, true
		}
	}
	return "", false
}

// drSpanShape / drBlockShape / drContentShape are the minimal typed views used
// by publish validation + summary derivation. They intentionally ignore fields
// the Go layer doesn't reason about (marks, table rows, …).
type drSpanShape struct {
	Text string `json:"text"`
}

type drBlockShape struct {
	Type  string        `json:"type"`
	Src   string        `json:"src"`
	Spans []drSpanShape `json:"spans"`
}

type drContentShape struct {
	Format string         `json:"format"`
	Blocks []drBlockShape `json:"blocks"`
}

func parseDrContent(raw []byte) (drContentShape, error) {
	var c drContentShape
	err := json.Unmarshal(raw, &c)
	return c, err
}

// assetRefs returns the canonical asset ids referenced by image/video/file
// blocks (in document order; may contain duplicates — callers dedupe).
func (c drContentShape) assetRefs() []string {
	var refs []string
	for _, b := range c.Blocks {
		if b.Type == "image" || b.Type == "video" || b.Type == "file" {
			if ref, ok := parseDrAssetRef(b.Src); ok {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// summary derives a summary from the first non-empty paragraph's text,
// truncated to drDerivedSummaryChars runes. "" when no such paragraph exists.
func (c drContentShape) summary() string {
	for _, b := range c.Blocks {
		if b.Type != "paragraph" {
			continue
		}
		var sb strings.Builder
		for _, s := range b.Spans {
			sb.WriteString(s.Text)
		}
		text := strings.TrimSpace(sb.String())
		if text == "" {
			continue
		}
		if utf8.RuneCountInString(text) > drDerivedSummaryChars {
			return string([]rune(text)[:drDerivedSummaryChars])
		}
		return text
	}
	return ""
}

// extractDrAssetRefs is the pure, unit-tested reference extractor used by
// publish validation.
func extractDrAssetRefs(raw []byte) ([]string, error) {
	c, err := parseDrContent(raw)
	if err != nil {
		return nil, err
	}
	return c.assetRefs(), nil
}

// deriveDrSummary is the pure, unit-tested summary derivation used at publish.
func deriveDrSummary(raw []byte) string {
	c, err := parseDrContent(raw)
	if err != nil {
		return ""
	}
	return c.summary()
}

// drCanDelete reports whether callerEmail is the document's creator (creator-only
// soft delete). Case-insensitive per §4.2; an empty created_by or an empty
// caller email never grants delete, and the seeded doc's "seed:migration"
// sentinel matches no real email — so no one ever sees a delete control on it.
func drCanDelete(createdBy, callerEmail string) bool {
	if strings.TrimSpace(createdBy) == "" || strings.TrimSpace(callerEmail) == "" {
		return false
	}
	return strings.EqualFold(createdBy, callerEmail)
}

// drCanEdit reports whether callerEmail may edit a document: the creator can
// ALWAYS edit their own document, and anyone (allowlisted — the API gate ran
// already) may edit when the creator opened it up with allow_partner_edits.
// Corollary: an ownerless document ("seed:migration" or empty created_by) is
// editable by everyone iff its flag is true — and since it has no creator,
// its flag can never be changed (the sharing endpoint is creator-only), which
// is why the 20260712001 migration grandfathers existing rows to true.
// Case-insensitive like drCanDelete; an empty caller email never edits (no
// verified identity, no access — belt and suspenders under the auth gate).
// Pure; unit-tested.
func drCanEdit(createdBy string, allowPartnerEdits bool, callerEmail string) bool {
	if strings.TrimSpace(callerEmail) == "" {
		return false
	}
	return drCanDelete(createdBy, callerEmail) || allowPartnerEdits
}

// startEditDecision is the pure resolution of a StartOrResumeEdit request given
// whether a session already exists. It encodes the §4.3 table so it can be unit
// tested without a database.
type startEditDecision int

const (
	startEditCreate  startEditDecision = iota // no session (or replace) → create/seed
	startEditResume                           // session exists, plain open → return it
	startEditConflict                         // session exists, seed-from-revision without replace → 409
)

func decideStartEdit(sessionExists bool, fromRevision *int, replace bool) startEditDecision {
	if !sessionExists || replace {
		return startEditCreate
	}
	if fromRevision != nil {
		return startEditConflict
	}
	return startEditResume
}

// ----------------------------------------------------------------------- //
// POST /docs — CreateDoc (draft)
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) CreateDoc(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	// Title is optional; tolerate an empty/absent body.
	var req models.DrCreateDocRequest
	_ = c.ShouldBindJSON(&req)
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "Untitled"
	}
	if utf8.RuneCountInString(title) > drMaxTitleChars {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Title is too long"})
		return
	}

	// Placeholder slug embeds a fresh uuid so it matches drSlugPattern and never
	// collides; the real slug is derived from the title at publish.
	slug := "draft-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var d models.DrDoc
	// Pass content as raw JSON bytes (not a Go string): pgx uses []byte /
	// json.RawMessage verbatim for a jsonb param, whereas a string would be
	// JSON-marshaled into a quoted scalar.
	err := h.pool.QueryRow(ctx, `
INSERT INTO dr_documents (slug, title, summary, status, content_format, content, created_by, updated_by)
VALUES ($1, $2, NULL, 'draft', 'dr-blocks/v1', $3, $4, $4)
RETURNING id, created_at, updated_at`, slug, title, []byte(drEmptyContent), claims.Email).
		Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		log.Printf("dr docs: create draft: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create document"})
		return
	}
	d.Slug = slug
	d.Title = title
	d.Summary = nil
	d.Status = string(models.DrDocStatusDraft)
	d.ContentFormat = "dr-blocks/v1"
	d.Content = json.RawMessage(drEmptyContent)
	c.JSON(http.StatusCreated, d)
}

// ----------------------------------------------------------------------- //
// PUT /docs/:slug — UpdateDoc (autosave). The ":slug" param carries the draft
// UUID (see the note on RegisterDrDocsRoutes).
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) UpdateDoc(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	id, ok := drDocIDParam(c)
	if !ok {
		return
	}
	var req models.DrUpdateDocRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "Untitled"
	}
	if utf8.RuneCountInString(title) > drMaxTitleChars {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Title is too long"})
		return
	}
	var summary *string
	if req.Summary != nil {
		if utf8.RuneCountInString(*req.Summary) > drMaxSummaryChars {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Summary is too long"})
			return
		}
		if s := strings.TrimSpace(*req.Summary); s != "" {
			summary = &s
		}
	}
	if err := validateDrBlocksJSON(req.Content); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Edit-sharing gate: a draft is content like anything else — only the
	// creator (or an opted-in partner) may autosave it.
	if _, ok := h.requireDocEditAccess(c, ctx, id, claims.Email); !ok {
		return
	}

	// Draft-only + live: distinguish 404 (missing/deleted) from 409 (published).
	var status string
	err := h.pool.QueryRow(ctx, `SELECT status FROM dr_documents WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		log.Printf("dr docs: update load %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save document"})
		return
	}
	if status != string(models.DrDocStatusDraft) {
		c.JSON(http.StatusConflict, gin.H{"error": "Only draft documents can be edited"})
		return
	}

	tag, err := h.pool.Exec(ctx, `
UPDATE dr_documents
SET title = $2, summary = $3, content = $4, updated_by = $5, updated_at = now()
WHERE id = $1 AND status = 'draft' AND deleted_at IS NULL`, id, title, summary, req.Content, claims.Email)
	if err != nil {
		log.Printf("dr docs: update %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save document"})
		return
	}
	if tag.RowsAffected() == 0 {
		// Raced with a publish/archive between the load and the update.
		c.JSON(http.StatusConflict, gin.H{"error": "Only draft documents can be edited"})
		return
	}
	c.JSON(http.StatusOK, models.DrUpdateDocResponse{OK: true})
}

// ----------------------------------------------------------------------- //
// POST /docs/:slug/assets — PresignAsset
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) PresignAsset(c *gin.Context) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	docID, ok := drDocIDParam(c)
	if !ok {
		return
	}
	var req models.DrPresignAssetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	ext, maxBytes, okExt := docAssetExt(req.Kind, req.ContentType)
	if !okExt {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Unsupported %s type: %s", req.Kind, req.ContentType)})
		return
	}
	if req.SizeBytes <= 0 || req.SizeBytes > maxBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s exceeds the %d MB limit", req.Kind, maxBytes>>20)})
		return
	}
	fileName := sanitizeDrFileName(req.FileName)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Assets may be uploaded to a live draft OR a live doc with an active edit
	// session (§4.4).
	allowed, exists, err := h.assetMutationAllowed(ctx, docID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if !allowed {
		c.JSON(http.StatusConflict, gin.H{"error": "Document is not being edited"})
		return
	}

	// Per-document asset cap.
	var count int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM dr_document_assets WHERE document_id = $1`, docID).Scan(&count); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	if count >= drMaxDocAssets {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("A document can have at most %d assets", drMaxDocAssets)})
		return
	}

	assetID := uuid.NewString()
	key := fmt.Sprintf("documents/%s/assets/%s.%s", docID, assetID, ext)
	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_document_assets (id, document_id, author_uid, kind, file_name, s3_key, content_type, size_bytes, width, height, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending')`,
		assetID, docID, claims.UID, req.Kind, fileName, key, req.ContentType, req.SizeBytes, req.Width, req.Height); err != nil {
		log.Printf("dr docs: insert asset: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}

	presignCtx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	defer pcancel()
	out, err := h.s3Presign.PresignPutObject(presignCtx, &s3.PutObjectInput{
		Bucket:      aws.String(h.cfg.S3Bucket),
		Key:         aws.String(key),
		ContentType: aws.String(req.ContentType),
	}, func(o *s3.PresignOptions) { o.Expires = h.cfg.S3PresignTTL })
	if err != nil {
		log.Printf("dr docs: presign put: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload URL"})
		return
	}
	c.JSON(http.StatusCreated, models.DrPresignAssetResponse{AssetID: assetID, UploadURL: out.URL})
}

// ----------------------------------------------------------------------- //
// POST /docs/:slug/assets/:assetId/complete — CompleteAsset
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) CompleteAsset(c *gin.Context) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	docID, ok := drDocIDParam(c)
	if !ok {
		return
	}
	assetID, ok := drAssetIDParam(c)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	// Same draft-or-editing guard as presign (§4.4).
	if allowed, exists, gerr := h.assetMutationAllowed(ctx, docID); gerr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	} else if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	} else if !allowed {
		c.JSON(http.StatusConflict, gin.H{"error": "Document is not being edited"})
		return
	}

	var (
		documentID, authorUID, status, key, contentType string
		declaredSize                                     int64
	)
	err := h.pool.QueryRow(ctx, `
SELECT document_id, author_uid, status, s3_key, content_type, size_bytes
FROM dr_document_assets WHERE id = $1`, assetID).
		Scan(&documentID, &authorUID, &status, &key, &contentType, &declaredSize)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && documentID != docID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	}
	if err := checkAuthorOnly(authorUID, claims.UID); err != nil {
		abortAuthzError(c, err)
		return
	}
	if status != "pending" {
		c.JSON(http.StatusConflict, gin.H{"error": "Asset upload already completed"})
		return
	}

	// Verify the object actually landed (mirrors the comments handler HEAD path).
	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("dr docs: head asset %s: %v", key, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file was not found"})
		return
	}
	objectSize := aws.ToInt64(head.ContentLength)
	if objectSize <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file is empty"})
		return
	}
	if declaredSize > 0 && absInt64(objectSize-declaredSize) > 1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file size does not match"})
		return
	}
	if ct := aws.ToString(head.ContentType); ct != "" && !strings.EqualFold(ct, contentType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file type does not match"})
		return
	}

	if _, err := h.pool.Exec(ctx, `UPDATE dr_document_assets SET status = 'uploaded' WHERE id = $1`, assetID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ----------------------------------------------------------------------- //
// DELETE /docs/:slug/assets/:assetId — DeleteAsset (author-only)
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) DeleteAsset(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	docID, ok := drDocIDParam(c)
	if !ok {
		return
	}
	assetID, ok := drAssetIDParam(c)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Same draft-or-editing guard as presign (§4.4).
	if allowed, exists, gerr := h.assetMutationAllowed(ctx, docID); gerr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete asset"})
		return
	} else if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	} else if !allowed {
		c.JSON(http.StatusConflict, gin.H{"error": "Document is not being edited"})
		return
	}

	var documentID, authorUID, key string
	err := h.pool.QueryRow(ctx, `SELECT document_id, author_uid, s3_key FROM dr_document_assets WHERE id = $1`, assetID).
		Scan(&documentID, &authorUID, &key)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && documentID != docID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete asset"})
		return
	}
	if err := checkAuthorOnly(authorUID, claims.UID); err != nil {
		abortAuthzError(c, err)
		return
	}
	h.deleteObject(ctx, key)
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_document_assets WHERE id = $1`, assetID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete asset"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ----------------------------------------------------------------------- //
// POST /docs/:slug/publish — PublishDoc
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) PublishDoc(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	id, ok := drDocIDParam(c)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Edit-sharing gate (publish is the ultimate content mutation).
	gate, gok := h.requireDocEditAccess(c, ctx, id, claims.Email)
	if !gok {
		return
	}

	var (
		title, contentFormat, status, createdBy string
		summary                                  *string
		content                                  []byte
		createdAt                                models.UTCTime
	)
	err := h.pool.QueryRow(ctx, `
SELECT title, summary, status, content_format, content, created_at, COALESCE(created_by, '')
FROM dr_documents WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&title, &summary, &status, &contentFormat, &content, &createdAt, &createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		log.Printf("dr docs: publish load %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
		return
	}
	if status != string(models.DrDocStatusDraft) {
		c.JSON(http.StatusConflict, gin.H{"error": "Only draft documents can be published"})
		return
	}

	// Content validation + non-empty guard.
	if err := validateDrBlocksJSON(content); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	parsed, err := parseDrContent(content)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Content could not be parsed"})
		return
	}
	if len(parsed.Blocks) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Document has no content to publish"})
		return
	}
	title = strings.TrimSpace(title)
	if title == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A title is required to publish"})
		return
	}

	// Every referenced dr-asset:// must be an uploaded asset of this document.
	referenced := map[string]bool{}
	for _, ref := range parsed.assetRefs() {
		referenced[ref] = true
	}
	if len(referenced) > 0 {
		uploaded, err := h.uploadedAssetIDs(ctx, id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
			return
		}
		for ref := range referenced {
			if !uploaded[ref] {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Content references an asset that has not finished uploading: dr-asset://%s", ref)})
				return
			}
		}
	}

	// Final slug: derived from the title, unique across other documents. This
	// uniqueness scan is intentionally NOT filtered by deleted_at — a
	// soft-deleted doc keeps occupying its slug so it is never silently reused.
	base := slugifyDrTitle(title)
	var slugErr error
	taken := func(cand string) bool {
		if slugErr != nil {
			return true
		}
		var exists bool
		if e := h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dr_documents WHERE slug = $1 AND id <> $2)`, cand, id).Scan(&exists); e != nil {
			slugErr = e
			return true
		}
		return exists
	}
	finalSlug := nextAvailableSlug(base, taken)
	if slugErr != nil {
		log.Printf("dr docs: publish slug check %s: %v", id, slugErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
		return
	}

	// Derive a summary server-side when the author left it blank.
	finalSummary := summary
	if finalSummary == nil || strings.TrimSpace(*finalSummary) == "" {
		if s := parsed.summary(); s != "" {
			finalSummary = &s
		} else {
			finalSummary = nil
		}
	}

	// Single transaction: publish the row + append the next revision snapshot.
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var updatedAt models.UTCTime
	err = tx.QueryRow(ctx, `
UPDATE dr_documents
SET slug = $2, summary = $3, status = 'published', updated_by = $4, updated_at = now()
WHERE id = $1 AND status = 'draft' AND deleted_at IS NULL
RETURNING updated_at`, id, finalSlug, finalSummary, claims.Email).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "Only draft documents can be published"})
		return
	}
	if err != nil {
		log.Printf("dr docs: publish update %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
		return
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO dr_document_revisions (document_id, revision_number, title, content_format, content, created_by)
VALUES ($1, (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM dr_document_revisions WHERE document_id = $1), $2, $3, $4, $5)`,
		id, title, contentFormat, content, claims.Email); err != nil {
		log.Printf("dr docs: publish revision %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr docs: publish commit %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
		return
	}

	// Best-effort prune of assets referenced by neither the new content NOR any
	// revision (so old versions keep their media). Runs on a fresh detached
	// context so it isn't cancelled by the request finishing; never fails the
	// request — the document is already published.
	pruneCtx, pruneCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer pruneCancel()
	h.prunePreservingRevisions(pruneCtx, id)

	c.JSON(http.StatusOK, models.DrDocSummary{
		ID:                id,
		Slug:              finalSlug,
		Title:             title,
		Summary:           finalSummary,
		Status:            string(models.DrDocStatusPublished),
		CreatedBy:         createdBy,
		CanDelete:         drCanDelete(createdBy, claims.Email),
		HasEditSession:    false,
		AllowPartnerEdits: gate.allowPartnerEdits,
		CanEdit:           drCanEdit(gate.createdBy, gate.allowPartnerEdits, claims.Email),
		CreatedAt:         createdAt,
		UpdatedAt:         updatedAt,
	})
}

// uploadedAssetIDs returns the set of 'uploaded' asset ids for a document.
func (h *DrDocsHandler) uploadedAssetIDs(ctx context.Context, docID string) (map[string]bool, error) {
	rows, err := h.pool.Query(ctx, `SELECT id FROM dr_document_assets WHERE document_id = $1 AND status = 'uploaded'`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		set[id] = true
	}
	return set, rows.Err()
}

// pruneUnreferencedAssets deletes (S3 + row) every 'pending' asset and every
// 'uploaded' asset NOT in the referenced set. Best-effort: errors are logged,
// never surfaced.
func (h *DrDocsHandler) pruneUnreferencedAssets(ctx context.Context, docID string, referenced map[string]bool) {
	rows, err := h.pool.Query(ctx, `SELECT id, s3_key, status FROM dr_document_assets WHERE document_id = $1`, docID)
	if err != nil {
		log.Printf("dr docs: prune query %s: %v", docID, err)
		return
	}
	var toDelete []string
	func() {
		defer rows.Close()
		for rows.Next() {
			var id, key, status string
			if err := rows.Scan(&id, &key, &status); err != nil {
				continue
			}
			if status == "pending" || (status == "uploaded" && !referenced[id]) {
				h.deleteObject(ctx, key)
				toDelete = append(toDelete, id)
			}
		}
	}()
	if len(toDelete) > 0 {
		if _, err := h.pool.Exec(ctx, `DELETE FROM dr_document_assets WHERE id = ANY($1)`, toDelete); err != nil {
			log.Printf("dr docs: prune delete %s: %v", docID, err)
		} else {
			log.Printf("dr docs: pruned %d unreferenced assets from doc %s", len(toDelete), docID)
		}
	}
}

// ----------------------------------------------------------------------- //
// Asset reaper (daily) — pending uploads never completed within 24h
// ----------------------------------------------------------------------- //

// ReapStalePendingAssets deletes dr_document_assets rows stuck in 'pending'
// older than 24h (best-effort removing their S3 objects first). Called on a
// daily ticker from cmd/api. It deliberately does NOT reap draft documents — a
// draft may hold hours of unpublished writing; drafts are only ever removed by
// publish. Safe to run repeatedly; errors are logged, not fatal.
func (h *DrDocsHandler) ReapStalePendingAssets(ctx context.Context) {
	if h.pool == nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	rows, err := h.pool.Query(ctx, `SELECT id, s3_key FROM dr_document_assets WHERE status = 'pending' AND created_at < $1`, cutoff)
	if err != nil {
		log.Printf("dr docs reaper: select pending assets: %v", err)
		return
	}
	var pendingIDs []string
	func() {
		defer rows.Close()
		for rows.Next() {
			var id, key string
			if err := rows.Scan(&id, &key); err != nil {
				continue
			}
			h.deleteObject(ctx, key)
			pendingIDs = append(pendingIDs, id)
		}
	}()
	if len(pendingIDs) > 0 {
		if _, err := h.pool.Exec(ctx, `DELETE FROM dr_document_assets WHERE id = ANY($1)`, pendingIDs); err != nil {
			log.Printf("dr docs reaper: delete pending assets: %v", err)
		} else {
			log.Printf("dr docs reaper: removed %d pending document assets", len(pendingIDs))
		}
	}
}

// ----------------------------------------------------------------------- //
// Shared param parsing
// ----------------------------------------------------------------------- //

// drDocIDParam reads the document UUID carried by the ":slug" route param (see
// the note on RegisterDrDocsRoutes) and 400s on a malformed id.
func drDocIDParam(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("slug"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid document id"})
		return "", false
	}
	return id, true
}

func drAssetIDParam(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("assetId"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid asset id"})
		return "", false
	}
	return id, true
}
