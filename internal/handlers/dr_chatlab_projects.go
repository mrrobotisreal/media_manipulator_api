package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Chat-lab Projects: grouped workspaces with four kinds of shared context
// (description, instructions, assets, living memory — see dr_chatlab_memory.go
// for the memory updater and dr_chatlab_stream.go for how the context reaches
// the model). Projects are COLLABORATIVELY editable: any allowlisted user can
// update name/description/instructions and upload/delete assets — the
// project's context is shared workspace state both partners curate. The ONLY
// creator-only operation is deleting the whole project (it destroys all chats
// and assets).

// drChatLabProjectsS3Root is the S3 key root for project assets:
// {root}/{projectId}/assets/{assetId}.{ext}. The owner explicitly wants
// project data under the `double-raven/` prefix; the existing DR features use
// bare prefixes (`chatlab/`, `feedback/`, `documents/`) — do NOT migrate
// those, only project assets live here.
const drChatLabProjectsS3Root = "double-raven/chatlab/projects"

const (
	drChatLabMaxProjectNameChars    = 120
	drChatLabMaxDescriptionBytes    = 4 << 10  // 4 KiB
	drChatLabMaxInstructionsBytes   = 16 << 10 // 16 KiB
	drChatLabProjectsCap            = 100
	drChatLabMaxAssetsPerProject    = 50
	drChatLabMaxCodeTextAssetBytes  = 2 << 20  // 2 MiB for text/code assets
	drChatLabMaxAudioAssetBytes     = 25 << 20 // 25 MiB for audio assets
	drChatLabProjectAssetsCTDefault = "text/plain; charset=utf-8"
)

// ----------------------------------------------------------------------- //
// Asset type policy (pure; unit-tested)
// ----------------------------------------------------------------------- //

// drChatLabTextExts / drChatLabCodeExts classify by extension FIRST — browsers
// report unreliable MIME types for code files (text/x-go,
// application/octet-stream, or nothing at all).
var drChatLabTextExts = map[string]bool{
	"md": true, "txt": true, "csv": true, "json": true, "yaml": true, "yml": true, "toml": true, "xml": true,
}

var drChatLabCodeExts = map[string]bool{
	"go": true, "ts": true, "tsx": true, "js": true, "jsx": true, "py": true, "rb": true, "rs": true,
	"java": true, "kt": true, "swift": true, "c": true, "h": true, "cpp": true, "hpp": true, "cs": true,
	"sh": true, "sql": true, "css": true, "html": true, "php": true,
}

var drChatLabImageExts = map[string]bool{"png": true, "jpg": true, "jpeg": true, "webp": true, "gif": true}

// Audio: mp3 + wav only — the formats OpenRouter's input_audio content part
// documents as broadly supported (the audio docs list more, but model support
// varies; mp3/wav are the safe common denominators).
var drChatLabAudioExts = map[string]bool{"mp3": true, "wav": true}

// projectAssetKind classifies an upload by extension first, with contentType
// as a sanity fallback check for the binary kinds, returning the stored kind,
// the S3-key extension, the size cap, and ok=false for anything unsupported
// (video, archives, arbitrary binaries).
//
// storedContentType (returned via normalizeProjectAssetContentType below) for
// text/code kinds is forced to a plain text type when the browser sent
// something exotic — the original extension survives in file_name / s3_key.
func projectAssetKind(fileName, contentType string) (kind, ext string, maxBytes int64, ok bool) {
	ext = strings.ToLower(strings.TrimPrefix(path.Ext(strings.TrimSpace(fileName)), "."))
	ct := strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case drChatLabTextExts[ext]:
		return "text", ext, drChatLabMaxCodeTextAssetBytes, true
	case drChatLabCodeExts[ext]:
		return "code", ext, drChatLabMaxCodeTextAssetBytes, true
	case drChatLabImageExts[ext]:
		if !strings.HasPrefix(ct, "image/") {
			return "", ext, 0, false
		}
		return "image", ext, drMaxImageAssetBytes, true
	case drChatLabAudioExts[ext]:
		if !strings.HasPrefix(ct, "audio/") {
			return "", ext, 0, false
		}
		return "audio", ext, drChatLabMaxAudioAssetBytes, true
	case ext == "pdf":
		if ct != "application/pdf" {
			return "", ext, 0, false
		}
		return "pdf", ext, drChatLabMaxPDFBytes, true
	}
	return "", ext, 0, false
}

