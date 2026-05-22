package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

type ConversionHandler struct {
	jobManager         *services.JobManager
	converter          *services.Converter
	cfg                *config.Config
	inspector          *services.MediaInspector
	analysisJobs       *services.AnalysisQueue
	transcription      *services.TranscriptionService
	transcode          *services.TranscodeService
	specializedTools   *services.SpecializedToolsService
	captionTranslator  *services.CaptionTranslatorService
	stitchAudioTool    *services.StitchAudioToVideoService
	s3Client           *s3.Client
	s3Presign          *s3.PresignClient
	faceDetectionStore *services.FaceDetectionStore
	aiService          *services.AIService
}

func NewConversionHandler(jobManager *services.JobManager, converter *services.Converter, cfg *config.Config, inspector *services.MediaInspector, analysisJobs *services.AnalysisQueue, transcription *services.TranscriptionService, s3Client *s3.Client, faceDetectionStore *services.FaceDetectionStore) *ConversionHandler {
	converter.SetJobManager(jobManager)
	converter.SetFaceDetectionStore(faceDetectionStore)
	var presign *s3.PresignClient
	if s3Client != nil {
		presign = s3.NewPresignClient(s3Client)
	}
	// Build a standalone AIService instance for the lightweight detect-only
	// preview endpoint. The converter has its own AI service for the final
	// conversion job; we keep this one separate so detect doesn't depend on a
	// job's lifecycle.
	var ai *services.AIService
	if cfg != nil && cfg.AIEnabled {
		ai = services.NewAIService(cfg)
	}
	var transcode *services.TranscodeService
	if s3Client != nil {
		transcode = services.NewTranscodeService(cfg, jobManager, inspector, transcription, s3Client)
	}
	specializedTools := services.NewSpecializedToolsService(cfg, jobManager)
	captionTranslator := services.NewCaptionTranslatorService(cfg, jobManager)
	stitchAudioTool := services.NewStitchAudioToVideoService(cfg, jobManager)
	return &ConversionHandler{
		jobManager:         jobManager,
		converter:          converter,
		cfg:                cfg,
		inspector:          inspector,
		analysisJobs:       analysisJobs,
		transcription:      transcription,
		transcode:          transcode,
		specializedTools:   specializedTools,
		captionTranslator:  captionTranslator,
		stitchAudioTool:    stitchAudioTool,
		s3Client:           s3Client,
		s3Presign:          presign,
		faceDetectionStore: faceDetectionStore,
		aiService:          ai,
	}
}

func RegisterConversionRoutes(r gin.IRouter, h *ConversionHandler) {
	r.POST("/details", h.IdentifyFile)
	r.POST("/upload", h.UploadFile)
	r.POST("/video-upload/presign", h.PresignVideoUpload)
	r.POST("/video-upload/complete", h.CompleteVideoUpload)
	r.GET("/job/:jobId", h.GetJobStatus)
	r.GET("/job/:jobId/events", h.StreamJobEvents)
	r.GET("/download/:jobId", h.DownloadFile)
	r.GET("/transcript/:jobId", h.GetTranscriptResult)
	r.GET("/analysis/:jobId", h.GetAnalysisResult)

	// Lightweight preview/helper endpoint that detects faces and stashes the
	// boxes server-side. The final conversion still goes through /upload.
	r.POST("/ai/faces/detect", h.DetectFaces)

	r.POST("/video-transcode/probe", h.ProbeVideoForTranscode)
	r.POST("/video-transcode/start", h.StartVideoTranscode)
	r.GET("/video-transcode/capabilities", h.GetTranscodeCapabilities)
}

