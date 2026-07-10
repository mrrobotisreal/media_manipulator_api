package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// This file implements the "edit published docs / version history / soft delete"
// feature on top of the create flow in dr_docs_editor.go. All handlers read
// identity from the verified DRClaims (drCallerClaims), never from the body, and
// every doc lookup is soft-delete filtered (deleted_at IS NULL). See §3–§4 of
// the feature prompt.

// ----------------------------------------------------------------------- //
// Pure helpers (unit-tested in dr_docs_editor_test.go)
// ----------------------------------------------------------------------- //

// unionDrAssetRefs returns the set of asset ids referenced by ANY of the given
// content snapshots (the new published content plus every revision). This is the
// prune keep-set: an uploaded asset survives if it is referenced by the current
// content OR any historical revision, so old versions keep rendering their media
// forever. Snapshots that fail to parse are skipped defensively.
func unionDrAssetRefs(contents [][]byte) map[string]bool {
	set := map[string]bool{}
	for _, raw := range contents {
		refs, err := extractDrAssetRefs(raw)
		if err != nil {
			continue
		}
		for _, ref := range refs {
			set[ref] = true
		}
	}
	return set
}

// parseRevisionNumber validates a :rev path segment: a positive integer.
func parseRevisionNumber(raw string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func drRevisionParam(c *gin.Context) (int, bool) {
	n, ok := parseRevisionNumber(c.Param("rev"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid revision number"})
	}
	return n, ok
}

// ----------------------------------------------------------------------- //
// Handler helpers
// ----------------------------------------------------------------------- //

// assetMutationAllowed reports whether a document currently accepts asset
// mutations (presign/complete/delete): it must be live AND either a draft OR
// have an active edit session. exists=false → 404; allowed=false with
// exists=true → 409 "Document is not being edited".
func (h *DrDocsHandler) assetMutationAllowed(ctx context.Context, docID string) (allowed, exists bool, err error) {
	var status string
	var hasSession bool
	e := h.pool.QueryRow(ctx, `
SELECT d.status, EXISTS(SELECT 1 FROM dr_document_edit_sessions s WHERE s.document_id = d.id)
FROM dr_documents d
WHERE d.id = $1 AND d.deleted_at IS NULL`, docID).Scan(&status, &hasSession)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, false, nil
	}
	if e != nil {
		return false, false, e
	}
	return status == string(models.DrDocStatusDraft) || hasSession, true, nil
}

// loadEditSession loads the edit session for a document (content is the raw
// canonical JSON; hydrate it at the call site before responding).
func (h *DrDocsHandler) loadEditSession(ctx context.Context, docID string) (models.DrEditSession, error) {
	var s models.DrEditSession
	var content []byte
	err := h.pool.QueryRow(ctx, `
SELECT document_id, title, summary, content, created_by, updated_by, created_at, updated_at
FROM dr_document_edit_sessions WHERE document_id = $1`, docID).
		Scan(&s.DocumentID, &s.Title, &s.Summary, &content, &s.CreatedBy, &s.UpdatedBy, &s.CreatedAt, &s.UpdatedAt)
	s.Content = content
	return s, err
}

// respondEditSession hydrates the session content (read-time only) and responds.
func (h *DrDocsHandler) respondEditSession(c *gin.Context, ctx context.Context, statusCode int, s models.DrEditSession) {
	s.Content = h.hydrateContent(ctx, s.DocumentID, s.Content)
	c.JSON(statusCode, s)
}

// collectRevisionRefs unions the asset refs across every revision snapshot of a
// document (the prune keep-set). Because publish appends the new content as a
// revision BEFORE prune runs, the current content's refs are included naturally.
func (h *DrDocsHandler) collectRevisionRefs(ctx context.Context, docID string) (map[string]bool, error) {
	rows, err := h.pool.Query(ctx, `SELECT content FROM dr_document_revisions WHERE document_id = $1`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var contents [][]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		contents = append(contents, raw)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return unionDrAssetRefs(contents), nil
}

// prunePreservingRevisions runs the best-effort asset prune with a keep-set that
// unions all revision refs, so no asset referenced by a historical version is
// ever deleted. On a failure to gather the keep-set it SKIPS the prune entirely
// (never over-deletes). Shared by PublishDoc and PublishEdit.
func (h *DrDocsHandler) prunePreservingRevisions(ctx context.Context, docID string) {
	keepSet, err := h.collectRevisionRefs(ctx, docID)
	if err != nil {
		log.Printf("dr docs: prune skipped for %s (collect revision refs: %v)", docID, err)
		return
	}
	h.pruneUnreferencedAssets(ctx, docID, keepSet)
}

// ----------------------------------------------------------------------- //
// DELETE /docs/:slug — DeleteDoc (creator-only soft delete). Param = UUID.
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) DeleteDoc(c *gin.Context) {
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

	var createdBy string
	err := h.pool.QueryRow(ctx, `SELECT COALESCE(created_by, '') FROM dr_documents WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		log.Printf("dr docs: delete load %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete document"})
		return
	}
	if !drCanDelete(createdBy, claims.Email) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the document's creator can delete it"})
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete document"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Soft delete: mark the row; ALL related data (revisions, comments, replies,
	// attachments, assets + S3 objects) is intentionally left intact for a future
	// archival API. Only the transient edit session is discarded.
	tag, err := tx.Exec(ctx, `UPDATE dr_documents SET deleted_at = now(), deleted_by = $2 WHERE id = $1 AND deleted_at IS NULL`, id, claims.Email)
	if err != nil {
		log.Printf("dr docs: soft delete %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete document"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Document was already deleted"})
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM dr_document_edit_sessions WHERE document_id = $1`, id); err != nil {
		log.Printf("dr docs: delete session on soft delete %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete document"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr docs: delete commit %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete document"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ----------------------------------------------------------------------- //
// POST /docs/:slug/edit — StartOrResumeEdit. Param = UUID.
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) StartOrResumeEdit(c *gin.Context) {
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
	var req models.DrStartEditRequest
	_ = c.ShouldBindJSON(&req) // tolerate empty body

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Edit-sharing gate: only the creator (or an opted-in partner) may open an
	// edit session.
	gate, gok := h.requireDocEditAccess(c, ctx, id, claims.Email)
	if !gok {
		return
	}

	// Only live, published docs can be edited (drafts use the create flow).
	var status string
	err := h.pool.QueryRow(ctx, `SELECT status FROM dr_documents WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start editing"})
		return
	}
	if status != string(models.DrDocStatusPublished) {
		c.JSON(http.StatusConflict, gin.H{"error": "Only published documents can be edited"})
		return
	}

	var sessionExists bool
	if err := h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dr_document_edit_sessions WHERE document_id = $1)`, id).Scan(&sessionExists); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start editing"})
		return
	}

	switch decideStartEdit(sessionExists, req.FromRevision, req.Replace) {
	case startEditResume:
		session, err := h.loadEditSession(ctx, id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load editing session"})
			return
		}
		session.AllowPartnerEdits = gate.allowPartnerEdits
		session.CanEdit = drCanEdit(gate.createdBy, gate.allowPartnerEdits, claims.Email)
		h.respondEditSession(c, ctx, http.StatusOK, session)
		return

	case startEditConflict:
		c.JSON(http.StatusConflict, gin.H{"error": "An editing session already exists", "hasEditSession": true})
		return

	case startEditCreate:
		// Seed the session content from the requested revision, else the current
		// published row.
		var (
			title   string
			summary *string
			content []byte
		)
		if req.FromRevision != nil {
			err := h.pool.QueryRow(ctx, `
SELECT title, content FROM dr_document_revisions WHERE document_id = $1 AND revision_number = $2`, id, *req.FromRevision).
				Scan(&title, &content)
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Revision not found"})
				return
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start editing"})
				return
			}
			// Revision snapshots carry no summary column → publish will re-derive.
		} else {
			err := h.pool.QueryRow(ctx, `
SELECT title, summary, content FROM dr_documents WHERE id = $1 AND deleted_at IS NULL`, id).
				Scan(&title, &summary, &content)
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
				return
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start editing"})
				return
			}
		}

		// Upsert on the UNIQUE(document_id): create when absent, overwrite when
		// replace=true. created_by/updated_by become the caller (a fresh session).
		var session models.DrEditSession
		var sessContent []byte
		err = h.pool.QueryRow(ctx, `
INSERT INTO dr_document_edit_sessions (document_id, title, summary, content, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $5)
ON CONFLICT (document_id) DO UPDATE
SET title = EXCLUDED.title, summary = EXCLUDED.summary, content = EXCLUDED.content,
    created_by = EXCLUDED.created_by, updated_by = EXCLUDED.updated_by,
    created_at = now(), updated_at = now()
RETURNING document_id, title, summary, content, created_by, updated_by, created_at, updated_at`,
			id, title, summary, content, claims.Email).
			Scan(&session.DocumentID, &session.Title, &session.Summary, &sessContent, &session.CreatedBy, &session.UpdatedBy, &session.CreatedAt, &session.UpdatedAt)
		if err != nil {
			log.Printf("dr docs: create edit session %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start editing"})
			return
		}
		session.Content = sessContent
		session.AllowPartnerEdits = gate.allowPartnerEdits
		session.CanEdit = drCanEdit(gate.createdBy, gate.allowPartnerEdits, claims.Email)
		h.respondEditSession(c, ctx, http.StatusCreated, session)
	}
}

