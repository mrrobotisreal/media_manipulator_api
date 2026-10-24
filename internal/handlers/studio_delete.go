package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// MM24: DELETE /api/studio/projects/:id. The asset-level delete (part 21) lives
// in studio_assets_manage.go and shares this store and the sweeper.

// ProjectDeleteStore is the slice of *telemetry.Store project deletion needs.
type ProjectDeleteStore interface {
	ProjectOwner(ctx context.Context, projectID string) (userID, sessionID string, found bool, err error)
	DeleteStudioProjectRows(ctx context.Context, projectID string) ([]string, error)
	// Part 21 asset delete.
	AssetProject(ctx context.Context, assetID string) (projectID string, found bool, err error)
	StudioAssetKeys(ctx context.Context, assetID string) ([]string, bool, error)
	DeleteStudioAssetRow(ctx context.Context, assetID string) error
}

// SetDeletion attaches the inline sweeper and the store. nil = the route 503s.
func (h *StudioHandler) SetDeletion(e Expirer, store ProjectDeleteStore) {
	if h != nil {
		h.expirer, h.projectStore = e, store
	}
}

// SetDeletionMetric attaches the data_deleted counter hook.
func (h *StudioHandler) SetDeletionMetric(f func(scope string)) {
	if h != nil {
		h.onDelete = f
	}
}

// ownsProjectRow is the ownership rule for projects: an account owns rows with
// its user_id; a guest owns rows with its session id and NO user.
func ownsProjectRow(rowUser, rowSession, userID, session string) bool {
	if userID != "" {
		return rowUser == userID
	}
	return session != "" && rowUser == "" && rowSession == session
}

// DeleteProject handles DELETE /api/studio/projects/:id.
//
//	200 {"deleted":{"objects":N,"bytes":N},"pending":N}
//	401 no account and no session header
//	404 not the owner / no such project
//
// The project, its versions and assets and every S3 key (originals plus
// proxy/sprite/peaks) go through the same per-kind deleters the sweeper uses.
func (h *StudioHandler) DeleteProject(c *gin.Context) {
	if h.expirer == nil || h.projectStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	userID, session := callerIdentity(c)
	if userID == "" && session == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	rowUser, rowSession, found, err := h.projectStore.ProjectOwner(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	if !found || !ownsProjectRow(rowUser, rowSession, userID, session) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	res, err := h.expirer.ExpireNow(ctx, ledgerFilterForProject(id))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	// A project from before the ledger (or whose ledger row was never written) has
	// nothing to sweep: remove its rows directly. Idempotent for a swept project.
	if _, err := h.projectStore.DeleteStudioProjectRows(ctx, id); err != nil {
		res.Pending++
	}
	if h.onDelete != nil {
		h.onDelete("project")
	}
	c.JSON(http.StatusOK, newDeletedBody(res))
}
