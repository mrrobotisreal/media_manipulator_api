package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Documentation filesystem: VS Code-style folders for the DR Documentation
// section. Folders are an adjacency list (dr_doc_folders, see the 20260711002
// migration header for the relational-not-manifest rationale); documents hang
// off them via dr_documents.folder_id (NULL = root, ON DELETE SET NULL — a
// folder's demise never takes a document with it).
//
// ROUTING (Hard Constraint): gin pins the wildcard directly under /docs/ to
// ":slug" (see RegisterDrDocsRoutes) — registering /docs/folders would
// conflict with it. Folder CRUD therefore lives under the distinct
// /doc-folders prefix; the document move/rename operations join the existing
// /docs/:slug subtree with UUID interpretation (drDocIDParam), like the other
// mutations there.
//
// Collaboration model: both allowlisted users can create/rename/move/delete
// folders and move/rename documents (matching the chat-lab projects decision).
// Document DELETE keeps its existing creator-only rule.

const (
	drDocFolderMaxDepth     = 10  // levels; root children are depth 1
	drDocFolderMaxNameChars = 120 // runes, after the no-trim rule below
	drDocFoldersCap         = 500 // flat-list cap (two users; plenty)
)

// ----------------------------------------------------------------------- //
// Pure validation + tree math (unit-tested)
// ----------------------------------------------------------------------- //

// validateDocFolderName enforces the folder-name rules, returning a
// client-facing message ("" = valid). Leading/trailing whitespace is REJECTED
// (not auto-trimmed) so what the user typed is exactly what is stored, and '/'
// is reserved as a path separator in future affordances (breadcrumbs, search).
func validateDocFolderName(name string) string {
	if name != strings.TrimSpace(name) {
		return "Folder name must not start or end with whitespace"
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > drDocFolderMaxNameChars {
		return "Folder name must be 1–120 characters"
	}
	if strings.Contains(name, "/") {
		return "Folder name must not contain '/'"
	}
	return ""
}

// wouldCreateCycle reports whether re-parenting folderID under newParentID
// would create a cycle: newParentID IS folderID, or newParentID is a
// descendant of folderID. Pure walk up the parent map ("" / missing key =
// root); the caller loads the map in one query — with ≤500 folders that is
// cheaper and simpler than a recursive CTE and unit-testable besides.
func wouldCreateCycle(parents map[string]string, folderID, newParentID string) bool {
	seen := 0
	for id := newParentID; id != ""; id = parents[id] {
		if id == folderID {
			return true
		}
		seen++
		if seen > len(parents)+1 {
			return true // corrupt map (pre-existing cycle) — fail safe
		}
	}
	return false
}

// folderDepth returns the depth of folderID walking up the parent map: a
// root-level folder is depth 1; "" (root itself) is depth 0.
func folderDepth(parents map[string]string, folderID string) int {
	depth := 0
	for id := folderID; id != ""; id = parents[id] {
		depth++
		if depth > len(parents)+1 {
			break // corrupt map guard
		}
	}
	return depth
}

// subtreeHeight returns how many levels hang BELOW folderID (0 for a leaf),
// so a move's deepest resulting level is depth(newParent)+1+subtreeHeight.
func subtreeHeight(parents map[string]string, folderID string) int {
	children := map[string][]string{}
	for id, parent := range parents {
		if parent != "" {
			children[parent] = append(children[parent], id)
		}
	}
	var walk func(id string, guard int) int
	walk = func(id string, guard int) int {
		if guard > len(parents)+1 {
			return 0
		}
		max := 0
		for _, child := range children[id] {
			if h := walk(child, guard+1) + 1; h > max {
				max = h
			}
		}
		return max
	}
	return walk(folderID, 0)
}

// ----------------------------------------------------------------------- //
// Shared plumbing
// ----------------------------------------------------------------------- //

func drDocFolderIDParam(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("folderId"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid folder id"})
		return "", false
	}
	return id, true
}

// loadFolderParents loads the whole folder tree as a child→parent map ("" for
// root-level folders). One tiny query; every cycle/depth decision runs on it.
func (h *DrDocsHandler) loadFolderParents(ctx context.Context) (map[string]string, error) {
	rows, err := h.pool.Query(ctx, `SELECT id, parent_id FROM dr_doc_folders`)
	if err != nil {
		return nil, err
	}
	parents := map[string]string{}
	func() {
		defer rows.Close()
		for rows.Next() {
			var id string
			var parent *string
			if err := rows.Scan(&id, &parent); err != nil {
				continue
			}
			if parent != nil {
				parents[id] = *parent
			} else {
				parents[id] = ""
			}
		}
	}()
	return parents, rows.Err()
}

const drDocFolderCols = `id, parent_id, name, created_by, created_at, updated_at`

func scanDocFolder(row pgx.Row) (models.DrDocFolder, error) {
	var f models.DrDocFolder
	var createdAt, updatedAt time.Time
	if err := row.Scan(&f.ID, &f.ParentID, &f.Name, &f.CreatedByEmail, &createdAt, &updatedAt); err != nil {
		return f, err
	}
	f.CreatedAt = models.UTCTime{Time: createdAt}
	f.UpdatedAt = models.UTCTime{Time: updatedAt}
	return f, nil
}

// ----------------------------------------------------------------------- //
// GET /doc-folders — the flat list (client assembles the tree)
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) ListDocFolders(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	rows, err := h.pool.Query(ctx, `
SELECT `+drDocFolderCols+` FROM dr_doc_folders ORDER BY name, id LIMIT $1`, drDocFoldersCap)
	if err != nil {
		log.Printf("dr doc folders: list: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load folders"})
		return
	}
	folders := make([]models.DrDocFolder, 0)
	func() {
		defer rows.Close()
		for rows.Next() {
			f, err := scanDocFolder(rows)
			if err != nil {
				log.Printf("dr doc folders: scan: %v", err)
				continue
			}
			folders = append(folders, f)
		}
	}()
	c.JSON(http.StatusOK, models.DrDocFoldersResponse{Folders: folders})
}