// normalizeProjectAssetContentType returns the content type to STORE (and to
// set on the S3 object). For text/code kinds, exotic browser types
// (application/octet-stream, text/x-go, empty) are normalized to plain text;
// well-known text types pass through.
func normalizeProjectAssetContentType(kind, contentType string) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if kind != "text" && kind != "code" {
		return ct
	}
	switch {
	case ct == "", ct == "application/octet-stream", strings.HasPrefix(ct, "text/x-"), strings.HasPrefix(ct, "application/x-"):
		return drChatLabProjectAssetsCTDefault
	case ct == "application/json", ct == "application/xml", ct == "application/yaml", ct == "application/toml",
		strings.HasPrefix(ct, "text/"):
		return ct
	default:
		return drChatLabProjectAssetsCTDefault
	}
}

// chatLabHumanSize renders a byte count for the asset manifest ("14.2 KB").
func chatLabHumanSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	kb := float64(bytes) / 1024
	if kb < 1024 {
		if kb < 100 {
			return fmt.Sprintf("%.1f KB", kb)
		}
		return fmt.Sprintf("%.0f KB", kb)
	}
	mb := kb / 1024
	if mb < 10 {
		return fmt.Sprintf("%.1f MB", mb)
	}
	return fmt.Sprintf("%.0f MB", mb)
}

// ----------------------------------------------------------------------- //
// Shared plumbing
// ----------------------------------------------------------------------- //

func drChatLabProjectID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("projectId"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid project id"})
		return "", false
	}
	return id, true
}

func drChatLabAssetID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("assetId"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid asset id"})
		return "", false
	}
	return id, true
}

// scannedChatProject is the raw column set for a project row.
type scannedChatProject struct {
	id, name, description, instructions, memory string
	memoryStatus, createdByEmail                string
	memoryUpdatedAt                             *time.Time
	createdAt, updatedAt                        time.Time
	chatCount, assetCount                       int
}

const chatProjectCols = `id, name, description, instructions, memory, memory_status, created_by_email, memory_updated_at, created_at, updated_at`

func (p *scannedChatProject) scanFields() []any {
	return []any{&p.id, &p.name, &p.description, &p.instructions, &p.memory, &p.memoryStatus, &p.createdByEmail, &p.memoryUpdatedAt, &p.createdAt, &p.updatedAt}
}

func (p scannedChatProject) toSummaryDTO(callerEmail string) models.DrChatProject {
	dto := models.DrChatProject{
		ID:             p.id,
		Name:           p.name,
		Description:    p.description,
		IsMine:         strings.EqualFold(p.createdByEmail, callerEmail),
		CreatedByEmail: p.createdByEmail,
		ChatCount:      p.chatCount,
		AssetCount:     p.assetCount,
		MemoryStatus:   p.memoryStatus,
		CreatedAt:      models.UTCTime{Time: p.createdAt},
		UpdatedAt:      models.UTCTime{Time: p.updatedAt},
	}
	if p.memoryUpdatedAt != nil {
		u := models.UTCTime{Time: *p.memoryUpdatedAt}
		dto.MemoryUpdatedAt = &u
	}
	return dto
}

// loadProject fetches one project row (pgx.ErrNoRows passes through → 404 at
// the call site). Counts are NOT populated here.
func (h *DrChatLabHandler) loadProject(ctx context.Context, id string) (scannedChatProject, error) {
	var p scannedChatProject
	err := h.pool.QueryRow(ctx, `SELECT `+chatProjectCols+` FROM dr_chat_projects WHERE id = $1`, id).
		Scan(p.scanFields()...)
	return p, err
}

