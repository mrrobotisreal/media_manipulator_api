package handlers

import (
	"context"
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
	jobManager    *services.JobManager
	converter     *services.Converter
	cfg           *config.Config
	inspector     *services.MediaInspector
	analysisJobs  *services.AnalysisQueue
	transcription *services.TranscriptionService
	s3Client      *s3.Client
	s3Presign     *s3.PresignClient
}

func NewConversionHandler(jobManager *services.JobManager, converter *services.Converter, cfg *config.Config, inspector *services.MediaInspector, analysisJobs *services.AnalysisQueue, transcription *services.TranscriptionService, s3Client *s3.Client) *ConversionHandler {
	converter.SetJobManager(jobManager)
	var presign *s3.PresignClient
	if s3Client != nil {
		presign = s3.NewPresignClient(s3Client)
	}
	return &ConversionHandler{
		jobManager:    jobManager,
		converter:     converter,
		cfg:           cfg,
		inspector:     inspector,
		analysisJobs:  analysisJobs,
		transcription: transcription,
		s3Client:      s3Client,
		s3Presign:     presign,
	}
}

func RegisterConversionRoutes(r gin.IRouter, h *ConversionHandler) {
	r.POST("/details", h.IdentifyFile)
	r.POST("/upload", h.UploadFile)
	r.POST("/video-upload/presign", h.PresignVideoUpload)
	r.POST("/video-upload/complete", h.CompleteVideoUpload)
	r.GET("/job/:jobId", h.GetJobStatus)
	r.GET("/download/:jobId", h.DownloadFile)
	r.GET("/transcript/:jobId", h.GetTranscriptResult)
	r.GET("/analysis/:jobId", h.GetAnalysisResult)
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

	if !isTranscribeMode(job) {
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

func isTranscribeMode(job *models.ConversionJob) bool {
	if job == nil || job.Options == nil {
		return false
	}
	mode, _ := job.Options["mode"].(string)
	return strings.EqualFold(strings.TrimSpace(mode), "transcribe")
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
	switch models.GetFileType(job.OriginalFile.Type) {
	case models.FileTypeImage:
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
		if format, ok := job.Options["format"].(string); ok && format != "" {
			return "." + strings.TrimPrefix(format, ".")
		}
		return ".mp3"
	default:
		return ".bin"
	}
}

func (h *ConversionHandler) getOutputFilename(job *models.ConversionJob) string {
	name := strings.TrimSuffix(safeFilename(job.OriginalFile.Name), filepath.Ext(job.OriginalFile.Name))
	if name == "" {
		name = "converted"
	}
	if isTranscribeMode(job) {
		return fmt.Sprintf("%s_transcript%s", name, h.getOutputExtension(job))
	}
	return fmt.Sprintf("%s_converted%s", name, h.getOutputExtension(job))
}

func (h *ConversionHandler) outputPath(job *models.ConversionJob, outputDir string) string {
	if isTranscribeMode(job) {
		return filepath.Join(outputDir, "transcript"+h.getOutputExtension(job))
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
