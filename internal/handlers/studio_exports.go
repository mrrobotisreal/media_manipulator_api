package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// Export depth (part 20): request validation, the effective-height rule shared
// with ExportHeightGate, the export history routes, render-again and the
// synchronous frame export. See docs/content-studio/design/02-target-design.md §13.

// studioExportResolutionRungs is the resolution ladder the UI offers. 0 means
// "the project's own canvas height". Every non-zero rung is checked against the
// tier cap by the same tiers.EnforceOutputHeight the canvas height uses; a
// rung the tier cannot render is a 403 output_height_limited, never a silent
// downgrade.
var studioExportResolutionRungs = map[int]bool{0: true, 720: true, 1080: true, 1440: true, 2160: true}

// validateExportRequest normalizes req in place and returns a client-facing
// message for an invalid value ("" when valid). Unknown formats / modes are a
// 400 rather than a quiet fallback, so a typo never yields the wrong file.
func validateExportRequest(req *models.StudioExportRequest) string {
	req.Format = strings.ToLower(strings.TrimSpace(req.Format))
	switch req.Format {
	case "", models.StudioExportFormatH264, models.StudioExportFormatHEVC, models.StudioExportFormatMP3, models.StudioExportFormatWAV:
	default:
		return "Unsupported export format"
	}
	req.CaptionsMode = strings.ToLower(strings.TrimSpace(req.CaptionsMode))
	switch req.CaptionsMode {
	case "", models.StudioCaptionsNone, models.StudioCaptionsBurnIn, models.StudioCaptionsSidecar:
	default:
		return "Unsupported captions mode"
	}
	if !studioExportResolutionRungs[req.Resolution] {
		return "Unsupported export resolution"
	}
	return ""
}

// effectiveExportHeight is the height an export request will actually render:
// 0 for audio formats (no video, nothing to cap), the picked rung when there is
// one, else the persisted project's height. The ONE input to the tier gate for
// every export path (ExportHeightGate, launchExport, render-again).
func effectiveExportHeight(req models.StudioExportRequest, projectHeight int) int {
	if services.IsAudioFormat(req.Format) {
		return 0
	}
	if req.Resolution > 0 {
		return req.Resolution
	}
	return projectHeight
}

// peekExportRequest reads the JSON body for the pre-quota height gate without
// consuming it (the handler binds it again). A bad body yields the zero
// request, i.e. the project's own height, exactly the pre-part-20 behavior.
func peekExportRequest(c *gin.Context) models.StudioExportRequest {
	var req models.StudioExportRequest
	if c.Request == nil || c.Request.Body == nil {
		return req
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
	_ = c.Request.Body.Close()
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return req
	}
	var parsed models.StudioExportRequest
	if jsonUnmarshalLoose(raw, &parsed) {
		req = parsed
	}
	_ = validateExportRequest(&req) // normalize case; an invalid value is rejected later by the handler
	return req
}

// exportContainer maps a format onto the output extension, the MIME the job
// records, and the analytics media kind.
func exportContainer(format string) (ext, mime, mediaKind string) {
	switch services.NormalizeExportFormat(format) {
	case services.StudioFormatMP3:
		return ".mp3", "audio/mpeg", string(models.FileTypeAudio)
	case services.StudioFormatWAV:
		return ".wav", "audio/wav", string(models.FileTypeAudio)
	}
	return ".mp4", "video/mp4", string(models.FileTypeVideo)
}

// effectiveCaptionsMode is the value stored on the history row: the explicit
// choice, or for the legacy empty request (CreaTV embed) what the render
// actually did: burn_in when captions went into the picture, else none.
func effectiveCaptionsMode(mode string, burned bool) string {
	if mode != "" {
		return mode
	}
	if burned {
		return models.StudioCaptionsBurnIn
	}
	return models.StudioCaptionsNone
}