// validateProjectFields enforces the §3 size caps, returning a client-facing
// error message ("" = valid). name is checked only when checkName is set
// (partial updates may omit it).
func validateProjectFields(name string, checkName bool, description, instructions string) string {
	if checkName {
		if n := utf8.RuneCountInString(strings.TrimSpace(name)); n < 1 || n > drChatLabMaxProjectNameChars {
			return "Project name must be 1–120 characters"
		}
	}
	if len(description) > drChatLabMaxDescriptionBytes {
		return "Description is too long (4 KiB max)"
	}
	if len(instructions) > drChatLabMaxInstructionsBytes {
		return "Instructions are too long (16 KiB max)"
	}
	return ""
}

// ----------------------------------------------------------------------- //
// GET /chatlab/projects
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) ListProjects(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// One query — counts via grouped subselects, no N+1.
	rows, err := h.pool.Query(ctx, `
SELECT `+chatProjectCols+`,
       (SELECT count(*) FROM dr_chat_sessions s WHERE s.project_id = p.id) AS chat_count,
       (SELECT count(*) FROM dr_chat_project_assets a WHERE a.project_id = p.id AND a.status = 'ready') AS asset_count
FROM dr_chat_projects p
ORDER BY updated_at DESC, id
LIMIT $1`, drChatLabProjectsCap)
	if err != nil {
		log.Printf("dr chatlab: list projects: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load projects"})
		return
	}
	defer rows.Close()

	projects := make([]models.DrChatProject, 0)
	for rows.Next() {
		var p scannedChatProject
		dests := append(p.scanFields(), &p.chatCount, &p.assetCount)
		if err := rows.Scan(dests...); err != nil {
			log.Printf("dr chatlab: scan project: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load projects"})
			return
		}
		projects = append(projects, p.toSummaryDTO(claims.Email))
	}
	if err := rows.Err(); err != nil {
		log.Printf("dr chatlab: list projects rows: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load projects"})
		return
	}
	c.JSON(http.StatusOK, models.DrChatProjectsResponse{Projects: projects})
}

// ----------------------------------------------------------------------- //
// POST /chatlab/projects
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) CreateProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	var req models.DrChatCreateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if msg := validateProjectFields(name, true, req.Description, req.Instructions); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var p scannedChatProject
	err := h.pool.QueryRow(ctx, `
INSERT INTO dr_chat_projects (name, description, instructions, created_by_uid, created_by_email)
VALUES ($1, $2, $3, $4, lower($5))
RETURNING `+chatProjectCols, name, req.Description, req.Instructions, claims.UID, claims.Email).
		Scan(p.scanFields()...)
	if err != nil {
		log.Printf("dr chatlab: create project: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create project"})
		return
	}
	c.JSON(http.StatusCreated, p.toSummaryDTO(claims.Email))
}

