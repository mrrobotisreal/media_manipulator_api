package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// Part 21: media bin organization (PATCH) and asset delete.
//
// Division of labour on delete: the SERVER does not parse project JSONB for
// clip references. The client owns usage protection: it holds the document, so
// it lists the clips that reference an asset, removes them as one undoable
// entry, and only then calls DELETE. A client that skips that leaves clips
// pointing at a dead asset, which the editor renders as "offline" clips.

// studioLabelPalette is the fixed set of bin label color slugs (UI mirrors it in
// lib/studio/mediaBin.ts).
var studioLabelPalette = map[string]bool{
	"red": true, "orange": true, "yellow": true, "green": true, "teal": true,
	"blue": true, "purple": true, "pink": true, "gray": true,
}

const (
	studioMaxDisplayNameRunes = 120
	studioMaxFolderRunes      = 40
)

// StudioAssetPatchRequest is the body of PATCH /api/studio/assets/:id. Absent
// fields are left alone; an empty string clears the field.
type StudioAssetPatchRequest struct {
	DisplayName *string `json:"displayName"`
	Folder      *string `json:"folder"`
	Label       *string `json:"label"`
}

// cleanBinText strips control characters, collapses whitespace runs and clamps
// the length (in runes).
func cleanBinText(s string, maxRunes int) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		if unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteRune(' ')
			}
			lastSpace = true
			continue
		}
		lastSpace = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) > maxRunes {
		out = strings.TrimSpace(string([]rune(out)[:maxRunes]))
	}
	return out
}

// sanitizeAssetPatch validates and normalizes a patch. ok=false means the label
// is not in the palette (400).
func sanitizeAssetPatch(req StudioAssetPatchRequest) (services.StudioAssetMetaPatch, bool) {
	var p services.StudioAssetMetaPatch
	if req.DisplayName != nil {
		v := cleanBinText(*req.DisplayName, studioMaxDisplayNameRunes)
		p.DisplayName = &v
	}
	if req.Folder != nil {
		// A folder is a single-level name: no path separators.
		v := cleanBinText(strings.NewReplacer("/", " ", "\\", " ").Replace(*req.Folder), studioMaxFolderRunes)
		p.Folder = &v
	}
	if req.Label != nil {
		v := strings.ToLower(strings.TrimSpace(*req.Label))
		if v != "" && !studioLabelPalette[v] {
			return p, false
		}
		p.Label = &v
	}
	return p, true
}

// PatchAsset handles PATCH /api/studio/assets/:id. Ownership is the asset's
// project (a stranger gets 404).
func (h *StudioHandler) PatchAsset(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	var req StudioAssetPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if req.DisplayName == nil && req.Folder == nil && req.Label == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nothing to update"})
		return
	}
	patch, ok := sanitizeAssetPatch(req)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown label"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	id := strings.TrimSpace(c.Param("id"))
	asset, err := h.repo.GetAsset(ctx, id)
	if err != nil {
		if errors.Is(err, services.ErrStudioNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
			return
		}
		log.Printf("studio: patch asset lookup failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update asset"})
		return
	}
	if !h.requireProjectAccess(c, asset.ProjectID) {
		return
	}
	updated, err := h.repo.UpdateAssetMeta(ctx, id, patch)
	if err != nil {
		log.Printf("studio: patch asset failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update asset"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"asset": updated})
}

// DeleteAsset handles DELETE /api/studio/assets/:id.
//
//	200 {"deleted":{"objects":N,"bytes":N},"pending":N}
//	401 no account and no session header
//	404 not the owner / no such asset
//
// The row and every S3 key (original, proxy, sprite, peaks, reversed and other
// ledger-registered derivatives) go through the sweeper's studio_asset deleter,
// the same one project deletion and expiry use. An asset from before the ledger
// (no studio_asset ledger row) falls back to deleting its row and keys directly.
func (h *StudioHandler) DeleteAsset(c *gin.Context) {
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
	projectID, found, err := h.projectStore.AssetProject(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	var rowUser, rowSession string
	if found {
		rowUser, rowSession, found, err = h.projectStore.ProjectOwner(c.Request.Context(), projectID)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
			return
		}
	}
	if !found || !ownsProjectRow(rowUser, rowSession, userID, session) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	res, err := h.expirer.ExpireNow(ctx, ledgerFilterForAsset(id))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	if res.Matched == 0 {
		// Pre-ledger asset: no ledger row to sweep. Remove its objects, then the row.
		keys, rowFound, kerr := h.projectStore.StudioAssetKeys(ctx, id)
		if kerr == nil && rowFound {
			failed := h.deleteKeysBestEffort(ctx, keys)
			if failed == 0 {
				if derr := h.projectStore.DeleteStudioAssetRow(ctx, id); derr != nil {
					res.Pending++
				} else {
					res.Objects += len(keys)
				}
			} else {
				res.Pending += failed
			}
		} else if kerr != nil {
			res.Pending++
		}
	} else {
		// The ledger row is gone only when the sweep finished; make sure the row
		// itself does not linger if the sweeper skipped it.
		if res.Pending == 0 {
			_ = h.projectStore.DeleteStudioAssetRow(ctx, id)
		}
	}
	if h.onDelete != nil {
		h.onDelete("asset")
	}
	c.JSON(http.StatusOK, newDeletedBody(res))
}

// deleteKeysBestEffort deletes keys from S3 and returns how many failed.
func (h *StudioHandler) deleteKeysBestEffort(ctx context.Context, keys []string) int {
	if h.s3Client == nil || h.cfg == nil {
		return len(keys)
	}
	failed := 0
	for _, k := range keys {
		if _, err := h.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(h.cfg.S3Bucket), Key: aws.String(k),
		}); err != nil {
			log.Printf("studio: legacy asset key delete failed (%s): %v", k, err)
			failed++
		}
	}
	return failed
}