func (h *ConversionHandler) IdentifyFile(c *gin.Context) {
	file, fileHeader, err := h.multipartFile(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer file.Close()

	tempPath := filepath.Join(h.cfg.TempDir, fmt.Sprintf("identify_%d_%s", time.Now().UnixNano(), safeFilename(fileHeader.Filename)))
	if err := h.saveUploadedFile(file, tempPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save temporary file"})
		return
	}
	defer func() { _ = os.Remove(tempPath) }()

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()

	fileType, mimeType := h.inspector.DetectFile(ctx, tempPath, fileHeader.GetHeader("Content-Type"))
	metadata, err := h.inspector.ProbeFile(ctx, tempPath, fileType)
	if err != nil && metadata == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to identify file: %v", err)})
		return
	}
	if metadata == nil {
		metadata = &services.MediaMetadata{FileType: fileType, MimeType: mimeType, Details: map[string]any{}, Error: stringOrErr(err)}
	}

	response := &models.FileIdentificationResponse{
		FileName:      fileHeader.Filename,
		FileSize:      fileHeader.Size,
		FileType:      fileType,
		MimeType:      mimeType,
		Details:       metadata.Details,
		ImageMetadata: metadata.ImageMetadata,
		Tool:          metadata.Tool,
		RawOutput:     metadata.Raw,
	}
	if metadata.Error != "" {
		response.Details["probe_error"] = metadata.Error
	}
	c.JSON(http.StatusOK, response)
}

func (h *ConversionHandler) UploadFile(c *gin.Context) {
	file, fileHeader, err := h.multipartFile(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer file.Close()

	options, err := parseOptions(c.Request.FormValue("options"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	incomingPath := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("incoming_%d_%s", time.Now().UnixNano(), safeFilename(fileHeader.Filename)))
	if err := h.saveUploadedFile(file, incomingPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()
	fileType, mimeType := h.inspector.DetectFile(ctx, incomingPath, fileHeader.GetHeader("Content-Type"))
	if fileType == models.FileTypeUnknown {
		_ = os.Remove(incomingPath)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported file type"})
		return
	}

	originalFile := models.OriginalFileInfo{Name: fileHeader.Filename, Size: fileHeader.Size, Type: mimeType}
	job := h.jobManager.CreateJob(originalFile, options)

	jobUploadDir := filepath.Join(h.cfg.UploadDir, job.ID)
	jobOutputDir := filepath.Join(h.cfg.OutputDir, job.ID)
	if err := os.MkdirAll(jobUploadDir, 0755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "Failed to create upload directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	if err := os.MkdirAll(jobOutputDir, 0755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "Failed to create output directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare output"})
		return
	}

	uploadPath := filepath.Join(jobUploadDir, "original_"+safeFilename(fileHeader.Filename))
	if err := os.Rename(incomingPath, uploadPath); err != nil {
		h.jobManager.UpdateJobError(job.ID, "Failed to finalize uploaded file")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to finalize upload"})
		return
	}

	metadata, probeErr := h.inspector.ProbeFile(ctx, uploadPath, fileType)
	if probeErr != nil {
		log.Printf("metadata probe failed for job %s: %v", job.ID, probeErr)
	}
	if metadata == nil {
		metadata = &services.MediaMetadata{FileType: fileType, MimeType: mimeType, Details: map[string]any{}, Error: stringOrErr(probeErr)}
	}
	if err := services.WriteMetadata(filepath.Join(jobOutputDir, "metadata.json"), metadata); err != nil {
		log.Printf("failed to write metadata for job %s: %v", job.ID, err)
	}

	if !isTranscribeMode(job) && specializedMode(job) == "" {
		h.analysisJobs.Enqueue(services.AnalysisJob{JobID: job.ID, InputPath: uploadPath, OutputDir: jobOutputDir, FileType: fileType, MimeType: mimeType})
	}
	go h.processConversion(job, uploadPath, jobOutputDir)

	c.JSON(http.StatusOK, models.UploadResponse{JobID: job.ID})
}

func (h *ConversionHandler) GetJobStatus(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job ID is required"})
		return
	}
	job, err := h.jobManager.GetJob(jobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		return
	}
	c.JSON(http.StatusOK, job)
}

func (h *ConversionHandler) DownloadFile(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job ID is required"})
		return
	}
	job, err := h.jobManager.GetJob(jobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		return
	}
	if job.Status != models.StatusCompleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job not completed"})
		return
	}

	outputPath := h.outputPath(job, filepath.Join(h.cfg.OutputDir, job.ID))
	if _, err := os.Stat(outputPath); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Converted file not found"})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", h.getOutputFilename(job)))
	c.Header("Content-Type", "application/octet-stream")
	c.File(outputPath)
}