// ----------------------------------------------------------------------- //
// GET /chatlab/projects/:projectId
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) GetProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	projectID, ok := drChatLabProjectID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	p, err := h.loadProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	if err != nil {
		log.Printf("dr chatlab: load project: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load project"})
		return
	}

	// Ready assets, hydrated with presigned URLs.
	assets := make([]models.DrChatProjectAsset, 0)
	aRows, err := h.pool.Query(ctx, `
SELECT id, kind, file_name, content_type, size_bytes, width, height, uploaded_by_email, s3_key, created_at
FROM dr_chat_project_assets
WHERE project_id = $1 AND status = 'ready'
ORDER BY created_at, id`, projectID)
	if err != nil {
		log.Printf("dr chatlab: list project assets: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load project"})
		return
	}
	func() {
		defer aRows.Close()
		for aRows.Next() {
			var a models.DrChatProjectAsset
			var key string
			var createdAt time.Time
			if err := aRows.Scan(&a.ID, &a.Kind, &a.FileName, &a.ContentType, &a.SizeBytes, &a.Width, &a.Height, &a.UploadedByEmail, &key, &createdAt); err != nil {
				log.Printf("dr chatlab: scan project asset: %v", err)
				continue
			}
			a.CreatedAt = models.UTCTime{Time: createdAt}
			a.ViewURL = h.presignGet(ctx, key, "")
			a.DownloadURL = h.presignGet(ctx, key, a.FileName)
			assets = append(assets, a)
		}
	}()

	// This project's sessions, newest activity first.
	sessions := make([]models.DrChatSession, 0)
	sRows, err := h.pool.Query(ctx, `
SELECT `+chatSessionCols+`
FROM dr_chat_sessions
WHERE project_id = $1
ORDER BY updated_at DESC, id
LIMIT $2`, projectID, drChatLabSessionsCap)
	if err != nil {
		log.Printf("dr chatlab: list project sessions: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load project"})
		return
	}
	func() {
		defer sRows.Close()
		for sRows.Next() {
			var s scannedChatSession
			if err := sRows.Scan(s.scanFields()...); err != nil {
				log.Printf("dr chatlab: scan project session: %v", err)
				continue
			}
			sessions = append(sessions, s.toDTO(claims.Email))
		}
	}()

	summary := p.toSummaryDTO(claims.Email)
	summary.ChatCount = len(sessions)
	summary.AssetCount = len(assets)
	c.JSON(http.StatusOK, models.DrChatProjectDetail{
		DrChatProject: summary,
		Instructions:  p.instructions,
		Memory:        p.memory,
		Assets:        assets,
		Sessions:      sessions,
	})
}

// ----------------------------------------------------------------------- //
// PUT /chatlab/projects/:projectId  (collaborative — any allowlisted user)
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) UpdateProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	projectID, ok := drChatLabProjectID(c)
	if !ok {
		return
	}
	var req models.DrChatUpdateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	p, err := h.loadProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update project"})
		return
	}

	// Pointer fields: only provided keys change.
	name := p.name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	description := p.description
	if req.Description != nil {
		description = *req.Description
	}
	instructions := p.instructions
	if req.Instructions != nil {
		instructions = *req.Instructions
	}
	if msg := validateProjectFields(name, true, description, instructions); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	contextChanged := description != p.description || instructions != p.instructions

	err = h.pool.QueryRow(ctx, `
UPDATE dr_chat_projects
SET name = $1, description = $2, instructions = $3, updated_at = now()
WHERE id = $4
RETURNING `+chatProjectCols, name, description, instructions, projectID).
		Scan(p.scanFields()...)
	if err != nil {
		log.Printf("dr chatlab: update project: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update project"})
		return
	}

	// The model's background context changed → regenerate the memory.
	if contextChanged {
		h.triggerMemoryUpdate(projectID, claims.UID, claims.Email)
	}
	c.JSON(http.StatusOK, p.toSummaryDTO(claims.Email))
}

