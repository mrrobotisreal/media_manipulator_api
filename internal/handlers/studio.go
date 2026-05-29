package handlers

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// StudioHandler owns the Content Studio endpoints (/api/studio/*). It embeds
// the same shared dependencies ConversionHandler uses — jobManager + cfg +
// inspector + s3 — plus a Postgres-backed repository (projects/assets) and the
// ingest/export services. We keep it separate from ConversionHandler so the
// editor's DB-backed surface doesn't bleed into the file-conversion handler.
type StudioHandler struct {
	jobManager *services.JobManager
	cfg        *config.Config
	inspector  *services.MediaInspector
	s3Client   *s3.Client
	s3Presign  *s3.PresignClient
	repo       *services.StudioRepository
	ingest     *services.StudioIngestService
	export     *services.StudioExportService
}

// NewStudioHandler mirrors NewConversionHandler's construction: it derives the
// presign client + the studio sub-services from the shared deps so callers only
// pass the base client + pool.
func NewStudioHandler(jobManager *services.JobManager, cfg *config.Config, inspector *services.MediaInspector, s3Client *s3.Client, pool *pgxpool.Pool) *StudioHandler {
	var presign *s3.PresignClient
	if s3Client != nil {
		presign = s3.NewPresignClient(s3Client)
	}
	return &StudioHandler{
		jobManager: jobManager,
		cfg:        cfg,
		inspector:  inspector,
		s3Client:   s3Client,
		s3Presign:  presign,
		repo:       services.NewStudioRepository(pool),
		ingest:     services.NewStudioIngestService(cfg, jobManager, s3Client),
		export:     services.NewStudioExportService(cfg, jobManager),
	}
}

// RegisterStudioRoutes wires the Content Studio endpoints under /api/studio.
// Ingest + export run through the existing JobManager so /api/job/:jobId and
// /api/download/:jobId remain the single source of truth for progress + result.
func RegisterStudioRoutes(r gin.IRouter, h *StudioHandler) {
	studio := r.Group("/studio")
	// Projects (Postgres-backed, keyed by X-MM-Session-ID).
	studio.POST("/projects", h.CreateProject)
	studio.GET("/projects", h.ListProjects)
	studio.GET("/projects/:id", h.GetProject)
	studio.PUT("/projects/:id", h.SaveProject)
	studio.GET("/projects/:id/assets", h.ListAssets)
	// Source media ingest (presign -> PUT to S3 -> complete -> proxy/filmstrip job).
	studio.POST("/assets/presign", h.PresignAsset)
	studio.POST("/assets/complete", h.CompleteAsset)
	// Serve the preview proxy + filmstrip to the browser. Passthrough (not
	// presigned) so the <video> element gets reliable Range support + the API's
	// CORS headers (needed for crossorigin="anonymous" Web Audio in Phase 3),
	// with no mid-session URL expiry.
	studio.GET("/assets/:id/proxy", h.ServeProxy)
	studio.GET("/assets/:id/sprite", h.ServeSprite)
	// Export the EDL to MP4 via NVENC on the dedicated GPU.
	studio.POST("/projects/:id/export", h.ExportProject)
}

func (h *StudioHandler) dbReady(c *gin.Context) bool {
	if h.repo == nil || !h.repo.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Content Studio requires a database, which is currently unavailable"})
		return false
	}
	return true
}

func (h *StudioHandler) sessionID(c *gin.Context) string {
	return sanitizeS3PathSegment(firstNonEmpty(c.GetHeader("X-MM-Session-ID"), uuid.NewString()))
}

// ----------------------------------------------------------------------- //
// PROJECTS
// ----------------------------------------------------------------------- //

func (h *StudioHandler) CreateProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	var req models.StudioCreateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	clampCreateProject(&req)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	project, err := h.repo.CreateProject(ctx, h.sessionID(c), req)
	if err != nil {
		log.Printf("studio: create project failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create project"})
		return
	}
	c.JSON(http.StatusCreated, project)
}

