package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM71: POST /api/studio/assets/:id/rebuild-proxy re-encodes an existing video
// asset's preview proxy from its ORIGINAL with the current proxy args (MM70's
// 1 s keyframe cadence), so assets imported before MM70 get cheap preview
// seeks without being imported again.
//
// Proxies are served with a week of private caching (MM70), so a rebuilt
// proxy is never written over the old key: it goes to the next generation key
// (<assetId>_proxy_g<N>.mp4, services.NextStudioVideoProxyKey) and the editor
// versions the URL with the generation. Sequence (runProxyRebuild):
//
//  1. download the original, encode, upload to the NEW key (registered in the
//     retention ledger with the original as parent, like every proxy);
//  2. swap the asset row onto the new key in one compare-and-set statement
//     that also clears s3_key_proxy_reversed (built from the old proxy);
//  3. only then retire the old proxy and old reversed proxy: their ledger rows
//     are expired and swept inline (the MM24 path), or, for a pre-ledger
//     asset, the objects are deleted directly.
//
// A swap that does not happen (asset deleted or changed meanwhile) retires the
// NEW key instead, so nothing is orphaned and the row is never clobbered.

// studioProxyRebuildConcurrency bounds how many rebuild encodes run at once
// on the Darkroom GPU; further jobs wait in "pending" (a "rebuild all" in a
// large project must not start twenty full-resolution decodes together).
const studioProxyRebuildConcurrency = 2

// studioProxyRebuildSlots is process-wide: one API process, one GPU.
var studioProxyRebuildSlots = make(chan struct{}, studioProxyRebuildConcurrency)

// proxyRebuildInFlight is one asset's running rebuild.
type proxyRebuildInFlight struct {
	jobID      string
	generation int
}

// proxyRebuildRegistry makes the endpoint idempotent per asset: a second
// request while a rebuild runs gets the running job back instead of a second
// encode. Process-local, like the peaks backfill dedupe; the compare-and-set
// swap is the cross-restart guard.
type proxyRebuildRegistry struct {
	mu      sync.Mutex
	running map[string]proxyRebuildInFlight
}

// claim registers a rebuild for assetID unless one is already running, in
// which case it returns that one and claimed=false.
func (r *proxyRebuildRegistry) claim(assetID string, next proxyRebuildInFlight) (proxyRebuildInFlight, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.running[assetID]; ok {
		return cur, false
	}
	if r.running == nil {
		r.running = map[string]proxyRebuildInFlight{}
	}
	r.running[assetID] = next
	return next, true
}

func (r *proxyRebuildRegistry) release(assetID string) {
	r.mu.Lock()
	delete(r.running, assetID)
	r.mu.Unlock()
}

// StudioProxyRebuildResponse is the rebuild-proxy response: the asset as it is
// now (still on the old proxy), the job to watch, and the proxy generation the
// asset will have once the job completes.
type StudioProxyRebuildResponse struct {
	Asset           *models.StudioAsset `json:"asset"`
	JobID           string              `json:"jobId"`
	ProxyGeneration int                 `json:"proxyGeneration"`
}

// requireAssetOwner is the MM24 strict owner check for an asset-scoped
// mutation (the rule DeleteAsset uses): the asset's project must belong to the
// caller's account, or, for a guest, to their session with no account owner.
// 503 without the deletion store, 401 with no identity, 404 otherwise.
func (h *StudioHandler) requireAssetOwner(c *gin.Context, assetID string) (string, bool) {
	if h.projectStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Content Studio requires a database, which is currently unavailable"})
		return "", false
	}
	userID, session := callerIdentity(c)
	if userID == "" && session == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return "", false
	}
	ctx := c.Request.Context()
	projectID, found, err := h.projectStore.AssetProject(ctx, assetID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Content Studio is temporarily unavailable"})
		return "", false
	}
	var rowUser, rowSession string
	if found {
		rowUser, rowSession, found, err = h.projectStore.ProjectOwner(ctx, projectID)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Content Studio is temporarily unavailable"})
			return "", false
		}
	}
	if !found || !ownsProjectRow(rowUser, rowSession, userID, session) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return "", false
	}
	return projectID, true
}