// ----------------------------------------------------------------------- //
// DELETE /chatlab/projects/:projectId  (creator-only, hard delete)
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) DeleteProject(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	projectID, ok := drChatLabProjectID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	p, err := h.loadProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete project"})
		return
	}
	if !strings.EqualFold(p.createdByEmail, claims.Email) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the project creator can delete it"})
		return
	}

	// Collect session ids FIRST — after the CASCADE delete they're gone, and
	// their attachment objects live under chatlab/{sessionId}/ prefixes.
	sessionIDs := make([]string, 0)
	sRows, err := h.pool.Query(ctx, `SELECT id FROM dr_chat_sessions WHERE project_id = $1`, projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete project"})
		return
	}
	func() {
		defer sRows.Close()
		for sRows.Next() {
			var id string
			if err := sRows.Scan(&id); err == nil {
				sessionIDs = append(sessionIDs, id)
			}
		}
	}()

	// Hard delete — CASCADE removes sessions, messages, attachments, assets.
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_chat_projects WHERE id = $1`, projectID); err != nil {
		log.Printf("dr chatlab: delete project: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete project"})
		return
	}

	// Best-effort S3 cleanup AFTER the DB commit: the project's asset prefix
	// plus every collected session's attachment prefix.
	go func(sessionIDs []string) {
		h.deletePrefixObjects(fmt.Sprintf("%s/%s/", drChatLabProjectsS3Root, projectID))
		for _, sid := range sessionIDs {
			h.deletePrefixObjects(fmt.Sprintf("chatlab/%s/", sid))
		}
	}(sessionIDs)

	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// deletePrefixObjects removes every object under an S3 prefix. Failures are
// logged, never surfaced (best-effort cleanup of disposable data).
func (h *DrChatLabHandler) deletePrefixObjects(prefix string) {
	if h.s3Client == nil || h.cfg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var token *string
	for {
		out, err := h.s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(h.cfg.S3Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			log.Printf("dr chatlab: list objects under %s: %v", prefix, err)
			return
		}
		for _, obj := range out.Contents {
			h.deleteObject(ctx, aws.ToString(obj.Key))
		}
		if !aws.ToBool(out.IsTruncated) {
			return
		}
		token = out.NextContinuationToken
	}
}

// ----------------------------------------------------------------------- //
// POST /chatlab/projects/:projectId/memory/refresh
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) RefreshProjectMemory(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	projectID, ok := drChatLabProjectID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if _, err := h.loadProject(ctx, projectID); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to refresh memory"})
		return
	}
	if h.cfg == nil || strings.TrimSpace(h.cfg.DRChatLabMemoryModel) == "" {
		c.JSON(http.StatusOK, gin.H{"status": "disabled"})
		return
	}
	h.triggerMemoryUpdate(projectID, claims.UID, claims.Email)
	c.JSON(http.StatusAccepted, gin.H{"status": "updating"})
}

// ----------------------------------------------------------------------- //
// Project assets — presign / complete / delete (shared library)
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) PresignProjectAsset(c *gin.Context) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	projectID, ok := drChatLabProjectID(c)
	if !ok {
		return
	}
	var req models.DrChatPresignProjectAssetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	kind, ext, maxBytes, okKind := projectAssetKind(req.FileName, req.ContentType)
	if !okKind {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Unsupported asset type: %s", ext)})
		return
	}
	if req.SizeBytes <= 0 || req.SizeBytes > maxBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s asset exceeds the %d MB limit", kind, maxBytes>>20)})
		return
	}
	fileName := sanitizeDrFileName(req.FileName)
	storedCT := normalizeProjectAssetContentType(kind, req.ContentType)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if _, err := h.loadProject(ctx, projectID); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}

	// Cap ready+pending so an upload burst can't blow past the library limit.
	var count int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM dr_chat_project_assets WHERE project_id = $1`, projectID).Scan(&count); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	if count >= drChatLabMaxAssetsPerProject {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("A project can have at most %d assets", drChatLabMaxAssetsPerProject)})
		return
	}

	assetID := uuid.NewString()
	key := fmt.Sprintf("%s/%s/assets/%s.%s", drChatLabProjectsS3Root, projectID, assetID, ext)
	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_chat_project_assets (id, project_id, uploaded_by_uid, uploaded_by_email, kind, file_name, s3_key, content_type, size_bytes, width, height, status)
