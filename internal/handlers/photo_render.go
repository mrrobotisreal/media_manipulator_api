package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM96: Aperture render API. Export (project, batch, stateless), subject mask
// (project, stateless) and heal (project, stateless). Design
// docs/design/PHOTO_EDITOR.md section 6.2/6.3; ticket MM96; coordinator notes
// D2 (heals), D3 (named masks), D5 (route contract).

// Job tools (JobFacts.Tool and Options["tool"]).
const (
	PhotoToolExport = "photo_export"
	PhotoToolHeal   = "photo_heal"

	photoDerivSubject = "subject_mask"
	photoDerivHeal    = "heal_base"
	photoHealedName   = "healed.png"
	photoMaxMaskBytes = 64 << 20
)

// quotaMetricPhotoBatch is the quotaRoutes key of POST /api/photo/batch: it is
// charged inside the handler, n units at once (middleware.SpendDeferredQuotaN).
const quotaMetricPhotoBatch = "photo_batch"

// QuotaMetricPhotoBatch is the quotaRoutes metric key of POST /photo/batch.
const QuotaMetricPhotoBatch = quotaMetricPhotoBatch

// IsPhotoExportPath matches POST /api/photo/projects/:id/export and /api/photo/export.
func IsPhotoExportPath(path string) bool {
	if path == "/api/photo/export" {
		return true
	}
	return photoProjectSub(path, "/export")
}

// IsPhotoSubjectMaskPath matches the project and stateless subject-mask routes.
func IsPhotoSubjectMaskPath(path string) bool {
	return path == "/api/photo/masks/subject" || photoProjectSub(path, "/masks/subject")
}

// IsPhotoHealPath matches the project and stateless heal routes.
func IsPhotoHealPath(path string) bool {
	return path == "/api/photo/heal" || photoProjectSub(path, "/heal")
}

func photoProjectSub(path, suffix string) bool {
	const p = "/api/photo/projects/"
	if !strings.HasPrefix(path, p) || !strings.HasSuffix(path, suffix) {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, p), suffix)
	return id != "" && !strings.Contains(id, "/")
}

// PhotoCaps is what the deployment can do (cached at first use).
type PhotoCaps struct {
	Render  bool
	AIMasks bool
	Heal    bool
}

// PhotoExporter renders one export (services.PhotoRenderer in production).
type PhotoExporter interface {
	RenderExport(ctx context.Context, req services.PhotoExportRequest) error
}

// PhotoAIRunner runs rembg and LaMa (services.PhotoAI in production).
type PhotoAIRunner interface {
	SubjectMask(ctx context.Context, jobID string, priority int, source, outMask string) error
	Heal(ctx context.Context, jobID string, priority int, source, mask, out string) error
}

// PhotoPipeline bundles the render and AI collaborators. Zero seams fall back
// to the production helpers; tests replace them (and run jobs inline via Async).
type PhotoPipeline struct {
	Jobs   *services.JobManager
	Render PhotoExporter
	AI     PhotoAIRunner
	// Probe returns the EXIF-upright size and alpha flag of an image file.
	Probe func(ctx context.Context, path string) (w, h int, alpha bool, err error)
	// Proxy makes the display proxy of a heal base.
	Proxy func(ctx context.Context, in, out string) error
	// Async runs a job body; nil = go f().
	Async func(f func())
}

func (p *PhotoPipeline) probe(ctx context.Context, path string) (int, int, bool, error) {
	if p.Probe != nil {
		return p.Probe(ctx, path)
	}
	return services.PhotoProbe(ctx, path)
}

func (p *PhotoPipeline) proxy(ctx context.Context, in, out string) error {
	if p.Proxy != nil {
		return p.Proxy(ctx, in, out)
	}
	return services.PhotoBaseProxy(ctx, in, out)
}

func (p *PhotoPipeline) async(f func()) {
	if p.Async != nil {
		p.Async(f)
		return
	}
	go f()
}

// SetPipeline attaches the render/AI collaborators (nil = those routes 503).
func (h *PhotoHandler) SetPipeline(p *PhotoPipeline) {
	if h != nil {
		h.pipe = p
	}
}

// SetCapabilities fixes the capability flags (tests, and boot-time wiring that
// already knows); otherwise they are probed from disk once.
func (h *PhotoHandler) SetCapabilities(c PhotoCaps) {
	if h == nil {
		return
	}
	h.capsOnce.Do(func() {})
	h.caps = c
}

func (h *PhotoHandler) capabilities() PhotoCaps {
	if h == nil {
		return PhotoCaps{}
	}
	h.capsOnce.Do(func() {
		if h.cfg == nil {
			return
		}
		exists := func(p string) bool {
			p = strings.TrimSpace(p)
			if p == "" {
				return false
			}
			st, err := os.Stat(p)
			return err == nil && !st.IsDir()
		}
		c := h.cfg
		h.caps = PhotoCaps{
			Render:  exists(c.PhotoPython) && exists(c.PhotoRenderScript),
			AIMasks: c.AIEnabled && exists(c.RembgBin),
			Heal:    c.AIEnabled && exists(c.LamaPython) && exists(c.RemoveObjectScript),
		}
	})
	return h.caps
}

