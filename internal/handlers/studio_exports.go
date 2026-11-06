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

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
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

// validateExportRequest normalizes req in place, resolves its output against
// the output table (MM118: container × video × audio, `format` as an alias)
// and returns a client-facing message for an invalid value ("" when valid).
// Unknown formats / modes / combinations are a 400 rather than a quiet
// fallback, so a typo never yields the wrong file.
func validateExportRequest(req *models.StudioExportRequest) (services.StudioOutput, string) {
	req.Format = strings.ToLower(strings.TrimSpace(req.Format))
	out, err := services.ResolveStudioOutput(exportOutputRequest(*req))
	if err != nil {
		return services.StudioOutput{}, err.Error()
	}
	req.CaptionsMode = strings.ToLower(strings.TrimSpace(req.CaptionsMode))
	switch req.CaptionsMode {
	case "", models.StudioCaptionsNone, models.StudioCaptionsBurnIn, models.StudioCaptionsSidecar:
	case models.StudioCaptionsEmbedded:
		// MM119: a subtitle stream needs a container that carries one.
		if out.SubtitleCodec() == "" {
			return services.StudioOutput{}, "Embedded captions need a video container (MP4, MOV, MKV or WebM); audio-only files cannot carry a subtitle track"
		}
	default:
		return services.StudioOutput{}, "Unsupported captions mode"
	}
	if !studioExportResolutionRungs[req.Resolution] {
		return services.StudioOutput{}, "Unsupported export resolution"
	}
	return out, ""
}

// exportOutputRequest is the output half of an export body.
func exportOutputRequest(req models.StudioExportRequest) services.StudioOutputRequest {
	return services.StudioOutputRequest{
		Format: req.Format, Preset: req.OutputPreset,
		Container: req.Container, VideoCodec: req.VideoCodec, AudioCodec: req.AudioCodec,
		AudioBitrate: req.AudioBitrate, AudioChannels: req.AudioChannels,
	}
}

// storedExportRequest rebuilds the request of a history row for render-again
// and its gates: the stored output parts (filled from `format` for a pre-MM118
// row) as explicit fields, never the alias.
func storedExportRequest(e *models.StudioExport) models.StudioExportRequest {
	req := models.StudioExportRequest{
		FileName: e.FileName, Preset: e.Preset, Loudness: e.Loudness,
		Resolution: e.Resolution, CaptionsMode: e.CaptionsMode,
		Container: e.Container, VideoCodec: e.VideoCodec, AudioCodec: e.AudioCodec,
	}
	if req.Container == "" {
		req.Format = e.Format
	}
	if e.AudioBitrate != nil {
		req.AudioBitrate = *e.AudioBitrate
	}
	if e.AudioChannels != nil {
		req.AudioChannels = *e.AudioChannels
	}
	return req
}

// effectiveExportHeight is the height an export request will actually render:
// 0 for audio-only outputs (no video, nothing to cap), the picked rung when
// there is one, else the persisted project's height. The ONE input to the tier
// gate for every export path (ExportHeightGate, launchExport, render-again).
// An unresolvable output is treated as video (the handler 400s it anyway).
func effectiveExportHeight(req models.StudioExportRequest, projectHeight int) int {
	if out, err := services.ResolveStudioOutput(exportOutputRequest(req)); err == nil && out.AudioOnly() {
		return 0
	}
	if req.Resolution > 0 {
		return req.Resolution
	}
	return projectHeight
}

// peekExportRequest reads the JSON body for the pre-quota gates without
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
	_, _ = validateExportRequest(&req) // normalize case; an invalid value is rejected later by the handler
	return req
}

// exportContainer maps a legacy format (or derived label) onto the output
// extension, the MIME the job records, and the analytics media kind. MM118:
// a table lookup (StudioOutput.File); kept for callers holding only a label.
func exportContainer(format string) (ext, mime, mediaKind string) {
	return services.LegacyStudioOutput(format).File()
}

// --- Darkroom output plan gate (MM118) -------------------------------------

// studioFormatRefusal is the handler-side entry to the output plan rule
// (tiers.CheckStudioOutput): nil when the caller may render out, or when no
// tier has been resolved (tier system off, or a route outside /api), the same
// fallback as outputHeightRefusal. registry supplies `required`; nil falls
// back to the output table's default ladder.
func studioFormatRefusal(c *gin.Context, registry *tiers.Registry, out services.StudioOutput, captionsMode string) *tiers.StudioFormatRefusal {
	res, ok := middleware.ResolveTier(c)
	if !ok {
		return nil
	}
	return tiers.CheckStudioOutput(res.Tier, res.Limits, exportTokens(out, captionsMode), out.Advanced, studioRequiredFor(registry))
}

