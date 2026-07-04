package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// DocumentScanHandler serves the AI Document Scan endpoints (printed/handwritten
// pages → searchable PDF / structured DOCX). Like ImageRestoreHandler it gets
// its own handler struct so the pipeline's deps — GPU manager, telemetry store,
// command-audit runner — don't bleed into the conversion handler. This tool is
// PUBLIC (no Firebase): it mounts on the plain /api group.
type DocumentScanHandler struct {
	jobManager *services.JobManager
	cfg        *config.Config
	inspector  *services.MediaInspector
	svc        *services.DocumentScanService
}

// NewDocumentScanHandler wires the document-scan service.
func NewDocumentScanHandler(jobManager *services.JobManager, cfg *config.Config, inspector *services.MediaInspector, gpuMgr *gpu.Manager, store *telemetry.Store, runner *cmdaudit.Runner) *DocumentScanHandler {
	return &DocumentScanHandler{
		jobManager: jobManager,
		cfg:        cfg,
		inspector:  inspector,
		svc:        services.NewDocumentScanService(cfg, jobManager, gpuMgr, store, runner),
	}
}

// RegisterDocumentScanRoutes mounts the document-scan endpoints.
func RegisterDocumentScanRoutes(r gin.IRouter, h *DocumentScanHandler) {
	r.GET("/document-scan/capabilities", h.GetCapabilities)
	r.POST("/document-scan/start", h.Start) // multipart image_0..n + options; 202 {jobId}
	r.GET("/document-scan/:jobId/results", h.GetResults)
	r.GET("/document-scan/:jobId/result", h.GetResultFile) // ?format=pdf|docx|summary ; pdf => inline
}

// GetCapabilities reports feature flags + per-engine availability (cheap probes).
func (h *DocumentScanHandler) GetCapabilities(c *gin.Context) {
	c.JSON(http.StatusOK, h.svc.Capabilities())
}