// pipeReady answers 503 when the render pipeline or the named capability is
// missing. The quota unit the route spent is refunded by the middleware (5xx).
func (h *PhotoHandler) pipeReady(c *gin.Context, need string) bool {
	if h.pipe == nil || h.pipe.Jobs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "photo_render_unavailable", "message": "Aperture rendering is not configured on this deployment"})
		return false
	}
	caps := h.capabilities()
	switch need {
	case "render":
		if !caps.Render || h.pipe.Render == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "photo_render_unavailable", "message": "The Aperture renderer is not installed on this deployment"})
			return false
		}
	case "mask":
		if !caps.AIMasks || h.pipe.AI == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "photo_ai_unavailable", "message": "Subject masks are not available on this deployment"})
			return false
		}
	case "heal":
		if !caps.Heal || h.pipe.AI == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "photo_ai_unavailable", "message": "Heal is not available on this deployment"})
			return false
		}
	}
	return true
}

// --- shared helpers --------------------------------------------------------

var (
	photoHashRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	photoWinChars = strings.NewReplacer(`"`, "'", ":", "-", "*", "", "?", "", "<", "", ">", "", "|", "-", "\t", " ")
)

func sha256Hex(b ...[]byte) string {
	h := sha256.New()
	for _, p := range b {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// photoResultName is "<name> (retouched).<ext>" with a filesystem-safe name.
func photoResultName(name, ext string) string {
	return photoSafeStem(name) + " (retouched)." + ext
}

func photoSafeStem(name string) string {
	name = safeFilename(name)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.TrimSpace(strings.TrimLeft(photoWinChars.Replace(name), "."))
	if name == "" {
		name = "photo"
	}
	if len(name) > 120 {
		cut := 120
		for cut > 0 && !utf8Start(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return name
}

type photoExportParams struct {
	Format        string
	Quality       int
	MetadataMode  string
	MaxMegapixels int
	Name          string
}

// normalizePhotoExport validates format/quality/metadataMode (400
// photo_export_invalid) and applies the defaults.
func normalizePhotoExport(c *gin.Context, p photoExportParams) (photoExportParams, bool) {
	p.Format = strings.ToLower(strings.TrimSpace(p.Format))
	if p.Format == "jpeg" {
		p.Format = "jpg"
	}
	if p.Format == "" {
		p.Format = "jpg"
	}
	if _, ok := services.PhotoFormats[p.Format]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "photo_export_invalid", "message": "format must be jpg, png, webp, avif or tiff"})
		return p, false
	}
	if p.Quality == 0 {
		p.Quality = 92
	}
	if p.Quality < 1 {
		p.Quality = 1
	}
	if p.Quality > 100 {
		p.Quality = 100
	}
	p.MetadataMode = strings.ToLower(strings.TrimSpace(p.MetadataMode))
	switch p.MetadataMode {
	case "":
		p.MetadataMode = "keep"
	case "keep", "strip":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "photo_export_invalid", "message": "metadataMode must be keep or strip"})
		return p, false
	}
	if p.MaxMegapixels < 0 {
		p.MaxMegapixels = 0
	}
	return p, true
}

// exportMegapixels is min(requested, the tier cap, the deployment ceiling).
func (h *PhotoHandler) exportMegapixels(c *gin.Context, requested int) int {
	cap := h.maxMegapixels()
	if res, ok := middleware.ResolveTier(c); ok && res.Limits.PhotoMaxMegapixels > 0 && res.Limits.PhotoMaxMegapixels < cap {
		cap = res.Limits.PhotoMaxMegapixels
	} else if h.cfg != nil && !ok && h.cfg.TierFreePhotoMaxMegapixels > 0 && h.cfg.TierFreePhotoMaxMegapixels < cap {
		cap = h.cfg.TierFreePhotoMaxMegapixels
	}
	if requested > 0 && requested < cap {
		return requested
	}
	return cap
}

// photoJobOwner/charge hand-off for one created job.
func (h *PhotoHandler) createPhotoJob(c *gin.Context, jobID string, original models.OriginalFileInfo, options map[string]interface{},
	facts services.JobFacts, charge *middleware.QuotaCharge) *models.ConversionJob {
	jm := h.pipe.Jobs
	job := jm.CreateJobWithID(jobID, original, options)
	jm.RecordJobCreated(c.Request.Context(), job.ID, jobOwnerFor(c), facts)
	jm.SetJobGPUPriority(job.ID, gpuPriorityFor(c))
	if charge != nil {
		jm.SetJobQuotaCharge(job.ID, charge)
	}
	return job
}

// photoBaseFor picks the project's render base (D2): the last heals[] entry
// whose baseRef is a server base that exists on disk, else the original.
func (h *PhotoHandler) photoBaseFor(p *telemetry.PhotoProject, doc []byte) string {
	dir := h.projectDir(p.ID)
	var d struct {
		Heals []struct {
			BaseRef string `json:"baseRef"`
		} `json:"heals"`
	}
	_ = json.Unmarshal(doc, &d)
	for i := len(d.Heals) - 1; i >= 0; i-- {
		ref := d.Heals[i].BaseRef
		if !strings.HasPrefix(ref, "bases/") || !photoFileRe.MatchString(ref) {
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(ref))
		if !retention.IsWithin(path, dir) {
			continue
		}
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path
		}
	}
	return filepath.Join(dir, "original."+p.OriginalExt)
}

// startExport creates and launches one export job. charge is the unit this job
// carries (refunded if it fails).
type photoExportSpec struct {
	project     *telemetry.PhotoProject // nil for the stateless form
	srcPath     string                  // absolute render base
	srcRel      string                  // stateless: relative to UPLOAD_DIR (shown in Options)
	origPath    string                  // metadata source
	displayName string
	origExt     string
	origBytes   int64
	hasAlpha    bool
	doc         []byte
	params      photoExportParams
	masksDir    string
}

