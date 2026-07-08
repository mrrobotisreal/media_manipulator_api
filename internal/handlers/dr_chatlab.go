package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// DrChatLabHandler serves the DR AI Chat Test Lab (/dr/demos/chat-lab): a
// ChatGPT-style chat backed by OpenRouter, called from THIS API so the key
// lives only on the home server and every endpoint — including the streaming
// send — inherits RequireDoubleRavenAuth on the /api/dr group. Authorship
// always comes from the verified claims in the gin context (DRContextKey),
// never from request bodies.
//
// Sessions are SHARED between the two portal users (like feedback channels):
// any allowlisted user can read/continue any session; rename + delete are
// creator-only (case-insensitive compare against created_by_email). Session
// delete is a HARD delete (disposable test data) with best-effort S3 prefix
// cleanup — deliberately NOT the docs soft-delete pattern.
//
// Attachments use Media Manipulator's standard S3 handshake — presign ->
// client PUT -> complete — with a chat-lab-specific allowlist
// (chatLabAttachmentExt): images become multimodal model input, PDFs go
// through OpenRouter's file parser, and small text files are inlined into the
// prompt.
type DrChatLabHandler struct {
	pool      *pgxpool.Pool
	cfg       *config.Config
	s3Client  *s3.Client
	s3Presign *s3.PresignClient
	// or is nil when OPENROUTER_API_KEY is unset — the models/send endpoints
	// then fail closed with 503 (orReady), like the nil DR verifier pattern.
	or *openrouter.Client
	// appCtx is the process root context (cancelled on shutdown) so the
	// streaming send can close promptly on graceful shutdown. nil-safe.
	appCtx context.Context
	// catalog is the in-memory filtered model catalog cache (see
	// dr_chatlab_models.go).
	catalog chatLabCatalogCache
}

// NewDrChatLabHandler wires the handler. Mirrors NewDrFeedbackHandler for the
// presign/S3 plumbing and adds the OpenRouter client (nil when unconfigured).
func NewDrChatLabHandler(appCtx context.Context, pool *pgxpool.Pool, cfg *config.Config, s3Client *s3.Client) *DrChatLabHandler {
	var presign *s3.PresignClient
	if s3Client != nil {
		presign = s3.NewPresignClient(s3Client)
	}
	var or *openrouter.Client
	if cfg != nil && strings.TrimSpace(cfg.OpenRouterAPIKey) != "" {
		or = openrouter.New(cfg.OpenRouterBaseURL, cfg.OpenRouterAPIKey, cfg.DRChatLabAttributionURL)
	}
	return &DrChatLabHandler{
		pool:      pool,
		cfg:       cfg,
		s3Client:  s3Client,
		s3Presign: presign,
		or:        or,
		appCtx:    appCtx,
	}
}

// RegisterDrChatLabRoutes wires the chat-lab endpoints onto the already-prefixed
// and already-authed /dr group, so they resolve to /api/dr/chatlab/…. The
// distinct /chatlab/… prefix keeps gin wildcards clear of /docs/:slug.
func RegisterDrChatLabRoutes(r gin.IRouter, h *DrChatLabHandler) {
	r.GET("/chatlab/models", h.ListModels)
	r.GET("/chatlab/sessions", h.ListSessions)
	r.POST("/chatlab/sessions", h.CreateSession)
	r.GET("/chatlab/sessions/:id", h.GetSession)
	r.PUT("/chatlab/sessions/:id", h.RenameSession)
	r.DELETE("/chatlab/sessions/:id", h.DeleteSession)
	r.POST("/chatlab/sessions/:id/attachments", h.PresignAttachment)
	r.POST("/chatlab/sessions/:id/attachments/:attachmentId/complete", h.CompleteAttachment)
	r.DELETE("/chatlab/sessions/:id/attachments/:attachmentId", h.DeleteAttachment)
	r.POST("/chatlab/sessions/:id/messages", h.SendChatMessage)
}

// ----------------------------------------------------------------------- //
// Constants + pure helpers (unit-tested in dr_chatlab_test.go)
// ----------------------------------------------------------------------- //

