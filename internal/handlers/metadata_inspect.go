package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// inspectMetadata is swapped by tests to inject a fake tool Runner.
var inspectMetadata = metadata.Inspect

// InspectMetadata handles POST /api/metadata/inspect (multipart field "file").
// It returns the canonical MetadataDocument. Tool failure is a warning inside
// the document (HTTP 200); only a missing or oversized upload is a 400.
func (h *ConversionHandler) InspectMetadata(c *gin.Context) {
	file, fileHeader, err := h.multipartFile(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer file.Close()

	tempPath := filepath.Join(h.cfg.TempDir, fmt.Sprintf("metadata_%d_%s", time.Now().UnixNano(), safeFilename(fileHeader.Filename)))
	hasher := sha256.New()
	if err := h.saveUploadedFile(io.TeeReader(file, hasher), tempPath); err != nil {
		_ = os.Remove(tempPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save temporary file"})
		return
	}
	defer func() { _ = os.Remove(tempPath) }()

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()

	insp := h.inspector
	if insp == nil {
		insp = services.NewMediaInspector(h.cfg.CommandTimeout)
	}
	fileType, mimeType := insp.DetectFile(ctx, tempPath, fileHeader.GetHeader("Content-Type"))

	doc := inspectMetadata(ctx, tempPath, string(fileType))
	// Report the upload's identity, not the server temp file's.
	doc.File.Name = fileHeader.Filename
	for i := range doc.Entries {
		if metadata.IsUploadFileName(doc.Entries[i]) {
			doc.Entries[i].Value = fileHeader.Filename
		}
	}
	doc.File.Mime = mimeType
	doc.File.Kind = string(fileType)
	doc.File.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	c.JSON(http.StatusOK, doc)
}
