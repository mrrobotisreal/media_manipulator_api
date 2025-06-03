package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"

	"github.com/gin-gonic/gin"
	"mime/multipart"
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

func (h *ConversionHandler) IdentifyFile(c *gin.Context) {
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

	// Validate file type
	mimeType := fileHeader.Header.Get("Content-Type")
	fileType := models.GetFileType(mimeType)

	// Create a temporary file for analysis
	tempDir := "temp"
	os.MkdirAll(tempDir, 0755)
	tempFileName := fmt.Sprintf("identify_%d_%s", time.Now().UnixNano(), fileHeader.Filename)
	tempPath := filepath.Join(tempDir, tempFileName)

	// Save uploaded file temporarily
	if err := h.saveUploadedFile(file, tempPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save temporary file"})
		return
	}
	defer os.Remove(tempPath) // Clean up

	// Get file details based on type
	var response *models.FileIdentificationResponse
	switch fileType {
	case models.FileTypeImage:
		response, err = h.identifyImageFile(tempPath, fileHeader)
	case models.FileTypeVideo:
		response, err = h.identifyVideoFile(tempPath, fileHeader)
	case models.FileTypeAudio:
		response, err = h.identifyAudioFile(tempPath, fileHeader)
	default:
		response, err = h.identifyGenericFile(tempPath, fileHeader)
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to identify file: %v", err)})
		return
	}

	// Set common fields
	response.FileName = fileHeader.Filename
	response.FileSize = fileHeader.Size
	response.FileType = fileType
	response.MimeType = mimeType

	c.JSON(http.StatusOK, response)
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
	fmt.Printf("[DEBUG] Starting processConversion for job %s\n", job.ID)

	// Update job status to processing
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusProcessing); err != nil {
		log.Printf("Failed to update job status: %v", err)
		return
	}
	fmt.Printf("[DEBUG] Job status updated to processing\n")

	// Determine output path and extension
	outputPath := filepath.Join("outputs", job.ID+"_converted"+h.getOutputExtension(job))
	fmt.Printf("[DEBUG] Output path: %s\n", outputPath)

	// Perform conversion
	fmt.Printf("[DEBUG] Starting conversion...\n")
	if err := h.converter.ConvertFile(job, inputPath, outputPath); err != nil {
		log.Printf("Conversion failed for job %s: %v", job.ID, err)
		h.jobManager.UpdateJobError(job.ID, err.Error())
		return
	}
	fmt.Printf("[DEBUG] Conversion completed successfully\n")

	// Update job with result
	resultURL := "/api/download/" + job.ID
	if err := h.jobManager.UpdateJobResult(job.ID, resultURL); err != nil {
		log.Printf("Failed to update job result: %v", err)
		h.jobManager.UpdateJobError(job.ID, "Failed to update job result")
		return
	}
	fmt.Printf("[DEBUG] Job result updated: %s\n", resultURL)

	// Mark job as completed
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		log.Printf("Failed to update job status to completed: %v", err)
	}
	fmt.Printf("[DEBUG] Job status updated to completed\n")

	// Clean up input file
	os.Remove(inputPath)
	fmt.Printf("[DEBUG] Input file cleaned up: %s\n", inputPath)
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

func (h *ConversionHandler) identifyImageFile(tempPath string, fileHeader *multipart.FileHeader) (*models.FileIdentificationResponse, error) {
	// Use ImageMagick identify with verbose output for comprehensive details
	cmd := exec.Command("magick", "identify", "-verbose", tempPath)
	rawOutput, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to identify image with magick: %v", err)
	}

	// Parse the verbose output into structured data
	details := make(map[string]interface{})
	lines := strings.Split(string(rawOutput), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse key-value pairs from ImageMagick verbose output
		if strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				details[key] = value
			}
		}
	}

	return &models.FileIdentificationResponse{
		Details:   details,
		Tool:      "ImageMagick identify",
		RawOutput: string(rawOutput),
	}, nil
}

func (h *ConversionHandler) identifyVideoFile(tempPath string, fileHeader *multipart.FileHeader) (*models.FileIdentificationResponse, error) {
	// Use ffprobe with JSON output for comprehensive video details
	cmd := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", tempPath)
	rawOutput, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to identify video with ffprobe: %v", err)
	}

	// Parse JSON output
	var ffprobeData map[string]interface{}
	if err := json.Unmarshal(rawOutput, &ffprobeData); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe output: %v", err)
	}

	return &models.FileIdentificationResponse{
		Details:   ffprobeData,
		Tool:      "FFprobe",
		RawOutput: string(rawOutput),
	}, nil
}

func (h *ConversionHandler) identifyAudioFile(tempPath string, fileHeader *multipart.FileHeader) (*models.FileIdentificationResponse, error) {
	// Use ffprobe with JSON output for comprehensive audio details
	cmd := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", tempPath)
	rawOutput, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to identify audio with ffprobe: %v", err)
	}

	// Parse JSON output
	var ffprobeData map[string]interface{}
	if err := json.Unmarshal(rawOutput, &ffprobeData); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe output: %v", err)
	}

	return &models.FileIdentificationResponse{
		Details:   ffprobeData,
		Tool:      "FFprobe",
		RawOutput: string(rawOutput),
	}, nil
}

func (h *ConversionHandler) identifyGenericFile(tempPath string, fileHeader *multipart.FileHeader) (*models.FileIdentificationResponse, error) {
	// For generic files, try to get basic file information using system tools
	details := make(map[string]interface{})

	// Get file info using 'file' command if available
	if cmd := exec.Command("file", "-b", "--mime-all", tempPath); cmd != nil {
		if output, err := cmd.Output(); err == nil {
			details["file_command_output"] = strings.TrimSpace(string(output))
		}
	}

	// Get basic file stats
	if stat, err := os.Stat(tempPath); err == nil {
		details["modification_time"] = stat.ModTime()
		details["permissions"] = stat.Mode().String()
		details["size_bytes"] = stat.Size()
	}

	return &models.FileIdentificationResponse{
		Details:   details,
		Tool:      "System file command",
		RawOutput: fmt.Sprintf("Generic file analysis for: %s", fileHeader.Filename),
	}, nil
}
