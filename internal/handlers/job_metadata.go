package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// metadataJob resolves :jobId and enforces ownership. On failure the response
// has been written. Strangers get the same 404 as a missing job.
func (h *ConversionHandler) metadataJob(c *gin.Context, allowQuerySID bool) (*models.ConversionJob, bool) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job ID is required"})
		return nil, false
	}
	job, err := h.jobManager.GetJob(jobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		return nil, false
	}
	if !authorizeJobAccessSID(c, h.jobManager, job, allowQuerySID) {
		denyJobAccess(c)
		return nil, false
	}
	return job, true
}

// metadataSide parses ?side= (default original). ok is false after a 400.
func metadataSide(c *gin.Context) (side string, ok bool) {
	side = strings.TrimSpace(c.Query("side"))
	switch side {
	case "":
		return "original", true
	case "original", "result":
		return side, true
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "side must be original or result"})
	return "", false
}

// writeMetadataErr maps a document error to a response.
func writeMetadataErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errMetadataMissing):
		c.JSON(http.StatusNotFound, gin.H{"error": "Metadata not available"})
	case errors.Is(err, errResultMissing):
		c.JSON(http.StatusNotFound, gin.H{"error": "Converted file not found"})
	case c.Request.Context().Err() != nil:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Request cancelled"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read metadata"})
	}
}

// GetJobMetadata serves GET /api/job/:jobId/metadata?side=original|result.
func (h *ConversionHandler) GetJobMetadata(c *gin.Context) {
	job, ok := h.metadataJob(c, false)
	if !ok {
		return
	}
	side, ok := metadataSide(c)
	if !ok {
		return
	}
	if side == "original" {
		doc, err := h.originalDoc(c.Request.Context(), job)
		if err != nil {
			writeMetadataErr(c, err)
			return
		}
		c.JSON(http.StatusOK, doc)
		return
	}
	if job.Status != models.StatusCompleted {
		c.JSON(http.StatusConflict, gin.H{"error": "Job not completed"})
		return
	}
	doc, _, err := h.resultDoc(c.Request.Context(), job)
	if err != nil {
		writeMetadataErr(c, err)
		return
	}
	c.JSON(http.StatusOK, doc)
}