// exportTokens are the gated tokens of an export: the output's, plus
// subs_embedded for the embedded captions mode (MM119).
func exportTokens(out services.StudioOutput, captionsMode string) []string {
	toks := out.Tokens()
	if strings.EqualFold(strings.TrimSpace(captionsMode), models.StudioCaptionsEmbedded) {
		toks = append(toks, services.StudioTokenSubsEmbedded)
	}
	return toks
}

// studioRequiredFor resolves the lowest public plan for an output token or
// "advanced": the live registry when there is one, else the table defaults.
func studioRequiredFor(registry *tiers.Registry) func(string) tiers.Tier {
	if registry != nil {
		return registry.RequiredFor
	}
	tab := services.StudioOutputs()
	return func(option string) tiers.Tier {
		if option == tiers.StudioOptionAdvanced {
			return tiers.Tier(tab.AdvancedMinTier)
		}
		return tiers.Tier(tab.MinTier[option])
	}
}

// refuseStudioFormat writes the 403 format_not_in_plan body and reports
// whether it did.
func refuseStudioFormat(c *gin.Context, registry *tiers.Registry, out services.StudioOutput, captionsMode string) bool {
	ref := studioFormatRefusal(c, registry, out, captionsMode)
	if ref == nil {
		return false
	}
	c.AbortWithStatusJSON(http.StatusForbidden, ref)
	return true
}

// StudioExportFacts is what the pre-quota export gate needs to know about one
// export request: the height it renders and its resolved output.
type StudioExportFacts struct {
	Height   int
	Output   services.StudioOutput
	OutputOK bool // false: the output is invalid; the handler answers 400
	// CaptionsMode is the normalized requested mode (MM119: `embedded` is a
	// gated token too).
	CaptionsMode string
}

// StudioExportGate is the pre-quota half of BOTH Studio export plan rules
// (MM18 output height, MM118 output format). The quota dispatcher runs it
// BEFORE RequireQuota, so a refused export never spends the allowance.
// lookup returns ok=false when the facts cannot be determined (no DB, unknown
// project, not the caller's); the gate then stays out of the way and the
// handler decides (and a 4xx refunds the charge).
func StudioExportGate(registry *tiers.Registry, lookup func(c *gin.Context) (StudioExportFacts, bool)) gin.HandlerFunc {
	return func(c *gin.Context) {
		facts, ok := lookup(c)
		if !ok {
			return
		}
		if refuseOutputHeight(c, facts.Height) {
			return
		}
		if facts.OutputOK {
			refuseStudioFormat(c, registry, facts.Output, facts.CaptionsMode)
		}
	}
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
	// MM118: the stored output parts replay as explicit fields, and
	// launchExport re-runs both plan gates, so a downgraded account cannot
	// replay an export its plan no longer includes.
	h.launchExport(c, projectID, storedExportRequest(stored))
}

// ExportOptions handles GET /api/studio/export-options (MM118): the output
// table, the live minimum plan of every output token (TIER_*_STUDIO_FORMATS
// may move one from the table's defaults) and the caller's own allowed set, so
// the export dialog never hard-codes a combination. Public (no sign-in) and
// cacheable by the caller's browser only: the `caller` block is per request.
//
//	200 {"table": StudioOutputTable, "required": {"mp4": "anonymous", ...,
//	     "advanced": "premium"}, "caller": {"tier", "formats", "advanced"}}
func (h *StudioHandler) ExportOptions(c *gin.Context) {
	tab := services.StudioOutputs()
	req := studioRequiredFor(h.tierRegistry)
	required := make(map[string]string, len(tab.Tokens)+1)
	for _, tok := range tab.Tokens {
		required[tok] = string(req(tok))
	}
	required[tiers.StudioOptionAdvanced] = string(req(tiers.StudioOptionAdvanced))
	caller := gin.H{"tier": string(tiers.TierAnonymous), "formats": []string{}, "advanced": false}
	if res, ok := middleware.ResolveTier(c); ok {
		caller = gin.H{"tier": string(res.Tier), "formats": res.Limits.StudioFormats.List(), "advanced": res.Limits.StudioAdvanced}
	} else if h.tierRegistry != nil {
		l := h.tierRegistry.For(tiers.TierAnonymous)
		caller["formats"], caller["advanced"] = l.StudioFormats.List(), l.StudioAdvanced
	}
	c.Header("Cache-Control", "private, max-age=300")
	c.Header("Vary", "Authorization")
	c.JSON(http.StatusOK, gin.H{"table": tab, "required": required, "caller": caller})
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