const (
	drChatLabMaxContentBytes     = 64 << 10 // 64 KiB of user message text
	drChatLabMaxAttachments      = 5
	drChatLabMaxTitleChars       = 120
	drChatLabDerivedTitleChars   = 60
	drChatLabGeneratedTitleChars = 80
	drChatLabSessionsCap         = 200
	drChatLabMaxInlineFileBytes  = 2 << 20   // 2 MiB per text-kind file (they get inlined)
	drChatLabMaxPDFBytes         = 25 << 20  // 25 MiB per PDF
	drChatLabInlineBudgetBytes   = 512 << 10 // 512 KiB of inlined file text per request
	drChatLabTitlePromptTruncate = 2 << 10   // first user message excerpt for the title model
)

// drChatLabEfforts is the full reasoning-effort enum the send endpoint accepts
// (” = off). The picker shows the per-model subset from the catalog, but the
// server accepts the whole enum for any reasoning-capable model — effort
// granularity is not reliably discoverable per model (see buildChatLabModel).
var drChatLabEfforts = map[string]bool{
	"":        true,
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
	"xhigh":   true,
}

// chatLabAttachmentExt allowlists a (kind, contentType) pair for chat-lab
// attachments and returns the S3-key extension plus that pair's max upload
// size. Deliberately its OWN allowlist (docAssetExt is not widened): images
// are the multimodal formats OpenRouter documents (png/jpeg/webp/gif, sharing
// the doc-asset image cap); "file" is limited to what can actually reach a
// model — PDFs (file-parser plugin, 25 MB) and small text files that get
// inlined into the prompt (2 MB each).
func chatLabAttachmentExt(kind, contentType string) (ext string, maxBytes int64, ok bool) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	switch kind {
	case "image":
		switch ct {
		case "image/png":
			return "png", drMaxImageAssetBytes, true
		case "image/jpeg":
			return "jpg", drMaxImageAssetBytes, true
		case "image/webp":
			return "webp", drMaxImageAssetBytes, true
		case "image/gif":
			return "gif", drMaxImageAssetBytes, true
		}
	case "file":
		switch ct {
		case "application/pdf":
			return "pdf", drChatLabMaxPDFBytes, true
		case "text/plain":
			return "txt", drChatLabMaxInlineFileBytes, true
		case "text/csv":
			return "csv", drChatLabMaxInlineFileBytes, true
		case "text/markdown":
			return "md", drChatLabMaxInlineFileBytes, true
		case "application/json":
			return "json", drChatLabMaxInlineFileBytes, true
		}
	}
	return "", 0, false
}

// deriveChatTitle is the no-LLM fallback title: the first user message's text,
// whitespace-collapsed and truncated to drChatLabDerivedTitleChars runes on a
// word boundary with an ellipsis.
func deriveChatTitle(content string) string {
	text := strings.Join(strings.Fields(content), " ")
	if text == "" {
		return "New Chat"
	}
	runes := []rune(text)
	if len(runes) <= drChatLabDerivedTitleChars {
		return text
	}
	cut := string(runes[:drChatLabDerivedTitleChars])
	// Prefer breaking at the last space so we don't slice a word — but only
	// when doing so keeps a reasonable amount of text.
	if i := strings.LastIndex(cut, " "); i > drChatLabDerivedTitleChars/3 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

// sanitizeGeneratedTitle cleans an LLM-produced title: newlines collapsed,
// wrapping quotes/backticks stripped, whitespace normalized, capped at
// drChatLabGeneratedTitleChars runes. Returns "" when nothing usable remains
// (callers then fall back to the derived title).
func sanitizeGeneratedTitle(raw string) string {
	s := strings.Join(strings.Fields(raw), " ")
	s = strings.Trim(s, "\"'`“”‘’ ")
	if utf8.RuneCountInString(s) > drChatLabGeneratedTitleChars {
		s = string([]rune(s)[:drChatLabGeneratedTitleChars])
		s = strings.TrimSpace(s)
	}
	return s
}

// ----------------------------------------------------------------------- //
// Shared handler plumbing
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) dbReady(c *gin.Context) bool {
	if h.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Chat storage is unavailable"})
		return false
	}
	return true
}

