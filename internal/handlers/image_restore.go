package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// ImageRestoreHandler serves the AI Image Restoration & Upscaling endpoints. It
// gets its own handler struct (like VideoRestoreHandler) so the pipeline's deps
// — GPU manager, telemetry store, command audit runner — don't bleed into the
// general conversion handler. Job status/result flows through the shared
// /api/job/:jobId machinery; the download tarball is served by the existing
// /api/download/:jobId handler.
type ImageRestoreHandler struct {
	jobManager   *services.JobManager
	cfg          *config.Config
	inspector    *services.MediaInspector
	imageRestore *services.ImageRestoreService
}

// NewImageRestoreHandler wires the image-restoration service. Unlike video
// restoration it has no S3 dependency (images are small, uploaded directly).
func NewImageRestoreHandler(jobManager *services.JobManager, cfg *config.Config, inspector *services.MediaInspector, gpuMgr *gpu.Manager, store *telemetry.Store, runner *cmdaudit.Runner) *ImageRestoreHandler {
	return &ImageRestoreHandler{
		jobManager:   jobManager,
		cfg:          cfg,
		inspector:    inspector,
		imageRestore: services.NewImageRestoreService(cfg, jobManager, gpuMgr, store, runner),
	}
}

// RegisterImageRestoreRoutes mounts the image-restoration endpoints.
func RegisterImageRestoreRoutes(r gin.IRouter, h *ImageRestoreHandler) {
	r.GET("/image-restore/capabilities", h.GetImageRestoreCapabilities)
	r.POST("/image-restore/start", h.StartImageRestore)
	r.GET("/image-restore/:jobId/results", h.GetImageRestoreResults)
	r.GET("/image-restore/:jobId/result/:resultId", h.GetImageRestoreResultImage)
}

// GetImageRestoreCapabilities reports feature flags, limits and per-model
// availability (cheap stat() checks at request time).
func (h *ImageRestoreHandler) GetImageRestoreCapabilities(c *gin.Context) {
	c.JSON(http.StatusOK, h.imageRestore.Capabilities())
}

