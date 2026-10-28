package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/validation"
)

// VideoSummaryHandler serves /api/summarize/* (MM74, design
// docs/design/VIDEO_SUMMARY.md §4, ADR-0014). It owns its own service and
// serial queue (SUMMARY_WORKERS); job status and stages[] go through the
// shared /api/job/:jobId (+ SSE) machinery.
//
//	GET  /api/summarize/capabilities
//	POST /api/summarize/start                multipart `file` + `options`, or JSON {s3Key,fileName,fileSizeBytes,options}
//	GET  /api/summarize/:jobId/result?format=md|json
//
// /api/summarize/start is quota-gated as ai_ops in cmd/api (quotaRoutes) and
// sign-in gated BEFORE the charge (SignInGate), so an anonymous request is
// refused without spending or refunding anything.
type VideoSummaryHandler struct {
	jobManager *services.JobManager
	cfg        *config.Config
	registry   *tiers.Registry
	svc        *services.VideoSummaryService
	s3Client   *s3.Client

	// Injectable for tests (no ffprobe/Ollama/S3 on the MacBook).
	probe     func(ctx context.Context, path string) (*models.VideoProbeResponse, error)
	enqueue   func(services.VideoSummaryRequest)
	reachable func(ctx context.Context, baseURL string) bool
	fetchS3   func(ctx context.Context, key, path string, maxBytes int64) (int64, error)
}

// NewVideoSummaryHandler wires the service. s3Client and gpuMgr may be nil
// (no presigned path / no lease accounting).
func NewVideoSummaryHandler(jobManager *services.JobManager, cfg *config.Config, registry *tiers.Registry, s3Client *s3.Client, gpuMgr *gpu.Manager) *VideoSummaryHandler {
	vs := cfg.VideoSummary
	svc := services.NewVideoSummaryService(services.VideoSummarySettings{
		OllamaURL:      vs.OllamaURL,
		VLMModel:       cfg.OllamaVLMModel,
		SummaryModel:   vs.SummaryModel,
		DeepModel:      vs.DeepModel,
		DeepURL:        vs.DeepURL,
		MaxInputTokens: vs.MaxInputTokens,
		Workers:        vs.Workers,
		StageTimeout:   vs.StageTimeout,
		JobTimeout:     cfg.CommandTimeout,
		OllamaTimeout:  vs.OllamaTimeout,
	}, jobManager, gpuMgr)
	h := &VideoSummaryHandler{
		jobManager: jobManager,
		cfg:        cfg,
		registry:   registry,
		svc:        svc,
		s3Client:   s3Client,
		probe:      services.ProbeVideoReport,
		enqueue:    svc.Enqueue,
		reachable:  services.OllamaBaseReachable,
	}
	h.fetchS3 = h.fetchFromS3
	return h
}

// Start launches the SUMMARY_WORKERS queue workers.
func (h *VideoSummaryHandler) Start() {
	if h != nil && h.svc != nil {
		h.svc.Start()
	}
}

// RegisterVideoSummaryRoutes mounts the summarize endpoints. A nil handler
// mounts nothing.
func RegisterVideoSummaryRoutes(r gin.IRouter, h *VideoSummaryHandler) {
	if h == nil {
		return
	}
	r.GET("/summarize/capabilities", h.GetCapabilities)
	r.POST("/summarize/start", h.Start202)
	r.GET("/summarize/:jobId/result", h.GetResult)
}

// SummarizeSignInGate is the pre-quota gate for POST /api/summarize/start: an
// anonymous caller (or any tier whose SummaryMaxFrames is 0) gets 403
// {"error":"signin_required"} BEFORE the ai_ops unit is charged, so there is
// nothing to refund. With the tier system off (no resolution) it stays out of
// the way and the handler decides. cmd/api runs it ahead of RequireQuota for
// this path only.
func SummarizeSignInGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		res, ok := middleware.ResolveTier(c)
		if !ok {
			return
		}
		if res.Limits.SummaryMaxFrames <= 0 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":       "signin_required",
				"message":     "Sign in to summarize videos",
				"tier":        string(res.Tier),
				"upgradePath": "signup",
			})
		}
	}
}

