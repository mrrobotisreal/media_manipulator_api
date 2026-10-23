package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// StudioHandler owns the Content Studio endpoints (/api/studio/*). It embeds
// the same shared dependencies ConversionHandler uses — jobManager + cfg +
// inspector + s3 — plus a Postgres-backed repository (projects/assets) and the
// ingest/export services. We keep it separate from ConversionHandler so the
// editor's DB-backed surface doesn't bleed into the file-conversion handler.
type StudioHandler struct {
	jobManager *services.JobManager
	cfg        *config.Config
	inspector  *services.MediaInspector
	s3Client   *s3.Client
	s3Presign  *s3.PresignClient
	repo          *services.StudioRepository
	ingest        *services.StudioIngestService
	export        *services.StudioExportService
	ai            *services.AIService
	transcription *services.TranscriptionService
	// peaksMu dedupes on-demand waveform backfill per asset id so concurrent
	// /peaks requests don't decode the same source twice.
	peaksMu sync.Map
}

// NewStudioHandler mirrors NewConversionHandler's construction: it derives the
// presign client + the studio sub-services from the shared deps so callers only
// pass the base client + pool.
func NewStudioHandler(jobManager *services.JobManager, cfg *config.Config, inspector *services.MediaInspector, s3Client *s3.Client, pool *pgxpool.Pool, transcription *services.TranscriptionService) *StudioHandler {
	var presign *s3.PresignClient
	if s3Client != nil {
		presign = s3.NewPresignClient(s3Client)
	}
	ai := services.NewAIService(cfg)
	ai.SetJobManager(jobManager)
	return &StudioHandler{
		jobManager:    jobManager,
		cfg:           cfg,
		inspector:     inspector,
		s3Client:      s3Client,
		s3Presign:     presign,
		repo:          services.NewStudioRepository(pool),
		ingest:        services.NewStudioIngestService(cfg, jobManager, s3Client),
		export:        services.NewStudioExportService(cfg, jobManager),
		ai:            ai,
		transcription: transcription,
	}
}

// RegisterStudioRoutes wires the Content Studio endpoints under /api/studio.
// Ingest + export run through the existing JobManager so /api/job/:jobId and
// /api/download/:jobId remain the single source of truth for progress + result.
func RegisterStudioRoutes(r gin.IRouter, h *StudioHandler) {
	studio := r.Group("/studio")
	// Projects (Postgres-backed, keyed by X-MM-Session-ID).
	studio.POST("/projects", h.CreateProject)
	studio.GET("/projects", h.ListProjects)
	studio.GET("/projects/:id", h.GetProject)
	studio.PUT("/projects/:id", h.SaveProject)
	studio.GET("/projects/:id/assets", h.ListAssets)
	// Version history (part 09): checkpoints + restore. Restore checkpoints
	// the current state first, so it is always reversible.
	studio.GET("/projects/:id/versions", h.ListStudioVersions)
	studio.POST("/projects/:id/versions", h.CreateStudioVersion)
	studio.POST("/projects/:id/versions/:versionId/restore", h.RestoreStudioVersion)
	// Source media ingest (presign -> PUT to S3 -> complete -> proxy/filmstrip job).
	studio.POST("/assets/presign", h.PresignAsset)
	studio.POST("/assets/complete", h.CompleteAsset)
	// AI-derived audio assets (clean voice / split vocals|music) → new asset.
	studio.POST("/assets/:id/derive", h.DeriveAsset)
	// Serve the preview proxy + filmstrip to the browser. Passthrough (not
	// presigned) so the <video> element gets reliable Range support + the API's
	// CORS headers (needed for crossorigin="anonymous" Web Audio in Phase 3),
	// with no mid-session URL expiry.
	studio.GET("/assets/:id/proxy", h.ServeProxy)
	studio.GET("/assets/:id/sprite", h.ServeSprite)
	// Audio waveform peaks JSON (generated at ingest or backfilled on demand).
	studio.GET("/assets/:id/peaks", h.ServePeaks)
	// Raw original passthrough (used to fetch .cube LUT assets for the compositor).
	studio.GET("/assets/:id/file", h.ServeFile)
	// Export the EDL to MP4 via NVENC on the dedicated GPU.
	studio.POST("/projects/:id/export", h.ExportProject)
	// Auto-captions: transcribe the timeline-aligned audio mix with whisper.
	studio.POST("/projects/:id/captions/generate", h.GenerateCaptions)
}

func (h *StudioHandler) dbReady(c *gin.Context) bool {
	if h.repo == nil || !h.repo.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Content Studio requires a database, which is currently unavailable"})
		return false
	}
	return true
}

func (h *StudioHandler) sessionID(c *gin.Context) string {
	return sanitizeS3PathSegment(firstNonEmpty(c.GetHeader("X-MM-Session-ID"), uuid.NewString()))
}

// owner is who this request may act as.
//
// FIX B13: the client-supplied X-MM-Session-ID used to be the ONLY scope on a
// project, and the read/write endpoints did not even check that. A signed-in
// caller is now identified by their mm_users id, which no client can forge;
// signed-out callers keep the session scope so the anonymous editor still
// works, and their rows are claimed by the account on first sign-in.
func (h *StudioHandler) owner(c *gin.Context) services.StudioOwner {
	out := services.StudioOwner{SessionID: h.sessionID(c)}
	if res, ok := middleware.ResolveTier(c); ok && res.User != nil {
		out.UserID = res.User.ID
	}
	return out
}

// ExportHeightLookup returns the persisted height of the project an export
// request targets, for ExportHeightGate. ok=false (no DB, unknown project, not
// the caller's) leaves the decision to ExportProject, which 404s as usual.
func (h *StudioHandler) ExportHeightLookup(c *gin.Context) (int, bool) {
	if h == nil || h.repo == nil || !h.repo.Enabled() {
		return 0, false
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if projectID == "" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if ok, err := h.repo.OwnsProject(ctx, projectID, h.owner(c)); err != nil || !ok {
		return 0, false
	}
	project, err := h.repo.GetProject(ctx, projectID)
	if err != nil || project == nil {
		return 0, false
	}
	return project.Height, true
}

// requireProjectAccess is the gate on every project-scoped endpoint. It answers
// 404 rather than 403 on a miss: telling a stranger that a project id exists
// but is not theirs is a disclosure in itself.
//
// Fails CLOSED. If the ownership check itself errors the request is refused —
// the opposite posture from the quota middleware, because here the failure mode
// is handing someone else's editor document over, not blocking a conversion.
func (h *StudioHandler) requireProjectAccess(c *gin.Context, projectID string) bool {
	if projectID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return false
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	ok, err := h.repo.OwnsProject(ctx, projectID, h.owner(c))
	if err != nil {
		log.Printf("studio: ownership check failed for project %s: %v", projectID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open project"})
		return false
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return false
	}
	return true
}

// ----------------------------------------------------------------------- //
// PROJECTS
// ----------------------------------------------------------------------- //

func (h *StudioHandler) CreateProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	var req models.StudioCreateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	clampCreateProject(&req)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	owner := h.owner(c)
	// Saved-project cap, per the tier matrix. Only counted for signed-in
	// callers: an anonymous visitor's single local project is not an account
	// entitlement, and counting hashed sessions would punish a shared browser.
	if owner.UserID != "" {
		if res, ok := middleware.ResolveTier(c); ok && res.Limits.StudioProjects >= 0 {
			count, countErr := h.repo.CountProjectsForUser(ctx, owner.UserID)
			if countErr != nil {
				// Fail open on the CAP only — a broken count must not stop
				// someone starting an edit. Ownership still fails closed.
				log.Printf("studio: project count failed, allowing create: %v", countErr)
			} else if count >= res.Limits.StudioProjects {
				c.JSON(http.StatusForbidden, gin.H{
					"error":       "project_limit_reached",
					"message":     "You have reached the saved-project limit for your plan.",
					"limit":       res.Limits.StudioProjects,
					"used":        count,
					"upgradePath": "premium",
				})
				return
			}
		}
	}

	project, err := h.repo.CreateProject(ctx, owner, req)
	if err != nil {
		log.Printf("studio: create project failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create project"})
		return
	}
	c.JSON(http.StatusCreated, project)
}