func (h *StudioHandler) ListProjects(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	sessionID := strings.TrimSpace(c.GetHeader("X-MM-Session-ID"))
	if sessionID == "" {
		c.JSON(http.StatusOK, gin.H{"projects": []models.StudioProject{}})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	projects, err := h.repo.ListRecentProjects(ctx, sanitizeS3PathSegment(sessionID), 25)
	if err != nil {
		log.Printf("studio: list projects failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list projects"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

func (h *StudioHandler) GetProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	project, err := h.repo.GetProject(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	c.JSON(http.StatusOK, project)
}

func (h *StudioHandler) SaveProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	var req models.StudioSaveProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	clampSaveProject(&req)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	project, err := h.repo.SaveProject(ctx, strings.TrimSpace(c.Param("id")), req)
	if err != nil {
		if err == services.ErrStudioNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
			return
		}
		log.Printf("studio: save project failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save project"})
		return
	}
	c.JSON(http.StatusOK, project)
}

func (h *StudioHandler) ListAssets(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	assets, err := h.repo.ListAssets(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil {
		log.Printf("studio: list assets failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list assets"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"assets": assets})
}

// ----------------------------------------------------------------------- //
// ASSET INGEST
// ----------------------------------------------------------------------- //

func (h *StudioHandler) PresignAsset(c *gin.Context) {
	if h.s3Presign == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 uploads are not configured"})
		return
	}
	var req models.StudioAssetPresignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	fileName := safeFilename(req.FileName)
	ext := sanitizeExtension(filepath.Ext(fileName))
	if ext == "" || !isSupportedStudioExtension(ext) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported file extension"})
		return
	}
	if req.FileSizeBytes <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "fileSizeBytes must be greater than 0"})
		return
	}
	if req.FileSizeBytes > h.cfg.MaxVideoUpload {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "File exceeds maximum upload size"})
		return
	}
	contentType := normalizeUploadContentType(req.ContentType, ext)
	lower := strings.ToLower(contentType)
	if !strings.HasPrefix(lower, "video/") && !strings.HasPrefix(lower, "audio/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "contentType must be a video or audio MIME type"})
		return
	}

	sessionID := h.sessionID(c)
	key := fmt.Sprintf("%s/%s/%s/%s.%s", h.studioPrefix(), time.Now().UTC().Format("20060102"), sessionID, uuid.NewString(), ext)
	expiresAt := time.Now().UTC().Add(h.cfg.S3PresignTTL)

	presignCtx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	result, err := h.s3Presign.PresignPutObject(presignCtx, &s3.PutObjectInput{
		Bucket:      aws.String(h.cfg.S3Bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, func(o *s3.PresignOptions) { o.Expires = h.cfg.S3PresignTTL })
	if err != nil {
		log.Printf("studio: failed to presign asset upload: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload URL"})
		return
	}
	c.JSON(http.StatusCreated, models.StudioAssetPresignResponse{
		UploadURL: result.URL,
		S3Key:     key,
		Bucket:    h.cfg.S3Bucket,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	})
}