// Start validates the multipart request (image_0..n + options JSON), creates the
// job with the full stage timeline pre-populated, kicks the pipeline goroutine,
// and returns 202 with the job id.
func (h *DocumentScanHandler) Start(c *gin.Context) {
	if !h.cfg.DocumentScanEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Document scanning is currently disabled"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.cfg.MaxFileSize)
	if err := c.Request.ParseMultipartForm(h.cfg.MaxFileSize); err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "The upload is too large or the form is malformed"})
		return
	}

	// --- options ----------------------------------------------------------
	var opts models.DocumentScanOptions
	if raw := strings.TrimSpace(c.Request.FormValue("options")); raw != "" {
		// Detect explicit booleans so mode-driven defaults (preclean/verify)
		// only apply when the client omitted them.
		var probe map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &probe)
		if err := json.Unmarshal([]byte(raw), &opts); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid options format"})
			return
		}
		if _, ok := probe["preclean"]; ok {
			opts.PrecleanSet = true
		}
		if _, ok := probe["verify"]; ok {
			opts.VerifySet = true
		}
	}

	caps := h.svc.Capabilities()

	mode, err := services.NormalizeDocumentScanMode(opts.ContentMode)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	outputs, err := services.NormalizeDocumentScanOutputs(opts.Outputs, h.cfg.DocumentScanDocxEnabled)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	language, err := services.NormalizeDocumentScanLanguage(opts.Language, h.cfg.DocumentScanLanguages)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	structureEngine, err := services.NormalizeDocumentScanStructureEngine(opts.StructureEngine, h.cfg.DocumentScanStructureEngine)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	secondOpinionEngine, err := services.NormalizeDocumentScanSecondOpinionEngine(opts.SecondOpinionEngine, h.cfg.DocumentScanSecondOpinionEngine)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// --- collect image_0..image_n ----------------------------------------
	form := c.Request.MultipartForm
	fieldNames := make([]string, 0)
	if form != nil {
		for name := range form.File {
			if strings.HasPrefix(name, "image_") {
				fieldNames = append(fieldNames, name)
			}
		}
	}
	if len(fieldNames) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No images provided"})
		return
	}
	if err := services.ValidateDocumentScanCounts(len(fieldNames), h.cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ordered, err := services.OrderDocumentScanImages(fieldNames, opts.Order)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// --- availability gates (friendly 4xx) -------------------------------
	if mode == models.DocumentScanModeHandwriting && !caps.HandwritingAvailable {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Handwriting transcription is unavailable right now (the AI engine is offline)"})
		return
	}
	if mode == models.DocumentScanModePrinted && !caps.PrintedAvailable {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Printed-document OCR is unavailable on this server"})
		return
	}
	if mode == models.DocumentScanModeAuto && !caps.PrintedAvailable && !caps.HandwritingAvailable {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Document scanning is unavailable right now"})
		return
	}
	wantsDocx := false
	for _, o := range outputs {
		if o == models.DocumentScanOutputDOCX {
			wantsDocx = true
		}
	}
	if wantsDocx && !caps.DOCXAvailable {
		c.JSON(http.StatusBadRequest, gin.H{"error": "DOCX output is unavailable right now (no document-structure engine reachable)"})
		return
	}
	if opts.Summarize && !caps.SummaryAvailable {
		c.JSON(http.StatusBadRequest, gin.H{"error": "AI summary is unavailable right now"})
		return
	}

	// Resolve mode-driven defaults. Preclean defaults ON for handwriting/auto,
	// OFF for printed, unless the client sent an explicit value. Verify defaults
	// ON. Both degrade to false when the engine is unavailable.
	preclean := opts.Preclean
	if !opts.PrecleanSet {
		preclean = mode != models.DocumentScanModePrinted
	}
	preclean = preclean && caps.PrecleanAvailable
	verify := opts.Verify
	if !opts.VerifySet {
		verify = true
	}
	secondOpinion := opts.SecondOpinion && secondOpinionEngine != "none" && caps.SecondOpinionAvailable

	// --- create the job + save uploads into uploads/{jobId}/src/ ---------
	originalFile := models.OriginalFileInfo{
		Name: fmt.Sprintf("%d page(s)", len(ordered)),
		Type: "image",
	}
	options := map[string]interface{}{
		"mode":          string(mode),
		"outputs":       documentScanOutputStrings(outputs),
		"pageCount":     len(ordered),
		"language":      language,
		"preclean":      preclean,
		"verify":        verify,
		"secondOpinion": secondOpinion,
		"summarize":     opts.Summarize,
	}
	job := h.jobManager.CreateJob(originalFile, options)
	_ = h.jobManager.SetMode(job.ID, "document_scan")

	srcDir := filepath.Join(h.cfg.UploadDir, job.ID, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to prepare upload")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()

	sourcePaths := make([]string, 0, len(ordered))
	for idx, fieldName := range ordered {
		headers := form.File[fieldName]
		if len(headers) == 0 || headers[0] == nil {
			_ = h.jobManager.UpdateJobError(job.ID, "Failed to read an uploaded image")
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read an uploaded image"})
			return
		}
		fileHeader := headers[0]
		if h.cfg.DocumentScanMaxImageBytes > 0 && fileHeader.Size > h.cfg.DocumentScanMaxImageBytes {
			_ = h.jobManager.UpdateJobError(job.ID, "An uploaded image is too large")
			c.JSON(http.StatusBadRequest, gin.H{"error": "An uploaded image is too large"})
			return
		}
		dst, derr := saveDocumentScanUpload(fileHeader, srcDir, idx+1)
		if derr != nil {
			_ = h.jobManager.UpdateJobError(job.ID, "Failed to save an uploaded image")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save an uploaded image"})
			return
		}
		fileType, _ := h.inspector.DetectFile(ctx, dst, fileHeader.Header.Get("Content-Type"))
		if fileType != models.FileTypeImage {
			_ = h.jobManager.UpdateJobError(job.ID, "One of the uploaded files is not a supported image")
			c.JSON(http.StatusBadRequest, gin.H{"error": "One of the uploaded files is not a supported image"})
			return
		}
		sourcePaths = append(sourcePaths, dst)
	}

	sessionID := sanitizeS3PathSegment(firstNonEmpty(c.GetHeader("X-MM-Session-ID"), opts.SessionID))
	req := services.DocumentScanRequest{
		JobID:               job.ID,
		SourcePaths:         sourcePaths,
		Outputs:             outputs,
		Mode:                mode,
		Language:            language,
		Deskew:              opts.Deskew,
		Rotate:              opts.Rotate,
		Clean:               opts.Clean,
		Preclean:            preclean,
		StructureEngine:     structureEngine,
		SecondOpinion:       secondOpinion,
		SecondOpinionEngine: secondOpinionEngine,
		Verify:              verify,
		Summarize:           opts.Summarize,
		SessionID:           sessionID,
		RequestID:           c.Writer.Header().Get("X-MM-Request-ID"),
	}
	_ = h.jobManager.ReplaceStages(job.ID, h.svc.BuildStages(req), "queued")

	go h.svc.Process(context.Background(), req)

	c.JSON(http.StatusAccepted, models.DocumentScanStartResponse{JobID: job.ID})
}

// GetResults serves the manifest-derived results listing for a completed
// document-scan job (no filesystem paths).
func (h *DocumentScanHandler) GetResults(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	job, err := h.jobManager.GetJob(jobID)
	if err != nil || job.Mode != "document_scan" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		return
	}
	if job.Status != models.StatusCompleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job not completed"})
		return
	}
	resp, err := h.svc.Results(jobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Results not found"})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// GetResultFile streams one artifact. PDF renders inline; DOCX/summary download
// as attachments. The format is resolved against the manifest only.
func (h *DocumentScanHandler) GetResultFile(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	job, err := h.jobManager.GetJob(jobID)
	if err != nil || job.Mode != "document_scan" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	format := strings.TrimSpace(c.Query("format"))
	if format == "" {
		format = "pdf"
	}
	path, contentType, err := h.svc.ResultArtifactPath(jobID, format)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "private, max-age=3600")
	if contentType == "application/pdf" {
		c.Header("Content-Disposition", `inline; filename="document.pdf"`)
	} else {
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="document.%s"`, documentScanDownloadExt(format)))
	}
	c.File(path)
}

// saveDocumentScanUpload streams one uploaded page to uploads/{jobId}/src/.
func saveDocumentScanUpload(fileHeader *multipart.FileHeader, srcDir string, index int) (string, error) {
	src, err := fileHeader.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	ext := strings.ToLower(filepath.Ext(safeFilename(fileHeader.Filename)))
	if ext == "" {
		ext = ".img"
	}
	dst := filepath.Join(srcDir, fmt.Sprintf("page-%03d%s", index, ext))
	if err := saveMultipartTo(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func documentScanOutputStrings(outputs []models.DocumentScanOutput) []string {
	out := make([]string, 0, len(outputs))
	for _, o := range outputs {
		out = append(out, string(o))
	}
	return out
}

func documentScanDownloadExt(format string) string {
	switch strings.ToLower(format) {
	case "summary", "summary-docx":
		return "summary.docx"
	case "docx":
		return "docx"
	default:
		return "pdf"
	}
}