func (h *StudioHandler) ListProjects(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	owner := h.owner(c)
	// A caller with neither an account nor a session has nothing to list. Do
	// not fall back to the generated session id from h.sessionID — that would
	// be a fresh uuid and could never match a row anyway.
	if owner.UserID == "" && strings.TrimSpace(c.GetHeader("X-MM-Session-ID")) == "" {
		c.JSON(http.StatusOK, gin.H{"projects": []models.StudioProject{}})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	projects, err := h.repo.ListRecentProjects(ctx, owner, 25)
	if err != nil {
		log.Printf("studio: list projects failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list projects"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

func (h *StudioHandler) GetProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if !h.requireProjectAccess(c, strings.TrimSpace(c.Param("id"))) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	project, err := h.repo.GetProject(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	c.JSON(http.StatusOK, project)
}

func (h *StudioHandler) SaveProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	var req models.StudioSaveProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	clampSaveProject(&req)

	if !h.requireProjectAccess(c, strings.TrimSpace(c.Param("id"))) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	// Image clips carry no speed/reverse/freeze (Go is authoritative; the UI
	// mirrors it). A failed asset lookup must not block the save.
	if assets, aerr := h.repo.ListAssets(ctx, strings.TrimSpace(c.Param("id"))); aerr != nil {
		log.Printf("studio: save project: list assets failed (image sanitize skipped): %v", aerr)
	} else {
		byID := make(map[string]*models.StudioAsset, len(assets))
		for _, a := range assets {
			byID[a.ID] = a
		}
		req.Tracks = models.SanitizeTracksForAssets(req.Tracks, studioAssetKinds(byID))
	}
	project, err := h.repo.SaveProject(ctx, strings.TrimSpace(c.Param("id")), req)
	if err != nil {
		if err == services.ErrStudioNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
			return
		}
		var conflict *services.StudioRevisionConflictError
		if errors.As(err, &conflict) {
			// Stale expectedRevision: another tab/device saved first. The
			// client resolves via its conflict dialog — "take over" is just a
			// re-send with expectedRevision = currentRevision (no force flag).
			c.JSON(http.StatusConflict, studioRevisionConflictBody(conflict))
			return
		}
		log.Printf("studio: save project failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save project"})
		return
	}
	c.JSON(http.StatusOK, project)
}

// studioRevisionConflictBody is the 409 payload for a stale save. Mirrored by
// the client's save-state machine (lib/studio/saveState.ts) — keep the keys
// stable.
func studioRevisionConflictBody(e *services.StudioRevisionConflictError) gin.H {
	return gin.H{
		"error":           "revision_conflict",
		"currentRevision": e.CurrentRevision,
		"updatedAt":       e.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// studioAssetKinds maps asset id -> media kind for SanitizeTracksForAssets.
func studioAssetKinds(assets map[string]*models.StudioAsset) map[string]models.StudioMediaKind {
	out := make(map[string]models.StudioMediaKind, len(assets))
	for id, a := range assets {
		if a != nil {
			out[id] = a.MediaKind
		}
	}
	return out
}

func (h *StudioHandler) ListAssets(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if !h.requireProjectAccess(c, strings.TrimSpace(c.Param("id"))) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	assets, err := h.repo.ListAssets(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil {
		log.Printf("studio: list assets failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list assets"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"assets": assets})
}

// ----------------------------------------------------------------------- //
// ASSET INGEST
// ----------------------------------------------------------------------- //

func (h *StudioHandler) PresignAsset(c *gin.Context) {
	if h.s3Presign == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 uploads are not configured"})
		return
	}
	var req models.StudioAssetPresignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	fileName := safeFilename(req.FileName)
	ext := sanitizeExtension(filepath.Ext(fileName))
	if ext == "" || !isSupportedStudioExtension(ext) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported file extension"})
		return
	}
	if req.FileSizeBytes <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "fileSizeBytes must be greater than 0"})
		return
	}
	contentType := normalizeUploadContentType(req.ContentType, ext)
	if isStudioImageExtension(ext) {
		// Browsers often send HEIC/AVIF/SVG as octet-stream or empty.
		if lc := strings.ToLower(strings.TrimSpace(contentType)); lc == "" || lc == "application/octet-stream" {
			contentType = studioImageMIMEForExt(ext)
		}
	}
	if ext == "cube" {
		// LUT asset: small, non-AV content type.
		if req.FileSizeBytes > studioMaxLUTBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "LUT file exceeds 8MB"})
			return
		}
		contentType = "application/octet-stream"
	} else {
		if req.FileSizeBytes > h.cfg.MaxVideoUpload {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "File exceeds maximum upload size"})
			return
		}
		lower := strings.ToLower(contentType)
		isImage := strings.HasPrefix(lower, "image/")
		if isStudioImageExtension(ext) {
			// Image extensions only accept image/* (no video/audio smuggling).
			if !isImage {
				c.JSON(http.StatusBadRequest, gin.H{"error": "contentType must be a video, audio, or image MIME type"})
				return
			}
		} else if !strings.HasPrefix(lower, "video/") && !strings.HasPrefix(lower, "audio/") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "contentType must be a video, audio, or image MIME type"})
			return
		}
	}

	sessionID := h.sessionID(c)
	key := fmt.Sprintf("%s/%s/%s/%s.%s", h.studioPrefix(), time.Now().UTC().Format("20060102"), sessionID, uuid.NewString(), ext)
	expiresAt := time.Now().UTC().Add(h.cfg.S3PresignTTL)

	presignCtx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	result, err := h.s3Presign.PresignPutObject(presignCtx, &s3.PutObjectInput{
		Bucket:      aws.String(h.cfg.S3Bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, func(o *s3.PresignOptions) { o.Expires = h.cfg.S3PresignTTL })
	if err != nil {
		log.Printf("studio: failed to presign asset upload: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload URL"})
		return
	}
	c.JSON(http.StatusCreated, models.StudioAssetPresignResponse{
		UploadURL: result.URL,
		S3Key:     key,
		Bucket:    h.cfg.S3Bucket,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	})
}