func (h *PhotoHandler) startExport(c *gin.Context, jobID string, s photoExportSpec, charge *middleware.QuotaCharge) *models.ConversionJob {
	maxMP := h.exportMegapixels(c, s.params.MaxMegapixels)
	opts := map[string]interface{}{
		"tool": PhotoToolExport, "mode": s.params.Format, "format": s.params.Format,
		"quality": s.params.Quality, "metadataMode": s.params.MetadataMode, "maxMegapixels": maxMP,
	}
	if s.project != nil {
		opts["projectId"] = s.project.ID
	} else {
		opts["sourcePath"] = s.srcRel
	}
	mime := "image/" + s.origExt
	if m, ok := photoExtMime[s.origExt]; ok {
		mime = m
	}
	job := h.createPhotoJob(c, jobID, models.OriginalFileInfo{Name: photoSafeStem(s.displayName) + "." + s.origExt, Size: s.origBytes, Type: mime},
		opts, services.JobFacts{Tool: PhotoToolExport, Mode: s.params.Format, MediaKind: "image", SourceFormat: s.origExt, TargetFormat: s.params.Format}, charge)
	_ = h.pipe.Jobs.SetMode(job.ID, s.params.Format)
	resultName := photoResultName(s.displayName, s.params.Format)
	_ = h.pipe.Jobs.SetResultMetadata(job.ID, "", resultName, time.Time{})
	outDir := filepath.Join(h.outputRoot(), job.ID)
	_ = os.MkdirAll(outDir, 0o755)
	req := services.PhotoExportRequest{
		JobID: job.ID, Input: s.srcPath, OriginalPath: s.origPath, Doc: s.doc, MasksDir: s.masksDir,
		OutPath: filepath.Join(outDir, resultName), Format: s.params.Format, Quality: s.params.Quality,
		MetadataMode: s.params.MetadataMode, MaxMegapixels: maxMP, HasAlpha: s.hasAlpha,
	}
	timeout := services.PhotoRenderTimeout(h.cfg)
	jm := h.pipe.Jobs
	h.pipe.async(func() {
		_ = jm.UpdateJobStatus(job.ID, models.StatusProcessing)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := h.pipe.Render.RenderExport(ctx, req); err != nil {
			log.Printf("photo export %s failed: %v", job.ID, err)
			_ = jm.UpdateJobError(job.ID, "Export failed: "+err.Error())
			return
		}
		st, err := os.Stat(req.OutPath)
		if err != nil || st.IsDir() {
			_ = jm.UpdateJobError(job.ID, "Export produced no file")
			return
		}
		_ = jm.SetResultSize(job.ID, st.Size())
		_ = jm.SetResultMime(job.ID, services.PhotoFormats[s.params.Format])
		_ = jm.UpdateJobResult(job.ID, "/api/download/"+job.ID)
		_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	})
	return job
}

func (h *PhotoHandler) outputRoot() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.OutputDir) != "" {
		return h.cfg.OutputDir
	}
	return "outputs"
}

func (h *PhotoHandler) uploadRoot() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.UploadDir) != "" {
		return h.cfg.UploadDir
	}
	return "uploads"
}

func (h *PhotoHandler) tempRoot() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.TempDir) != "" {
		_ = os.MkdirAll(h.cfg.TempDir, 0o755)
		return h.cfg.TempDir
	}
	return ""
}

func upgradePathForCaller(c *gin.Context) string {
	if res, ok := middleware.ResolveTier(c); ok {
		return vectorUpgradePath(res.Tier)
	}
	return "signup"
}

// --- project export ----------------------------------------------------------

type photoExportBody struct {
	Doc           json.RawMessage `json:"doc"`
	Format        string          `json:"format"`
	Quality       int             `json:"quality"`
	MetadataMode  string          `json:"metadataMode"`
	MaxMegapixels int             `json:"maxMegapixels"`
	Name          string          `json:"name"`
}

// ProjectExport handles POST /photo/projects/:id/export (owner; one `ops`).
//
//	202 {jobId}; 409 revision_conflict {revision}; 422 photo_doc_invalid; 400 photo_export_invalid
func (h *PhotoHandler) ProjectExport(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, ok := requirePhotoOwner(c, h.store, h.logger, id); !ok {
		return
	}
	if !h.pipeReady(c, "render") {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(h.maxDocBytes())+64*1024)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_doc_too_large", "maxBytes": h.maxDocBytes()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	var body photoExportBody
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
			return
		}
	}
	params, ok := normalizePhotoExport(c, photoExportParams{Format: body.Format, Quality: body.Quality, MetadataMode: body.MetadataMode, MaxMegapixels: body.MaxMegapixels, Name: body.Name})
	if !ok {
		return
	}
	p, err := h.store.GetPhotoProject(c.Request.Context(), id)
	if errors.Is(err, telemetry.ErrPhotoNotFound) {
		denyPhotoProject(c)
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Project lookup is temporarily unavailable"})
		return
	}
	if p.Status != telemetry.PhotoStatusReady {
		c.JSON(http.StatusConflict, gin.H{"error": "photo_not_ready", "status": p.Status})
		return
	}
	doc := p.Doc
	if len(body.Doc) > 0 && string(body.Doc) != "null" {
		clean, ok := h.sanitizeDoc(c, body.Doc)
		if !ok {
			return
		}
		expected := p.Revision
		if v, present, valid := parseIfMatch(c.GetHeader("If-Match")); present {
			if !valid {
				c.JSON(http.StatusBadRequest, gin.H{"error": "if_match_invalid", "message": "If-Match must be the integer revision"})
				return
			}
			expected = v
		}
		saved, err := h.store.SavePhotoProject(c.Request.Context(), id, p.OwnerUserID, expected, nil, clean)
		var conflict *telemetry.PhotoRevisionConflictError
		switch {
		case errors.As(err, &conflict):
			c.JSON(http.StatusConflict, gin.H{"error": "revision_conflict", "revision": conflict.Revision})
			return
		case errors.Is(err, telemetry.ErrPhotoNotFound):
			denyPhotoProject(c)
			return
		case err != nil:
			h.logger.Warn("photo: save before export failed", "error", err.Error())
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to save project"})
			return
		}
		p, doc = saved, clean
	}
	name := clampPhotoName(params.Name, p.Name)
	jobID := newJobID()
	dir := h.projectDir(id)
	job := h.startExport(c, jobID, photoExportSpec{
		project: p, srcPath: h.photoBaseFor(p, doc), origPath: filepath.Join(dir, "original."+p.OriginalExt),
		displayName: name, origExt: p.OriginalExt, origBytes: p.OriginalBytes, hasAlpha: p.HasAlpha,
		doc: doc, params: params, masksDir: filepath.Join(dir, "masks"),
	}, middleware.QuotaChargeOf(c))
	registerJobDirs(c, job.ID, false, true)
	c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID})
}