func (h *DrChatLabHandler) s3Ready(c *gin.Context) bool {
	if h.s3Client == nil || h.s3Presign == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Attachment storage is unavailable"})
		return false
	}
	return true
}

// orReady fails closed when OPENROUTER_API_KEY is unset (nil client).
func (h *DrChatLabHandler) orReady(c *gin.Context) bool {
	if h.or == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "AI chat is not configured"})
		return false
	}
	return true
}

func drChatLabSessionID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid session id"})
		return "", false
	}
	return id, true
}

func drChatLabAttachmentID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("attachmentId"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid attachment id"})
		return "", false
	}
	return id, true
}

// presignGet mirrors the feedback handler's presignGet: a presigned GET URL
// for key, with an attachment Content-Disposition when downloadName is set.
func (h *DrChatLabHandler) presignGet(ctx context.Context, key, downloadName string) string {
	if h.s3Presign == nil || key == "" || h.cfg == nil {
		return ""
	}
	ttl := h.cfg.S3ResultPresignTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	in := &s3.GetObjectInput{Bucket: aws.String(h.cfg.S3Bucket), Key: aws.String(key)}
	if downloadName != "" {
		in.ResponseContentDisposition = aws.String(fmt.Sprintf(`attachment; filename="%s"`, downloadName))
	}
	out, err := h.s3Presign.PresignGetObject(ctx, in, func(o *s3.PresignOptions) { o.Expires = ttl })
	if err != nil {
		log.Printf("dr chatlab: presign get %s: %v", key, err)
		return ""
	}
	return out.URL
}

func (h *DrChatLabHandler) deleteObject(ctx context.Context, key string) {
	if h.s3Client == nil || key == "" || h.cfg == nil {
		return
	}
	if _, err := h.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	}); err != nil {
		log.Printf("dr chatlab: best-effort delete %s failed: %v", key, err)
	}
}

// scannedChatSession is the raw column set for a session row.
type scannedChatSession struct {
	id, title, titleSource, createdByEmail string
	lastModel, lastReasoningEffort         *string
	createdAt, updatedAt                   time.Time
}

func (s scannedChatSession) toDTO(callerEmail string) models.DrChatSession {
	return models.DrChatSession{
		ID:                  s.id,
		Title:               s.title,
		TitleSource:         s.titleSource,
		CreatedByEmail:      s.createdByEmail,
		IsMine:              strings.EqualFold(s.createdByEmail, callerEmail),
		LastModel:           s.lastModel,
		LastReasoningEffort: s.lastReasoningEffort,
		CreatedAt:           models.UTCTime{Time: s.createdAt},
		UpdatedAt:           models.UTCTime{Time: s.updatedAt},
	}
}

const chatSessionCols = `id, title, title_source, created_by_email, last_model, last_reasoning_effort, created_at, updated_at`

// loadSession fetches one session row. pgx.ErrNoRows passes through so callers
// can 404.
func (h *DrChatLabHandler) loadSession(ctx context.Context, id string) (scannedChatSession, error) {
	var s scannedChatSession
	err := h.pool.QueryRow(ctx, `SELECT `+chatSessionCols+` FROM dr_chat_sessions WHERE id = $1`, id).
		Scan(&s.id, &s.title, &s.titleSource, &s.createdByEmail, &s.lastModel, &s.lastReasoningEffort, &s.createdAt, &s.updatedAt)
	return s, err
}