func (h *StudioHandler) CompleteAsset(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 uploads are not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	var req models.StudioAssetCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "projectId is required"})
		return
	}
	key := h.sanitizeStudioKey(req.S3Key)
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid s3Key"})
		return
	}

	// The project id comes from the request body here, so it is exactly as
	// untrusted as a path parameter and gets the same gate.
	if !h.requireProjectAccess(c, projectID) {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()

	if _, err := h.repo.GetProject(ctx, projectID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}

	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("studio: failed to verify uploaded asset %s: %v", key, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file was not found"})
		return
	}
	objectSize := aws.ToInt64(head.ContentLength)
	if objectSize <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file is empty"})
		return
	}
	if objectSize > h.cfg.MaxVideoUpload {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "File exceeds maximum upload size"})
		return
	}

	contentType := firstNonEmpty(aws.ToString(head.ContentType), req.ContentType, "application/octet-stream")
	fileName := safeFilename(req.FileName)
	if fileName == "" || fileName == "upload" {
		fileName = "media_" + filepath.Base(key)
	}

	// LUT (.cube) assets skip probe + ingest entirely: stored raw, served via
	// /file, applied by the lut effect. Return an already-completed job so the
	// media bin flips it to "ready" immediately.
	if sanitizeExtension(filepath.Ext(key)) == "cube" {
		asset := &models.StudioAsset{
			ProjectID:        projectID,
			OriginalFileName: fileName,
			S3KeyOriginal:    key,
			MediaKind:        models.StudioMediaKindLUT,
			DurationSeconds:  0,
			HasAudio:         false,
			ProbeJSON:        map[string]any{},
		}
		created, err := h.repo.CreateAsset(ctx, asset)
		if err != nil {
			log.Printf("studio: create LUT asset failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record asset"})
			return
		}
		job := h.jobManager.CreateJob(
			models.OriginalFileInfo{Name: fileName, Size: objectSize, Type: "application/octet-stream"},
			map[string]interface{}{"mode": "studio_lut"},
		)
		_ = h.jobManager.UpdateJobResult(job.ID, "/api/studio/assets/"+created.ID+"/file")
		_ = h.jobManager.UpdateJobStatus(job.ID, models.StatusCompleted)
		c.JSON(http.StatusOK, models.StudioAssetCompleteResponse{Asset: created, JobID: job.ID})
		return
	}

	// Stage the original under the ingest job's upload dir so the background
	// job can read it. (Cleanup worker reaps it later; export re-downloads from
	// S3, so the local copy is disposable.)
	originalFile := models.OriginalFileInfo{Name: fileName, Size: objectSize, Type: contentType}
	job := h.jobManager.CreateJob(originalFile, map[string]interface{}{"mode": "studio_ingest"})
	_ = h.jobManager.SetMode(job.ID, "studio_ingest")

	jobUploadDir := filepath.Join(h.cfg.UploadDir, job.ID)
	if err := os.MkdirAll(jobUploadDir, 0o755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "Failed to create upload directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	uploadPath := filepath.Join(jobUploadDir, "original_"+fileName)
	if err := h.downloadS3Object(ctx, key, uploadPath); err != nil {
		log.Printf("studio: failed to download uploaded asset %s: %v", key, err)
		h.jobManager.UpdateJobError(job.ID, "Failed to download uploaded file")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download uploaded file"})
		return
	}

	fileType, _ := h.inspector.DetectFile(ctx, uploadPath, contentType)
	kind, supported := studioKindForFileType(fileType)
	if !supported {
		h.jobManager.UpdateJobError(job.ID, "Unsupported media type")
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded object is not a supported video, audio, or image file"})
		return
	}

	var asset *models.StudioAsset
	var totalSeconds float64
	if kind == models.StudioMediaKindImage {
		meta, probeErr := h.inspector.ProbeFile(ctx, uploadPath, models.FileTypeImage)
		var iw, ih int
		if probeErr == nil && meta != nil {
			iw, ih, _ = studioImageDimensions(meta.Details)
		}
		if probeErr != nil || iw <= 0 || ih <= 0 {
			log.Printf("studio: image probe failed for %s: %v", key, probeErr)
			h.jobManager.UpdateJobError(job.ID, "Uploaded image could not be read")
			c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded image could not be read"})
			return
		}
		if int64(iw)*int64(ih) > studioMaxImagePixels {
			h.jobManager.UpdateJobError(job.ID, "Image dimensions are too large")
			c.JSON(http.StatusBadRequest, gin.H{"error": "Image dimensions are too large"})
			return
		}
		asset = &models.StudioAsset{
			ProjectID:        projectID,
			OriginalFileName: fileName,
			S3KeyOriginal:    key,
			MediaKind:        kind,
			DurationSeconds:  0,
			HasAudio:         false,
			ProbeJSON:        meta.Details,
			Width:            &iw,
			Height:           &ih,
		}
	} else {
		probe, probeErr := services.ProbeVideoReport(ctx, uploadPath)
		if probeErr != nil || probe == nil {
			log.Printf("studio: probe failed for %s: %v", key, probeErr)
			probe = &models.VideoProbeResponse{}
		}
		totalSeconds = probe.DurationSeconds

		asset = &models.StudioAsset{
			ProjectID:        projectID,
			OriginalFileName: fileName,
			S3KeyOriginal:    key,
			MediaKind:        kind,
			DurationSeconds:  probe.DurationSeconds,
			VideoCodec:       probe.VideoCodec,
			AudioCodec:       probe.AudioCodec,
			HasAudio:         probe.HasAudio,
			ProbeJSON:        probe,
		}
		if probe.Width > 0 {
			w := probe.Width
			asset.Width = &w
		}
		if probe.Height > 0 {
			hh := probe.Height
			asset.Height = &hh
		}
		if probe.FPS > 0 {
			f := probe.FPS
			asset.FPS = &f
		}
		if sr, ch := studioAudioParams(probe); sr > 0 || ch > 0 {
			if sr > 0 {
				asset.SampleRate = &sr
			}
			if ch > 0 {
				asset.Channels = &ch
			}
		}
	}

	created, err := h.repo.CreateAsset(ctx, asset)
	if err != nil {
		log.Printf("studio: create asset failed: %v", err)
		h.jobManager.UpdateJobError(job.ID, "Failed to record asset")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record asset"})
		return
	}

	go h.runIngestJob(job.ID, created, key, kind, uploadPath, totalSeconds)

	c.JSON(http.StatusOK, models.StudioAssetCompleteResponse{Asset: created, JobID: job.ID})
}