// ----------------------------------------------------------------------- //
// POST /doc-folders
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) CreateDocFolder(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	var req models.DrCreateDocFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if msg := validateDocFolderName(req.Name); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	var parentID *string
	if req.ParentID != nil && strings.TrimSpace(*req.ParentID) != "" {
		pid := strings.TrimSpace(*req.ParentID)
		if _, err := uuid.Parse(pid); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid parent folder id"})
			return
		}
		parentID = &pid
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	parents, err := h.loadFolderParents(ctx)
	if err != nil {
		log.Printf("dr doc folders: load parents: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create folder"})
		return
	}
	if parentID != nil {
		if _, exists := parents[*parentID]; !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "Parent folder not found"})
			return
		}
		if folderDepth(parents, *parentID)+1 > drDocFolderMaxDepth {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Folders can be nested at most 10 levels deep"})
			return
		}
	}

	folder, err := scanDocFolder(h.pool.QueryRow(ctx, `
INSERT INTO dr_doc_folders (parent_id, name, created_by)
VALUES ($1, $2, lower($3))
RETURNING `+drDocFolderCols, parentID, req.Name, claims.Email))
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "A folder with that name already exists here"})
			return
		}
		log.Printf("dr doc folders: create: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create folder"})
		return
	}
	c.JSON(http.StatusCreated, folder)
}

// ----------------------------------------------------------------------- //
// PUT /doc-folders/:folderId — rename and/or move (partial; pointer fields)
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) UpdateDocFolder(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	folderID, ok := drDocFolderIDParam(c)
	if !ok {
		return
	}
	var req models.DrUpdateDocFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	current, err := scanDocFolder(h.pool.QueryRow(ctx, `
SELECT `+drDocFolderCols+` FROM dr_doc_folders WHERE id = $1`, folderID))
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
		return
	}
	if err != nil {
		log.Printf("dr doc folders: load: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update folder"})
		return
	}

	name := current.Name
	if req.Name != nil {
		name = *req.Name
		if msg := validateDocFolderName(name); msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
	}

	// parentId semantics: ABSENT (nil RawMessage) = unchanged; JSON null =
	// move to root; a UUID string = move under that folder.
	newParentID := current.ParentID
	parentChanging := len(req.ParentID) > 0
	if parentChanging {
		if string(req.ParentID) == "null" {
			newParentID = nil
		} else {
			var pid string
			if err := json.Unmarshal(req.ParentID, &pid); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid parent folder id"})
				return
			}
			pid = strings.TrimSpace(pid)
			if _, err := uuid.Parse(pid); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid parent folder id"})
				return
			}
			newParentID = &pid
		}
	}

	if parentChanging {
		parents, perr := h.loadFolderParents(ctx)
		if perr != nil {
			log.Printf("dr doc folders: load parents: %v", perr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update folder"})
			return
		}
		if newParentID != nil {
			if _, exists := parents[*newParentID]; !exists {
				c.JSON(http.StatusNotFound, gin.H{"error": "Parent folder not found"})
				return
			}
			if wouldCreateCycle(parents, folderID, *newParentID) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "A folder cannot be moved into itself or its own subfolder"})
				return
			}
			// The deepest resulting level is the target depth plus everything
			// hanging below the moved folder.
			if folderDepth(parents, *newParentID)+1+subtreeHeight(parents, folderID) > drDocFolderMaxDepth {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Folders can be nested at most 10 levels deep"})
				return
			}
		}
	}

	folder, err := scanDocFolder(h.pool.QueryRow(ctx, `
UPDATE dr_doc_folders
SET name = $1, parent_id = $2, updated_at = now()
WHERE id = $3
RETURNING `+drDocFolderCols, name, newParentID, folderID))
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "A folder with that name already exists here"})
			return
		}
		log.Printf("dr doc folders: update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update folder"})
		return
	}
	c.JSON(http.StatusOK, folder)
}