// hydrateChatAttachments batch-loads ready, bound attachments for the given
// message ids and presigns view + download URLs, keyed by message id.
func (h *DrChatLabHandler) hydrateChatAttachments(ctx context.Context, messageIDs []string) map[string][]models.DrChatAttachment {
	out := map[string][]models.DrChatAttachment{}
	if len(messageIDs) == 0 {
		return out
	}
	rows, err := h.pool.Query(ctx, `
SELECT id, message_id, kind, file_name, content_type, size_bytes, width, height, s3_key
FROM dr_chat_attachments
WHERE message_id = ANY($1) AND status = 'ready'
ORDER BY created_at, id`, messageIDs)
	if err != nil {
		log.Printf("dr chatlab: hydrate attachments: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var a models.DrChatAttachment
		var messageID, key string
		if err := rows.Scan(&a.ID, &messageID, &a.Kind, &a.FileName, &a.ContentType, &a.SizeBytes, &a.Width, &a.Height, &key); err != nil {
			log.Printf("dr chatlab: scan attachment: %v", err)
			continue
		}
		a.ViewURL = h.presignGet(ctx, key, "")
		a.DownloadURL = h.presignGet(ctx, key, a.FileName)
		out[messageID] = append(out[messageID], a)
	}
	return out
}

// ----------------------------------------------------------------------- //
// GET /chatlab/sessions
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) ListSessions(c *gin.Context) {
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

	rows, err := h.pool.Query(ctx, `
SELECT `+chatSessionCols+`
FROM dr_chat_sessions
ORDER BY updated_at DESC, id
LIMIT $1`, drChatLabSessionsCap)
	if err != nil {
		log.Printf("dr chatlab: list sessions: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load chats"})
		return
	}
	defer rows.Close()

	sessions := make([]models.DrChatSession, 0)
	for rows.Next() {
		var s scannedChatSession
		if err := rows.Scan(&s.id, &s.title, &s.titleSource, &s.createdByEmail, &s.lastModel, &s.lastReasoningEffort, &s.createdAt, &s.updatedAt); err != nil {
			log.Printf("dr chatlab: scan session: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load chats"})
			return
		}
		sessions = append(sessions, s.toDTO(claims.Email))
	}
	if err := rows.Err(); err != nil {
		log.Printf("dr chatlab: list sessions rows: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load chats"})
		return
	}
	c.JSON(http.StatusOK, models.DrChatSessionsResponse{Sessions: sessions})
}

// ----------------------------------------------------------------------- //
// POST /chatlab/sessions
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) CreateSession(c *gin.Context) {
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

	var s scannedChatSession
	err := h.pool.QueryRow(ctx, `
INSERT INTO dr_chat_sessions (created_by_uid, created_by_email)
VALUES ($1, lower($2))
RETURNING `+chatSessionCols, claims.UID, claims.Email).
		Scan(&s.id, &s.title, &s.titleSource, &s.createdByEmail, &s.lastModel, &s.lastReasoningEffort, &s.createdAt, &s.updatedAt)
	if err != nil {
		log.Printf("dr chatlab: create session: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create chat"})
		return
	}
	c.JSON(http.StatusCreated, s.toDTO(claims.Email))
}

// ----------------------------------------------------------------------- //
// GET /chatlab/sessions/:id
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) GetSession(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	sessionID, ok := drChatLabSessionID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	session, err := h.loadSession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Chat not found"})
		return
	}
	if err != nil {
		log.Printf("dr chatlab: load session: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load chat"})
		return
	}

	rows, err := h.pool.Query(ctx, `
SELECT id, role, author_email, content, reasoning, model, reasoning_effort,
       status, error_message, prompt_tokens, completion_tokens, reasoning_tokens,
       total_cost_usd, created_at
FROM dr_chat_messages
WHERE session_id = $1
ORDER BY created_at, seq`, sessionID)
	if err != nil {
		log.Printf("dr chatlab: list messages: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load chat"})
		return
	}
	msgs := make([]models.DrChatMessage, 0)
	func() {
		defer rows.Close()
		for rows.Next() {
			var m models.DrChatMessage
			var createdAt time.Time
			if err := rows.Scan(&m.ID, &m.Role, &m.AuthorEmail, &m.Content, &m.Reasoning, &m.Model, &m.ReasoningEffort,
				&m.Status, &m.ErrorMessage, &m.PromptTokens, &m.CompletionTokens, &m.ReasoningTokens,
				&m.TotalCostUsd, &createdAt); err != nil {
				log.Printf("dr chatlab: scan message: %v", err)
				continue
			}
			m.CreatedAt = models.UTCTime{Time: createdAt}
			m.Attachments = []models.DrChatAttachment{}
			msgs = append(msgs, m)
		}
	}()

	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	attByMsg := h.hydrateChatAttachments(ctx, ids)
	for i := range msgs {
		if atts, ok := attByMsg[msgs[i].ID]; ok {
			msgs[i].Attachments = atts
		}
	}

	c.JSON(http.StatusOK, models.DrChatSessionDetailResponse{
		Session:  session.toDTO(claims.Email),
		Messages: msgs,
	})
}