func (h *StudioHandler) runIngestJob(jobID string, asset *models.StudioAsset, originalKey string, kind models.StudioMediaKind, inputPath string, totalSeconds float64) {
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusProcessing); err != nil {
		log.Printf("studio ingest: failed to mark job %s processing: %v", jobID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()

	res, err := h.ingest.Generate(ctx, jobID, originalKey, asset.ID, kind, asset.HasAudio, inputPath, totalSeconds)
	if err != nil {
		log.Printf("studio ingest: job %s failed: %v", jobID, err)
		_ = h.jobManager.UpdateJobError(jobID, err.Error())
		return
	}
	if err := h.repo.SetAssetDerived(ctx, asset.ID, res.ProxyKey, res.SpriteKey); err != nil {
		log.Printf("studio ingest: failed to persist derivatives for asset %s: %v", asset.ID, err)
		_ = h.jobManager.UpdateJobError(jobID, "Failed to record proxy")
		return
	}
	if res.PeaksKey != "" {
		if err := h.repo.SetAssetPeaks(ctx, asset.ID, res.PeaksKey); err != nil {
			// Non-fatal: the /peaks route backfills on first request.
			log.Printf("studio ingest: failed to persist peaks for asset %s: %v", asset.ID, err)
		}
	}
	_ = h.jobManager.UpdateJobResult(jobID, "/api/studio/assets/"+asset.ID+"/proxy")
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusCompleted); err != nil {
		log.Printf("studio ingest: failed to mark job %s completed: %v", jobID, err)
	}
}

// ----------------------------------------------------------------------- //
// AI-DERIVED AUDIO (clean voice / split vocals|music)
// ----------------------------------------------------------------------- //

// DeriveAsset creates a new audio asset from an AI transform of an existing
// audio-bearing one (DeepFilterNet voice cleanup or Demucs stem split). The new
// asset is created immediately (status processing) and ingested in the
// background; the response mirrors CompleteAsset ({asset, jobId}).
func (h *StudioHandler) DeriveAsset(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	var req models.StudioAssetDeriveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	op := strings.TrimSpace(req.Operation)
	switch op {
	case models.StudioDeriveVoiceClean, models.StudioDeriveSplitVocals, models.StudioDeriveSplitInstrumental:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported operation"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	src, err := h.repo.GetAsset(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	// Derive spends GPU time and writes a new asset into someone's project, so
	// it is gated on that project. Checked via the asset's own projectId rather
	// than a client-supplied one.
	if !h.requireProjectAccess(c, src.ProjectID) {
		return
	}
	if !src.HasAudio {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Asset has no audio to process"})
		return
	}

	sessionID := h.sessionID(c)
	derivedKey := fmt.Sprintf("%s/%s/%s/%s.wav", h.studioPrefix(), time.Now().UTC().Format("20060102"), sessionID, uuid.NewString())
	asset := &models.StudioAsset{
		ProjectID:        src.ProjectID,
		OriginalFileName: deriveAssetName(src.OriginalFileName, op),
		S3KeyOriginal:    derivedKey,
		MediaKind:        models.StudioMediaKindAudio,
		DurationSeconds:  src.DurationSeconds, // DeepFilter/Demucs preserve length
		HasAudio:         true,
		ProbeJSON:        map[string]any{},
	}
	created, err := h.repo.CreateAsset(ctx, asset)
	if err != nil {
		log.Printf("studio derive: create asset failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record asset"})
		return
	}

	job := h.jobManager.CreateJob(
		models.OriginalFileInfo{Name: asset.OriginalFileName, Size: 0, Type: "audio/wav"},
		map[string]interface{}{"mode": "studio_derive"},
	)
	_ = h.jobManager.SetMode(job.ID, "studio_derive")
	recordJobCreated(c, h.jobManager, job, services.JobFacts{
		Tool:         "studio_derive",
		Mode:         "studio_derive",
		MediaKind:    string(models.FileTypeAudio),
		SourceFormat: formatOf(asset.OriginalFileName),
		TargetFormat: "wav",
	})

	go h.runDeriveJob(job.ID, created, src.S3KeyOriginal, derivedKey, op)
	c.JSON(http.StatusOK, models.StudioAssetCompleteResponse{Asset: created, JobID: job.ID})
}

func (h *StudioHandler) runDeriveJob(jobID string, asset *models.StudioAsset, srcKey, derivedKey, op string) {
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusProcessing); err != nil {
		log.Printf("studio derive: failed to mark job %s processing: %v", jobID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()

	workDir := filepath.Join(h.cfg.UploadDir, "studio_derive_"+jobID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		_ = h.jobManager.UpdateJobError(jobID, "Failed to prepare workspace")
		return
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	inPath := filepath.Join(workDir, "in"+strings.ToLower(filepath.Ext(srcKey)))
	if err := h.downloadS3Object(ctx, srcKey, inPath); err != nil {
		log.Printf("studio derive: download source failed: %v", err)
		_ = h.jobManager.UpdateJobError(jobID, "Failed to download source media")
		return
	}
	outPath := filepath.Join(workDir, "out.wav")

	var err error
	switch op {
	case models.StudioDeriveVoiceClean:
		err = h.ai.CleanVoice(ctx, jobID, inPath, outPath, false)
	case models.StudioDeriveSplitVocals:
		err = h.ai.SeparateVocals(ctx, jobID, inPath, outPath, services.AIAudioOpIsolateVocals)
	case models.StudioDeriveSplitInstrumental:
		err = h.ai.SeparateVocals(ctx, jobID, inPath, outPath, models.StudioDeriveSplitInstrumental)
	}
	if err != nil {
		log.Printf("studio derive: %s failed for job %s: %v", op, jobID, err)
		_ = h.jobManager.UpdateJobError(jobID, err.Error())
		return
	}

	if err := h.uploadLocalToS3(ctx, outPath, derivedKey, "audio/wav"); err != nil {
		log.Printf("studio derive: upload derived failed: %v", err)
		_ = h.jobManager.UpdateJobError(jobID, "Failed to store the result")
		return
	}

	// Ingest the new asset (audio proxy + waveform peaks).
	res, err := h.ingest.Generate(ctx, jobID, derivedKey, asset.ID, models.StudioMediaKindAudio, true, outPath, asset.DurationSeconds)
	if err != nil {
		log.Printf("studio derive: ingest failed for job %s: %v", jobID, err)
		_ = h.jobManager.UpdateJobError(jobID, err.Error())
		return
	}
	if err := h.repo.SetAssetDerived(ctx, asset.ID, res.ProxyKey, res.SpriteKey); err != nil {
		_ = h.jobManager.UpdateJobError(jobID, "Failed to record proxy")
		return
	}
	if res.PeaksKey != "" {
		_ = h.repo.SetAssetPeaks(ctx, asset.ID, res.PeaksKey)
	}
	_ = h.jobManager.UpdateJobResult(jobID, "/api/studio/assets/"+asset.ID+"/proxy")
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusCompleted); err != nil {
		log.Printf("studio derive: failed to mark job %s completed: %v", jobID, err)
	}
}

func (h *StudioHandler) uploadLocalToS3(ctx context.Context, localPath, key, contentType string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = h.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(h.cfg.S3Bucket),
		Key:         aws.String(key),
		Body:        f,
		ContentType: aws.String(contentType),
	})
	return err
}

