package handlers

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

func (h *ConversionHandler) PresignVideoUpload(c *gin.Context) {
	if h.s3Presign == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 uploads are not configured"})
		return
	}

	var req models.VideoUploadPresignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	fileName := safeFilename(req.FileName)
	ext := sanitizeExtension(filepath.Ext(fileName))
	if ext == "" || !isSupportedVideoExtension(ext) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported video file extension"})
		return
	}
	if req.FileSizeBytes <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "fileSizeBytes must be greater than 0"})
		return
	}
	if req.FileSizeBytes > h.cfg.MaxVideoUpload {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Video exceeds maximum upload size"})
		return
	}

	contentType := normalizeUploadContentType(req.ContentType, ext)
	if !strings.HasPrefix(strings.ToLower(contentType), "video/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "contentType must be a video MIME type"})
		return
	}

	sessionID := sanitizeS3PathSegment(firstNonEmpty(c.GetHeader("X-MM-Session-ID"), req.SessionID, uuid.NewString()))
	key := fmt.Sprintf("videos/%s/%s/%s.%s", time.Now().UTC().Format("20060102"), sessionID, uuid.NewString(), ext)
	expiresAt := time.Now().UTC().Add(h.cfg.S3PresignTTL)

	putInput := &s3.PutObjectInput{
		Bucket:      aws.String(h.cfg.S3Bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}
	presignCtx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	result, err := h.s3Presign.PresignPutObject(presignCtx, putInput, func(options *s3.PresignOptions) {
		options.Expires = h.cfg.S3PresignTTL
	})
	if err != nil {
		log.Printf("failed to presign video upload: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload URL"})
		return
	}

	c.JSON(http.StatusCreated, models.VideoUploadTarget{
		UploadURL: result.URL,
		S3Key:     key,
		Bucket:    h.cfg.S3Bucket,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	})
}

func (h *ConversionHandler) CompleteVideoUpload(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 uploads are not configured"})
		return
	}

	var req models.VideoUploadCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	key := sanitizeUploadedVideoKey(req.S3Key)
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid s3Key"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()

	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("failed to verify uploaded video %s: %v", key, err)
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded video size does not match the requested size"})
		return
	}

	contentType := firstNonEmpty(aws.ToString(head.ContentType), req.ContentType, "application/octet-stream")
	fileName := safeFilename(req.FileName)
	if fileName == "" || fileName == "upload" {
		fileName = "video_" + filepath.Base(key)
	}

	incomingPath := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("s3_incoming_%d_%s", time.Now().UnixNano(), fileName))
	if err := h.downloadS3Object(ctx, key, incomingPath); err != nil {
		log.Printf("failed to download uploaded video %s: %v", key, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download uploaded video"})
		return
	}

	fileType, mimeType := h.inspector.DetectFile(ctx, incomingPath, contentType)
	if fileType != models.FileTypeVideo {
		_ = os.Remove(incomingPath)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded object is not a supported video"})
		return
	}

	options := req.Options
	if options == nil {
		options = map[string]interface{}{}
	}
	originalFile := models.OriginalFileInfo{Name: fileName, Size: objectSize, Type: mimeType}
	job := h.jobManager.CreateJob(originalFile, options)

	jobUploadDir := filepath.Join(h.cfg.UploadDir, job.ID)
	jobOutputDir := filepath.Join(h.cfg.OutputDir, job.ID)
	if err := os.MkdirAll(jobUploadDir, 0755); err != nil {
		_ = os.Remove(incomingPath)
		h.jobManager.UpdateJobError(job.ID, "Failed to create upload directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	if err := os.MkdirAll(jobOutputDir, 0755); err != nil {
		_ = os.Remove(incomingPath)
		h.jobManager.UpdateJobError(job.ID, "Failed to create output directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare output"})
		return
	}

	uploadPath := filepath.Join(jobUploadDir, "original_"+fileName)
	if err := os.Rename(incomingPath, uploadPath); err != nil {
		_ = os.Remove(incomingPath)
		h.jobManager.UpdateJobError(job.ID, "Failed to finalize uploaded file")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to finalize upload"})
		return
	}

	metadata, probeErr := h.inspector.ProbeFile(ctx, uploadPath, fileType)
	if probeErr != nil {
		log.Printf("metadata probe failed for S3 video job %s: %v", job.ID, probeErr)
	}
	if metadata == nil {
		metadata = &services.MediaMetadata{FileType: fileType, MimeType: mimeType, Details: map[string]any{}, Error: stringOrErr(probeErr)}
	}
	if err := services.WriteMetadata(filepath.Join(jobOutputDir, "metadata.json"), metadata); err != nil {
		log.Printf("failed to write metadata for S3 video job %s: %v", job.ID, err)
	}

	if !isTranscribeMode(job) && specializedMode(job) == "" {
		h.analysisJobs.Enqueue(services.AnalysisJob{JobID: job.ID, InputPath: uploadPath, OutputDir: jobOutputDir, FileType: fileType, MimeType: mimeType})
	}
	go h.processConversion(job, uploadPath, jobOutputDir)

	c.JSON(http.StatusOK, models.UploadResponse{JobID: job.ID})
}

func (h *ConversionHandler) downloadS3Object(ctx context.Context, key, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
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

func sanitizeExtension(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	ext = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, ext)
	return ext
}

func normalizeUploadContentType(contentType, ext string) string {
	contentType = strings.TrimSpace(contentType)
	if contentType != "" {
		return contentType
	}
	if detected := mime.TypeByExtension("." + ext); detected != "" {
		return detected
	}
	return "application/octet-stream"
}

func isSupportedVideoExtension(ext string) bool {
	switch ext {
	case "mp4", "mov", "m4v", "webm", "mkv", "avi", "flv", "wmv", "mpeg", "mpg":
		return true
	default:
		return false
	}
}

func sanitizeS3PathSegment(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	sanitized := strings.Trim(b.String(), "_")
	if sanitized == "" {
		return uuid.NewString()
	}
	if len(sanitized) > 128 {
		return sanitized[:128]
	}
	return sanitized
}

func sanitizeUploadedVideoKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || strings.Contains(key, "..") {
		return ""
	}
	if !strings.HasPrefix(key, "videos/") || strings.HasSuffix(key, "/") {
		return ""
	}
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return ""
	}
	if sanitizeS3PathSegment(parts[1]) != parts[1] || sanitizeS3PathSegment(parts[2]) != parts[2] {
		return ""
	}
	ext := sanitizeExtension(filepath.Ext(parts[3]))
	if ext == "" || !isSupportedVideoExtension(ext) {
		return ""
	}
	return key
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