// ----------------------------------------------------------------------- //
// PUT /chatlab/sessions/:id  (rename, creator-only)
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) RenameSession(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	sessionID, ok := drChatLabSessionID(c)
	if !ok {
		return
	}
	var req models.DrChatRenameSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	title := strings.TrimSpace(req.Title)
	if n := utf8.RuneCountInString(title); n < 1 || n > drChatLabMaxTitleChars {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Title must be 1–120 characters"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	session, err := h.loadSession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Chat not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rename chat"})
		return
	}
	if !strings.EqualFold(session.createdByEmail, claims.Email) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the chat creator can rename it"})
		return
	}

	err = h.pool.QueryRow(ctx, `
UPDATE dr_chat_sessions
SET title = $1, title_source = 'manual'
WHERE id = $2
RETURNING `+chatSessionCols, title, sessionID).
		Scan(&session.id, &session.title, &session.titleSource, &session.createdByEmail, &session.lastModel, &session.lastReasoningEffort, &session.createdAt, &session.updatedAt)
	if err != nil {
		log.Printf("dr chatlab: rename session: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rename chat"})
		return
	}
	c.JSON(http.StatusOK, session.toDTO(claims.Email))
}

// ----------------------------------------------------------------------- //
// DELETE /chatlab/sessions/:id  (hard delete, creator-only)
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) DeleteSession(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	sessionID, ok := drChatLabSessionID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	session, err := h.loadSession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Chat not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete chat"})
		return
	}
	if !strings.EqualFold(session.createdByEmail, claims.Email) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the chat creator can delete it"})
		return
	}

	// Hard delete — CASCADE removes messages/attachments rows.
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_chat_sessions WHERE id = $1`, sessionID); err != nil {
		log.Printf("dr chatlab: delete session: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete chat"})
		return
	}

	// Best-effort S3 prefix cleanup AFTER the DB commit, detached from the
	// request lifetime so a client disconnect can't orphan objects mid-sweep.
	go h.deleteSessionObjects(sessionID)

	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// deleteSessionObjects removes every object under the session's S3 prefix
// (chatlab/{sessionId}/). Failures are logged, never surfaced — this is
// best-effort cleanup of disposable test data.
func (h *DrChatLabHandler) deleteSessionObjects(sessionID string) {
	if h.s3Client == nil || h.cfg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	prefix := fmt.Sprintf("chatlab/%s/", sessionID)
	var token *string
	for {
		out, err := h.s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(h.cfg.S3Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			log.Printf("dr chatlab: list session objects %s: %v", prefix, err)
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
// Attachments — presign / complete / delete (compose-before-send)
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) PresignAttachment(c *gin.Context) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	sessionID, ok := drChatLabSessionID(c)
	if !ok {
		return
	}
	var req models.DrChatPresignAttachmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	ext, maxBytes, okExt := chatLabAttachmentExt(req.Kind, req.ContentType)
	if !okExt {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Unsupported %s type: %s", req.Kind, req.ContentType)})
		return
	}
	if req.SizeBytes <= 0 || req.SizeBytes > maxBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s exceeds the %d MB limit", req.Kind, maxBytes>>20)})
		return
	}
	fileName := sanitizeDrFileName(req.FileName)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if _, err := h.loadSession(ctx, sessionID); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Chat not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}

	attachmentID := uuid.NewString()
	key := fmt.Sprintf("chatlab/%s/attachments/%s.%s", sessionID, attachmentID, ext)
	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_chat_attachments (id, session_id, author_uid, kind, file_name, s3_key, content_type, size_bytes, width, height, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending')`,
		attachmentID, sessionID, claims.UID, req.Kind, fileName, key, req.ContentType, req.SizeBytes, req.Width, req.Height); err != nil {
		log.Printf("dr chatlab: insert attachment: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}

	presignCtx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	defer pcancel()
	out, err := h.s3Presign.PresignPutObject(presignCtx, &s3.PutObjectInput{
		Bucket:      aws.String(h.cfg.S3Bucket),
		Key:         aws.String(key),
		ContentType: aws.String(req.ContentType),
	}, func(o *s3.PresignOptions) { o.Expires = h.cfg.S3PresignTTL })
	if err != nil {
		log.Printf("dr chatlab: presign put: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload URL"})
		return
	}
	c.JSON(http.StatusCreated, models.DrChatPresignResponse{AttachmentID: attachmentID, UploadURL: out.URL, Key: key})
}