func deriveAssetName(orig, op string) string {
	base := strings.TrimSuffix(orig, filepath.Ext(orig))
	switch op {
	case models.StudioDeriveVoiceClean:
		return base + " — cleaned"
	case models.StudioDeriveSplitVocals:
		return base + " — vocals"
	case models.StudioDeriveSplitInstrumental:
		return base + " — music"
	default:
		return base + " — derived"
	}
}

// ----------------------------------------------------------------------- //
// SERVE PROXY + SPRITE (range-forwarding S3 passthrough)
// ----------------------------------------------------------------------- //

func (h *StudioHandler) ServeProxy(c *gin.Context)  { h.serveAssetDerivative(c, "proxy") }
func (h *StudioHandler) ServeSprite(c *gin.Context) { h.serveAssetDerivative(c, "sprite") }
func (h *StudioHandler) ServeFile(c *gin.Context)   { h.serveAssetDerivative(c, "file") }

func (h *StudioHandler) serveAssetDerivative(c *gin.Context, which string) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	// NOT owner-scoped, deliberately. These bytes are fetched by <video>,
	// <audio> and <img> elements, which cannot attach an Authorization or
	// X-MM-Session-ID header — gating them would break playback in the editor
	// for exactly the signed-in users the scoping is meant to protect. They are
	// capability URLs: unguessable asset uuids, no listing endpoint, and no
	// project metadata in the response. The editor document itself (the thing
	// B13 was about) IS owner-scoped, above.
	ctx := c.Request.Context()
	asset, err := h.repo.GetAsset(ctx, strings.TrimSpace(c.Param("id")))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}

	var key string
	switch which {
	case "sprite":
		key = asset.ThumbnailSpriteURL // stores the sprite's S3 key
	case "file":
		key = asset.S3KeyOriginal
	default:
		key = asset.S3KeyProxy
	}
	contentType := studioDerivativeContentType(asset, which, key)
	if key == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": which + " is not ready yet"})
		return
	}

	getIn := &s3.GetObjectInput{Bucket: aws.String(h.cfg.S3Bucket), Key: aws.String(key)}
	if rng := c.GetHeader("Range"); rng != "" {
		getIn.Range = aws.String(rng)
	}
	out, err := h.s3Client.GetObject(ctx, getIn)
	if err != nil {
		log.Printf("studio: failed to fetch %s for asset %s: %v", which, asset.ID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to fetch media"})
		return
	}
	defer out.Body.Close()

	if out.ContentType != nil && *out.ContentType != "" {
		contentType = *out.ContentType
	}
	if which == "file" && asset.MediaKind == models.StudioMediaKindImage && strings.EqualFold(sanitizeExtension(filepath.Ext(key)), "svg") {
		// Never let an uploaded SVG render as a document on the API origin.
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Accept-Ranges", "bytes")
	c.Header("Cache-Control", "private, max-age=3600")
	if out.ContentLength != nil {
		c.Header("Content-Length", strconv.FormatInt(*out.ContentLength, 10))
	}
	status := http.StatusOK
	if out.ContentRange != nil && *out.ContentRange != "" {
		c.Header("Content-Range", *out.ContentRange)
		status = http.StatusPartialContent
	}
	c.Status(status)
	if _, err := io.Copy(c.Writer, out.Body); err != nil {
		log.Printf("studio: stream %s for asset %s interrupted: %v", which, asset.ID, err)
	}
}

// studioDerivativeContentType is the default Content-Type for an asset
// derivative ("proxy", "sprite", "file") before S3's stored type is applied.
func studioDerivativeContentType(asset *models.StudioAsset, which, key string) string {
	ext := strings.ToLower(sanitizeExtension(filepath.Ext(key)))
	isImage := asset != nil && asset.MediaKind == models.StudioMediaKindImage
	switch which {
	case "sprite":
		if isImage && ext == "png" {
			return "image/png"
		}
		return "image/jpeg"
	case "file":
		if isImage {
			return studioImageMIMEForExt(ext)
		}
		return "application/octet-stream"
	default:
		if isImage {
			if ext == "png" {
				return "image/png"
			}
			return "image/jpeg"
		}
		if asset != nil && asset.MediaKind == models.StudioMediaKindAudio {
			return "audio/mp4"
		}
		return "video/mp4"
	}
}

// ServePeaks streams the asset's waveform peaks JSON. If the asset has no peaks
// key yet (pre-existing asset, or ingest peaks step failed), it generates them
// from the original synchronously (deduped per asset), persists the key, then
// serves. Returns 404 if the asset has no audio or generation fails.
func (h *StudioHandler) ServePeaks(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	ctx := c.Request.Context()
	assetID := strings.TrimSpace(c.Param("id"))
	asset, err := h.repo.GetAsset(ctx, assetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	if !asset.HasAudio {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset has no audio"})
		return
	}

	key := asset.S3KeyPeaks
	if key == "" {
		generated, gerr := h.backfillPeaks(ctx, asset)
		if gerr != nil {
			log.Printf("studio: peaks backfill failed for asset %s: %v", assetID, gerr)
			c.JSON(http.StatusNotFound, gin.H{"error": "Waveform is not available"})
			return
		}
		key = generated
	}

	out, err := h.s3Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(h.cfg.S3Bucket), Key: aws.String(key)})
	if err != nil {
		log.Printf("studio: failed to fetch peaks for asset %s: %v", assetID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to fetch waveform"})
		return
	}
	defer out.Body.Close()
	c.Header("Content-Type", "application/json")
	c.Header("Cache-Control", "public, max-age=86400")
	if out.ContentLength != nil {
		c.Header("Content-Length", strconv.FormatInt(*out.ContentLength, 10))
	}
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, out.Body); err != nil {
		log.Printf("studio: stream peaks for asset %s interrupted: %v", assetID, err)
	}
}