// ListStudioExports handles GET /api/studio/projects/:id/exports.
//
//	200 {"exports":[StudioExport...]}   newest first
//	404 not the owner / no such project
//
// Each completed row carries `available`: true while the artifact is still on
// disk (inside its retention window), so the panel can offer re-download only
// when it will work. A 'running' row whose job this process no longer knows (the
// API restarted mid-render) is reconciled to failed/interrupted here, so the
// panel can never spin forever on a dead job.
func (h *StudioHandler) ListStudioExports(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if !h.requireProjectAccess(c, projectID) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	rows, err := h.repo.ListExports(ctx, projectID)
	if err != nil {
		log.Printf("studio exports: list failed for project %s: %v", projectID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list exports"})
		return
	}
	stale := services.StaleExportAfter(h.cfg.CommandTimeout)
	for _, e := range rows {
		switch e.Status {
		case models.StudioExportRunning:
			if _, jobErr := h.jobManager.GetJob(e.JobID); jobErr != nil {
				// Not in this process's memory => it died with the last process.
				e.Status, e.Error = models.StudioExportFailed, "interrupted"
				if sErr := h.repo.SettleExport(ctx, e.JobID, false, "interrupted"); sErr != nil {
					log.Printf("studio exports: reconcile %s failed: %v", e.JobID, sErr)
				}
			} else if time.Since(e.CreatedAt) > stale {
				e.Status, e.Error = models.StudioExportFailed, "timeout"
				_ = h.repo.SettleExport(ctx, e.JobID, false, "timeout")
			}
		case models.StudioExportCompleted:
			e.Available = findResultFile(h.cfg.OutputDir, e.JobID) != ""
		}
	}
	c.JSON(http.StatusOK, gin.H{"exports": rows})
}

// RenderExportAgain handles POST /api/studio/projects/:id/exports/:exportId/again.
// It re-submits the stored options as a NEW export (new job, new row, new
// ledger registration) of the project's CURRENT saved state. It rides the same
// quota (export allowance) and the same output-height gate as a fresh export,
// because it is one. 404 for a stranger or an unknown export.
func (h *StudioHandler) RenderExportAgain(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if !h.requireProjectAccess(c, projectID) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	stored, err := h.repo.GetExport(ctx, projectID, strings.TrimSpace(c.Param("exportId")))
	cancel()
	if err != nil {
		if errors.Is(err, services.ErrStudioNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Export not found"})
			return
		}
		log.Printf("studio exports: load %s failed: %v", c.Param("exportId"), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load export"})
		return
	}
	loudness := stored.Loudness
	h.launchExport(c, projectID, models.StudioExportRequest{
		FileName: stored.FileName, Preset: stored.Preset, Loudness: loudness,
		Format: stored.Format, Resolution: stored.Resolution, CaptionsMode: stored.CaptionsMode,
	})
}

// frameSlots bounds concurrent synchronous frame renders (each is a full
// ffmpeg graph on the render GPU). A burst beyond it answers 503 + Retry-After
// instead of queueing request goroutines behind the GPU.
var frameSlots = make(chan struct{}, 2)

const frameRenderTimeout = 90 * time.Second

// ExportFrame handles POST /api/studio/projects/:id/frame {t, fileName?, captions?}.
// It renders the ONE frame at timeline time t through the export pipeline's
// graph from the ORIGINAL media (not the proxy) and streams a PNG back. The
// frame is transient: it is written under the temp dir, streamed, and removed,
// so nothing is stored and nothing is registered with the retention ledger.
//
// Gating: the frame is the project's canvas size, so it is held to the same
// tier output-height cap as an export (refuseOutputHeight). It is NOT counted
// against the daily export allowance (it is not a render job) and instead rides
// the analysis rate-limit bucket (see routeLimitDispatcher).
func (h *StudioHandler) ExportFrame(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	var req models.StudioFrameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if req.T < 0 || req.T != req.T || req.T > 24*3600 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid frame time"})
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if !h.requireProjectAccess(c, projectID) {
		return
	}
	loadCtx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	project, err := h.repo.GetProject(loadCtx, projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	if refuseOutputHeight(c, project.Height) {
		return
	}
	assets, err := h.repo.ListAssets(loadCtx, projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load project media"})
		return
	}
	assetByID := make(map[string]*models.StudioAsset, len(assets))
	for _, a := range assets {
		assetByID[a.ID] = a
	}
	project.Tracks = models.SanitizeTracksForAssets(models.SanitizeTracks(project.Tracks), studioAssetKinds(assetByID))
	refs, duration, ok := services.CollectExportRefs(project, assetByID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Project has no clip to export"})
		return
	}
	t := req.T
	if t >= duration {
		t = duration - 1.0/maxFloat(project.FPS, 1)
	}
	if t < 0 {
		t = 0
	}

	select {
	case frameSlots <- struct{}{}:
		defer func() { <-frameSlots }()
	default:
		c.Header("Retry-After", "5")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Frame export is busy, try again in a moment"})
		return
	}

	workDir, err := os.MkdirTemp(h.cfg.TempDir, "studio_frame_")
	if err != nil {
		// TempDir may not exist yet on a fresh host.
		_ = os.MkdirAll(h.cfg.TempDir, 0o755)
		workDir, err = os.MkdirTemp(h.cfg.TempDir, "studio_frame_")
	}
	if err != nil {
		log.Printf("studio frame: temp dir failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare frame export"})
		return
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	ctx, cancelRender := context.WithTimeout(c.Request.Context(), frameRenderTimeout)
	defer cancelRender()

	visible := services.FrameRefs(refs, t)
	localByKey := map[string]string{}
	for _, r := range visible {
		if r.Clip.Title != nil {
			continue
		}
		key := r.Asset.S3KeyOriginal
		if _, done := localByKey[key]; done {
			continue
		}
		local := filepath.Join(workDir, fmt.Sprintf("src_%d%s", len(localByKey), strings.ToLower(filepath.Ext(key))))
		if err := h.downloadS3Object(ctx, key, local); err != nil {
			log.Printf("studio frame: download original failed: %v", err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to download source media"})
			return
		}
		localByKey[key] = local
	}
	exportCfg := buildStudioExportConfig(project, assetByID, models.StudioExportRequest{CaptionsMode: captionsModeForFrame(req.Captions)})
	lutLocal := map[string]string{}
	for assetID, key := range exportCfg.lutKeys {
		local := filepath.Join(workDir, "lut_"+sanitizeS3PathSegment(assetID)+".cube")
		if err := h.downloadS3Object(ctx, key, local); err == nil {
			lutLocal[assetID] = local
		}
	}
	plan := services.AssembleExportPlan(visible, services.StudioPlanSpec{
		Width: project.Width, Height: project.Height, FPS: project.FPS, Duration: duration,
		LocalByKey: localByKey, LutPaths: lutLocal,
	})
	if len(exportCfg.captions) > 0 {
		assPath := filepath.Join(workDir, "captions.ass")
		if err := os.WriteFile(assPath, []byte(services.BuildASS(exportCfg.captions, exportCfg.captionStyle, project.Width, project.Height, "")), 0o644); err == nil {
			plan.CaptionsASSPath = assPath
		}
	}
	if cmds := services.GenerateStudioSendCmd(plan); cmds != "" {
		cmdPath := filepath.Join(workDir, "keyframes.cmd")
		if err := os.WriteFile(cmdPath, []byte(cmds), 0o644); err == nil {
			plan.SendCmdPath = cmdPath
		}
	}

	outPath := filepath.Join(workDir, "frame-"+uuid.NewString()+".png")
	if err := h.export.RunFrame(ctx, plan, t, outPath); err != nil {
		log.Printf("studio frame: render failed for project %s: %v", projectID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to render frame"})
		return
	}
	f, err := os.Open(outPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to render frame"})
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to render frame"})
		return
	}
	name := safeFilename(firstNonEmpty(req.FileName, project.Name, "frame"))
	name = strings.TrimSuffix(name, filepath.Ext(name)) + ".png"
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "image/png")
	c.Header("Content-Length", fmt.Sprintf("%d", info.Size()))
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, f)
}

func captionsModeForFrame(burn bool) string {
	if burn {
		return models.StudioCaptionsBurnIn
	}
	return models.StudioCaptionsNone
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// jsonUnmarshalLoose decodes raw into v, reporting success. Unknown fields are
// ignored (the same leniency as c.ShouldBindJSON).
func jsonUnmarshalLoose(raw []byte, v any) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}