// --- batch -------------------------------------------------------------------

type photoBatchBody struct {
	ProjectIDs   []string        `json:"projectIds"`
	Doc          json.RawMessage `json:"doc"`
	Format       string          `json:"format"`
	Quality      int             `json:"quality"`
	MetadataMode string          `json:"metadataMode"`
}

// Batch handles POST /photo/batch: one export per project, n x `ops` up front,
// refunded per failed job.
//
//	202 {jobs:[{projectId,jobId}]}; 403 sign_in_required / photo_batch_limit {limit,upgradePath}
func (h *PhotoHandler) Batch(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	userID, _ := callerIdentity(c)
	limit := 0
	if res, ok := middleware.ResolveTier(c); ok {
		limit = res.Limits.PhotoBatch
	} else if h.cfg != nil {
		limit = h.cfg.TierFreePhotoBatch
	}
	if userID == "" && limit <= 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "sign_in_required", "message": "Sign in to apply edits to several photos", "upgradePath": "signup"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(h.maxDocBytes())+256*1024)
	var body photoBatchBody
	if err := json.NewDecoder(c.Request.Body).Decode(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if limit <= 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "photo_batch_limit", "limit": 0, "upgradePath": upgradePathForCaller(c)})
		return
	}
	ids := dedupeStrings(body.ProjectIDs)
	if len(ids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "photo_batch_invalid", "message": "projectIds must list at least one project"})
		return
	}
	if len(ids) > limit {
		c.JSON(http.StatusForbidden, gin.H{"error": "photo_batch_limit", "limit": limit, "upgradePath": upgradePathForCaller(c)})
		return
	}
	if !h.pipeReady(c, "render") {
		return
	}
	params, ok := normalizePhotoExport(c, photoExportParams{Format: body.Format, Quality: body.Quality, MetadataMode: body.MetadataMode})
	if !ok {
		return
	}
	if len(body.Doc) == 0 || string(body.Doc) == "null" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "photo_batch_invalid", "message": "doc is required"})
		return
	}
	clean, ok := h.sanitizeDoc(c, body.Doc)
	if !ok {
		return
	}
	// Ownership of every project is checked before anything is spent or saved.
	projects := make([]*telemetry.PhotoProject, 0, len(ids))
	for _, id := range ids {
		if _, ok := requirePhotoOwner(c, h.store, h.logger, id); !ok {
			return
		}
		p, err := h.store.GetPhotoProject(c.Request.Context(), id)
		if errors.Is(err, telemetry.ErrPhotoNotFound) {
			denyPhotoProject(c)
			return
		}
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Project lookup is temporarily unavailable"})
			return
		}
		if p.Status != telemetry.PhotoStatusReady {
			c.JSON(http.StatusConflict, gin.H{"error": "photo_not_ready", "projectId": id, "status": p.Status})
			return
		}
		projects = append(projects, p)
	}
	charges, proceed := middleware.SpendDeferredQuotaN(c, models.MetricOps, len(projects))
	if !proceed {
		return
	}
	refundFrom := func(i int) {
		for ; i < len(charges); i++ {
			charges[i].Refund("batch_aborted")
		}
	}
	jobs := make([]gin.H, 0, len(projects))
	for i, p := range projects {
		// The template's heals/masks refer to the template photo's files: each
		// project keeps its own source size and starts from its own base.
		perDoc, err := h.batchDocFor(clean, p)
		if err != nil {
			refundFrom(i)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "photo_batch_failed", "jobs": jobs})
			return
		}
		saved, err := h.store.SavePhotoProject(c.Request.Context(), p.ID, p.OwnerUserID, p.Revision, nil, perDoc)
		if err != nil {
			refundFrom(i)
			status := http.StatusServiceUnavailable
			code := "photo_batch_failed"
			var conflict *telemetry.PhotoRevisionConflictError
			if errors.As(err, &conflict) {
				status, code = http.StatusConflict, "revision_conflict"
			}
			c.JSON(status, gin.H{"error": code, "projectId": p.ID, "jobs": jobs})
			return
		}
		var charge *middleware.QuotaCharge
		if i < len(charges) {
			charge = charges[i]
		}
		dir := h.projectDir(p.ID)
		job := h.startExport(c, newJobID(), photoExportSpec{
			project: saved, srcPath: h.photoBaseFor(saved, perDoc), origPath: filepath.Join(dir, "original."+p.OriginalExt),
			displayName: p.Name, origExt: p.OriginalExt, origBytes: p.OriginalBytes, hasAlpha: p.HasAlpha,
			doc: perDoc, params: params, masksDir: filepath.Join(dir, "masks"),
		}, charge)
		registerJobDirs(c, job.ID, false, true)
		jobs = append(jobs, gin.H{"projectId": p.ID, "jobId": job.ID})
	}
	c.JSON(http.StatusAccepted, gin.H{"jobs": jobs})
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// batchDocFor returns the canonical template doc re-sourced for project p with
// heals cleared.
func (h *PhotoHandler) batchDocFor(template []byte, p *telemetry.PhotoProject) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(template, &m); err != nil {
		return nil, err
	}
	m["heals"] = []any{}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return h.withSource(raw, p.OriginalWidth, p.OriginalHeight)
}