// backfillPeaks generates + persists peaks for an asset that has none, deduping
// concurrent requests per asset id via peaksMu.
func (h *StudioHandler) backfillPeaks(ctx context.Context, asset *models.StudioAsset) (string, error) {
	muIface, _ := h.peaksMu.LoadOrStore(asset.ID, &sync.Mutex{})
	mu := muIface.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// Another request may have generated it while we waited on the lock.
	if fresh, err := h.repo.GetAsset(ctx, asset.ID); err == nil && fresh.S3KeyPeaks != "" {
		return fresh.S3KeyPeaks, nil
	}

	genCtx, cancel := context.WithTimeout(ctx, h.cfg.CommandTimeout)
	defer cancel()

	tmp := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("studio_peaks_%s%s", asset.ID, strings.ToLower(filepath.Ext(asset.S3KeyOriginal))))
	if err := h.downloadS3Object(genCtx, asset.S3KeyOriginal, tmp); err != nil {
		return "", fmt.Errorf("download original: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()

	data, err := h.ingest.GeneratePeaksBytes(genCtx, "", tmp, asset.DurationSeconds)
	if err != nil {
		return "", err
	}
	key, err := h.ingest.UploadPeaks(genCtx, asset.S3KeyOriginal, asset.ID, data)
	if err != nil {
		return "", err
	}
	if err := h.repo.SetAssetPeaks(genCtx, asset.ID, key); err != nil {
		log.Printf("studio: failed to persist backfilled peaks for asset %s: %v", asset.ID, err)
	}
	return key, nil
}

// ----------------------------------------------------------------------- //
// EXPORT
// ----------------------------------------------------------------------- //

func (h *StudioHandler) ExportProject(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	var req models.StudioExportRequest
	_ = c.ShouldBindJSON(&req) // body is optional

	loadCtx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	projectID := strings.TrimSpace(c.Param("id"))
	if !h.requireProjectAccess(c, projectID) {
		return
	}
	project, err := h.repo.GetProject(loadCtx, projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	assets, err := h.repo.ListAssets(loadCtx, projectID)
	if err != nil {
		log.Printf("studio export: list assets failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load project media"})
		return
	}
	assetByID := make(map[string]*models.StudioAsset, len(assets))
	for _, a := range assets {
		assetByID[a.ID] = a
	}
	// Plan output height: read from the PERSISTED project (never the request).
	// The quota dispatcher already ran ExportHeightGate; this is the
	// authoritative check, before the checkpoint and before any job exists.
	if refuseOutputHeight(c, project.Height) {
		return
	}
	// Sanitize the (possibly pre-v2 or hand-edited) EDL before building the
	// render plan so the export compiler can trust every value.
	project.Tracks = models.SanitizeTracksForAssets(models.SanitizeTracks(project.Tracks), studioAssetKinds(assetByID))
	refs, duration, ok := services.CollectExportRefs(project, assetByID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Project has no clip to export"})
		return
	}

	// Version-history auto-checkpoint (part 09): snapshot the EDL this render
	// will represent, before the job starts. Best-effort — a failed checkpoint
	// must never block an export the user already paid quota for.
	if _, vErr := h.repo.CreateVersion(loadCtx, projectID, models.StudioVersionKindAuto, "Before export", h.versionAutoKeep(c)); vErr != nil {
		log.Printf("studio export: auto-checkpoint failed for project %s: %v", projectID, vErr)
	}

	exportCfg := buildStudioExportConfig(project, assetByID, req)
	quality := normalizeExportQuality(req.Preset)
	baseName := safeFilename(firstNonEmpty(req.FileName, project.Name, "export")) + ".mp4"

	job := h.jobManager.CreateJob(
		models.OriginalFileInfo{Name: baseName, Size: 0, Type: "video/mp4"},
		map[string]interface{}{"mode": "studio_export", "format": "mp4"},
	)
	recordJobCreated(c, h.jobManager, job, services.JobFacts{
		Tool:      "studio_export",
		Mode:      "studio_export",
		MediaKind: string(models.FileTypeVideo),
		// The source is a multi-track timeline, not one file.
		TargetFormat: "mp4",
	})
	jobOutputDir := filepath.Join(h.cfg.OutputDir, job.ID)
	if err := os.MkdirAll(jobOutputDir, 0o755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "Failed to create output directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare export"})
		return
	}
	// converted.mp4 is what ConversionHandler.DownloadFile resolves for a
	// default video job, so /api/download/:jobId serves it unchanged.
	outputPath := filepath.Join(jobOutputDir, "converted.mp4")

	go h.runExportJob(job.ID, refs, exportCfg, project.Width, project.Height, project.FPS, duration, quality, outputPath)

	c.JSON(http.StatusOK, models.UploadResponse{JobID: job.ID})
}

func (h *StudioHandler) runExportJob(jobID string, refs []services.StudioClipRef, exportCfg studioExportConfig, width, height int, fps, duration float64, quality, outputPath string) {
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusProcessing); err != nil {
		log.Printf("studio export: failed to mark job %s processing: %v", jobID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()

	// Download each distinct source once; clips that share an asset reuse the
	// same local file (referenced as repeated ffmpeg inputs).
	localByKey := make(map[string]string)
	var cleanup []string
	defer func() {
		for _, p := range cleanup {
			_ = os.Remove(p)
		}
	}()
	for _, r := range refs {
		if r.Clip.Title != nil {
			continue // assetless title clip (part 16) — no source to download
		}
		key := r.Asset.S3KeyOriginal
		if _, done := localByKey[key]; done {
			continue
		}
		local := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("studio_export_%s_%d%s", jobID, len(localByKey), strings.ToLower(filepath.Ext(key))))
		if err := h.downloadS3Object(ctx, key, local); err != nil {
			log.Printf("studio export: download original failed: %v", err)
			_ = h.jobManager.UpdateJobError(jobID, "Failed to download source media")
			return
		}
		localByKey[key] = local
		cleanup = append(cleanup, local)
	}

	// Download each referenced LUT (.cube) once, keyed by lut asset id.
	lutLocalByAssetID := make(map[string]string, len(exportCfg.lutKeys))
	for assetID, key := range exportCfg.lutKeys {
		local := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("studio_export_%s_lut_%s.cube", jobID, sanitizeS3PathSegment(assetID)))
		if err := h.downloadS3Object(ctx, key, local); err != nil {
			log.Printf("studio export: download LUT %s failed: %v", assetID, err)
			continue // a missing LUT just skips that effect
		}
		lutLocalByAssetID[assetID] = local
		cleanup = append(cleanup, local)
	}

	// Build the render plan via the shared seam (also driven by
	// cmd/parity-render): one ffmpeg input per ref (shared assets repeat the
	// same local path). Video clips composite; audio-bearing clips on non-muted
	// tracks mix in.
	plan := services.AssembleExportPlan(refs, services.StudioPlanSpec{
		Width: width, Height: height, FPS: fps, Duration: duration,
		Ducking: exportCfg.ducking, VoiceTrackID: exportCfg.voiceTrackID, Loudness: exportCfg.loudness,
		LocalByKey: localByKey, LutPaths: lutLocalByAssetID,
	})

	// Caption burn-in: write the .ass into the job output dir and point the plan
	// at it (the compiler appends the subtitles filter after the overlay cascade).
	if len(exportCfg.captions) > 0 {
		assPath := filepath.Join(filepath.Dir(outputPath), "captions.ass")
		ass := services.BuildASS(exportCfg.captions, exportCfg.captionStyle, width, height, "")
		if err := os.WriteFile(assPath, []byte(ass), 0o644); err != nil {
			log.Printf("studio export: write ASS failed: %v", err)
		} else {
			plan.CaptionsASSPath = assPath
			cleanup = append(cleanup, assPath)
		}
	}

	// Keyframe sidecar (part 15): the generated sendcmd script is written next
	// to the output and referenced by the graph (sendcmd=f=…). Best-effort —
	// a failed write drops the animation ramps, not the export.
	if cmds := services.GenerateStudioSendCmd(plan); cmds != "" {
		cmdPath := filepath.Join(filepath.Dir(outputPath), "keyframes.cmd")
		if err := os.WriteFile(cmdPath, []byte(cmds), 0o644); err != nil {
			log.Printf("studio export: write keyframe sidecar failed: %v", err)
		} else {
			plan.SendCmdPath = cmdPath
			cleanup = append(cleanup, cmdPath)
		}
	}

	// Part 10 (ADR ws/0003): server render telemetry. Emitted BEFORE the
	// terminal status update in both branches — the terminal transition
	// consumes the job's remembered analytics identity.
	renderStart := time.Now()
	stats, err := h.export.RunExport(ctx, jobID, plan, quality, outputPath)
	renderSeconds := time.Since(renderStart).Seconds()
	if err != nil {
		log.Printf("studio export: job %s failed: %v", jobID, err)
		h.jobManager.EmitStudioRender(jobID, stats, renderSeconds, duration, err)
		_ = h.jobManager.UpdateJobError(jobID, err.Error())
		return
	}
	h.jobManager.EmitStudioRender(jobID, stats, renderSeconds, duration, nil)
	if err := h.jobManager.UpdateJobResult(jobID, "/api/download/"+jobID); err != nil {
		_ = h.jobManager.UpdateJobError(jobID, "Failed to update job result")
		return
	}
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusCompleted); err != nil {
		log.Printf("studio export: failed to mark job %s completed: %v", jobID, err)
	}
}