func (h *StudioHandler) CompleteAsset(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 uploads are not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	var req models.StudioAssetCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "projectId is required"})
		return
	}
	key := h.sanitizeStudioKey(req.S3Key)
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid s3Key"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()

	if _, err := h.repo.GetProject(ctx, projectID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}

	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("studio: failed to verify uploaded asset %s: %v", key, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file was not found"})
		return
	}
	objectSize := aws.ToInt64(head.ContentLength)
	if objectSize <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file is empty"})
		return
	}
	if objectSize > h.cfg.MaxVideoUpload {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "File exceeds maximum upload size"})
		return
	}

	contentType := firstNonEmpty(aws.ToString(head.ContentType), req.ContentType, "application/octet-stream")
	fileName := safeFilename(req.FileName)
	if fileName == "" || fileName == "upload" {
		fileName = "media_" + filepath.Base(key)
	}

	// Stage the original under the ingest job's upload dir so the background
	// job can read it. (Cleanup worker reaps it later; export re-downloads from
	// S3, so the local copy is disposable.)
	originalFile := models.OriginalFileInfo{Name: fileName, Size: objectSize, Type: contentType}
	job := h.jobManager.CreateJob(originalFile, map[string]interface{}{"mode": "studio_ingest"})
	_ = h.jobManager.SetMode(job.ID, "studio_ingest")

	jobUploadDir := filepath.Join(h.cfg.UploadDir, job.ID)
	if err := os.MkdirAll(jobUploadDir, 0o755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "Failed to create upload directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	uploadPath := filepath.Join(jobUploadDir, "original_"+fileName)
	if err := h.downloadS3Object(ctx, key, uploadPath); err != nil {
		log.Printf("studio: failed to download uploaded asset %s: %v", key, err)
		h.jobManager.UpdateJobError(job.ID, "Failed to download uploaded file")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download uploaded file"})
		return
	}

	fileType, _ := h.inspector.DetectFile(ctx, uploadPath, contentType)
	var kind models.StudioMediaKind
	switch fileType {
	case models.FileTypeVideo:
		kind = models.StudioMediaKindVideo
	case models.FileTypeAudio:
		kind = models.StudioMediaKindAudio
	default:
		h.jobManager.UpdateJobError(job.ID, "Unsupported media type")
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded object is not a supported video or audio file"})
		return
	}

	probe, probeErr := services.ProbeVideoReport(ctx, uploadPath)
	if probeErr != nil || probe == nil {
		log.Printf("studio: probe failed for %s: %v", key, probeErr)
		probe = &models.VideoProbeResponse{}
	}

	asset := &models.StudioAsset{
		ProjectID:        projectID,
		OriginalFileName: fileName,
		S3KeyOriginal:    key,
		MediaKind:        kind,
		DurationSeconds:  probe.DurationSeconds,
		VideoCodec:       probe.VideoCodec,
		AudioCodec:       probe.AudioCodec,
		HasAudio:         probe.HasAudio,
		ProbeJSON:        probe,
	}
	if probe.Width > 0 {
		w := probe.Width
		asset.Width = &w
	}
	if probe.Height > 0 {
		hh := probe.Height
		asset.Height = &hh
	}
	if probe.FPS > 0 {
		f := probe.FPS
		asset.FPS = &f
	}
	if sr, ch := studioAudioParams(probe); sr > 0 || ch > 0 {
		if sr > 0 {
			asset.SampleRate = &sr
		}
		if ch > 0 {
			asset.Channels = &ch
		}
	}

	created, err := h.repo.CreateAsset(ctx, asset)
	if err != nil {
		log.Printf("studio: create asset failed: %v", err)
		h.jobManager.UpdateJobError(job.ID, "Failed to record asset")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record asset"})
		return
	}

	go h.runIngestJob(job.ID, created, key, kind, uploadPath, probe.DurationSeconds)

	c.JSON(http.StatusOK, models.StudioAssetCompleteResponse{Asset: created, JobID: job.ID})
}

func (h *StudioHandler) runIngestJob(jobID string, asset *models.StudioAsset, originalKey string, kind models.StudioMediaKind, inputPath string, totalSeconds float64) {
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusProcessing); err != nil {
		log.Printf("studio ingest: failed to mark job %s processing: %v", jobID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()

	res, err := h.ingest.Generate(ctx, jobID, originalKey, asset.ID, kind, inputPath, totalSeconds)
	if err != nil {
		log.Printf("studio ingest: job %s failed: %v", jobID, err)
		_ = h.jobManager.UpdateJobError(jobID, err.Error())
		return
	}
	if err := h.repo.SetAssetDerived(ctx, asset.ID, res.ProxyKey, res.SpriteKey); err != nil {
		log.Printf("studio ingest: failed to persist derivatives for asset %s: %v", asset.ID, err)
		_ = h.jobManager.UpdateJobError(jobID, "Failed to record proxy")
		return
	}
	_ = h.jobManager.UpdateJobResult(jobID, "/api/studio/assets/"+asset.ID+"/proxy")
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusCompleted); err != nil {
		log.Printf("studio ingest: failed to mark job %s completed: %v", jobID, err)
	}
}

// ----------------------------------------------------------------------- //
// SERVE PROXY + SPRITE (range-forwarding S3 passthrough)
// ----------------------------------------------------------------------- //

func (h *StudioHandler) ServeProxy(c *gin.Context)  { h.serveAssetDerivative(c, "proxy") }
func (h *StudioHandler) ServeSprite(c *gin.Context) { h.serveAssetDerivative(c, "sprite") }