// RebuildProxy handles POST /api/studio/assets/:id/rebuild-proxy.
//
//	200 {asset, jobId, proxyGeneration}  job started, or already running (same jobId)
//	400 not a video asset
//	401 no account and no session header
//	404 not the owner / no such asset
//	409 the asset has no preview proxy yet (ingest still running)
//	503 no database / no S3
//
// The client watches the job like an ingest (/api/job/:jobId); its result URL
// is the versioned proxy URL.
func (h *StudioHandler) RebuildProxy(c *gin.Context) {
	assetID := strings.TrimSpace(c.Param("id"))
	projectID, ok := h.requireAssetOwner(c, assetID)
	if !ok {
		return
	}
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	src, err := h.repo.GetAsset(ctx, assetID)
	if err != nil || src.ProjectID != projectID {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	if code, msg := proxyRebuildEligibility(src); code != 0 {
		c.JSON(code, gin.H{"error": msg})
		return
	}

	newKey, gen := services.NextStudioVideoProxyKey(src.S3KeyOriginal, src.ID, src.S3KeyProxy)
	jobID := newJobID()
	running, claimed := h.rebuilds.claim(src.ID, proxyRebuildInFlight{jobID: jobID, generation: gen})
	if !claimed {
		// Idempotent: the rebuild already running for this asset is the answer.
		c.JSON(http.StatusOK, StudioProxyRebuildResponse{Asset: src, JobID: running.jobID, ProxyGeneration: running.generation})
		return
	}
	job := h.jobManager.CreateJobWithID(jobID,
		models.OriginalFileInfo{Name: src.OriginalFileName, Size: 0, Type: "video/mp4"},
		map[string]interface{}{"mode": "studio_proxy_rebuild"},
	)
	_ = h.jobManager.SetMode(job.ID, "studio_proxy_rebuild")
	recordJobCreated(c, h.jobManager, job, services.JobFacts{
		Tool:         "studio_proxy_rebuild",
		Mode:         "studio_proxy_rebuild",
		MediaKind:    string(models.FileTypeVideo),
		SourceFormat: formatOf(src.OriginalFileName),
		TargetFormat: "mp4",
	})
	registerJobDirs(c, job.ID, true, false) // the original is staged under uploads/<jobId>/
	go h.runProxyRebuildJob(retentionScopeFor(c, job.ID, src.ProjectID), job.ID, src, newKey, gen)
	c.JSON(http.StatusOK, StudioProxyRebuildResponse{Asset: src, JobID: job.ID, ProxyGeneration: gen})
}

// proxyRebuildEligibility answers which assets may be rebuilt: video assets
// that already have a proxy. 0 = eligible.
func proxyRebuildEligibility(a *models.StudioAsset) (int, string) {
	if a.MediaKind != models.StudioMediaKindVideo {
		return http.StatusBadRequest, "Only video assets have a rebuildable preview"
	}
	if a.S3KeyProxy == "" {
		return http.StatusConflict, "The preview proxy is not ready yet"
	}
	return 0, ""
}

// proxyRebuildSteps is the I/O of one rebuild, injectable so the sequence
// (encode, swap, then retire) is testable without S3, ffmpeg or Postgres.
type proxyRebuildSteps struct {
	// encode downloads the original and uploads the rebuilt proxy to newKey.
	encode func(ctx context.Context, newKey string) error
	// swap points the asset at newKey iff it still holds oldKey, clearing the
	// reversed proxy; it returns the reversed key it cleared.
	swap func(ctx context.Context, oldKey, newKey string) (oldReversed string, swapped bool, err error)
	// retire removes S3 objects the asset no longer references.
	retire func(ctx context.Context, keys []string)
}

// errProxyRebuildSuperseded: the asset changed (or went away) during the
// rebuild, so the new proxy was discarded.
var errProxyRebuildSuperseded = errors.New("the media changed while its preview was rebuilding; nothing was replaced")

// runProxyRebuild is the rebuild sequence. The old keys are retired strictly
// AFTER the row points at the new one; a failed or skipped swap retires only
// the new key and leaves the asset exactly as it was.
func runProxyRebuild(ctx context.Context, st proxyRebuildSteps, oldKey, newKey string) error {
	if newKey == "" || newKey == oldKey {
		return fmt.Errorf("refusing to rebuild a proxy onto its own key %q", oldKey)
	}
	if err := st.encode(ctx, newKey); err != nil {
		// An upload that did land must not linger unreferenced.
		st.retire(context.WithoutCancel(ctx), []string{newKey})
		return err
	}
	oldReversed, swapped, err := st.swap(ctx, oldKey, newKey)
	if err != nil || !swapped {
		st.retire(context.WithoutCancel(ctx), []string{newKey})
		if err != nil {
			return fmt.Errorf("record rebuilt proxy: %w", err)
		}
		return errProxyRebuildSuperseded
	}
	retire := []string{oldKey}
	if oldReversed != "" {
		retire = append(retire, oldReversed)
	}
	st.retire(context.WithoutCancel(ctx), retire)
	return nil
}

func (h *StudioHandler) runProxyRebuildJob(scope retention.Scope, jobID string, asset *models.StudioAsset, newKey string, gen int) {
	defer h.rebuilds.release(asset.ID)
	ctx, cancel := context.WithTimeout(retention.WithScope(context.Background(), scope), h.cfg.CommandTimeout)
	defer cancel()
	select {
	case studioProxyRebuildSlots <- struct{}{}:
		defer func() { <-studioProxyRebuildSlots }()
	case <-ctx.Done():
		_ = h.jobManager.UpdateJobError(jobID, "Timed out waiting for the encoder")
		return
	}
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusProcessing); err != nil {
		log.Printf("studio rebuild: failed to mark job %s processing: %v", jobID, err)
		return
	}

	workDir := filepath.Join(h.cfg.UploadDir, jobID, "rebuild")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		_ = h.jobManager.UpdateJobError(jobID, "Failed to prepare workspace")
		return
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	steps := proxyRebuildSteps{
		encode: func(ctx context.Context, newKey string) error {
			inPath := filepath.Join(workDir, "original"+strings.ToLower(filepath.Ext(asset.S3KeyOriginal)))
			if err := h.downloadS3Object(ctx, asset.S3KeyOriginal, inPath); err != nil {
				log.Printf("studio rebuild: download original failed: %v", err)
				return errors.New("Failed to download the original media")
			}
			return h.ingest.RebuildVideoProxy(ctx, jobID, asset.S3KeyOriginal, asset.ID, newKey, inPath, asset.DurationSeconds)
		},
		swap: func(ctx context.Context, oldKey, newKey string) (string, bool, error) {
			return h.repo.SwapAssetProxy(ctx, asset.ID, oldKey, newKey)
		},
		retire: h.retireStudioKeys,
	}
	if err := runProxyRebuild(ctx, steps, asset.S3KeyProxy, newKey); err != nil {
		log.Printf("studio rebuild: job %s (asset %s) failed: %v", jobID, asset.ID, err)
		_ = h.jobManager.UpdateJobError(jobID, err.Error())
		return
	}
	_ = h.jobManager.UpdateJobResult(jobID, fmt.Sprintf("/api/studio/assets/%s/proxy?v=g%d", asset.ID, gen))
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusCompleted); err != nil {
		log.Printf("studio rebuild: failed to mark job %s completed: %v", jobID, err)
	}
}