// GetCapabilities reports whether the feature is usable and the per-tier
// budgets. Ollama reachability is probed like /video-transcode/capabilities.
func (h *VideoSummaryHandler) GetCapabilities(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	settings := h.svc.Settings()
	vs := h.cfg.VideoSummary
	resp := models.VideoSummaryCapabilitiesResponse{
		VLMModel:        settings.VLMModel,
		SummaryModel:    settings.SummaryModel,
		MaxFramesByTier: map[string]int{},
		DeepTiers:       []string{},
		OutputLanguages: models.VideoSummaryOutputLanguages(),
	}
	if vs.Enabled {
		resp.Enabled = h.reachable(ctx, settings.OllamaURL)
	}
	if vs.Enabled && settings.DeepAvailable() {
		resp.DeepModel = settings.DeepModel
		resp.Deep = h.reachable(ctx, settings.DeepURL)
	}
	for _, t := range tiers.All() {
		l := h.registry.For(t)
		resp.MaxFramesByTier[string(t)] = l.SummaryMaxFrames
		if l.SummaryDeep {
			resp.DeepTiers = append(resp.DeepTiers, string(t))
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, resp)
}

// summaryLimits is the caller's frame budget and deep entitlement. With the
// tier system off (TIER_QUOTA_ENABLED=false) the Free row applies: the
// feature must not become unlimited just because tiers are switched off.
func (h *VideoSummaryHandler) summaryLimits(c *gin.Context) (frames int, deep bool, maxSeconds int) {
	if res, ok := middleware.ResolveTier(c); ok {
		return res.Limits.SummaryMaxFrames, res.Limits.SummaryDeep, res.Limits.MaxVideoSeconds
	}
	l := h.registry.For(tiers.TierFree)
	return l.SummaryMaxFrames, l.SummaryDeep, 0
}

// EffectiveSummaryDepth downgrades "deep" to "standard" unless the tier has
// SummaryDeep and deep synthesis is configured. Unknown depths are an error.
func EffectiveSummaryDepth(requested string, tierDeep, deepConfigured bool) (string, error) {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "", models.VideoSummaryDepthStandard:
		return models.VideoSummaryDepthStandard, nil
	case models.VideoSummaryDepthDeep:
		if tierDeep && deepConfigured {
			return models.VideoSummaryDepthDeep, nil
		}
		return models.VideoSummaryDepthStandard, nil
	default:
		return "", fmt.Errorf("depth must be standard or deep")
	}
}

// summaryInput is a staged upload ready for probing.
type summaryInput struct {
	path      string
	fileName  string
	size      int64
	mime      string
	uploadDir string
	s3Key     string
	opts      models.VideoSummaryOptions
}

// Start202 validates the request, stages the input, refuses an unusable file
// with a 4xx (the ai_ops unit spent by the dispatcher is refunded by MM73's
// rule), creates the job and enqueues it; 202 {jobId}.
func (h *VideoSummaryHandler) Start202(c *gin.Context) {
	if !h.cfg.VideoSummary.Enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "summary_unavailable", "message": "Video summaries are not available right now"})
		return
	}
	frames, tierDeep, _ := h.summaryLimits(c)
	if frames <= 0 {
		// Normally refused by SummarizeSignInGate before the charge; this is
		// the belt-and-braces path (the 403 is refunded if anything was spent).
		c.JSON(http.StatusForbidden, gin.H{"error": "signin_required", "message": "Sign in to summarize videos", "upgradePath": "signup"})
		return
	}

	jobID := newJobID()
	in, status, body := h.stageInput(c, jobID)
	if body != nil {
		if in.uploadDir != "" {
			_ = os.RemoveAll(in.uploadDir)
		}
		c.JSON(status, body)
		return
	}

	outputLanguage, err := models.NormalizeVideoSummaryLanguage(in.opts.OutputLanguage)
	if err != nil {
		_ = os.RemoveAll(in.uploadDir)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_output_language", "message": err.Error()})
		return
	}
	depth, err := EffectiveSummaryDepth(in.opts.Depth, tierDeep, h.svc.Settings().DeepAvailable())
	if err != nil {
		_ = os.RemoveAll(in.uploadDir)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_depth", "message": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	report, err := h.probe(ctx, in.path)
	if err != nil || report == nil {
		_ = os.RemoveAll(in.uploadDir)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "unreadable_media", "message": "This file could not be read as audio or video"})
		return
	}
	hasVideo := services.UsableVideoStream(report)
	if !report.HasAudio && !hasVideo {
		_ = os.RemoveAll(in.uploadDir)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "no_audio_or_video", "message": "This file has no audio or video to summarize"})
		return
	}
	// Duration follows the tier's MaxVideoSeconds (validation.ValidateVideo:
	// the tighter of the deployment ceiling and the tier cap).
	probe := &validation.ProbeReport{
		DurationSeconds: report.DurationSeconds, Width: report.Width, Height: report.Height,
		FPS: report.FPS, VideoCodec: report.VideoCodec, AudioCodec: report.AudioCodec,
		HasVideo: hasVideo, HasAudio: report.HasAudio,
	}
	var rejection *validation.Rejection
	if errors.As(validation.ValidateVideo(h.cfg, tierValidationLimits(c), probe), &rejection) {
		_ = os.RemoveAll(in.uploadDir)
		c.JSON(rejection.HTTPStatus, gin.H{"error": rejection.UserMessage, "reason": rejection.Reason})
		return
	}

	mediaKind := models.FileTypeAudio
	if hasVideo {
		mediaKind = models.FileTypeVideo
	}
	options := map[string]interface{}{
		"mode":           models.VideoSummaryMode,
		"outputLanguage": outputLanguage,
		"depth":          depth,
		"maxFrames":      frames,
	}
	if requested := strings.ToLower(strings.TrimSpace(in.opts.Depth)); requested != "" && requested != depth {
		options["requestedDepth"] = requested
	}
	job := h.jobManager.CreateJobWithID(jobID, models.OriginalFileInfo{Name: in.fileName, Size: in.size, Type: in.mime}, options)
	_ = h.jobManager.SetMode(job.ID, models.VideoSummaryMode)
	recordJobCreated(c, h.jobManager, job, services.JobFacts{
		Tool:         models.VideoSummaryTool,
		Mode:         models.VideoSummaryMode,
		MediaKind:    mediaKindFor(mediaKind),
		SourceFormat: formatOf(in.fileName),
		TargetFormat: "md",
	})
	registerJobDirs(c, job.ID, true, true)
	if in.s3Key != "" {
		registerS3Object(c, in.s3Key, "", job.ID, "", in.size)
	}
	outputDir := filepath.Join(h.cfg.OutputDir, job.ID)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to prepare the job workspace")
		_ = os.RemoveAll(in.uploadDir)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare the job workspace"})
		return
	}
	_ = h.jobManager.ReplaceStages(job.ID, services.BuildVideoSummaryStages(), "queued")
	h.enqueue(services.VideoSummaryRequest{
		JobID:          job.ID,
		InputPath:      in.path,
		OutputDir:      outputDir,
		UploadDir:      in.uploadDir,
		OutputLanguage: outputLanguage,
		Depth:          depth,
		MaxFrames:      frames,
		RequestID:      c.Writer.Header().Get("X-MM-Request-ID"),
		GPUPriority:    gpuPriorityFor(c),
		Probe:          report,
	})
	c.JSON(http.StatusAccepted, models.VideoSummaryStartResponse{JobID: job.ID})
}