func (h *ConversionHandler) GetTranscriptResult(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job ID is required"})
		return
	}
	resultPath := filepath.Join(h.cfg.OutputDir, jobID, "transcribe_result.json")
	if _, err := os.Stat(resultPath); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Transcript result not found"})
		return
	}
	c.File(resultPath)
}

func (h *ConversionHandler) GetAnalysisResult(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job ID is required"})
		return
	}
	resultPath := filepath.Join(h.cfg.OutputDir, jobID, "analysis.json")
	if _, err := os.Stat(resultPath); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Analysis result not yet available"})
		return
	}
	c.File(resultPath)
}

func (h *ConversionHandler) processConversion(job *models.ConversionJob, inputPath string, outputDir string) {
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusProcessing); err != nil {
		log.Printf("failed to update job %s status: %v", job.ID, err)
		return
	}
	if isTranscribeMode(job) {
		h.processTranscription(job, inputPath, outputDir)
		return
	}
	if mode := specializedMode(job); mode != "" {
		h.processSpecializedTool(job, mode, inputPath, outputDir)
		return
	}
	outputPath := h.outputPath(job, outputDir)
	if err := h.converter.ConvertFile(job, inputPath, outputPath); err != nil {
		log.Printf("conversion failed for job %s: %v", job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, err.Error())
		return
	}
	if err := h.jobManager.UpdateJobResult(job.ID, "/api/download/"+job.ID); err != nil {
		log.Printf("failed to update job %s result: %v", job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to update job result")
		return
	}
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		log.Printf("failed to mark job %s completed: %v", job.ID, err)
	}
}

func (h *ConversionHandler) processTranscription(job *models.ConversionJob, inputPath, outputDir string) {
	if h.transcription == nil {
		_ = h.jobManager.UpdateJobError(job.ID, "Transcription service is not available")
		return
	}
	fileType := models.GetFileType(job.OriginalFile.Type)
	if fileType != models.FileTypeVideo && fileType != models.FileTypeAudio {
		_ = h.jobManager.UpdateJobError(job.ID, "Transcription only supports video or audio files")
		return
	}
	format, _ := job.Options["format"].(string)
	language, _ := job.Options["language"].(string)
	opts := services.TranscribeOptions{Format: format, Language: language}
	outputPath := h.outputPath(job, outputDir)

	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()
	if _, err := h.transcription.Transcribe(ctx, job, inputPath, outputPath, opts); err != nil {
		log.Printf("transcription failed for job %s: %v", job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, err.Error())
		return
	}
	if err := h.jobManager.UpdateJobResult(job.ID, "/api/download/"+job.ID); err != nil {
		log.Printf("failed to update job %s result: %v", job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to update job result")
		return
	}
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		log.Printf("failed to mark job %s completed: %v", job.ID, err)
	}
}

