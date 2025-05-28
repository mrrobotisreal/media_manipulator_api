package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"

	"github.com/gin-gonic/gin"
)

type ConversionHandler struct {
	jobManager *services.JobManager
	converter  *services.Converter
}

func NewConversionHandler(jobManager *services.JobManager, converter *services.Converter) *ConversionHandler {
	converter.SetJobManager(jobManager)
	return &ConversionHandler{
		jobManager: jobManager,
		converter:  converter,
	}
}

func (h *ConversionHandler) UploadFile(c *gin.Context) {
	// Parse multipart form
	if err := c.Request.ParseMultipartForm(100 << 20); err != nil { // 100MB max
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse form"})
		return
	}

	// Get file from form
	file, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No file provided"})
		return
	}
	defer file.Close()

	// Get conversion options from form
	optionsStr := c.Request.FormValue("options")
	var options map[string]interface{}
	if optionsStr != "" {
		if err := json.Unmarshal([]byte(optionsStr), &options); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid options format"})
			return
		}
	}

	// Validate file type
	fileType := models.GetFileType(fileHeader.Header.Get("Content-Type"))
	if fileType == models.FileTypeUnknown {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported file type"})
		return
	}

	// Create original file info
	originalFile := models.OriginalFileInfo{
		Name: fileHeader.Filename,
		Size: fileHeader.Size,
		Type: fileHeader.Header.Get("Content-Type"),
	}

	// Create conversion job
	job := h.jobManager.CreateJob(originalFile, options)

	// Save uploaded file
	uploadPath := filepath.Join("uploads", job.ID+"_"+fileHeader.Filename)
	if err := h.saveUploadedFile(file, uploadPath); err != nil {
		h.jobManager.UpdateJobError(job.ID, "Failed to save uploaded file")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file"})
		return
	}

	// Start conversion in background
	go h.processConversion(job, uploadPath)

	// Return job ID
	c.JSON(http.StatusOK, models.UploadResponse{JobID: job.ID})
}

func (h *ConversionHandler) GetJobStatus(c *gin.Context) {
	jobID := c.Param("jobId")
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
	jobID := c.Param("jobId")
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

	// Get the actual file path from the result URL
	// The ResultURL contains the relative path from our outputs directory
	outputPath := filepath.Join("outputs", jobID+"_converted"+h.getOutputExtension(job))

	// Check if file exists
	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Converted file not found"})
		return
	}

	// Set appropriate headers
	filename := h.getOutputFilename(job)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	c.Header("Content-Type", "application/octet-stream")

	// Serve the file
	c.File(outputPath)
}

func (h *ConversionHandler) processConversion(job *models.ConversionJob, inputPath string) {
	// Update job status to processing
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusProcessing); err != nil {
		log.Printf("Failed to update job status: %v", err)
		return
	}

	// Determine output path and extension
	outputPath := filepath.Join("outputs", job.ID+"_converted"+h.getOutputExtension(job))

	// Perform conversion
	if err := h.converter.ConvertFile(job, inputPath, outputPath); err != nil {
		log.Printf("Conversion failed for job %s: %v", job.ID, err)
		h.jobManager.UpdateJobError(job.ID, err.Error())
		return
	}

	// Update job with result
	resultURL := "/api/download/" + job.ID
	if err := h.jobManager.UpdateJobResult(job.ID, resultURL); err != nil {
		log.Printf("Failed to update job result: %v", err)
		h.jobManager.UpdateJobError(job.ID, "Failed to update job result")
		return
	}

	// Mark job as completed
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		log.Printf("Failed to update job status to completed: %v", err)
	}

	// Clean up input file
	os.Remove(inputPath)
}

func (h *ConversionHandler) saveUploadedFile(file io.Reader, path string) error {
	// Create the directory if it doesn't exist
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Create output file
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	// Copy file content
	_, err = io.Copy(out, file)
	return err
}

func (h *ConversionHandler) getOutputExtension(job *models.ConversionJob) string {
	fileType := models.GetFileType(job.OriginalFile.Type)

	switch fileType {
	case models.FileTypeImage:
		if format, ok := job.Options["format"].(string); ok {
			return "." + format
		}
		return ".jpg"
	case models.FileTypeVideo:
		if format, ok := job.Options["format"].(string); ok {
			return "." + format
		}
		return ".mp4"
	case models.FileTypeAudio:
		if format, ok := job.Options["format"].(string); ok {
			return "." + format
		}
		return ".mp3"
	}

	return ".bin"
}

func (h *ConversionHandler) getOutputFilename(job *models.ConversionJob) string {
	originalName := job.OriginalFile.Name
	ext := h.getOutputExtension(job)

	// Remove original extension and add new one
	name := strings.TrimSuffix(originalName, filepath.Ext(originalName))
	return fmt.Sprintf("%s_converted%s", name, ext)
}