VALUES ($1, $2, $3, lower($4), $5, $6, $7, $8, $9, $10, $11, 'pending')`,
		assetID, projectID, claims.UID, claims.Email, kind, fileName, key, storedCT, req.SizeBytes, req.Width, req.Height); err != nil {
		log.Printf("dr chatlab: insert project asset: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}

	presignCtx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	defer pcancel()
	out, err := h.s3Presign.PresignPutObject(presignCtx, &s3.PutObjectInput{
		Bucket:      aws.String(h.cfg.S3Bucket),
		Key:         aws.String(key),
		ContentType: aws.String(storedCT),
	}, func(o *s3.PresignOptions) { o.Expires = h.cfg.S3PresignTTL })
	if err != nil {
		log.Printf("dr chatlab: presign project asset put: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload URL"})
		return
	}
	c.JSON(http.StatusCreated, models.DrChatProjectPresignResponse{AssetID: assetID, UploadURL: out.URL, Key: key})
}

func (h *DrChatLabHandler) CompleteProjectAsset(c *gin.Context) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	projectID, ok := drChatLabProjectID(c)
	if !ok {
		return
	}
	assetID, ok := drChatLabAssetID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var (
		aProject, key, contentType, status string
		declaredSize                       int64
	)
	err := h.pool.QueryRow(ctx, `
SELECT project_id, s3_key, content_type, size_bytes, status
FROM dr_chat_project_assets WHERE id = $1`, assetID).
		Scan(&aProject, &key, &contentType, &declaredSize, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && aProject != projectID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	}
	if status != "pending" {
		c.JSON(http.StatusConflict, gin.H{"error": "Asset upload already completed"})
		return
	}

	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("dr chatlab: head project asset %s: %v", key, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file was not found"})
		return
	}
	objectSize := aws.ToInt64(head.ContentLength)
	if objectSize <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file is empty"})
		return
	}
	if declaredSize > 0 && absInt64(objectSize-declaredSize) > 1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file size does not match"})
		return
	}
	if ct := aws.ToString(head.ContentType); ct != "" && !strings.EqualFold(ct, contentType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded file type does not match"})
		return
	}

	if _, err := h.pool.Exec(ctx, `UPDATE dr_chat_project_assets SET status = 'ready' WHERE id = $1`, assetID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DeleteProjectAsset removes an asset from the shared library. Any allowlisted
// user may delete any asset (collaborative curation) — including ready ones,
// since project assets are library items, never message-bound.
func (h *DrChatLabHandler) DeleteProjectAsset(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	projectID, ok := drChatLabProjectID(c)
	if !ok {
		return
	}
	assetID, ok := drChatLabAssetID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var aProject, key string
	err := h.pool.QueryRow(ctx, `SELECT project_id, s3_key FROM dr_chat_project_assets WHERE id = $1`, assetID).
		Scan(&aProject, &key)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && aProject != projectID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete asset"})
		return
	}
	h.deleteObject(ctx, key)
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_chat_project_assets WHERE id = $1`, assetID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete asset"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ----------------------------------------------------------------------- //
// Reaper — pending project assets > 24h (companion to unbound attachments)
// ----------------------------------------------------------------------- //

// ReapStaleProjectAssets deletes 'pending' project-asset rows older than 24h
// (uploads that never completed) plus their objects. Ready assets are never
// reaped. Called from the same daily ticker as ReapUnboundAttachments.
func (h *DrChatLabHandler) ReapStaleProjectAssets(ctx context.Context) {
	if h.pool == nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	rows, err := h.pool.Query(ctx, `
SELECT id, s3_key FROM dr_chat_project_assets
WHERE status = 'pending' AND created_at < $1`, cutoff)
	if err != nil {
		log.Printf("dr chatlab reaper: select stale project assets: %v", err)
		return
	}
	var ids []string
	func() {
		defer rows.Close()
		for rows.Next() {
			var id, key string
			if err := rows.Scan(&id, &key); err != nil {
				continue
			}
			h.deleteObject(ctx, key)
			ids = append(ids, id)
		}
	}()
	if len(ids) > 0 {
		if _, err := h.pool.Exec(ctx, `DELETE FROM dr_chat_project_assets WHERE id = ANY($1)`, ids); err != nil {
			log.Printf("dr chatlab reaper: delete stale project assets: %v", err)
		} else {
			log.Printf("dr chatlab reaper: removed %d stale pending project assets", len(ids))
		}
	}
}