// ----------------------------------------------------------------------- //
// PUT /docs/:slug/edit — UpdateEdit (autosave). Param = UUID.
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) UpdateEdit(c *gin.Context) {
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
	var req models.DrUpdateEditRequest
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

	// Edit-sharing gate. This doubles as the liveness check (it 404s a
	// missing/deleted doc). Saves 403 when the creator flipped the toggle off
	// MID-session — the author can still Discard (see dr_docs_sharing.go).
	if _, ok := h.requireDocEditAccess(c, ctx, id, claims.Email); !ok {
		return
	}

	tag, err := h.pool.Exec(ctx, `
UPDATE dr_document_edit_sessions
SET title = $2, summary = $3, content = $4, updated_by = $5, updated_at = now()
WHERE document_id = $1`, id, title, summary, req.Content, claims.Email)
	if err != nil {
		log.Printf("dr docs: update edit session %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save changes"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "No editing session for this document"})
		return
	}
	c.JSON(http.StatusOK, models.DrUpdateDocResponse{OK: true})
}

// ----------------------------------------------------------------------- //
// POST /docs/:slug/edit/publish — PublishEdit. Param = UUID.
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) PublishEdit(c *gin.Context) {
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

	// Edit-sharing gate: publishing a session 403s if the creator revoked
	// access mid-edit (Discard remains available — see dr_docs_sharing.go).
	gate, gok := h.requireDocEditAccess(c, ctx, id, claims.Email)
	if !gok {
		return
	}

	// Load the staged session.
	var (
		title   string
		summary *string
		content []byte
	)
	err := h.pool.QueryRow(ctx, `
SELECT title, summary, content FROM dr_document_edit_sessions WHERE document_id = $1`, id).
		Scan(&title, &summary, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "No editing session for this document"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish changes"})
		return
	}

	// Doc must be live + published, and we need created_by/created_at for the DTO.
	var (
		status, createdBy string
		createdAt         models.UTCTime
	)
	err = h.pool.QueryRow(ctx, `
SELECT status, COALESCE(created_by, ''), created_at FROM dr_documents WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&status, &createdBy, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish changes"})
		return
	}
	if status != string(models.DrDocStatusPublished) {
		c.JSON(http.StatusConflict, gin.H{"error": "Only published documents can be edited"})
		return
	}

	// Validation mirrors PublishDoc (no slug recomputation — slug is immutable).
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

	referenced := map[string]bool{}
	for _, ref := range parsed.assetRefs() {
		referenced[ref] = true
	}
	if len(referenced) > 0 {
		uploaded, err := h.uploadedAssetIDs(ctx, id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish changes"})
			return
		}
		for ref := range referenced {
			if !uploaded[ref] {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Content references an asset that has not finished uploading: dr-asset://" + ref})
				return
			}
		}
	}

	finalSummary := summary
	if finalSummary == nil || strings.TrimSpace(*finalSummary) == "" {
		if s := parsed.summary(); s != "" {
			finalSummary = &s
		} else {
			finalSummary = nil
		}
	}

	// Single transaction: apply to the doc (slug UNCHANGED), append revision N+1,
	// delete the session.
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish changes"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		slug      string
		updatedAt models.UTCTime
	)
	err = tx.QueryRow(ctx, `
UPDATE dr_documents
SET title = $2, summary = $3, content = $4, updated_by = $5, updated_at = now()
WHERE id = $1 AND status = 'published' AND deleted_at IS NULL
RETURNING slug, updated_at`, id, title, finalSummary, content, claims.Email).Scan(&slug, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "Only published documents can be edited"})
		return
	}
	if err != nil {
		log.Printf("dr docs: publish edit update %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish changes"})
		return
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO dr_document_revisions (document_id, revision_number, title, content_format, content, created_by)
VALUES ($1, (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM dr_document_revisions WHERE document_id = $1), $2, 'dr-blocks/v1', $3, $4)`,
		id, title, content, claims.Email); err != nil {
		log.Printf("dr docs: publish edit revision %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish changes"})
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM dr_document_edit_sessions WHERE document_id = $1`, id); err != nil {
		log.Printf("dr docs: publish edit delete session %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish changes"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr docs: publish edit commit %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish changes"})
		return
	}

	// Prune with a keep-set unioning all revision refs so old versions keep their
	// media. Detached context; best-effort.
	pruneCtx, pruneCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer pruneCancel()
	h.prunePreservingRevisions(pruneCtx, id)

	c.JSON(http.StatusOK, models.DrDocSummary{
		ID:                id,
		Slug:              slug,
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

// ----------------------------------------------------------------------- //
// DELETE /docs/:slug/edit — DiscardEdit. Param = UUID.
// ----------------------------------------------------------------------- //

// DiscardEdit is deliberately NOT gated by the edit-sharing flag: the author
// of an existing session must always be able to discard it, even after the
// creator flips "Partner can edit" off mid-session — otherwise a partner whose
// access was revoked would be trapped with an undiscardable session (their
// save/publish 403 per the gate). Discard destroys only the staged copy.
func (h *DrDocsHandler) DiscardEdit(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	id, ok := drDocIDParam(c)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// 404 only if the document itself is missing/deleted; otherwise idempotent.
	var exists bool
	if err := h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dr_documents WHERE id = $1 AND deleted_at IS NULL)`, id).Scan(&exists); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to discard changes"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	// Discard is intentionally cheap: no asset prune (an asset uploaded during a
	// discarded session is cleaned by the next publish's prune or stays harmless).
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_document_edit_sessions WHERE document_id = $1`, id); err != nil {
		log.Printf("dr docs: discard edit %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to discard changes"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ----------------------------------------------------------------------- //
// GET /docs/:slug/revisions — ListRevisions. Param = SLUG.
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) ListRevisions(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	slug := strings.TrimSpace(c.Param("slug"))
	if !drSlugPattern.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid document slug"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	docID, err := h.liveDocIDBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load versions"})
		return
	}

	rows, err := h.pool.Query(ctx, `