func (h *StudioHandler) serveAssetDerivative(c *gin.Context, which string) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	ctx := c.Request.Context()
	asset, err := h.repo.GetAsset(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}

	var key, contentType string
	switch which {
	case "sprite":
		key = asset.ThumbnailSpriteURL // stores the sprite's S3 key
		contentType = "image/jpeg"
	default:
		key = asset.S3KeyProxy
		contentType = "video/mp4"
		if asset.MediaKind == models.StudioMediaKindAudio {
			contentType = "audio/mp4"
		}
	}
	if key == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": which + " is not ready yet"})
		return
	}

	getIn := &s3.GetObjectInput{Bucket: aws.String(h.cfg.S3Bucket), Key: aws.String(key)}
	if rng := c.GetHeader("Range"); rng != "" {
		getIn.Range = aws.String(rng)
	}
	out, err := h.s3Client.GetObject(ctx, getIn)
	if err != nil {
		log.Printf("studio: failed to fetch %s for asset %s: %v", which, asset.ID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to fetch media"})
		return
	}
	defer out.Body.Close()

	if out.ContentType != nil && *out.ContentType != "" {
		contentType = *out.ContentType
	}
	c.Header("Content-Type", contentType)
	c.Header("Accept-Ranges", "bytes")
	c.Header("Cache-Control", "private, max-age=3600")
	if out.ContentLength != nil {
		c.Header("Content-Length", strconv.FormatInt(*out.ContentLength, 10))
	}
	status := http.StatusOK
	if out.ContentRange != nil && *out.ContentRange != "" {
		c.Header("Content-Range", *out.ContentRange)
		status = http.StatusPartialContent
	}
	c.Status(status)
	if _, err := io.Copy(c.Writer, out.Body); err != nil {
		log.Printf("studio: stream %s for asset %s interrupted: %v", which, asset.ID, err)
	}
}

// ----------------------------------------------------------------------- //
// EXPORT
// ----------------------------------------------------------------------- //

func (h *StudioHandler) ExportProject(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	var req models.StudioExportRequest
	_ = c.ShouldBindJSON(&req) // body is optional

	loadCtx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	projectID := strings.TrimSpace(c.Param("id"))
	project, err := h.repo.GetProject(loadCtx, projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	assets, err := h.repo.ListAssets(loadCtx, projectID)
	if err != nil {
		log.Printf("studio export: list assets failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load project media"})
		return
	}
	assetByID := make(map[string]*models.StudioAsset, len(assets))
	for _, a := range assets {
		assetByID[a.ID] = a
	}
	refs, duration, ok := collectExportRefs(project, assetByID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Project has no clip to export"})
		return
	}

	quality := normalizeExportQuality(req.Preset)
	baseName := safeFilename(firstNonEmpty(req.FileName, project.Name, "export")) + ".mp4"

	job := h.jobManager.CreateJob(
		models.OriginalFileInfo{Name: baseName, Size: 0, Type: "video/mp4"},
		map[string]interface{}{"mode": "studio_export", "format": "mp4"},
	)
	jobOutputDir := filepath.Join(h.cfg.OutputDir, job.ID)
	if err := os.MkdirAll(jobOutputDir, 0o755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "Failed to create output directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare export"})
		return
	}
	// converted.mp4 is what ConversionHandler.DownloadFile resolves for a
	// default video job, so /api/download/:jobId serves it unchanged.
	outputPath := filepath.Join(jobOutputDir, "converted.mp4")

	go h.runExportJob(job.ID, refs, project.Width, project.Height, project.FPS, duration, quality, outputPath)

	c.JSON(http.StatusOK, models.UploadResponse{JobID: job.ID})
}