// --- stateless export --------------------------------------------------------

// StatelessExport handles POST /photo/export (anyone; one `ops`): multipart
// `file` + `doc` + format/quality/metadataMode/maxMegapixels/name.
func (h *PhotoHandler) StatelessExport(c *gin.Context) {
	if h == nil || (h.cfg != nil && !h.cfg.PhotoEnabled) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "photo_disabled", "message": "Aperture is switched off on this deployment"})
		return
	}
	if !h.pipeReady(c, "render") {
		return
	}
	jobID := newJobID()
	up, ok := h.stageStatelessUpload(c, jobID, "file")
	if !ok {
		return
	}
	defer up.release()
	mp := 0
	_, _ = fmt.Sscanf(c.PostForm("maxMegapixels"), "%d", &mp)
	q := 0
	_, _ = fmt.Sscanf(c.PostForm("quality"), "%d", &q)
	params, ok := normalizePhotoExport(c, photoExportParams{
		Format: c.PostForm("format"), Quality: q, MetadataMode: c.PostForm("metadataMode"), MaxMegapixels: mp, Name: c.PostForm("name"),
	})
	if !ok {
		up.discard()
		return
	}
	docRaw := strings.TrimSpace(c.PostForm("doc"))
	if docRaw == "" {
		docRaw = fmt.Sprintf(`{"version":1,"source":{"width":%d,"height":%d}}`, up.w, up.h)
	}
	doc, ok := h.sanitizeDoc(c, []byte(docRaw))
	if !ok {
		up.discard()
		return
	}
	doc, err := h.withSource(doc, up.w, up.h)
	if err != nil {
		up.discard()
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "photo_doc_invalid"})
		return
	}
	job := h.startExport(c, jobID, photoExportSpec{
		srcPath: up.path, srcRel: filepath.ToSlash(filepath.Join(jobID, filepath.Base(up.path))), origPath: up.path,
		displayName: clampPhotoName(params.Name, strings.TrimSuffix(up.name, filepath.Ext(up.name))), origExt: up.ext, origBytes: up.size,
		hasAlpha: up.alpha, doc: doc, params: params, masksDir: filepath.Join(h.uploadRoot(), jobID, "masks"),
	}, middleware.QuotaChargeOf(c))
	registerJobDirs(c, job.ID, true, true)
	up.keep()
	c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID})
}

// photoUpload is a validated image saved under uploads/<jobId>/.
type photoUpload struct {
	path, name, ext string
	size            int64
	w, h            int
	alpha           bool
	dir             string
	kept            bool
}

func (u *photoUpload) keep()    { u.kept = true }
func (u *photoUpload) discard() { _ = os.RemoveAll(u.dir); u.kept = true }
func (u *photoUpload) release() {
	if !u.kept {
		_ = os.RemoveAll(u.dir)
	}
}

// stageStatelessUpload saves multipart field `field` as an ordinary upload
// (uploads/<jobId>/original_<name>), probes it and enforces the type, size and
// megapixel limits. On failure the response is written and ok is false.
func (h *PhotoHandler) stageStatelessUpload(c *gin.Context, jobID, field string) (*photoUpload, bool) {
	return h.stageUploadUnder(c, h.uploadRoot(), jobID, field)
}

func (h *PhotoHandler) stageUploadUnder(c *gin.Context, root, jobID, field string) (*photoUpload, bool) {
	maxBytes := int64(0)
	if h.cfg != nil {
		maxBytes = maxUploadBytes(c, h.cfg)
	}
	if maxBytes > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes*2+2<<20)
	}
	if err := c.Request.ParseMultipartForm(photoFormMemory); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_file_too_large", "maxBytes": maxBytes})
			return nil, false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid multipart request"})
		return nil, false
	}
	fh, err := c.FormFile(field)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A multipart `" + field + "` field is required"})
		return nil, false
	}
	if maxBytes > 0 && fh.Size > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_file_too_large", "maxBytes": maxBytes})
		return nil, false
	}
	ext, accepted := photoAcceptedExts[strings.ToLower(strings.TrimPrefix(filepath.Ext(fh.Filename), "."))]
	if !accepted || photoLooksLikeSVG(fh) {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "photo_unsupported_source", "message": "Aperture opens JPG, PNG, WebP, AVIF, TIFF, BMP, GIF, HEIC and HEIF photos"})
		return nil, false
	}
	dir := filepath.Join(root, jobID)
	path := filepath.Join(dir, "original_"+safeFilename(fh.Filename))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file"})
		return nil, false
	}
	if err := c.SaveUploadedFile(fh, path); err != nil {
		_ = os.RemoveAll(dir)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file"})
		return nil, false
	}
	up := &photoUpload{path: path, name: fh.Filename, ext: ext, size: fh.Size, dir: dir}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	w, hgt, alpha, err := h.pipe.probe(ctx, path)
	if err != nil || w <= 0 || hgt <= 0 {
		up.discard()
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "photo_unreadable", "message": "This file could not be read as an image"})
		return nil, false
	}
	if int64(w)*int64(hgt) > int64(h.maxMegapixels())*1_000_000 {
		up.discard()
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo_too_many_pixels", "maxMegapixels": h.maxMegapixels()})
		return nil, false
	}
	up.w, up.h, up.alpha = w, hgt, alpha
	return up, true
}