// ----------------------------------------------------------------------- //
// AUTO-CAPTIONS
// ----------------------------------------------------------------------- //

// GenerateCaptions transcribes the project's timeline-aligned audio mix with
// whisper and persists the resulting cues. Job-based: responds {jobId}; the
// client refetches the project on completion.
func (h *StudioHandler) GenerateCaptions(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if !h.dbReady(c) {
		return
	}
	if h.transcription == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Caption generation is unavailable"})
		return
	}
	var req models.StudioCaptionsGenerateRequest
	_ = c.ShouldBindJSON(&req)

	loadCtx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	projectID := strings.TrimSpace(c.Param("id"))
	if !h.requireProjectAccess(c, projectID) {
		return
	}
	project, err := h.repo.GetProject(loadCtx, projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	assets, err := h.repo.ListAssets(loadCtx, projectID)
	if err != nil {
		log.Printf("studio captions: list assets failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load project media"})
		return
	}
	assetByID := make(map[string]*models.StudioAsset, len(assets))
	for _, a := range assets {
		assetByID[a.ID] = a
	}
	project.Tracks = models.SanitizeTracksForAssets(models.SanitizeTracks(project.Tracks), studioAssetKinds(assetByID))
	refs, duration, ok := services.CollectExportRefs(project, assetByID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Project has no media to transcribe"})
		return
	}
	hasAudio := false
	for _, r := range refs {
		if !r.TrackMuted && r.Asset.HasAudio {
			hasAudio = true
			break
		}
	}
	if !hasAudio {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Project has no audio to transcribe"})
		return
	}

	job := h.jobManager.CreateJob(
		models.OriginalFileInfo{Name: "captions.vtt", Size: 0, Type: "audio/wav"},
		map[string]interface{}{"mode": "studio_captions"},
	)
	_ = h.jobManager.SetMode(job.ID, "studio_captions")
	recordJobCreated(c, h.jobManager, job, services.JobFacts{
		Tool:         "studio_captions",
		Mode:         "studio_captions",
		MediaKind:    string(models.FileTypeAudio),
		TargetFormat: "vtt",
	})
	go h.runCaptionsJob(job, projectID, refs, duration, strings.TrimSpace(req.Language))
	c.JSON(http.StatusOK, gin.H{"jobId": job.ID})
}