func (h *StudioHandler) runExportJob(jobID string, refs []studioClipRef, width, height int, fps, duration float64, quality, outputPath string) {
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusProcessing); err != nil {
		log.Printf("studio export: failed to mark job %s processing: %v", jobID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()

	// Download each distinct source once; clips that share an asset reuse the
	// same local file (referenced as repeated ffmpeg inputs).
	localByKey := make(map[string]string)
	var cleanup []string
	defer func() {
		for _, p := range cleanup {
			_ = os.Remove(p)
		}
	}()
	for _, r := range refs {
		key := r.asset.S3KeyOriginal
		if _, done := localByKey[key]; done {
			continue
		}
		local := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("studio_export_%s_%d%s", jobID, len(localByKey), strings.ToLower(filepath.Ext(key))))
		if err := h.downloadS3Object(ctx, key, local); err != nil {
			log.Printf("studio export: download original failed: %v", err)
			_ = h.jobManager.UpdateJobError(jobID, "Failed to download source media")
			return
		}
		localByKey[key] = local
		cleanup = append(cleanup, local)
	}

	// Build the render plan: one ffmpeg input per ref (shared assets repeat the
	// same local path). Video clips composite; audio-bearing clips on non-muted
	// tracks mix in.
	plan := services.StudioExportPlan{Width: width, Height: height, FPS: fps, Duration: duration}
	for _, r := range refs {
		idx := len(plan.Inputs)
		plan.Inputs = append(plan.Inputs, localByKey[r.asset.S3KeyOriginal])
		opacity := 1.0
		if r.clip.Opacity != nil {
			opacity = *r.clip.Opacity
		}
		volume := 1.0
		if r.clip.Volume != nil {
			volume = *r.clip.Volume
		}
		if r.trackKind == models.StudioTrackKindVideo {
			plan.Video = append(plan.Video, services.StudioExportVideoSeg{
				InputIndex: idx, SourceIn: r.clip.SourceIn, SourceOut: r.clip.SourceOut,
				TimelineStart: r.clip.TimelineStart, Opacity: opacity, TrackIndex: r.trackIndex,
				FadeIn: r.fadeIn, Adjustments: r.clip.Adjustments, TextOverlays: r.clip.TextOverlays,
			})
		}
		if !r.trackMuted && r.asset.HasAudio && volume > 0 {
			plan.Audio = append(plan.Audio, services.StudioExportAudioSeg{
				InputIndex: idx, SourceIn: r.clip.SourceIn, SourceOut: r.clip.SourceOut,
				TimelineStart: r.clip.TimelineStart, Volume: volume,
				FadeIn: r.fadeIn, FadeOut: r.fadeOut,
			})
		}
	}

	if err := h.export.RunExport(ctx, jobID, plan, quality, outputPath); err != nil {
		log.Printf("studio export: job %s failed: %v", jobID, err)
		_ = h.jobManager.UpdateJobError(jobID, err.Error())
		return
	}
	if err := h.jobManager.UpdateJobResult(jobID, "/api/download/"+jobID); err != nil {
		_ = h.jobManager.UpdateJobError(jobID, "Failed to update job result")
		return
	}
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusCompleted); err != nil {
		log.Printf("studio export: failed to mark job %s completed: %v", jobID, err)
	}
}

// ----------------------------------------------------------------------- //
// HELPERS
// ----------------------------------------------------------------------- //

func (h *StudioHandler) downloadS3Object(ctx context.Context, key, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	result, err := h.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer result.Body.Close()
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, result.Body)
	return err
}

func (h *StudioHandler) studioPrefix() string {
	prefix := strings.Trim(h.cfg.ContentStudioS3Prefix, "/")
	if prefix == "" {
		return "studio"
	}
	return prefix
}

// sanitizeStudioKey validates a client-supplied S3 key, requiring the layout
// <prefix>/<date>/<session>/<uuid>.<ext> with a supported media extension. Same
// path-escape guards as sanitizeUploadedVideoKey.
func (h *StudioHandler) sanitizeStudioKey(key string) string {
	prefix := h.studioPrefix()
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || strings.Contains(key, "..") || strings.HasSuffix(key, "/") {
		return ""
	}
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != prefix || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return ""
	}
	if sanitizeS3PathSegment(parts[2]) != parts[2] {
		return ""
	}
	ext := sanitizeExtension(filepath.Ext(parts[3]))
	if ext == "" || !isSupportedStudioExtension(ext) {
		return ""
	}
	return key
}

func isSupportedStudioExtension(ext string) bool {
	switch ext {
	case "mp4", "mov", "m4v", "webm", "mkv", "avi", "flv", "wmv", "mpeg", "mpg",
		"mp3", "wav", "aac", "m4a", "ogg", "flac":
		return true
	default:
		return false
	}
}

// studioAudioParams pulls sample rate + channel count from the first audio
// stream in a probe report.
func studioAudioParams(probe *models.VideoProbeResponse) (sampleRate, channels int) {
	for _, s := range probe.Streams {
		if s.CodecType == "audio" {
			if v, err := strconv.Atoi(strings.TrimSpace(s.SampleRate)); err == nil {
				sampleRate = v
			}
			channels = s.Channels
			break
		}
	}
	return sampleRate, channels
}

