package handlers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// LaunchVectorize implements VectorizeLauncher (MM43, POST /api/vector/vectorize).
//
// It is the /upload pipeline reduced to the one conversion Motion needs:
// raster image -> SVG through the existing potrace path (convertImageToSVG),
// with forced options {format:"svg"}. The multipart form is already parsed and
// the vector_export allowance already spent by the caller. Same job directory
// layout, ledger registration and durable job record as UploadFile, so the
// usual /api/job/:id, /api/download/:id, retention and history all apply.
func (h *ConversionHandler) LaunchVectorize(c *gin.Context, threshold, turdSize int) {
	file, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no file provided"})
		return
	}
	defer file.Close()

	options := map[string]interface{}{
		"format":  "svg",
		"quality": 90,
		"filter":  "none",
		// normalizeVectorizeParams clamps both (threshold 1..99, default 50;
		// turd >= 0, default 2); zero means "use the default".
		"vectorize": map[string]interface{}{"threshold": threshold, "turdSize": turdSize},
	}

	jobID := newJobID()
	incomingPath := stagedUploadPath(h.cfg.UploadDir, jobID, "incoming", safeFilename(fileHeader.Filename))
	if err := h.saveUploadedFile(file, incomingPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()
	fileType, mimeType := h.inspector.DetectFile(ctx, incomingPath, fileHeader.Header.Get("Content-Type"))
	if fileType != models.FileTypeImage {
		discardStaged(incomingPath)
		c.JSON(http.StatusBadRequest, gin.H{"error": "vectorize_unsupported_file", "message": "Vectorize takes a raster image"})
		return
	}

	originalFile := models.OriginalFileInfo{Name: fileHeader.Filename, Size: fileHeader.Size, Type: mimeType}
	job := h.jobManager.CreateJobWithID(jobID, originalFile, options)
	recordJobCreated(c, h.jobManager, job, services.JobFacts{
		Tool:         "vector_vectorize",
		MediaKind:    mediaKindFor(fileType),
		SourceFormat: formatOf(fileHeader.Filename),
		TargetFormat: "svg",
	})

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
	registerJobDirs(c, job.ID, true, true)

	uploadPath := filepath.Join(jobUploadDir, "original_"+safeFilename(fileHeader.Filename))
	if err := os.Rename(incomingPath, uploadPath); err != nil {
		h.jobManager.UpdateJobError(job.ID, "Failed to finalize uploaded file")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to finalize upload"})
		return
	}
	go h.processConversion(job, uploadPath, jobOutputDir)
	c.JSON(http.StatusOK, models.UploadResponse{JobID: job.ID})
}

// S3ThumbnailPresigner is the production ThumbnailPresigner: a presigned PUT
// with the content type and length pinned, like PresignAsset's.
type S3ThumbnailPresigner struct {
	Client *s3.PresignClient
	Bucket string
}

// PresignPut implements ThumbnailPresigner.
func (p S3ThumbnailPresigner) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, error) {
	if p.Client == nil || p.Bucket == "" {
		return "", errors.New("s3 presign is not configured")
	}
	in := &s3.PutObjectInput{Bucket: aws.String(p.Bucket), Key: aws.String(key), ContentType: aws.String(contentType)}
	if size > 0 {
		in.ContentLength = aws.Int64(size)
	}
	res, err := p.Client.PresignPutObject(ctx, in, func(o *s3.PresignOptions) { o.Expires = ttl })
	if err != nil {
		return "", err
	}
	return res.URL, nil
}