func (h *ConversionHandler) processSpecializedTool(job *models.ConversionJob, mode, inputPath, outputDir string) {
	if h.specializedTools == nil {
		_ = h.jobManager.UpdateJobError(job.ID, "Specialized tools service is not available")
		return
	}
	outputPath := h.outputPath(job, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()
	if err := h.specializedTools.Run(ctx, job, mode, inputPath, outputPath); err != nil {
		log.Printf("specialized tool %s failed for job %s: %v", mode, job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, err.Error())
		return
	}
	if err := h.jobManager.UpdateJobResult(job.ID, "/api/download/"+job.ID); err != nil {
		log.Printf("failed to update job %s result: %v", job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to update job result")
		return
	}
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		log.Printf("failed to mark job %s completed: %v", job.ID, err)
	}
}

func isTranscribeMode(job *models.ConversionJob) bool {
	if job == nil || job.Options == nil {
		return false
	}
	mode, _ := job.Options["mode"].(string)
	return strings.EqualFold(strings.TrimSpace(mode), "transcribe")
}

// specializedMode returns the specialized-tool mode for a job, if any, in its
// canonical lowercase form. Returns "" for non-specialized jobs.
func specializedMode(job *models.ConversionJob) string {
	if job == nil || job.Options == nil {
		return ""
	}
	mode, _ := job.Options["mode"].(string)
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case services.SpecializedModeAudioWaveform,
		services.SpecializedModeExtractAudio,
		services.SpecializedModeExtractVideoOnly,
		services.SpecializedModeExtractFrames:
		return mode
	}
	return ""
}

func (h *ConversionHandler) multipartFile(c *gin.Context) (io.ReadCloser, *multipartFileHeader, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.cfg.MaxFileSize)
	if err := c.Request.ParseMultipartForm(h.cfg.MaxFileSize); err != nil {
		return nil, nil, fmt.Errorf("failed to parse form")
	}
	file, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		return nil, nil, fmt.Errorf("no file provided")
	}
	return file, &multipartFileHeader{Filename: fileHeader.Filename, Size: fileHeader.Size, Header: fileHeader.Header}, nil
}

type multipartFileHeader struct {
	Filename string
	Size     int64
	Header   map[string][]string
}

func (h multipartFileHeader) GetHeader(key string) string { return http.Header(h.Header).Get(key) }

func (h *ConversionHandler) saveUploadedFile(file io.Reader, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, file)
	return err
}

func (h *ConversionHandler) getOutputExtension(job *models.ConversionJob) string {
	if isTranscribeMode(job) {
		if format, ok := job.Options["format"].(string); ok && format != "" {
			return "." + strings.TrimPrefix(strings.ToLower(format), ".")
		}
		return ".txt"
	}
	if mode := specializedMode(job); mode != "" {
		return specializedExtension(job, mode)
	}
	if mode, _ := job.Options["mode"].(string); strings.EqualFold(strings.TrimSpace(mode), "caption_translator") {
		if fmtStr, _ := job.Options["format"].(string); strings.TrimSpace(fmtStr) != "" {
			return "." + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fmtStr)), ".")
		}
		return ".srt"
	}
	if mode, _ := job.Options["mode"].(string); strings.EqualFold(strings.TrimSpace(mode), "stitch_audio_to_video") {
		return ".mp4"
	}
	switch models.GetFileType(job.OriginalFile.Type) {
	case models.FileTypeImage:
		if ext := aiImageExtension(job); ext != "" {
			return ext
		}
		if format, ok := job.Options["format"].(string); ok && format != "" {
			return "." + strings.TrimPrefix(format, ".")
		}
		return ".jpg"
	case models.FileTypeVideo:
		if format, ok := job.Options["format"].(string); ok && format != "" {
			return "." + strings.TrimPrefix(format, ".")
		}
		return ".mp4"
	case models.FileTypeAudio:
		if ext := aiAudioExtension(job); ext != "" {
			return ext
		}
		if format, ok := job.Options["format"].(string); ok && format != "" {
			return "." + strings.TrimPrefix(format, ".")
		}
		return ".mp3"
	default:
		return ".bin"
	}
}