// studioClipRef pairs a clip with its resolved asset and the track context the
// exporter needs (kind for video-vs-audio routing, index for overlay order,
// muted to drop a track's audio). fadeIn is the clip's own cross-dissolve;
// fadeOut is the next clip-on-track's dissolve into it (for audio crossfade).
type studioClipRef struct {
	clip       models.StudioClip
	asset      *models.StudioAsset
	trackKind  models.StudioTrackKind
	trackIndex int
	trackMuted bool
	fadeIn     float64
	fadeOut    float64
}

// collectExportRefs flattens the EDL into render refs and the timeline length.
// A ref is kept only if it produces output: every video clip (mute affects only
// audio), plus audio-bearing clips on non-muted tracks with volume > 0.
// Per-track clips are walked in timeline order so transition fades can be paired
// with their neighbours. Duration spans all clips so the export length matches
// the timeline.
func collectExportRefs(p *models.StudioProject, assets map[string]*models.StudioAsset) ([]studioClipRef, float64, bool) {
	refs := make([]studioClipRef, 0)
	var duration float64
	for _, track := range p.Tracks {
		ordered := append([]models.StudioClip(nil), track.Clips...)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].TimelineStart < ordered[j].TimelineStart })

		for i, clip := range ordered {
			dur := clipEffectiveDur(clip)
			if end := clip.TimelineStart + dur; end > duration {
				duration = end
			}
			asset, ok := assets[clip.AssetID]
			if !ok {
				continue
			}
			volume := 1.0
			if clip.Volume != nil {
				volume = *clip.Volume
			}
			contributesAudio := !track.Muted && asset.HasAudio && volume > 0
			if track.Kind != models.StudioTrackKindVideo && !contributesAudio {
				continue // muted/silent audio clip — nothing to render
			}

			fadeIn := clampFade(transitionOf(clip), dur)
			fadeOut := 0.0
			if i+1 < len(ordered) {
				fadeOut = clampFade(transitionOf(ordered[i+1]), dur)
			}

			refs = append(refs, studioClipRef{
				clip:       clip,
				asset:      asset,
				trackKind:  track.Kind,
				trackIndex: track.Index,
				trackMuted: track.Muted,
				fadeIn:     fadeIn,
				fadeOut:    fadeOut,
			})
		}
	}
	if len(refs) == 0 || duration <= 0 {
		return nil, 0, false
	}
	return refs, duration, true
}

func transitionOf(c models.StudioClip) float64 {
	if c.TransitionInSeconds != nil && *c.TransitionInSeconds > 0 {
		return *c.TransitionInSeconds
	}
	return 0
}

// clampFade keeps a transition no longer than the clip it applies to.
func clampFade(d, clipDur float64) float64 {
	if d <= 0 {
		return 0
	}
	if d > clipDur {
		return clipDur
	}
	return d
}

func clipEffectiveDur(c models.StudioClip) float64 {
	if d := c.SourceOut - c.SourceIn; d > 0 {
		return d
	}
	return 0
}

func normalizeExportQuality(preset string) string {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "low", "high":
		return strings.ToLower(strings.TrimSpace(preset))
	default:
		return "medium"
	}
}

func clampCreateProject(req *models.StudioCreateProjectRequest) {
	req.Name = clampProjectName(req.Name)
	req.FPS = clampFPS(req.FPS)
	req.Width = clampDim(req.Width, 1920, 7680)
	req.Height = clampDim(req.Height, 1080, 4320)
}

func clampSaveProject(req *models.StudioSaveProjectRequest) {
	req.Name = clampProjectName(req.Name)
	req.FPS = clampFPS(req.FPS)
	req.Width = clampDim(req.Width, 1920, 7680)
	req.Height = clampDim(req.Height, 1080, 4320)
	if req.Tracks == nil {
		req.Tracks = []models.StudioTrack{}
	}
}

func clampProjectName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Untitled project"
	}
	if len(name) > 200 {
		return name[:200]
	}
	return name
}

func clampFPS(fps float64) float64 {
	if fps <= 0 {
		return 30
	}
	if fps > 240 {
		return 240
	}
	return fps
}

func clampDim(v, fallback, max int) int {
	if v <= 0 {
		return fallback
	}
	if v > max {
		return max
	}
	return v
}