SELECT revision_number, title, created_by, created_at
FROM dr_document_revisions WHERE document_id = $1 ORDER BY revision_number DESC`, docID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load versions"})
		return
	}
	defer rows.Close()

	revisions := make([]models.DrRevisionSummary, 0)
	maxRev := -1
	for rows.Next() {
		var r models.DrRevisionSummary
		if err := rows.Scan(&r.RevisionNumber, &r.Title, &r.CreatedBy, &r.CreatedAt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load versions"})
			return
		}
		if maxRev < 0 {
			maxRev = r.RevisionNumber // DESC order → first row is the newest
		}
		revisions = append(revisions, r)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load versions"})
		return
	}
	for i := range revisions {
		revisions[i].IsCurrent = revisions[i].RevisionNumber == maxRev
	}
	c.JSON(http.StatusOK, models.DrRevisionsListResponse{Revisions: revisions})
}

// ----------------------------------------------------------------------- //
// GET /docs/:slug/revisions/:rev — GetRevision. Param = SLUG; :rev = positive int.
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) GetRevision(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	slug := strings.TrimSpace(c.Param("slug"))
	if !drSlugPattern.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid document slug"})
		return
	}
	rev, ok := drRevisionParam(c)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	docID, err := h.liveDocIDBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load version"})
		return
	}

	var maxRev int
	if err := h.pool.QueryRow(ctx, `SELECT COALESCE(MAX(revision_number), 0) FROM dr_document_revisions WHERE document_id = $1`, docID).Scan(&maxRev); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load version"})
		return
	}

	var r models.DrRevision
	var content []byte
	err = h.pool.QueryRow(ctx, `
SELECT revision_number, title, created_by, created_at, content_format, content
FROM dr_document_revisions WHERE document_id = $1 AND revision_number = $2`, docID, rev).
		Scan(&r.RevisionNumber, &r.Title, &r.CreatedBy, &r.CreatedAt, &r.ContentFormat, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Version not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load version"})
		return
	}
	r.IsCurrent = r.RevisionNumber == maxRev
	// Revision snapshots hold canonical dr-asset:// refs; hydrate read-time only.
	r.Content = h.hydrateContent(ctx, docID, content)
	c.JSON(http.StatusOK, r)
}

// liveDocIDBySlug resolves a slug to its document id, excluding soft-deleted
// documents (pgx.ErrNoRows when missing or deleted).
func (h *DrDocsHandler) liveDocIDBySlug(ctx context.Context, slug string) (string, error) {
	var id string
	err := h.pool.QueryRow(ctx, `SELECT id FROM dr_documents WHERE slug = $1 AND deleted_at IS NULL`, slug).Scan(&id)
	return id, err
}
