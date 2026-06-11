package handlers

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// VideoRestoreHandler serves the AI Video Restoration endpoints. It gets its
// own handler struct (like StudioHandler) so the restoration pipeline's deps —
// GPU manager, telemetry store, command audit runner — don't bleed into the
// general conversion handler. Job status/result flows through the shared
// /api/job/:jobId machinery.
type VideoRestoreHandler struct {
	jobManager *services.JobManager
	cfg        *config.Config
	s3Client   *s3.Client
	restore    *services.RestoreService
}

// NewVideoRestoreHandler wires the restoration service. The service is only
// constructed when S3 is configured — without it uploads can't exist, so the
// endpoints answer 503.
func NewVideoRestoreHandler(jobManager *services.JobManager, cfg *config.Config, s3Client *s3.Client, gpuMgr *gpu.Manager, store *telemetry.Store, runner *cmdaudit.Runner) *VideoRestoreHandler {
	var restore *services.RestoreService
	if s3Client != nil {
		restore = services.NewRestoreService(cfg, jobManager, s3Client, gpuMgr, store, runner)
	}
	return &VideoRestoreHandler{
		jobManager: jobManager,
		cfg:        cfg,
		s3Client:   s3Client,
		restore:    restore,
	}
}

// RegisterVideoRestoreRoutes mounts the restoration endpoints on the /api group.
func RegisterVideoRestoreRoutes(r gin.IRouter, h *VideoRestoreHandler) {
	r.GET("/video-restore/capabilities", h.GetRestoreCapabilities)
	r.POST("/video-restore/start", h.StartVideoRestore)
}

// GetRestoreCapabilities reports feature flags, limits and per-model
// availability (cheap stat() checks against the configured script/venv paths
// at request time).
func (h *VideoRestoreHandler) GetRestoreCapabilities(c *gin.Context) {
	if h.restore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Video restoration is not configured on this server"})
		return
	}
	c.JSON(http.StatusOK, h.restore.Capabilities())
}

// StartVideoRestore validates the request, creates the job with the full
// stage timeline pre-populated (so "queued" is visible immediately), kicks
// the pipeline goroutine, and returns 202 with the job id.
func (h *VideoRestoreHandler) StartVideoRestore(c *gin.Context) {
	if h.restore == nil || h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Video restoration is not configured on this server"})
		return
	}
	if !h.cfg.RestoreEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Video restoration is currently disabled"})
		return
	}

	var req models.RestoreStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	key := sanitizeUploadedVideoKey(req.S3Key)
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid s3Key"})
		return
	}

	if err := services.ValidateRestoreClipWindow(req.ClipStartSeconds, req.ClipEndSeconds, h.cfg.RestoreMaxClipSeconds); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	selected, err := services.NormalizeRestoreModels(req.Models)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	for _, id := range selected {
		if available, reason := h.restore.ModelAvailability(id); !available {
			c.JSON(http.StatusBadRequest, gin.H{"error": models.RestoreModelDisplayName(id) + ": " + reason})
			return
		}
	}

	// Scale membership only — auto-resolution against the real source height
	// (and the 4x-above-1080p rejection) happens at the probe stage, where
	// the dimensions are known server-side.
	if req.Scale != 0 && req.Scale != 2 && req.Scale != 4 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Scale must be 0 (auto), 2, or 4"})
		return
	}

	headCtx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	head, err := h.s3Client.HeadObject(headCtx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("video-restore: failed to verify uploaded video %s: %v", key, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded video was not found"})
		return
	}
	objectSize := aws.ToInt64(head.ContentLength)
	if objectSize <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded video is empty"})
		return
	}
	if objectSize > h.cfg.MaxVideoUpload {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Video exceeds maximum upload size"})
		return
	}
	if req.FileSizeBytes > 0 && req.FileSizeBytes != objectSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file size does not match the request"})
		return
	}

	fileName := safeFilename(firstNonEmpty(req.FileName, filepath.Base(key)))
	contentType := firstNonEmpty(aws.ToString(head.ContentType), "video/mp4")
	sessionID := sanitizeS3PathSegment(firstNonEmpty(c.GetHeader("X-MM-Session-ID"), req.SessionID))

	options := map[string]interface{}{
		"mode":             "restore",
		"models":           restoreModelStrings(selected),
		"clipStartSeconds": req.ClipStartSeconds,
		"clipEndSeconds":   req.ClipEndSeconds,
		"scale":            req.Scale,
		"includeFrames":    req.IncludeFrames,
	}
	originalFile := models.OriginalFileInfo{Name: fileName, Size: objectSize, Type: contentType}
	job := h.jobManager.CreateJob(originalFile, options)
	_ = h.jobManager.SetMode(job.ID, "restore")
	stages := h.restore.BuildStages(selected)
	_ = h.jobManager.ReplaceStages(job.ID, stages, "queued")

	go h.restore.Process(context.Background(), services.RestoreRequest{
		JobID:            job.ID,
		S3Key:            key,
		FileName:         fileName,
		FileSizeBytes:    objectSize,
		ClipStartSeconds: req.ClipStartSeconds,
		ClipEndSeconds:   req.ClipEndSeconds,
		Models:           selected,
		Scale:            req.Scale,
		IncludeFrames:    req.IncludeFrames,
		SessionID:        sessionID,
		RequestID:        c.Writer.Header().Get("X-MM-Request-ID"),
	})

	c.JSON(http.StatusAccepted, models.RestoreStartResponse{JobID: job.ID})
}

func restoreModelStrings(ids []models.RestoreModelID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}