// stageInput saves the multipart `file` (with `options` JSON) or downloads the
// JSON body's presigned S3 upload into uploads/<jobId>/. A non-nil body is the
// error response to send.
func (h *VideoSummaryHandler) stageInput(c *gin.Context, jobID string) (summaryInput, int, gin.H) {
	in := summaryInput{uploadDir: filepath.Join(h.cfg.UploadDir, jobID)}
	if strings.HasPrefix(strings.ToLower(c.ContentType()), "application/json") {
		var body models.VideoSummaryStartJSON
		dec := json.NewDecoder(io.LimitReader(c.Request.Body, 64*1024))
		if err := dec.Decode(&body); err != nil {
			return in, http.StatusBadRequest, gin.H{"error": "invalid_request", "message": "Invalid request body"}
		}
		key := sanitizeUploadedVideoKey(body.S3Key)
		if key == "" {
			return in, http.StatusBadRequest, gin.H{"error": "invalid_s3_key", "message": "Invalid s3Key"}
		}
		if h.fetchS3 == nil {
			return in, http.StatusServiceUnavailable, gin.H{"error": "s3_unavailable", "message": "Large uploads are not available right now"}
		}
		name := safeFilename(body.FileName)
		if name == "" || name == "upload" {
			name = "video_" + filepath.Base(key)
		}
		in.fileName, in.s3Key, in.opts = name, key, body.Options
		in.path = stagedUploadPath(h.cfg.UploadDir, jobID, "original", name)
		ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
		defer cancel()
		// The tighter of the deployment's direct-upload ceiling and the
		// caller's tier byte cap.
		maxBytes := h.cfg.MaxVideoUpload
		if res, ok := middleware.ResolveTier(c); ok && res.Limits.MaxFileBytes > 0 && (maxBytes <= 0 || res.Limits.MaxFileBytes < maxBytes) {
			maxBytes = res.Limits.MaxFileBytes
		}
		size, err := h.fetchS3(ctx, key, in.path, maxBytes)
		if err != nil {
			var tooLarge errTooLarge
			if errors.As(err, &tooLarge) {
				return in, http.StatusRequestEntityTooLarge, gin.H{"error": "file_too_large", "message": "The upload is larger than your plan allows"}
			}
			return in, http.StatusBadRequest, gin.H{"error": "upload_not_found", "message": "The uploaded file was not found"}
		}
		in.size = size
		in.mime = mimeFromName(name)
		return in, 0, nil
	}

	uploadCap := maxUploadBytes(c, h.cfg)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, uploadCap)
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return in, http.StatusRequestEntityTooLarge, gin.H{"error": "file_too_large", "message": "The upload is larger than your plan allows"}
		}
		return in, http.StatusBadRequest, gin.H{"error": "invalid_request", "message": "The upload is malformed"}
	}
	if raw := strings.TrimSpace(c.Request.FormValue("options")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &in.opts); err != nil {
			return in, http.StatusBadRequest, gin.H{"error": "invalid_options", "message": "Invalid options format"}
		}
	}
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		return in, http.StatusBadRequest, gin.H{"error": "no_file", "message": "No file provided"}
	}
	defer file.Close()
	in.fileName = safeFilename(header.Filename)
	in.size = header.Size
	in.mime = firstNonEmpty(header.Header.Get("Content-Type"), mimeFromName(in.fileName))
	in.path = stagedUploadPath(h.cfg.UploadDir, jobID, "original", in.fileName)
	if err := os.MkdirAll(filepath.Dir(in.path), 0o755); err != nil {
		return in, http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"}
	}
	if err := saveMultipartTo(file, in.path); err != nil {
		return in, http.StatusInternalServerError, gin.H{"error": "Failed to save upload"}
	}
	return in, 0, nil
}