// specializedExtension returns the output extension for the specialized
// tool modes. We branch by mode so each output type uses the user-selected
// container — and so "both" / "frames" jobs cleanly produce a single .zip
// that the existing /api/download handler can serve.
func specializedExtension(job *models.ConversionJob, mode string) string {
	switch mode {
	case services.SpecializedModeAudioWaveform:
		if nested, ok := job.Options["waveform"].(map[string]any); ok {
			sel, _ := nested["outputSelection"].(string)
			switch strings.ToLower(strings.TrimSpace(sel)) {
			case "both":
				return ".zip"
			case "image":
				if fmtStr, _ := nested["imageFormat"].(string); strings.TrimSpace(fmtStr) != "" {
					return "." + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fmtStr)), ".")
				}
				return ".png"
			default: // video
				if fmtStr, _ := nested["videoFormat"].(string); strings.TrimSpace(fmtStr) != "" {
					return "." + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fmtStr)), ".")
				}
				return ".mp4"
			}
		}
		return ".mp4"
	case services.SpecializedModeExtractAudio:
		if fmtStr, _ := job.Options["format"].(string); strings.TrimSpace(fmtStr) != "" {
			return "." + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fmtStr)), ".")
		}
		return ".mp3"
	case services.SpecializedModeExtractVideoOnly:
		if fmtStr, _ := job.Options["format"].(string); strings.TrimSpace(fmtStr) != "" {
			return "." + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fmtStr)), ".")
		}
		return ".mp4"
	case services.SpecializedModeExtractFrames:
		return ".zip"
	}
	return ".bin"
}

// aiImageExtension forces AI ops that need a specific container to use it.
// remove_background must be PNG (transparency); ai_upscale prefers PNG for
// safety. Other ops fall back to the user's chosen format.
func aiImageExtension(job *models.ConversionJob) string {
	op, enabled := aiOperation(job)
	if !enabled {
		return ""
	}
	switch op {
	case "remove_background", "ai_upscale":
		return ".png"
	}
	return ""
}

// aiAudioExtension defaults vocals/no_vocals stems to WAV when the caller did
// not pick a format. Other AI audio ops honor the requested format.
func aiAudioExtension(job *models.ConversionJob) string {
	op, enabled := aiOperation(job)
	if !enabled {
		return ""
	}
	switch op {
	case "isolate_vocals", "remove_vocals":
		if format, ok := job.Options["format"].(string); ok && strings.TrimSpace(format) != "" {
			return "." + strings.TrimPrefix(format, ".")
		}
		return ".wav"
	}
	return ""
}

func aiOperation(job *models.ConversionJob) (string, bool) {
	ai, ok := job.Options["ai"].(map[string]interface{})
	if !ok {
		return "", false
	}
	enabled, _ := ai["enabled"].(bool)
	op, _ := ai["operation"].(string)
	op = strings.ToLower(strings.TrimSpace(op))
	if !enabled || op == "" || op == "none" {
		return "", false
	}
	return op, true
}

func (h *ConversionHandler) getOutputFilename(job *models.ConversionJob) string {
	name := strings.TrimSuffix(safeFilename(job.OriginalFile.Name), filepath.Ext(job.OriginalFile.Name))
	if name == "" {
		name = "converted"
	}
	if isTranscribeMode(job) {
		return fmt.Sprintf("%s_transcript%s", name, h.getOutputExtension(job))
	}
	if mode := specializedMode(job); mode != "" {
		suffix := map[string]string{
			services.SpecializedModeAudioWaveform:    "_waveform",
			services.SpecializedModeExtractAudio:     "_audio",
			services.SpecializedModeExtractVideoOnly: "_silent",
			services.SpecializedModeExtractFrames:    "_frames",
		}[mode]
		return fmt.Sprintf("%s%s%s", name, suffix, h.getOutputExtension(job))
	}
	if mode, _ := job.Options["mode"].(string); strings.EqualFold(strings.TrimSpace(mode), "caption_translator") {
		return fmt.Sprintf("%s_translated%s", name, h.getOutputExtension(job))
	}
	if mode, _ := job.Options["mode"].(string); strings.EqualFold(strings.TrimSpace(mode), "stitch_audio_to_video") {
		return fmt.Sprintf("%s_stitched%s", name, h.getOutputExtension(job))
	}
	return fmt.Sprintf("%s_converted%s", name, h.getOutputExtension(job))
}