// --- subject mask ------------------------------------------------------------

type photoSubjectBody struct {
	Base string `json:"base"`
}

func photoTimedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || (err != nil && strings.Contains(err.Error(), "timed out"))
}

// ProjectSubjectMask handles POST /photo/projects/:id/masks/subject (owner; one
// `ops`, free again when served from the cache).
//
//	200 {maskId, maskRef, url, width, height}; 404 photo_base_not_found; 504 photo_subject_timeout
func (h *PhotoHandler) ProjectSubjectMask(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, ok := requirePhotoOwner(c, h.store, h.logger, id); !ok {
		return
	}
	if !h.pipeReady(c, "mask") {
		return
	}
	var body photoSubjectBody
	if raw, _ := io.ReadAll(io.LimitReader(c.Request.Body, 4096)); len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
			return
		}
	}
	p, err := h.store.GetPhotoProject(c.Request.Context(), id)
	if errors.Is(err, telemetry.ErrPhotoNotFound) {
		denyPhotoProject(c)
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Project lookup is temporarily unavailable"})
		return
	}
	if p.Status != telemetry.PhotoStatusReady {
		c.JSON(http.StatusConflict, gin.H{"error": "photo_not_ready", "status": p.Status})
		return
	}
	dir := h.projectDir(id)
	baseKey, source := "original", filepath.Join(dir, "original."+p.OriginalExt)
	if b := strings.TrimSpace(body.Base); b != "" {
		if !photoHashRe.MatchString(b) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "photo_base_invalid"})
			return
		}
		source = filepath.Join(dir, "bases", b+".png")
		if st, err := os.Stat(source); err != nil || st.IsDir() || !retention.IsWithin(source, dir) {
			c.JSON(http.StatusNotFound, gin.H{"error": "photo_base_not_found"})
			return
		}
		baseKey = b
	}
	model := "isnet-general-use"
	if h.cfg != nil && strings.TrimSpace(h.cfg.PhotoSubjectModel) != "" {
		model = strings.TrimSpace(h.cfg.PhotoSubjectModel)
	}
	paramsHash := sha256Hex([]byte(baseKey + model))
	maskID := "subj-" + paramsHash[:16]
	rel := "masks/" + maskID + ".png"
	dest := filepath.Join(dir, filepath.FromSlash(rel))
	reply := func(w, hgt int) {
		c.JSON(http.StatusOK, gin.H{"maskId": maskID, "maskRef": rel, "url": "/api/photo/projects/" + id + "/files/" + rel, "width": w, "height": hgt})
	}
	if d, err := h.store.FindPhotoDerivative(c.Request.Context(), id, photoDerivSubject, paramsHash); err == nil && d != nil {
		if st, serr := os.Stat(dest); serr == nil && !st.IsDir() {
			if ch := middleware.QuotaChargeOf(c); ch != nil {
				ch.Refund("cache_hit")
			}
			reply(d.Width, d.Height)
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), services.PhotoSubjectTimeout(h.cfg))
	defer cancel()
	tmp := dest + ".tmp.png"
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	defer os.Remove(tmp)
	if err := h.pipe.AI.SubjectMask(ctx, "", gpuPriorityFor(c), source, tmp); err != nil {
		if photoTimedOut(err) {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "photo_subject_timeout", "message": "The subject mask took too long"})
			return
		}
		h.logger.Warn("photo: subject mask failed", "project", id, "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "photo_subject_failed", "message": "The subject mask could not be made"})
		return
	}
	w, hgt, _, perr := h.pipe.probe(ctx, tmp)
	if perr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "photo_subject_failed", "message": "The subject mask could not be read"})
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "photo_subject_failed"})
		return
	}
	st, _ := os.Stat(dest)
	var size int64
	if st != nil {
		size = st.Size()
	}
	if _, err := h.store.UpsertPhotoDerivative(c.Request.Context(), telemetry.PhotoDerivative{
		ProjectID: id, Kind: photoDerivSubject, Relpath: rel, ParamsHash: paramsHash, Width: w, Height: hgt, Bytes: size,
	}); err != nil {
		h.logger.Warn("photo: recording subject mask failed", "project", id, "error", err.Error())
	}
	reply(w, hgt)
}

// StatelessSubjectMask handles POST /photo/masks/subject (anyone; one `ops`):
// multipart `file` -> 200 image/png (8-bit alpha). Nothing is stored.
func (h *PhotoHandler) StatelessSubjectMask(c *gin.Context) {
	if h == nil || (h.cfg != nil && !h.cfg.PhotoEnabled) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "photo_disabled", "message": "Aperture is switched off on this deployment"})
		return
	}
	if !h.pipeReady(c, "mask") {
		return
	}
	work, err := os.MkdirTemp(h.tempRoot(), "photo_subject_")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare the mask"})
		return
	}
	defer os.RemoveAll(work)
	up, ok := h.stageInto(c, work, "file")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), services.PhotoSubjectTimeout(h.cfg))
	defer cancel()
	out := filepath.Join(work, "mask.png")
	if err := h.pipe.AI.SubjectMask(ctx, "", gpuPriorityFor(c), up, out); err != nil {
		if photoTimedOut(err) {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "photo_subject_timeout", "message": "The subject mask took too long"})
			return
		}
		h.logger.Warn("photo: stateless subject mask failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "photo_subject_failed", "message": "The subject mask could not be made"})
		return
	}
	data, err := os.ReadFile(out)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "photo_subject_failed"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "image/png", data)
}