// errTooLarge marks an S3 object over the caller's byte cap.
type errTooLarge struct{ size int64 }

func (e errTooLarge) Error() string { return fmt.Sprintf("object too large (%d bytes)", e.size) }

// fetchFromS3 downloads a presigned upload after checking its size (HEAD)
// against maxBytes (0 = no cap).
func (h *VideoSummaryHandler) fetchFromS3(ctx context.Context, key, path string, maxBytes int64) (int64, error) {
	if h.s3Client == nil {
		return 0, errors.New("s3 not configured")
	}
	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(h.cfg.S3Bucket), Key: aws.String(key)})
	if err != nil {
		return 0, err
	}
	size := aws.ToInt64(head.ContentLength)
	if size <= 0 {
		return 0, errors.New("empty object")
	}
	if maxBytes > 0 && size > maxBytes {
		return size, errTooLarge{size: size}
	}
	if err := downloadS3ObjectTo(ctx, h.s3Client, h.cfg.S3Bucket, key, path); err != nil {
		return 0, err
	}
	return size, nil
}

// GetResult serves summary.md or summary.json. Ownership is checked with the
// same helper as /transcript/:jobId (authorizeJobAccess): a stranger gets the
// identical 404 {"error":"Job not found"}.
func (h *VideoSummaryHandler) GetResult(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job ID is required"})
		return
	}
	job, err := h.jobManager.GetJob(jobID)
	if err != nil || job.Mode != models.VideoSummaryMode {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		return
	}
	if !authorizeJobAccess(c, h.jobManager, job) {
		denyJobAccess(c)
		return
	}
	if job.Status != models.StatusCompleted {
		c.JSON(http.StatusConflict, gin.H{"error": "not_ready", "status": string(job.Status)})
		return
	}
	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", "md")))
	var name, contentType, ext string
	switch format {
	case "md", "markdown":
		name, contentType, ext = services.SummaryMarkdownFile, "text/markdown; charset=utf-8", "md"
	case "json":
		name, contentType, ext = services.SummaryJSONFile, "application/json; charset=utf-8", "json"
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_format", "message": "format must be md or json"})
		return
	}
	path := filepath.Join(h.cfg.OutputDir, job.ID, name)
	body, err := os.ReadFile(path)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Summary not found"})
		return
	}
	base := strings.TrimSuffix(safeFilename(job.OriginalFile.Name), filepath.Ext(job.OriginalFile.Name))
	if base == "" || base == "upload" {
		base = "video"
	}
	c.Header("Cache-Control", "private, max-age=300")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=%q", base+"_summary."+ext))
	c.Data(http.StatusOK, contentType, body)
}

// mimeFromName is a best-effort type from the extension, for the job record.
func mimeFromName(name string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
	case "mp4", "m4v":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	case "webm":
		return "video/webm"
	case "mkv":
		return "video/x-matroska"
	case "avi":
		return "video/x-msvideo"
	case "mp3":
		return "audio/mpeg"
	case "m4a":
		return "audio/mp4"
	case "wav":
		return "audio/wav"
	case "ogg", "oga":
		return "audio/ogg"
	case "opus":
		return "audio/opus"
	case "flac":
		return "audio/flac"
	}
	return "application/octet-stream"
}