// StartImageRestore validates the multipart request (image + options JSON),
// creates the job with the full stage timeline pre-populated, kicks the
// pipeline goroutine, and returns 202 with the job id.
func (h *ImageRestoreHandler) StartImageRestore(c *gin.Context) {
	if !h.cfg.ImageRestoreEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Image restoration is currently disabled"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.cfg.MaxFileSize)
	if err := c.Request.ParseMultipartForm(h.cfg.MaxFileSize); err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "The uploaded image is too large or the form is malformed"})
		return
	}
	file, fileHeader, err := c.Request.FormFile("image")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No image provided"})
		return
	}
	defer file.Close()

	var opts models.ImageRestoreOptions
	if raw := strings.TrimSpace(c.Request.FormValue("options")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &opts); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid options format"})
			return
		}
	}

	// --- validate the selection (before touching disk much) ---------------
	preclean, err := services.NormalizeImageRestorePreclean(opts.Preclean)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	general, face, err := services.NormalizeImageRestoreModels(opts.Models, opts.FaceModels)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := services.ValidateImageRestoreSelection(preclean, general, face); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := services.ValidateImageRestoreChain(opts.Chain, general, face); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := services.ValidateFBCNNQualityFactor(opts.FBCNNQualityFactor); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := services.ValidateImageRestoreCrop(opts.Crop); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if opts.Scale != 0 && opts.Scale != 2 && opts.Scale != 4 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Scale must be 0 (auto), 2, or 4"})
		return
	}
	outputs := services.CountImageRestoreOutputs(len(preclean), len(general), len(face), opts.Chain)
	if err := services.ValidateImageRestoreOutputBudget(outputs, h.cfg.ImageRestoreMaxOutputs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	for _, id := range append(append(append([]models.ImageRestoreModelID{}, preclean...), general...), face...) {
		if available, reason := h.imageRestore.ModelAvailability(id); !available {
			c.JSON(http.StatusBadRequest, gin.H{"error": models.ImageRestoreModelDisplayName(id) + ": " + reason})
			return
		}
	}

	// --- save + verify the upload is actually an image --------------------
	incomingPath := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("image_restore_incoming_%d_%s", time.Now().UnixNano(), safeFilename(fileHeader.Filename)))
	if err := os.MkdirAll(filepath.Dir(incomingPath), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	if err := saveMultipartTo(file, incomingPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save the uploaded image"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()
	fileType, contentType := h.inspector.DetectFile(ctx, incomingPath, fileHeader.Header.Get("Content-Type"))
	if fileType != models.FileTypeImage {
		_ = os.Remove(incomingPath)
		c.JSON(http.StatusBadRequest, gin.H{"error": "The uploaded file is not a supported image"})
		return
	}

	// --- create the job + move the upload into its workspace --------------
	fileName := safeFilename(fileHeader.Filename)
	originalFile := models.OriginalFileInfo{Name: fileName, Size: fileHeader.Size, Type: contentType}
	options := map[string]interface{}{
		"mode":       "image_restore",
		"preclean":   imageRestoreIDStrings(preclean),
		"models":     imageRestoreIDStrings(general),
		"faceModels": imageRestoreIDStrings(face),
		"chain":      opts.Chain,
		"scale":      opts.Scale,
	}
	job := h.jobManager.CreateJob(originalFile, options)
	_ = h.jobManager.SetMode(job.ID, "image_restore")

	jobUploadDir := filepath.Join(h.cfg.UploadDir, job.ID)
	if err := os.MkdirAll(jobUploadDir, 0o755); err != nil {
		_ = os.Remove(incomingPath)
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to prepare upload")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == "" {
		ext = ".img"
	}
	sourcePath := filepath.Join(jobUploadDir, "source"+ext)
	if err := os.Rename(incomingPath, sourcePath); err != nil {
		_ = os.Remove(incomingPath)
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to finalize the uploaded image")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to finalize upload"})
		return
	}

	sessionID := sanitizeS3PathSegment(firstNonEmpty(c.GetHeader("X-MM-Session-ID"), opts.SessionID))
	req := services.ImageRestoreRequest{
		JobID:              job.ID,
		SourcePath:         sourcePath,
		FileName:           fileName,
		FileSizeBytes:      fileHeader.Size,
		ContentType:        contentType,
		Crop:               opts.Crop,
		Preclean:           preclean,
		Models:             general,
		FaceModels:         face,
		Chain:              opts.Chain,
		Scale:              opts.Scale,
		CodeFormerFidelity: services.ClampCodeFormerFidelity(opts.CodeFormerFidelity),
		FBCNNQualityFactor: opts.FBCNNQualityFactor,
		SessionID:          sessionID,
		RequestID:          c.Writer.Header().Get("X-MM-Request-ID"),
	}
	_ = h.jobManager.ReplaceStages(job.ID, h.imageRestore.PlanStages(req), "queued")

	go h.imageRestore.Process(context.Background(), req)

	c.JSON(http.StatusAccepted, models.ImageRestoreStartResponse{JobID: job.ID})
}

// GetImageRestoreResults serves the manifest-derived results listing for a
// completed image_restore job (no filesystem paths).
func (h *ImageRestoreHandler) GetImageRestoreResults(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	job, err := h.jobManager.GetJob(jobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		return
	}
	if job.Mode != "image_restore" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		return
	}
	if job.Status != models.StatusCompleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job not completed"})
		return
	}
	resp, err := h.imageRestore.Results(jobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Results not found"})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// GetImageRestoreResultImage streams one result PNG inline. The result id is
// resolved against the job's manifest only — anything else is 404.
func (h *ImageRestoreHandler) GetImageRestoreResultImage(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	resultID := strings.TrimSpace(c.Param("resultId"))
	job, err := h.jobManager.GetJob(jobID)
	if err != nil || job.Mode != "image_restore" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	path, err := h.imageRestore.ResultImagePath(jobID, resultID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	c.Header("Content-Type", "image/png")
	c.Header("Cache-Control", "private, max-age=3600")
	c.File(path)
}

// saveMultipartTo streams an uploaded file to disk.
func saveMultipartTo(file io.Reader, path string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		return err
	}
	return out.Close()
}

// imageRestoreIDStrings converts model ids to strings for job-options echo.
func imageRestoreIDStrings(ids []models.ImageRestoreModelID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}