func (h *DrChatLabHandler) CompleteAttachment(c *gin.Context) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	sessionID, ok := drChatLabSessionID(c)
	if !ok {
		return
	}
	attachmentID, ok := drChatLabAttachmentID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var (
		aSession, key, contentType, authorUID, status string
		declaredSize                                  int64
	)
	err := h.pool.QueryRow(ctx, `
SELECT session_id, s3_key, content_type, size_bytes, author_uid, status
FROM dr_chat_attachments WHERE id = $1`, attachmentID).
		Scan(&aSession, &key, &contentType, &declaredSize, &authorUID, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && aSession != sessionID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	}
	if err := checkAuthorOnly(authorUID, claims.UID); err != nil {
		abortAuthzError(c, err)
		return
	}
	if status != "pending" {
		c.JSON(http.StatusConflict, gin.H{"error": "Attachment upload already completed"})
		return
	}

	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("dr chatlab: head attachment %s: %v", key, err)
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

	if _, err := h.pool.Exec(ctx, `UPDATE dr_chat_attachments SET status = 'ready' WHERE id = $1`, attachmentID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *DrChatLabHandler) DeleteAttachment(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	sessionID, ok := drChatLabSessionID(c)
	if !ok {
		return
	}
	attachmentID, ok := drChatLabAttachmentID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var aSession, key, authorUID string
	var messageID *string
	err := h.pool.QueryRow(ctx, `SELECT session_id, s3_key, author_uid, message_id FROM dr_chat_attachments WHERE id = $1`, attachmentID).
		Scan(&aSession, &key, &authorUID, &messageID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && aSession != sessionID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete attachment"})
		return
	}
	if err := checkAuthorOnly(authorUID, claims.UID); err != nil {
		abortAuthzError(c, err)
		return
	}
	if messageID != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Attachment is already attached to a message"})
		return
	}
	h.deleteObject(ctx, key)
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_chat_attachments WHERE id = $1`, attachmentID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete attachment"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ----------------------------------------------------------------------- //
// Reaper (daily) — unbound attachments > 24h
// ----------------------------------------------------------------------- //

// ReapUnboundAttachments deletes dr_chat_attachments rows that were never bound
// to a message (message_id IS NULL, any status) and are older than 24h,
// best-effort removing their S3 objects first. Bound attachments are never
// reaped. Called on a daily ticker from cmd/api (exactly mirroring the
// feedback attachment reaper). Safe to run repeatedly; errors are logged, not
// fatal.
func (h *DrChatLabHandler) ReapUnboundAttachments(ctx context.Context) {
	if h.pool == nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	rows, err := h.pool.Query(ctx, `
SELECT id, s3_key FROM dr_chat_attachments
WHERE message_id IS NULL AND created_at < $1`, cutoff)
	if err != nil {
		log.Printf("dr chatlab reaper: select unbound attachments: %v", err)
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
		if _, err := h.pool.Exec(ctx, `DELETE FROM dr_chat_attachments WHERE id = ANY($1)`, ids); err != nil {
			log.Printf("dr chatlab reaper: delete unbound attachments: %v", err)
		} else {
			log.Printf("dr chatlab reaper: removed %d unbound chat attachments", len(ids))
		}
	}
}