// stageInto is stageStatelessUpload into an arbitrary (temp) directory; the
// probe/limits are the same. Returns the saved path.
func (h *PhotoHandler) stageInto(c *gin.Context, dir, field string) (string, bool) {
	up, ok := h.stageUploadUnder(c, filepath.Dir(dir), filepath.Base(dir), field)
	if !ok {
		return "", false
	}
	up.keep()
	return up.path, true
}

// --- heal --------------------------------------------------------------------

// healMask reads multipart `mask` (a PNG) to dst and checks its size against
// the expected pixel size. It writes the 400 itself.
func (h *PhotoHandler) healMask(c *gin.Context, dst string, expW, expH int) (digest string, ok bool) {
	fh, err := c.FormFile("mask")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A multipart `mask` field is required"})
		return "", false
	}
	if fh.Size <= 0 || fh.Size > photoMaxMaskBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "photo_mask_invalid", "message": "The mask must be a PNG of at most 64 MB"})
		return "", false
	}
	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "photo_mask_invalid"})
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, photoMaxMaskBytes+1))
	if err != nil || len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "photo_mask_invalid", "message": "The mask must be a PNG image"})
		return "", false
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save the mask"})
		return "", false
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save the mask"})
		return "", false
	}
	w, hgt, _, perr := h.pipe.probe(c.Request.Context(), dst)
	if perr != nil || w <= 0 {
		_ = os.Remove(dst)
		c.JSON(http.StatusBadRequest, gin.H{"error": "photo_mask_invalid", "message": "The mask could not be read"})
		return "", false
	}
	if w != expW || hgt != expH {
		_ = os.Remove(dst)
		c.JSON(http.StatusBadRequest, gin.H{"error": "photo_mask_size", "expectedWidth": expW, "expectedHeight": expH, "width": w, "height": hgt})
		return "", false
	}
	return sha256Hex(data), true
}

// ProjectHeal handles POST /photo/projects/:id/heal (owner; one `ai_ops`):
// multipart `mask` (PNG at the base's size, white = heal) + `base` (a base hash,
// "" for the original). 202 {jobId, baseHash, maskRef}; the finished job carries
// GET /api/job/:id -> healResult {baseHash, baseRef, proxyRef, maskRef, width, height}.
func (h *PhotoHandler) ProjectHeal(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, ok := requirePhotoOwner(c, h.store, h.logger, id); !ok {
		return
	}
	if !h.pipeReady(c, "heal") {
		return
	}
	if err := c.Request.ParseMultipartForm(photoFormMemory); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid multipart request"})
		return
	}
	defer func() {
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
	}()
	p, err := h.store.GetPhotoProject(c.Request.Context(), id)
	if errors.Is(err, telemetry.ErrPhotoNotFound) {
		denyPhotoProject(c)
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Project lookup is temporarily unavailable"})
		return
	}
	if p.Status != telemetry.PhotoStatusReady {
		c.JSON(http.StatusConflict, gin.H{"error": "photo_not_ready", "status": p.Status})
		return
	}
	dir := h.projectDir(id)
	baseHash, source := "", filepath.Join(dir, "original."+p.OriginalExt)
	expW, expH := p.OriginalWidth, p.OriginalHeight
	if b := strings.TrimSpace(c.PostForm("base")); b != "" {
		if !photoHashRe.MatchString(b) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "photo_base_invalid"})
			return
		}
		source = filepath.Join(dir, "bases", b+".png")
		if st, err := os.Stat(source); err != nil || st.IsDir() || !retention.IsWithin(source, dir) {
			c.JSON(http.StatusNotFound, gin.H{"error": "photo_base_not_found"})
			return
		}
		baseHash = b
		if d, derr := h.store.FindPhotoDerivative(c.Request.Context(), id, photoDerivHeal, b); derr == nil && d != nil {
			expW, expH = d.Width, d.Height
		}
	}
	tmpMask := filepath.Join(dir, "masks", ".incoming-"+newJobID()+".png")
	defer os.Remove(tmpMask)
	maskDigest, ok := h.healMask(c, tmpMask, expW, expH)
	if !ok {
		return
	}
	newHash := sha256Hex([]byte(baseHash + maskDigest))
	maskID := "heal-" + maskDigest[:16]
	maskRel := "masks/" + maskID + ".png"
	if err := os.Rename(tmpMask, filepath.Join(dir, filepath.FromSlash(maskRel))); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save the mask"})
		return
	}
	baseRel, proxyRel := "bases/"+newHash+".png", "bases/"+newHash+".proxy.jpg"
	payload := models.PhotoHealPayload{BaseHash: newHash, BaseRef: baseRel, ProxyRef: proxyRel, MaskRef: maskRel, Width: expW, Height: expH}
	charge := middleware.QuotaChargeOf(c)
	jobID := newJobID()
	job := h.createPhotoJob(c, jobID, models.OriginalFileInfo{Name: photoSafeStem(p.Name) + "." + p.OriginalExt, Size: p.OriginalBytes, Type: photoExtMime[p.OriginalExt]},
		map[string]interface{}{"tool": PhotoToolHeal, "projectId": id, "base": baseHash}, services.JobFacts{Tool: PhotoToolHeal, Mode: "heal", MediaKind: "image", SourceFormat: p.OriginalExt, TargetFormat: "png"}, charge)
	jm := h.pipe.Jobs
	_ = jm.SetMode(job.ID, "heal")
	dest := filepath.Join(dir, filepath.FromSlash(baseRel))
	if d, derr := h.store.FindPhotoDerivative(c.Request.Context(), id, photoDerivHeal, newHash); derr == nil && d != nil {
		if st, serr := os.Stat(dest); serr == nil && !st.IsDir() {
			// The same heal was done before: the cached base is the result, free of charge.
			payload.Width, payload.Height = d.Width, d.Height
			if charge != nil {
				charge.Refund("cache_hit")
			}
			_ = jm.SetPhotoHeal(job.ID, payload)
			_ = jm.UpdateJobResult(job.ID, "")
			_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
			c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID, "baseHash": newHash, "maskRef": maskRel})
			return
		}
	}
	timeout := services.PhotoHealTimeout(h.cfg)
	prio := gpuPriorityFor(c)
	maskPath := filepath.Join(dir, filepath.FromSlash(maskRel))
	h.pipe.async(func() {
		_ = jm.UpdateJobStatus(job.ID, models.StatusProcessing)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		jm.SendProgressUpdate(job.ID, 10)
		tmp := dest + ".tmp.png"
		defer os.Remove(tmp)
		if err := h.pipe.AI.Heal(ctx, job.ID, prio, source, maskPath, tmp); err != nil {
			log.Printf("photo heal %s failed: %v", job.ID, err)
			_ = jm.UpdateJobError(job.ID, "Heal failed: "+err.Error())
			return
		}
		if err := os.Rename(tmp, dest); err != nil {
			_ = jm.UpdateJobError(job.ID, "Heal produced no file")
			return
		}
		proxy := filepath.Join(dir, filepath.FromSlash(proxyRel))
		if err := h.pipe.proxy(ctx, dest, proxy); err != nil {
			log.Printf("photo heal %s proxy failed: %v", job.ID, err)
			_ = os.Remove(dest)
			_ = jm.UpdateJobError(job.ID, "Heal proxy failed: "+err.Error())
			return
		}
		w, hgt, _, perr := h.pipe.probe(ctx, dest)
		if perr != nil {
			w, hgt = expW, expH
		}
		payload.Width, payload.Height = w, hgt
		var size int64
		if st, err := os.Stat(dest); err == nil {
			size = st.Size()
		}
		if _, err := h.store.UpsertPhotoDerivative(context.Background(), telemetry.PhotoDerivative{
			ProjectID: id, Kind: photoDerivHeal, Relpath: baseRel, ParamsHash: newHash, Width: w, Height: hgt, Bytes: size,
		}); err != nil {
			h.logger.Warn("photo: recording heal base failed", "project", id, "error", err.Error())
		}
		_ = jm.SetPhotoHeal(job.ID, payload)
		_ = jm.UpdateJobResult(job.ID, "")
		_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	})
	c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID, "baseHash": newHash, "maskRef": maskRel})
}

