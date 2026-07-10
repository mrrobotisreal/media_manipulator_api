package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Per-document edit sharing. New documents are editable only by their creator;
// the creator opts the partner in per document with the "Partner can edit"
// toggle (PUT /docs/:slug/sharing → allow_partner_edits). The gate below
// protects every CONTENT mutation (edit sessions, draft autosave, publish,
// rename); reading, comments, revisions, folder moves, and delete semantics
// are untouched (delete was already creator-only).
//
// Deliberate exception — DiscardEdit is NOT gated: the author of an existing
// edit session may always discard their own session even if the creator
// flipped the toggle off mid-edit. Without this, a partner whose access was
// revoked mid-session would be trapped with an undiscardable session (their
// save/publish 403 per the gate, and nothing could ever remove the session).
// Discard destroys nothing but the reverted staged copy, so it is safe to
// leave open.

const drDocEditRestrictedError = "Editing this document is restricted to its creator"

// docEditGate carries the sharing columns the edit gate loads — handlers reuse
// them to fill the additive DTO fields without a second query.
type docEditGate struct {
	createdBy         string
	allowPartnerEdits bool
}

// requireDocEditAccess loads the live document's sharing columns and enforces
// drCanEdit for the caller. ok=false → the house-style 404/403/500 response
// was already written.
func (h *DrDocsHandler) requireDocEditAccess(c *gin.Context, ctx context.Context, docID, callerEmail string) (docEditGate, bool) {
	var g docEditGate
	err := h.pool.QueryRow(ctx, `
SELECT COALESCE(created_by, ''), allow_partner_edits
FROM dr_documents WHERE id = $1 AND deleted_at IS NULL`, docID).
		Scan(&g.createdBy, &g.allowPartnerEdits)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return g, false
	}
	if err != nil {
		log.Printf("dr docs sharing: load edit gate %s: %v", docID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load document"})
		return g, false
	}
	if !drCanEdit(g.createdBy, g.allowPartnerEdits, callerEmail) {
		c.JSON(http.StatusForbidden, gin.H{"error": drDocEditRestrictedError})
		return g, false
	}
	return g, true
}

// ----------------------------------------------------------------------- //
// PUT /docs/:slug/sharing — UpdateDocSharing (UUID-interpreted, creator-only)
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) UpdateDocSharing(c *gin.Context) {
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
	var req models.DrUpdateDocSharingRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.AllowPartnerEdits == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var createdBy string
	err := h.pool.QueryRow(ctx, `
SELECT COALESCE(created_by, '') FROM dr_documents WHERE id = $1 AND deleted_at IS NULL`, docID).Scan(&createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update sharing"})
		return
	}
	// Creator-only — the same identity rule as delete. An ownerless
	// (seed:migration) document therefore has an immutable flag.
	if !drCanDelete(createdBy, claims.Email) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the document's creator can change sharing"})
		return
	}

	var d models.DrDocSummary
	err = h.pool.QueryRow(ctx, `
UPDATE dr_documents
SET allow_partner_edits = $2, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING id, slug, title, summary, status, COALESCE(created_by, ''), folder_id, allow_partner_edits, created_at, updated_at,
          EXISTS(SELECT 1 FROM dr_document_edit_sessions s WHERE s.document_id = dr_documents.id)`,
		docID, *req.AllowPartnerEdits).
		Scan(&d.ID, &d.Slug, &d.Title, &d.Summary, &d.Status, &d.CreatedBy, &d.FolderID, &d.AllowPartnerEdits, &d.CreatedAt, &d.UpdatedAt, &d.HasEditSession)
	if err != nil {
		log.Printf("dr docs sharing: update %s: %v", docID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update sharing"})
		return
	}
	d.CanDelete = drCanDelete(d.CreatedBy, claims.Email)
	d.CanEdit = drCanEdit(d.CreatedBy, d.AllowPartnerEdits, claims.Email)
	c.JSON(http.StatusOK, d)
}