// retireStudioKeys removes Darkroom S3 objects an asset no longer references.
// Each key's ledger row is expired and swept inline (MM24 ExpireNow, the same
// deleter the sweeper uses, which also stamps the row); a key with no ledger
// row (pre-ledger asset, or no deletion store) is deleted directly, and only
// when it sits under the Darkroom prefix. Best effort: a failure is logged and
// left to the sweeper or the S3 lifecycle backstop, never surfaced to the job.
func (h *StudioHandler) retireStudioKeys(ctx context.Context, keys []string) {
	prefix := h.studioPrefix() + "/"
	for _, key := range keys {
		if key == "" || !strings.HasPrefix(key, prefix) || strings.Contains(key, "..") {
			continue
		}
		if h.expirer != nil {
			sweepCtx, cancel := context.WithTimeout(ctx, deleteTimeout)
			res, err := h.expirer.ExpireNow(sweepCtx, telemetry.LedgerFilter{S3Key: key})
			cancel()
			if err == nil && res.Matched > 0 {
				if res.Pending > 0 {
					log.Printf("studio rebuild: old key %s left for the sweeper (pending %d)", key, res.Pending)
				}
				continue
			}
		}
		if failed := h.deleteKeysBestEffort(ctx, []string{key}); failed > 0 {
			log.Printf("studio rebuild: could not delete retired key %s; the S3 lifecycle rule backstops it", key)
		}
	}
}