// ----------------------------------------------------------------------- //
// DELETE /doc-folders/:folderId — EMPTY-only (v1 safety rule)
// ----------------------------------------------------------------------- //

// DeleteDocFolder deletes a folder only when it has no child folders AND no
// live (non-soft-deleted) documents inside it — no recursive delete of shared
// documents in v1. Soft-deleted stragglers are handled by the FK's SET NULL.
func (h *DrDocsHandler) DeleteDocFolder(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	folderID, ok := drDocFolderIDParam(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var exists bool
	if err := h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dr_doc_folders WHERE id = $1)`, folderID).Scan(&exists); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete folder"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
		return
	}

	var childFolders, liveDocs int
	err := h.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM dr_doc_folders WHERE parent_id = $1),
       (SELECT count(*) FROM dr_documents WHERE folder_id = $1 AND deleted_at IS NULL)`, folderID).
		Scan(&childFolders, &liveDocs)
	if err != nil {
		log.Printf("dr doc folders: emptiness check: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete folder"})
		return
	}
	if childFolders > 0 || liveDocs > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "This folder isn't empty — move or delete its contents first"})
		return
	}

	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_doc_folders WHERE id = $1`, folderID); err != nil {
		log.Printf("dr doc folders: delete: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete folder"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// ----------------------------------------------------------------------- //
// PUT /docs/:slug/move — UUID-interpreted, like the other doc mutations
// ----------------------------------------------------------------------- //

func (h *DrDocsHandler) MoveDoc(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	docID, ok := drDocIDParam(c)
	if !ok {
		return
	}
	var req models.DrMoveDocRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	var folderID *string
	if req.FolderID != nil && strings.TrimSpace(*req.FolderID) != "" {
		fid := strings.TrimSpace(*req.FolderID)
		if _, err := uuid.Parse(fid); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid folder id"})
			return
		}
		folderID = &fid
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if folderID != nil {
		var exists bool
		if err := h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dr_doc_folders WHERE id = $1)`, *folderID).Scan(&exists); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move document"})
			return
		}
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
			return
		}
	}

	tag, err := h.pool.Exec(ctx, `
UPDATE dr_documents SET folder_id = $1, updated_at = now()
WHERE id = $2 AND deleted_at IS NULL`, folderID, docID)
	if err != nil {
		log.Printf("dr doc folders: move doc: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move document"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ----------------------------------------------------------------------- //
// PUT /docs/:slug/rename — title ONLY, never the slug
// ----------------------------------------------------------------------- //

// RenameDoc updates a document's TITLE only. The slug is deliberately never
// touched — slugs are stable identifiers baked into URLs and the
// comments/revisions chain. A title-only rename does NOT create a revision
// (revisions capture content changes); updated_at is bumped so recency
// ordering reflects the rename.
func (h *DrDocsHandler) RenameDoc(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	docID, ok := drDocIDParam(c)
	if !ok {
		return
	}
	var req models.DrRenameDocRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	title := strings.TrimSpace(req.Title)
	if n := utf8.RuneCountInString(title); n < 1 || n > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Title must be 1–200 characters"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	tag, err := h.pool.Exec(ctx, `
UPDATE dr_documents SET title = $1, updated_at = now()
WHERE id = $2 AND deleted_at IS NULL`, title, docID)
	if err != nil {
		log.Printf("dr doc folders: rename doc: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rename document"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