func (h *StudioHandler) runCaptionsJob(job *models.ConversionJob, projectID string, refs []services.StudioClipRef, duration float64, language string) {
	jobID := job.ID
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusProcessing); err != nil {
		log.Printf("studio captions: failed to mark job %s processing: %v", jobID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()

	workDir := filepath.Join(h.cfg.UploadDir, "studio_captions_"+jobID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		_ = h.jobManager.UpdateJobError(jobID, "Failed to prepare workspace")
		return
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	// Download each distinct audio-bearing source once, then build an audio-only
	// plan (timeline-aligned, so segment times == timeline times).
	localByKey := make(map[string]string)
	for _, r := range refs {
		if r.TrackMuted || !r.Asset.HasAudio {
			continue
		}
		key := r.Asset.S3KeyOriginal
		if _, done := localByKey[key]; done {
			continue
		}
		local := filepath.Join(workDir, fmt.Sprintf("src_%d%s", len(localByKey), strings.ToLower(filepath.Ext(key))))
		if err := h.downloadS3Object(ctx, key, local); err != nil {
			log.Printf("studio captions: download source failed: %v", err)
			_ = h.jobManager.UpdateJobError(jobID, "Failed to download source media")
			return
		}
		localByKey[key] = local
	}

	plan := services.StudioExportPlan{Duration: duration}
	for _, r := range refs {
		if r.TrackMuted || !r.Asset.HasAudio {
			continue
		}
		volume := 1.0
		if r.Clip.Volume != nil {
			volume = *r.Clip.Volume
		}
		if volume <= 0 && len(r.Clip.VolumeKeyframes) == 0 {
			continue
		}
		pan := 0.0
		if r.Clip.Pan != nil {
			pan = *r.Clip.Pan
		}
		idx := len(plan.Inputs)
		plan.Inputs = append(plan.Inputs, services.ExportInput{Path: localByKey[r.Asset.S3KeyOriginal]})
		plan.Audio = append(plan.Audio, services.StudioExportAudioSeg{
			InputIndex: idx, SourceIn: r.Clip.SourceIn, SourceOut: r.Clip.SourceOut,
			TimelineStart: r.Clip.TimelineStart, Volume: volume,
			VolumeKeyframes: r.Clip.VolumeKeyframes, Pan: pan,
		})
	}
	if len(plan.Audio) == 0 {
		_ = h.jobManager.UpdateJobError(jobID, "Project has no audio to transcribe")
		return
	}

	mixPath := filepath.Join(workDir, "mix.wav")
	if err := h.export.RunAudioMix(ctx, jobID, plan, mixPath); err != nil {
		log.Printf("studio captions: audio mix failed for job %s: %v", jobID, err)
		_ = h.jobManager.UpdateJobError(jobID, err.Error())
		return
	}

	transcriptPath := filepath.Join(workDir, "transcript.vtt")
	result, err := h.transcription.Transcribe(ctx, job, mixPath, transcriptPath, services.TranscribeOptions{Format: "vtt", Language: language})
	if err != nil {
		log.Printf("studio captions: transcribe failed for job %s: %v", jobID, err)
		_ = h.jobManager.UpdateJobError(jobID, err.Error())
		return
	}

	cues := services.SegmentsToCues(result.Segments, uuid.NewString)
	if err := h.repo.SaveCaptions(ctx, projectID, cues); err != nil {
		log.Printf("studio captions: save failed for job %s: %v", jobID, err)
		_ = h.jobManager.UpdateJobError(jobID, "Failed to save captions")
		return
	}

	_ = h.jobManager.UpdateJobResult(jobID, "/api/studio/projects/"+projectID)
	if err := h.jobManager.UpdateJobStatus(jobID, models.StatusCompleted); err != nil {
		log.Printf("studio captions: failed to mark job %s completed: %v", jobID, err)
	}
}

// ----------------------------------------------------------------------- //
// HELPERS
// ----------------------------------------------------------------------- //

func (h *StudioHandler) downloadS3Object(ctx context.Context, key, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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

func (h *StudioHandler) studioPrefix() string {
	prefix := strings.Trim(h.cfg.ContentStudioS3Prefix, "/")
	if prefix == "" {
		return "studio"
	}
	return prefix
}

// sanitizeStudioKey validates a client-supplied S3 key, requiring the layout
// <prefix>/<date>/<session>/<uuid>.<ext> with a supported media extension. Same
// path-escape guards as sanitizeUploadedVideoKey.
func (h *StudioHandler) sanitizeStudioKey(key string) string {
	prefix := h.studioPrefix()
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || strings.Contains(key, "..") || strings.HasSuffix(key, "/") {
		return ""
	}
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != prefix || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return ""
	}
	if sanitizeS3PathSegment(parts[2]) != parts[2] {
		return ""
	}
	ext := sanitizeExtension(filepath.Ext(parts[3]))
	if ext == "" || !isSupportedStudioExtension(ext) {
		return ""
	}
	return key
}

func isSupportedStudioExtension(ext string) bool {
	switch ext {
	case "mp4", "mov", "m4v", "webm", "mkv", "avi", "flv", "wmv", "mpeg", "mpg",
		"mp3", "wav", "aac", "m4a", "ogg", "flac",
		"cube": // 3D LUT asset
		return true
	default:
		return isStudioImageExtension(ext)
	}
}

// studioImageMIMEs maps still-image extensions to their canonical MIME type.
var studioImageMIMEs = map[string]string{
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png",
	"webp": "image/webp", "gif": "image/gif", "bmp": "image/bmp",
	"tif": "image/tiff", "tiff": "image/tiff", "heic": "image/heic",
	"heif": "image/heif", "avif": "image/avif", "svg": "image/svg+xml",
}

func isStudioImageExtension(ext string) bool {
	_, ok := studioImageMIMEs[strings.ToLower(ext)]
	return ok
}

// studioImageMIMEForExt returns the image MIME for ext, or
// application/octet-stream when ext is not a still-image extension.
func studioImageMIMEForExt(ext string) string {
	if m, ok := studioImageMIMEs[strings.ToLower(ext)]; ok {
		return m
	}
	return "application/octet-stream"
}

// studioMaxLUTBytes caps .cube uploads (LUTs are tiny; an 8MB ceiling is
// generous even for a 64³ float table).
const studioMaxLUTBytes int64 = 8 << 20

// studioAudioParams pulls sample rate + channel count from the first audio
// stream in a probe report.
func studioAudioParams(probe *models.VideoProbeResponse) (sampleRate, channels int) {
	for _, s := range probe.Streams {
		if s.CodecType == "audio" {
			if v, err := strconv.Atoi(strings.TrimSpace(s.SampleRate)); err == nil {
				sampleRate = v
			}
			channels = s.Channels
			break
		}
	}
	return sampleRate, channels
}

// studioExportConfig carries the EDL v2 project-level export inputs resolved up
// front (before the async render job runs).
type studioExportConfig struct {
	lutKeys      map[string]string // lutAssetId → S3 key of the .cube
	ducking      *services.StudioDucking
	loudness     string
	voiceTrackID string
	// Caption burn-in (when enabled + present).
	captions     []models.StudioCaptionCue
	captionStyle *models.StudioCaptionStyle
}

// buildStudioExportConfig resolves ducking, loudness and the LUT assets the EDL
// references so the render job can download them and build the plan.
func buildStudioExportConfig(project *models.StudioProject, assetByID map[string]*models.StudioAsset, req models.StudioExportRequest) studioExportConfig {
	cfg := studioExportConfig{lutKeys: map[string]string{}, loudness: normalizeLoudness(req.Loudness)}
	if a := project.Audio; a != nil && a.DuckingEnabled && strings.TrimSpace(a.DuckVoiceTrackID) != "" {
		cfg.ducking = &services.StudioDucking{AmountDb: a.DuckAmountDb, AttackMs: a.DuckAttackMs, ReleaseMs: a.DuckReleaseMs}
		cfg.voiceTrackID = a.DuckVoiceTrackID
	}
	if project.CaptionsEnabled && len(project.Captions) > 0 {
		cfg.captions = project.Captions
		cfg.captionStyle = project.CaptionStyle
	}
	for _, track := range project.Tracks {
		for _, clip := range track.Clips {
			for _, e := range clip.Effects {
				if e.Type == models.StudioEffectLUT && e.LutAssetID != nil {
					if asset, ok := assetByID[*e.LutAssetID]; ok && asset.S3KeyOriginal != "" {
						cfg.lutKeys[*e.LutAssetID] = asset.S3KeyOriginal
					}
				}
			}
		}
	}
	return cfg
}

func normalizeLoudness(preset string) string {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "streaming", "podcast", "broadcast":
		return strings.ToLower(strings.TrimSpace(preset))
	default:
		return ""
	}
}

// The EDL flattening (CollectExportRefs) and plan assembly (AssembleExportPlan)
// moved to internal/services/studio_export_plan.go in part 13, shared with the
// parity-render CLI.

func normalizeExportQuality(preset string) string {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "low", "high":
		return strings.ToLower(strings.TrimSpace(preset))
	default:
		return "medium"
	}
}

func clampCreateProject(req *models.StudioCreateProjectRequest) {
	req.Name = clampProjectName(req.Name)
	req.FPS = clampFPS(req.FPS)
	req.Width = clampDim(req.Width, 1920, 7680)
	req.Height = clampDim(req.Height, 1080, 4320)
}

func clampSaveProject(req *models.StudioSaveProjectRequest) {
	req.Name = clampProjectName(req.Name)
	req.FPS = clampFPS(req.FPS)
	req.Width = clampDim(req.Width, 1920, 7680)
	req.Height = clampDim(req.Height, 1080, 4320)
	if req.Tracks == nil {
		req.Tracks = []models.StudioTrack{}
	}
}

func clampProjectName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Untitled project"
	}
	if len(name) > 200 {
		return name[:200]
	}
	return name
}

func clampFPS(fps float64) float64 {
	if fps <= 0 {
		return 30
	}
	if fps > 240 {
		return 240
	}
	return fps
}

func clampDim(v, fallback, max int) int {
	if v <= 0 {
		return fallback
	}
	if v > max {
		return max
	}
	return v
}
