package handlers

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// MM24 (the gap MM23 flagged): GET /api/download/:jobId and GET /api/result/:jobId
// resolved a job from the in-memory JobManager map only, so after an API restart
// the links account history lists (served from Postgres and the ledger) 404ed.
// resolveRecordedResult is the fallback: it rebuilds enough of the job from
// mm_conversion_jobs + the ledger to serve the file that still sits in
// outputs/<jobId>/, under the SAME ownership rules as MM05.

const ctxResultName = "mm_result_name"

// resultFilePrefixes are the basenames the pipelines write into outputs/<jobId>/
// (see outputPath), in the order they are looked for.
var resultFilePrefixes = []string{"converted", "transcript", "translated", "stitched",
	"waveform", "audio", "silent", "frames", "trimmed", "image_restoration_results"}

// findResultFile locates the result file inside a job's output directory, or "".
func findResultFile(outputRoot, jobID string) string {
	dir := retention.JobDirPath(outputRoot, jobID)
	if dir == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	// MM74: a video summary's download is exactly summary.md (summary.json is
	// served by /api/summarize/:jobId/result).
	for _, e := range entries {
		if !e.IsDir() && e.Name() == services.SummaryMarkdownFile {
			return filepath.Join(dir, e.Name())
		}
	}
	for _, prefix := range resultFilePrefixes {
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), prefix+".") {
				continue
			}
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// resolveRecordedResult mirrors the tail of resolveCompletedResult for a job the
// process no longer remembers. On failure it has written the response.
func (h *ConversionHandler) resolveRecordedResult(c *gin.Context, jobID string, allowQuerySID bool) (*models.ConversionJob, string, os.FileInfo, bool) {
	rec, err := h.records.JobRecord(c.Request.Context(), jobID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Job lookup is temporarily unavailable"})
		return nil, "", nil, false
	}
	if !rec.Found || !authorizeRecordAccess(c, rec, jobID, allowQuerySID) {
		denyJobAccess(c)
		return nil, "", nil, false
	}
	if rec.Status != string(models.StatusCompleted) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job not completed"})
		return nil, "", nil, false
	}
	path := ""
	if !rec.AnyLedger || rec.AnyLive {
		path = findResultFile(h.cfg.OutputDir, jobID)
	}
	var info os.FileInfo
	if path != "" {
		info, err = os.Stat(path)
	}
	if path == "" || err != nil || info.IsDir() {
		c.JSON(http.StatusNotFound, gin.H{"error": "Converted file not found"})
		return nil, "", nil, false
	}
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	name := rec.ResultFileName
	if name == "" {
		name = filepath.Base(path)
	}
	c.Set(ctxResultName, name)
	job := &models.ConversionJob{
		ID: jobID, Status: models.StatusCompleted, Mode: rec.Mode, ResultFileName: name,
		Options: map[string]interface{}{"format": ext},
	}
	return job, path, info, true
}

// resultName is the download filename: the recorded name for a job rebuilt from
// Postgres, otherwise the usual derivation.
func (h *ConversionHandler) resultName(c *gin.Context, job *models.ConversionJob) string {
	if v, ok := c.Get(ctxResultName); ok {
		if s, _ := v.(string); s != "" {
			return s
		}
	}
	return h.getOutputFilename(job)
}