func (h *ConversionHandler) outputPath(job *models.ConversionJob, outputDir string) string {
	if isTranscribeMode(job) {
		return filepath.Join(outputDir, "transcript"+h.getOutputExtension(job))
	}
	if mode := specializedMode(job); mode != "" {
		prefix := map[string]string{
			services.SpecializedModeAudioWaveform:    "waveform",
			services.SpecializedModeExtractAudio:     "audio",
			services.SpecializedModeExtractVideoOnly: "silent",
			services.SpecializedModeExtractFrames:    "frames",
		}[mode]
		return filepath.Join(outputDir, prefix+h.getOutputExtension(job))
	}
	if mode, _ := job.Options["mode"].(string); strings.EqualFold(strings.TrimSpace(mode), "caption_translator") {
		return filepath.Join(outputDir, "translated"+h.getOutputExtension(job))
	}
	if mode, _ := job.Options["mode"].(string); strings.EqualFold(strings.TrimSpace(mode), "stitch_audio_to_video") {
		return filepath.Join(outputDir, "stitched"+h.getOutputExtension(job))
	}
	return filepath.Join(outputDir, "converted"+h.getOutputExtension(job))
}

func parseOptions(optionsStr string) (map[string]interface{}, error) {
	options := map[string]interface{}{}
	if strings.TrimSpace(optionsStr) == "" {
		return options, nil
	}
	if err := json.Unmarshal([]byte(optionsStr), &options); err != nil {
		return nil, fmt.Errorf("invalid options format")
	}
	return options, nil
}

func safeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.NewReplacer("/", "_", "\\", "_", "\x00", "", "\n", "_", "\r", "_").Replace(name)
	if name == "" || name == "." {
		return "upload"
	}
	return name
}

func stringOrErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// DetectFaces runs the face-privacy script in --detect-only mode against the
// uploaded image, stores the resulting boxes in the in-memory session cache,
// and returns the session ID + normalized boxes to the UI. The uploaded bytes
// are removed before returning — we only keep the SHA256 hash + metadata so
// the eventual /api/upload job can validate the user didn't swap images.
func (h *ConversionHandler) DetectFaces(c *gin.Context) {
	if h.aiService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "AI service is not enabled"})
		return
	}
	if h.faceDetectionStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Face detection store is not configured"})
		return
	}

	file, fileHeader, err := h.multipartFile(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer file.Close()

	tempPath := filepath.Join(h.cfg.TempDir, fmt.Sprintf("detect_faces_%d_%s", time.Now().UnixNano(), safeFilename(fileHeader.Filename)))
	if err := h.saveUploadedFile(file, tempPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save temporary file"})
		return
	}
	defer func() { _ = os.Remove(tempPath) }()

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()

	fileType, _ := h.inspector.DetectFile(ctx, tempPath, fileHeader.GetHeader("Content-Type"))
	if fileType != models.FileTypeImage {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Face detection only supports image files"})
		return
	}

	sha, err := hashFile(tempPath)
	if err != nil {
		log.Printf("face detect hash failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to hash uploaded image"})
		return
	}

	resp, err := h.aiService.DetectFaces(ctx, tempPath)
	if err != nil {
		log.Printf("face detect failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Face detection failed"})
		return
	}

	session := h.faceDetectionStore.NewSession()
	session.ImageSHA256 = sha
	session.OriginalFileName = fileHeader.Filename
	session.ImageWidth = resp.ImageWidth
	session.ImageHeight = resp.ImageHeight
	session.Faces = resp.Faces
	h.faceDetectionStore.Store(session)

	resp.FaceDetectionSessionID = session.ID
	resp.ExpiresAt = session.ExpiresAt
	c.JSON(http.StatusOK, resp)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
