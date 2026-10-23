package handlers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// maxBinaryEntryBytes caps a binary entry download (413 above).
const maxBinaryEntryBytes = 8 << 20

// binaryTimeout caps one exiftool -b extraction.
const binaryTimeout = 20 * time.Second

// binaryRunner executes exiftool for binary entries; tests swap in a fake.
var binaryRunner metadata.Runner = metadata.DefaultRunner()

// entryIDRE is the allowlist for "<group>:<tag>[#n]". Group and tag may not
// start with '-' or contain '..' (checked separately), and are never joined
// into anything but a single "-Group:Tag" argument.
var entryIDRE = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.-]*):([A-Za-z0-9_][A-Za-z0-9_.-]*)(?:#([0-9]{1,3}))?$`)

// parseEntryID validates a binary entry id and returns its exiftool selector.
func parseEntryID(id string) (selector string, dup int, ok bool) {
	m := entryIDRE.FindStringSubmatch(id)
	if m == nil || strings.Contains(id, "..") {
		return "", 0, false
	}
	dup = 1
	if m[3] != "" {
		n, err := strconv.Atoi(m[3])
		if err != nil || n < 1 {
			return "", 0, false
		}
		dup = n
	}
	return "-" + m[1] + ":" + m[2], dup, true
}

// originalUploadPath finds the stored upload of a job, if it still exists.
func (h *ConversionHandler) originalUploadPath(jobID string) (string, bool) {
	matches, _ := filepath.Glob(filepath.Join(h.cfg.UploadDir, jobID, "original_*"))
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && !st.IsDir() {
			return m, true
		}
	}
	return "", false
}

// GetJobMetadataBinary serves GET /api/job/:jobId/metadata/binary/:entryId.
func (h *ConversionHandler) GetJobMetadataBinary(c *gin.Context) {
	job, ok := h.metadataJob(c, false)
	if !ok {
		return
	}
	side, ok := metadataSide(c)
	if !ok {
		return
	}
	entryID := c.Param("entryId")
	selector, dup, valid := parseEntryID(entryID)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid entry id"})
		return
	}
	if dup > 1 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Repeated binary entries cannot be extracted individually"})
		return
	}

	var doc *metadata.MetadataDocument
	var path string
	var err error
	if side == "original" {
		doc, err = h.originalDoc(c.Request.Context(), job)
		if err == nil {
			var found bool
			if path, found = h.originalUploadPath(job.ID); !found {
				c.JSON(http.StatusNotFound, gin.H{"error": "Original file is no longer available"})
				return
			}
		}
	} else {
		if job.Status != models.StatusCompleted {
			c.JSON(http.StatusConflict, gin.H{"error": "Job not completed"})
			return
		}
		doc, _, err = h.resultDoc(c.Request.Context(), job)
		path = h.outputPath(job, h.jobOutputDir(job))
	}
	if err != nil {
		writeMetadataErr(c, err)
		return
	}
	var entry *metadata.MetadataEntry
	for i := range doc.Entries {
		if doc.Entries[i].ID == entryID {
			entry = &doc.Entries[i]
			break
		}
	}
	if entry == nil || !entry.Binary {
		c.JSON(http.StatusNotFound, gin.H{"error": "Binary entry not found"})
		return
	}
	// Rebuild the selector from the document's own validated parts, not the URL.
	if sel, _, ok := parseEntryID(entry.ID); ok {
		selector = sel
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), binaryTimeout)
	defer cancel()
	out, stderr, runErr := binaryRunner.Run(ctx, "exiftool", "-b", selector, "--", path)
	if runErr != nil && out == "" {
		switch {
		case errors.Is(runErr, context.DeadlineExceeded):
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "Extraction timed out"})
		default:
			_ = stderr
			c.JSON(http.StatusBadGateway, gin.H{"error": "Extraction failed"})
		}
		return
	}
	if len(out) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Binary entry is empty"})
		return
	}
	if len(out) > maxBinaryEntryBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Binary entry exceeds 8 MiB"})
		return
	}
	body := []byte(out)
	sniffed := http.DetectContentType(body)
	isImg := sniffed == "image/jpeg" || sniffed == "image/png"
	ctype, disp := "application/octet-stream", "attachment"
	if isImg {
		ctype = sniffed
		if c.Query("inline") == "1" {
			disp = "inline"
		}
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Disposition", disp+"; filename="+strconv.Quote(safeBinaryName(entry.Tag, sniffed)))
	c.Data(http.StatusOK, ctype, body)
}

func safeBinaryName(tag, ctype string) string {
	ext := ".bin"
	switch ctype {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	}
	return tag + ext
}

// GetJobMetadataDiff serves GET /api/job/:jobId/metadata/diff.
func (h *ConversionHandler) GetJobMetadataDiff(c *gin.Context) {
	job, ok := h.metadataJob(c, false)
	if !ok {
		return
	}
	res, err := h.computeJobDiff(c.Request.Context(), job)
	if err != nil {
		var ce *diffConflict
		if errors.As(err, &ce) {
			c.JSON(http.StatusConflict, gin.H{"error": ce.msg})
			return
		}
		writeMetadataErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

type diffConflict struct{ msg string }

func (e *diffConflict) Error() string { return e.msg }

func (h *ConversionHandler) computeJobDiff(ctx context.Context, job *models.ConversionJob) (metadata.DiffResult, error) {
	if job.Status != models.StatusCompleted {
		return metadata.DiffResult{}, &diffConflict{"Job not completed"}
	}
	orig, err := h.originalDoc(ctx, job)
	if err != nil {
		return metadata.DiffResult{}, err
	}
	res, inspectable, err := h.resultDoc(ctx, job)
	if err != nil {
		if errors.Is(err, errResultMissing) {
			return metadata.DiffResult{}, &diffConflict{"Result file not available"}
		}
		return metadata.DiffResult{}, err
	}
	if !inspectable {
		return metadata.DiffResult{}, &diffConflict{"Result has no comparable metadata"}
	}
	return metadata.Diff(orig, res), nil
}