// StatelessHeal handles POST /photo/heal (anyone; one `ai_ops`): multipart
// `file` + `mask`. The result is outputs/<jobId>/healed.png (an ordinary result).
func (h *PhotoHandler) StatelessHeal(c *gin.Context) {
	if h == nil || (h.cfg != nil && !h.cfg.PhotoEnabled) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "photo_disabled", "message": "Aperture is switched off on this deployment"})
		return
	}
	if !h.pipeReady(c, "heal") {
		return
	}
	jobID := newJobID()
	up, ok := h.stageStatelessUpload(c, jobID, "file")
	if !ok {
		return
	}
	defer up.release()
	maskPath := filepath.Join(up.dir, "mask.png")
	if _, ok := h.healMask(c, maskPath, up.w, up.h); !ok {
		up.discard()
		return
	}
	charge := middleware.QuotaChargeOf(c)
	mime := photoExtMime[up.ext]
	job := h.createPhotoJob(c, jobID, models.OriginalFileInfo{Name: safeFilename(up.name), Size: up.size, Type: mime},
		map[string]interface{}{"tool": PhotoToolHeal, "mode": "heal", "format": "png", "sourcePath": filepath.ToSlash(filepath.Join(jobID, filepath.Base(up.path)))},
		services.JobFacts{Tool: PhotoToolHeal, Mode: "heal", MediaKind: "image", SourceFormat: up.ext, TargetFormat: "png"}, charge)
	jm := h.pipe.Jobs
	_ = jm.SetMode(job.ID, "heal")
	_ = jm.SetResultMetadata(job.ID, "", photoHealedName, time.Time{})
	outDir := filepath.Join(h.outputRoot(), job.ID)
	_ = os.MkdirAll(outDir, 0o755)
	out := filepath.Join(outDir, photoHealedName)
	registerJobDirs(c, job.ID, true, true)
	up.keep()
	timeout := services.PhotoHealTimeout(h.cfg)
	prio := gpuPriorityFor(c)
	source := up.path
	h.pipe.async(func() {
		_ = jm.UpdateJobStatus(job.ID, models.StatusProcessing)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		jm.SendProgressUpdate(job.ID, 10)
		if err := h.pipe.AI.Heal(ctx, job.ID, prio, source, maskPath, out); err != nil {
			log.Printf("photo heal %s failed: %v", job.ID, err)
			_ = jm.UpdateJobError(job.ID, "Heal failed: "+err.Error())
			return
		}
		st, err := os.Stat(out)
		if err != nil || st.IsDir() {
			_ = jm.UpdateJobError(job.ID, "Heal produced no file")
			return
		}
		_ = jm.SetResultSize(job.ID, st.Size())
		_ = jm.SetResultMime(job.ID, "image/png")
		_ = jm.UpdateJobResult(job.ID, "/api/download/"+job.ID)
		_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	})
	c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID})
}
