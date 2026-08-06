package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// Version history (part 09, ADR ws/0001 item 3): list / create / restore
// endpoints under the existing /api/studio group, ownership-gated by
// requireProjectAccess like every other project-scoped route.
//
// Auto-checkpoints have two triggers. Server-side: ExportProject snapshots
// before starting a render (studio.go). Client-side: the editor POSTs
// {kind:'auto'} on open and on a ~5-minute active-editing interval. The
// client-driven half is acceptable because the server has no session concept —
// it cannot know when an editor "opened" or whether edits are flowing, and
// inventing a heartbeat protocol just for checkpoints would be a bigger
// surface than letting the client ask; server-side pruning (newest N autos,
// tier lever) keeps the cost bounded no matter what a client does.

// studioVersionNameMaxRunes caps a version name (matches the client input).
const studioVersionNameMaxRunes = 80

// studioVersionAutoKeepFallback is used only if tier resolution is absent
// (TIER_QUOTA_ENABLED=false) — same value as the uniform tier default.
const studioVersionAutoKeepFallback = 20

// versionAutoKeep resolves the auto-checkpoint retention for this caller.
func (h *StudioHandler) versionAutoKeep(c *gin.Context) int {
	if res, ok := middleware.ResolveTier(c); ok && res.Limits.StudioVersionAutoKeep > 0 {
		return res.Limits.StudioVersionAutoKeep
	}
	return studioVersionAutoKeepFallback
}

// clampVersionName trims and cuts to studioVersionNameMaxRunes. Rune-aware —
// a byte slice could split a multi-byte character and Postgres rejects
// invalid UTF-8 TEXT.
func clampVersionName(name string) string {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > studioVersionNameMaxRunes {
		name = string([]rune(name)[:studioVersionNameMaxRunes])
	}
	return name
}

// GET /api/studio/projects/:id/versions — metadata only, newest first.
func (h *StudioHandler) ListStudioVersions(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if !h.requireProjectAccess(c, projectID) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	versions, err := h.repo.ListVersions(ctx, projectID)
	if err != nil {
		log.Printf("studio versions: list failed for project %s: %v", projectID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list versions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"versions": versions})
}

// POST /api/studio/projects/:id/versions — checkpoint the current state.
// {kind:'auto'} (client-driven trigger, name optional) or {kind:'manual',
// name} ("Save version…").
func (h *StudioHandler) CreateStudioVersion(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	var req models.StudioCreateVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind != models.StudioVersionKindAuto && kind != models.StudioVersionKindManual {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be 'auto' or 'manual'"})
		return
	}
	name := clampVersionName(req.Name)
	if kind == models.StudioVersionKindManual && name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required for a manual version"})
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if !h.requireProjectAccess(c, projectID) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	version, err := h.repo.CreateVersion(ctx, projectID, kind, name, h.versionAutoKeep(c))
	if err != nil {
		if errors.Is(err, services.ErrStudioNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
			return
		}
		log.Printf("studio versions: create failed for project %s: %v", projectID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create version"})
		return
	}
	c.JSON(http.StatusCreated, version)
}

// POST /api/studio/projects/:id/versions/:versionId/restore — checkpoint the
// CURRENT state first (auto, "Before restore"), then overwrite the project's
// tracks + captions sidecar from the version and bump revision. Returns the
// full updated project; the client replaces its document + revision and
// resets undo history. Because of the leading checkpoint, restore itself can
// never lose work.
func (h *StudioHandler) RestoreStudioVersion(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if !h.requireProjectAccess(c, projectID) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Current project: restore replaces only tracks + captions; name/fps/
	// dimensions stay whatever they are now.
	project, err := h.repo.GetProject(ctx, projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	// Validate the target BEFORE checkpointing, or a typo'd versionId would
	// mint a junk "Before restore" checkpoint on every attempt. Scoped by
	// project id, so a version from someone else's project is a plain 404.
	version, err := h.repo.GetVersion(ctx, projectID, strings.TrimSpace(c.Param("versionId")))
	if err != nil {
		if errors.Is(err, services.ErrStudioNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Version not found"})
			return
		}
		log.Printf("studio versions: load version failed for project %s: %v", projectID, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "Version not found"})
		return
	}

	updated, err := restoreStudioVersion(
		func() error {
			_, cErr := h.repo.CreateVersion(ctx, projectID, models.StudioVersionKindAuto, "Before restore", h.versionAutoKeep(c))
			return cErr
		},
		func() (*models.StudioProject, error) {
			// The ordinary save path: sanitizes, recomputes duration, writes
			// tracks + sidecar atomically and bumps revision. No
			// ExpectedRevision — restore is a deliberate overwrite, and the
			// bump still flips any concurrent tab into its 409 flow.
			return h.repo.SaveProject(ctx, projectID, models.StudioSaveProjectRequest{
				Name:            project.Name,
				FPS:             project.FPS,
				Width:           project.Width,
				Height:          project.Height,
				Tracks:          version.Tracks,
				Captions:        version.Captions,
				CaptionStyle:    version.CaptionStyle,
				CaptionsEnabled: version.CaptionsEnabled,
				Audio:           version.Audio,
				Markers:         version.Markers,
			})
		},
	)
	if err != nil {
		if errors.Is(err, services.ErrStudioNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
			return
		}
		log.Printf("studio versions: restore failed for project %s: %v", projectID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to restore version"})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// restoreStudioVersion is the restore composition: checkpoint the current
// state, and only if that lands, apply the version. A failed checkpoint MUST
// abort the restore — overwriting the working document without a recoverable
// snapshot of it is exactly the data loss this feature exists to prevent.
// Standalone (not a repository special, per the design) and injected so the
// ordering is unit-testable without Postgres.
func restoreStudioVersion(
	checkpoint func() error,
	apply func() (*models.StudioProject, error),
) (*models.StudioProject, error) {
	if err := checkpoint(); err != nil {
		return nil, fmt.Errorf("checkpoint before restore: %w", err)
	}
	return apply()
}
