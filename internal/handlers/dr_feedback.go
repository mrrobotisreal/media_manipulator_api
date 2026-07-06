package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// DrFeedbackHandler serves the Double Raven Communication/Feedback workspace
// (Slack-style messaging) on the /api/dr group, so every endpoint — including
// the SSE stream — inherits RequireDoubleRavenAuth. Authorship always comes from
// the verified claims in the gin context (DRContextKey), never from the request
// body. The user directory IS the existing DR_ALLOWED_EMAILS allowlist
// (cfg.DRAllowedEmails, already lowercased) — there is no users table.
//
// Message attachments use Media Manipulator's standard S3 handshake — presign ->
// client PUT -> complete — reusing the shared S3 client + bucket/config and the
// SAME kind/content-type allowlist + per-kind size caps as document assets
// (docAssetExt). The realtime nudge stream (dr_feedback_events.go) is an
// in-memory, single-process broadcaster and a cache-invalidation ACCELERANT
// only: the feature is fully functional with the stream disconnected because the
// client polls over REST as an always-on fallback.
type DrFeedbackHandler struct {
	pool        *pgxpool.Pool
	cfg         *config.Config
	s3Client    *s3.Client
	s3Presign   *s3.PresignClient
	broadcaster *drFeedbackBroadcaster
	// appCtx is the process root context (cancelled on shutdown). The SSE stream
	// selects on it so a graceful shutdown closes long-lived streams promptly
	// instead of holding the shutdown timeout open. nil-safe (see shutdownDone).
	appCtx context.Context
}

// NewDrFeedbackHandler wires the handler. Mirrors NewDrCommentsHandler /
// NewDrDocsHandler (presign client derived from s3Client, nil-safe) and adds the
// in-memory broadcaster + the process root context for shutdown-aware streaming.
func NewDrFeedbackHandler(appCtx context.Context, pool *pgxpool.Pool, cfg *config.Config, s3Client *s3.Client) *DrFeedbackHandler {
	var presign *s3.PresignClient
	if s3Client != nil {
		presign = s3.NewPresignClient(s3Client)
	}
	return &DrFeedbackHandler{
		pool:        pool,
		cfg:         cfg,
		s3Client:    s3Client,
		s3Presign:   presign,
		broadcaster: newDrFeedbackBroadcaster(),
		appCtx:      appCtx,
	}
}

// RegisterDrFeedbackRoutes wires the feedback endpoints onto the already-prefixed
// and already-authed /dr group (see setupRouter), so they resolve to
// /api/dr/feedback/…. The route wildcards live under a distinct /feedback/…
// prefix, so they never collide with the /docs/:slug wildcard the docs/comments
// handlers pin.
func RegisterDrFeedbackRoutes(r gin.IRouter, h *DrFeedbackHandler) {
	r.GET("/feedback/users", h.ListUsers)
	r.GET("/feedback/conversations", h.ListConversations)
	r.POST("/feedback/conversations", h.CreateConversation)
	r.GET("/feedback/conversations/:id/messages", h.ListMessages)
	r.POST("/feedback/conversations/:id/messages", h.SendMessage)
	r.POST("/feedback/conversations/:id/read", h.MarkRead)
	r.POST("/feedback/conversations/:id/attachments", h.PresignAttachment)
	r.POST("/feedback/conversations/:id/attachments/:attachmentId/complete", h.CompleteAttachment)
	r.DELETE("/feedback/conversations/:id/attachments/:attachmentId", h.DeleteAttachment)
	r.GET("/feedback/messages/:id/replies", h.ListReplies)
	r.GET("/feedback/threads", h.ListThreads)
	r.GET("/feedback/events", h.StreamFeedbackEvents)
}

// ----------------------------------------------------------------------- //
// Constants + pure helpers (unit-tested in dr_feedback_test.go)
// ----------------------------------------------------------------------- //

const (
	drFeedbackMaxMessageBytes          int64 = 256 << 10 // 256 KiB raw content
	drFeedbackMaxMessageBlocks               = 200
	drFeedbackMaxAttachmentsPerMessage       = 10
	drFeedbackSnippetChars                   = 140
	drFeedbackMaxTopicChars                  = 250
	drFeedbackDefaultPageLimit               = 50
	drFeedbackMaxPageLimit                   = 100
)

// drFeedbackChannelNamePattern matches a normalized channel name: lowercase
// alphanumerics in hyphen/underscore-separated runs (mirrored client-side).
var drFeedbackChannelNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*$`)

// drMessageBlockTypes is the restricted subset of dr-blocks/v1 a message may
// contain (§3.3): the four text blocks. No headings/tables/callouts/dividers and
// no media blocks (attachments are separate rows).
var drMessageBlockTypes = map[string]bool{
	"paragraph":  true,
	"code":       true,
	"list":       true,
	"blockquote": true,
}

// canonicalDMKey builds the UNIQUE dm_key for a pair of participant emails:
// both lowercased + trimmed, sorted, joined by '|'. Order-independent so
// (a,b) and (b,a) yield the same key.
func canonicalDMKey(a, b string) string {
	x := strings.ToLower(strings.TrimSpace(a))
	y := strings.ToLower(strings.TrimSpace(b))
	if x <= y {
		return x + "|" + y
	}
	return y + "|" + x
}

// normalizeDrChannelName lowercases + trims the requested name and validates it
// against the shape/length rules (2–80 chars, the channel-name pattern),
// returning the normalized name or a client-facing error.
func normalizeDrChannelName(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	n := utf8.RuneCountInString(name)
	if n < 2 || n > 80 {
		return "", errors.New("Channel name must be 2–80 characters")
	}
	if !drFeedbackChannelNamePattern.MatchString(name) {
		return "", errors.New("Channel name may only contain lowercase letters, numbers, hyphens, and underscores")
	}
	return name, nil
}

// validateDrMessageJSON is validateDrBlocksJSON's structural check PLUS the
// message restriction: only the four allowed block types, ≤ 200 blocks, raw
// ≤ 256 KiB. An empty-but-well-formed blocks array ([]) passes here; the
// "message must have text or an attachment" rule is enforced at the handler.
func validateDrMessageJSON(raw []byte) error {
	if len(raw) == 0 {
		return errors.New("content is empty")
	}
	if int64(len(raw)) > drFeedbackMaxMessageBytes {
		return errors.New("message is too large")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return errors.New("content must be a JSON object")
	}
	var format string
	if err := json.Unmarshal(top["format"], &format); err != nil || format != "dr-blocks/v1" {
		return errors.New(`content format must be "dr-blocks/v1"`)
	}
	blocksRaw, ok := top["blocks"]
	if !ok || len(blocksRaw) == 0 || strings.TrimSpace(string(blocksRaw)) == "null" {
		return errors.New("content.blocks must be an array")
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(blocksRaw, &blocks); err != nil {
		return errors.New("content.blocks must be an array")
	}
	if len(blocks) > drFeedbackMaxMessageBlocks {
		return fmt.Errorf("message has too many blocks (max %d)", drFeedbackMaxMessageBlocks)
	}
	for i, b := range blocks {
		var bm map[string]json.RawMessage
		if err := json.Unmarshal(b, &bm); err != nil {
			return fmt.Errorf("block %d must be an object", i)
		}
		var t string
		if err := json.Unmarshal(bm["type"], &t); err != nil {
			return fmt.Errorf("block %d is missing a string type", i)
		}
		if !drMessageBlockTypes[t] {
			return fmt.Errorf("block %d has unsupported type %q", i, t)
		}
	}
	return nil
}

// drMessageSpan / drMessageBlockShape / drMessageContentShape are the minimal
// typed views used by messageSnippet + the empty-message check. They cover the
// four allowed block types' text-bearing fields.
type drMessageSpan struct {
	Text string `json:"text"`
}

type drMessageBlockShape struct {
	Type  string            `json:"type"`
	Spans []drMessageSpan   `json:"spans"` // paragraph
	Lines [][]drMessageSpan `json:"lines"` // blockquote
	Items [][]drMessageSpan `json:"items"` // list
	Code  string            `json:"code"`  // code
}

type drMessageContentShape struct {
	Format string                `json:"format"`
	Blocks []drMessageBlockShape `json:"blocks"`
}

// messageSnippet concatenates the plain-text of a message's blocks (spans across
// paragraphs, blockquote lines, list items, and code text), collapses
// whitespace, and truncates to drFeedbackSnippetChars runes. Factored alongside
// deriveDrSummary's logic but message-aware (it walks all four block kinds).
func messageSnippet(raw []byte) string {
	var c drMessageContentShape
	if err := json.Unmarshal(raw, &c); err != nil {
		return ""
	}
	var sb strings.Builder
	writeSpans := func(spans []drMessageSpan) {
		for _, s := range spans {
			sb.WriteString(s.Text)
		}
		sb.WriteByte(' ')
	}
	for _, b := range c.Blocks {
		switch b.Type {
		case "paragraph":
			writeSpans(b.Spans)
		case "blockquote":
			for _, line := range b.Lines {
				writeSpans(line)
			}
		case "list":
			for _, item := range b.Items {
				writeSpans(item)
			}
		case "code":
			sb.WriteString(b.Code)
			sb.WriteByte(' ')
		}
		// Stop early once we clearly have enough text for the snippet.
		if sb.Len() > drFeedbackSnippetChars*4 {
			break
		}
	}
	text := strings.Join(strings.Fields(sb.String()), " ")
	if utf8.RuneCountInString(text) > drFeedbackSnippetChars {
		return strings.TrimSpace(string([]rune(text)[:drFeedbackSnippetChars]))
	}
	return text
}

// parseFeedbackLimit clamps the ?limit query to [1, drFeedbackMaxPageLimit],
// defaulting to drFeedbackDefaultPageLimit.
func parseFeedbackLimit(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return drFeedbackDefaultPageLimit
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return drFeedbackDefaultPageLimit
	}
	if n > drFeedbackMaxPageLimit {
		return drFeedbackMaxPageLimit
	}
	return n
}

// isUniqueViolation reports whether err is a Postgres unique-constraint (23505)
// violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ----------------------------------------------------------------------- //
// Shared handler plumbing
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) dbReady(c *gin.Context) bool {
	if h.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Feedback storage is unavailable"})
		return false
	}
	return true
}

func (h *DrFeedbackHandler) s3Ready(c *gin.Context) bool {
	if h.s3Client == nil || h.s3Presign == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Attachment storage is unavailable"})
		return false
	}
	return true
}

// presignGet mirrors the docs/comments handlers' presignGet: a presigned GET URL
// for key, with an attachment Content-Disposition when downloadName is set.
func (h *DrFeedbackHandler) presignGet(ctx context.Context, key, downloadName string) string {
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
		log.Printf("dr feedback: presign get %s: %v", key, err)
		return ""
	}
	return out.URL
}

func (h *DrFeedbackHandler) deleteObject(ctx context.Context, key string) {
	if h.s3Client == nil || key == "" || h.cfg == nil {
		return
	}
	if _, err := h.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	}); err != nil {
		log.Printf("dr feedback: best-effort delete %s failed: %v", key, err)
	}
}

// conversationAccess resolves whether the caller may see a conversation.
// Channels are visible to any DR user; DMs only to a participant (case-insensitive
// email). ok=false covers BOTH a nonexistent conversation and a DM the caller is
// not in — so every caller-facing endpoint returns 404 (never 403) and no DM's
// existence leaks. A non-nil err is a real DB failure (→ 500 at the call site).
func (h *DrFeedbackHandler) conversationAccess(ctx context.Context, convoID, callerEmail string) (kind string, ok bool, err error) {
	var k string
	err = h.pool.QueryRow(ctx, `SELECT kind FROM dr_conversations WHERE id = $1`, convoID).Scan(&k)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if k == "channel" {
		return k, true, nil
	}
	var member bool
	err = h.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM dr_conversation_participants WHERE conversation_id = $1 AND lower(email) = lower($2))`,
		convoID, callerEmail).Scan(&member)
	if err != nil {
		return "", false, err
	}
	return k, member, nil
}

// isAllowlisted reports whether email is on the DR allowlist (lowercased at
// config load, so we only lowercase the argument).
func (h *DrFeedbackHandler) isAllowlisted(email string) bool {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return false
	}
	for _, a := range h.cfg.DRAllowedEmails {
		if a == e {
			return true
		}
	}
	return false
}

// hydrateAttachments batch-loads uploaded attachments for the given message ids
// and presigns view (inline) + download (attachment) URLs, keyed by message id
// (mirrors loadAssetsByID + presignGet).
func (h *DrFeedbackHandler) hydrateAttachments(ctx context.Context, messageIDs []string) map[string][]models.DrMessageAttachment {
	out := map[string][]models.DrMessageAttachment{}
	if len(messageIDs) == 0 {
		return out
	}
	rows, err := h.pool.Query(ctx, `
SELECT id, message_id, kind, file_name, content_type, size_bytes, width, height, s3_key
FROM dr_message_attachments
WHERE message_id = ANY($1) AND status = 'uploaded'
ORDER BY created_at, id`, messageIDs)
	if err != nil {
		log.Printf("dr feedback: hydrate attachments: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var a models.DrMessageAttachment
		var messageID, key string
		if err := rows.Scan(&a.ID, &messageID, &a.Kind, &a.FileName, &a.ContentType, &a.SizeBytes, &a.Width, &a.Height, &key); err != nil {
			log.Printf("dr feedback: scan attachment: %v", err)
			continue
		}
		a.ViewURL = h.presignGet(ctx, key, "")
		a.DownloadURL = h.presignGet(ctx, key, a.FileName)
		out[messageID] = append(out[messageID], a)
	}
	return out
}

// replyAggregates returns per-parent reply counts + latest reply time for the
// given top-level message ids, in ONE grouped query (no N+1).
func (h *DrFeedbackHandler) replyAggregates(ctx context.Context, parentIDs []string) (counts map[string]int, last map[string]time.Time) {
	counts = map[string]int{}
	last = map[string]time.Time{}
	if len(parentIDs) == 0 {
		return counts, last
	}
	rows, err := h.pool.Query(ctx,
		`SELECT parent_id, count(*), max(created_at) FROM dr_messages WHERE parent_id = ANY($1) GROUP BY parent_id`, parentIDs)
	if err != nil {
		log.Printf("dr feedback: reply aggregates: %v", err)
		return counts, last
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		var cnt int
		var mx time.Time
		if err := rows.Scan(&pid, &cnt, &mx); err != nil {
			continue
		}
		counts[pid] = cnt
		last[pid] = mx
	}
	return counts, last
}

// scannedMessage is the raw column set for a message row.
type scannedMessage struct {
	id, conversationID, authorUID, authorEmail string
	parentID                                   *string
	content                                    []byte
	createdAt                                  time.Time
}

// toDTO builds a hydrated message DTO from a scanned row + its attachments +
// reply aggregates.
func (s scannedMessage) toDTO(callerUID string, atts []models.DrMessageAttachment, replyCount int, lastReplyAt *time.Time) models.DrMessage {
	if atts == nil {
		atts = []models.DrMessageAttachment{}
	}
	m := models.DrMessage{
		ID:             s.id,
		ConversationID: s.conversationID,
		ParentID:       s.parentID,
		AuthorUID:      s.authorUID,
		AuthorEmail:    s.authorEmail,
		IsMine:         s.authorUID == callerUID,
		Content:        json.RawMessage(s.content),
		CreatedAt:      models.UTCTime{Time: s.createdAt},
		Attachments:    atts,
		ReplyCount:     replyCount,
	}
	if lastReplyAt != nil {
		u := models.UTCTime{Time: *lastReplyAt}
		m.LastReplyAt = &u
	}
	return m
}

func drFeedbackConvoID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation id"})
		return "", false
	}
	return id, true
}

func drFeedbackMessageID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message id"})
		return "", false
	}
	return id, true
}

func drFeedbackAttachmentID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("attachmentId"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid attachment id"})
		return "", false
	}
	return id, true
}

// ----------------------------------------------------------------------- //
// GET /feedback/users
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) ListUsers(c *gin.Context) {
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	users := make([]models.DrFeedbackUser, 0, len(h.cfg.DRAllowedEmails))
	for _, email := range h.cfg.DRAllowedEmails {
		users = append(users, models.DrFeedbackUser{Email: email, IsMe: strings.EqualFold(email, claims.Email)})
	}
	c.JSON(http.StatusOK, models.DrFeedbackUsersResponse{Users: users})
}

// ----------------------------------------------------------------------- //
// GET /feedback/conversations  (sidebar payload, one round trip)
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) ListConversations(c *gin.Context) {
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

	// One query: every channel + the caller's DMs, each with its DM partner,
	// latest message (any message, incl. replies), and unread count (messages
	// created after the caller's last_read by someone else).
	rows, err := h.pool.Query(ctx, `
SELECT
  c.id, c.kind, c.name, c.topic, c.created_at,
  (SELECT p2.email FROM dr_conversation_participants p2
     WHERE p2.conversation_id = c.id AND lower(p2.email) <> lower($1) LIMIT 1) AS dm_partner,
  lm.last_at, lm.last_content,
  COALESCE(u.unread, 0) AS unread
FROM dr_conversations c
LEFT JOIN LATERAL (
  SELECT m.created_at AS last_at, m.content AS last_content
  FROM dr_messages m WHERE m.conversation_id = c.id
  ORDER BY m.created_at DESC, m.id DESC LIMIT 1
) lm ON true
LEFT JOIN dr_conversation_reads r ON r.conversation_id = c.id AND r.user_uid = $2
LEFT JOIN LATERAL (
  SELECT count(*) AS unread FROM dr_messages m2
  WHERE m2.conversation_id = c.id
    AND m2.author_uid <> $2
    AND m2.created_at > COALESCE(r.last_read_at, '-infinity'::timestamptz)
) u ON true
WHERE c.kind = 'channel'
   OR EXISTS (SELECT 1 FROM dr_conversation_participants p
              WHERE p.conversation_id = c.id AND lower(p.email) = lower($1))`,
		claims.Email, claims.UID)
	if err != nil {
		log.Printf("dr feedback: list conversations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load conversations"})
		return
	}
	defer rows.Close()

	// activity is a DM's last-activity sort key: the latest message time, else the
	// conversation's own created_at (a brand-new empty DM sorts by when it was made).
	type convoRow struct {
		summary  models.DrConversationSummary
		activity time.Time
	}
	rowsData := make([]convoRow, 0)
	for rows.Next() {
		var (
			d         models.DrConversationSummary
			createdAt time.Time
			lastAt    *time.Time
			lastRaw   []byte
		)
		if err := rows.Scan(&d.ID, &d.Kind, &d.Name, &d.Topic, &createdAt, &d.DMPartnerEmail, &lastAt, &lastRaw, &d.UnreadCount); err != nil {
			log.Printf("dr feedback: scan conversation: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load conversations"})
			return
		}
		activity := createdAt
		if lastAt != nil {
			u := models.UTCTime{Time: *lastAt}
			d.LastMessageAt = &u
			d.LastMessageSnippet = messageSnippet(lastRaw)
			activity = *lastAt
		}
		rowsData = append(rowsData, convoRow{summary: d, activity: activity})
	}
	if err := rows.Err(); err != nil {
		log.Printf("dr feedback: list conversations rows: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load conversations"})
		return
	}

	// Channels first (alphabetical), then DMs (most recent activity first).
	sort.SliceStable(rowsData, func(i, j int) bool {
		a, b := rowsData[i], rowsData[j]
		if a.summary.Kind != b.summary.Kind {
			return a.summary.Kind == "channel"
		}
		if a.summary.Kind == "channel" {
			return derefStr(a.summary.Name) < derefStr(b.summary.Name)
		}
		return a.activity.After(b.activity)
	})

	convos := make([]models.DrConversationSummary, 0, len(rowsData))
	for _, r := range rowsData {
		convos = append(convos, r.summary)
	}

	c.JSON(http.StatusOK, models.DrConversationsResponse{Conversations: convos})
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ----------------------------------------------------------------------- //
// POST /feedback/conversations  (create channel or DM)
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) CreateConversation(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	var req models.DrCreateConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	switch req.Kind {
	case "channel":
		h.createChannel(c, ctx, claims.Email, req)
	case "dm":
		h.createDM(c, ctx, claims.Email, req)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown conversation kind"})
	}
}

func (h *DrFeedbackHandler) createChannel(c *gin.Context, ctx context.Context, callerEmail string, req models.DrCreateConversationRequest) {
	name, err := normalizeDrChannelName(req.Name)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	topic := strings.TrimSpace(req.Topic)
	if utf8.RuneCountInString(topic) > drFeedbackMaxTopicChars {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Topic is too long"})
		return
	}
	var topicPtr *string
	if topic != "" {
		topicPtr = &topic
	}

	var id string
	var createdAt time.Time
	err = h.pool.QueryRow(ctx, `
INSERT INTO dr_conversations (kind, name, topic, created_by)
VALUES ('channel', $1, $2, $3)
RETURNING id, created_at`, name, topicPtr, callerEmail).Scan(&id, &createdAt)
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "A channel with that name already exists"})
		return
	}
	if err != nil {
		log.Printf("dr feedback: create channel: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create channel"})
		return
	}

	h.broadcaster.Broadcast(drFeedbackEvent{Type: "conversation"})
	nameCopy := name
	c.JSON(http.StatusCreated, models.DrConversationSummary{
		ID:          id,
		Kind:        "channel",
		Name:        &nameCopy,
		Topic:       topicPtr,
		UnreadCount: 0,
	})
}

func (h *DrFeedbackHandler) createDM(c *gin.Context, ctx context.Context, callerEmail string, req models.DrCreateConversationRequest) {
	partner := strings.ToLower(strings.TrimSpace(req.ParticipantEmail))
	if partner == "" || !h.isAllowlisted(partner) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "That user isn't a member of this portal"})
		return
	}
	if strings.EqualFold(partner, callerEmail) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "You can't start a direct message with yourself"})
		return
	}
	dmKey := canonicalDMKey(callerEmail, partner)

	// Idempotent "create": an existing DM with this key is returned with 200.
	if existing, ok := h.findDMByKey(ctx, dmKey, partner); ok {
		c.JSON(http.StatusOK, existing)
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create direct message"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
INSERT INTO dr_conversations (kind, dm_key, created_by)
VALUES ('dm', $1, $2)
RETURNING id, created_at`, dmKey, callerEmail).Scan(&id, &createdAt)
	if isUniqueViolation(err) {
		// Raced with a concurrent create of the same pair — return the existing.
		if existing, ok := h.findDMByKey(ctx, dmKey, partner); ok {
			c.JSON(http.StatusOK, existing)
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": "That direct message already exists"})
		return
	}
	if err != nil {
		log.Printf("dr feedback: create dm: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create direct message"})
		return
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO dr_conversation_participants (conversation_id, email)
VALUES ($1, $2), ($1, $3)`, id, strings.ToLower(strings.TrimSpace(callerEmail)), partner); err != nil {
		log.Printf("dr feedback: insert dm participants: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create direct message"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr feedback: commit dm: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create direct message"})
		return
	}

	h.broadcaster.Broadcast(drFeedbackEvent{Type: "conversation"})
	partnerCopy := partner
	c.JSON(http.StatusCreated, models.DrConversationSummary{
		ID:             id,
		Kind:           "dm",
		DMPartnerEmail: &partnerCopy,
		UnreadCount:    0,
	})
}

// findDMByKey returns a summary for an existing DM with dmKey (partner is the
// caller's counterpart), or ok=false when none exists.
func (h *DrFeedbackHandler) findDMByKey(ctx context.Context, dmKey, partner string) (models.DrConversationSummary, bool) {
	var id string
	err := h.pool.QueryRow(ctx, `SELECT id FROM dr_conversations WHERE dm_key = $1`, dmKey).Scan(&id)
	if err != nil {
		return models.DrConversationSummary{}, false
	}
	partnerCopy := partner
	return models.DrConversationSummary{
		ID:             id,
		Kind:           "dm",
		DMPartnerEmail: &partnerCopy,
		UnreadCount:    0,
	}, true
}

// ----------------------------------------------------------------------- //
// GET /feedback/conversations/:id/messages  (top-level, keyset paginated)
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) ListMessages(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	convoID, ok := drFeedbackConvoID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if _, access, err := h.conversationAccess(ctx, convoID, claims.Email); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load messages"})
		return
	} else if !access {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conversation not found"})
		return
	}

	limit := parseFeedbackLimit(c.Query("limit"))
	beforeID := strings.TrimSpace(c.Query("before"))
	var beforeCreatedAt time.Time
	if beforeID != "" {
		if _, err := uuid.Parse(beforeID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pagination cursor"})
			return
		}
		err := h.pool.QueryRow(ctx,
			`SELECT created_at FROM dr_messages WHERE id = $1 AND conversation_id = $2 AND parent_id IS NULL`,
			beforeID, convoID).Scan(&beforeCreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pagination cursor"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load messages"})
			return
		}
	}

	args := []any{convoID}
	where := "conversation_id = $1 AND parent_id IS NULL"
	if beforeID != "" {
		// Explicit casts so Postgres never has to infer a parameter's type inside
		// the row comparison.
		args = append(args, beforeCreatedAt, beforeID)
		where += " AND (created_at, id) < ($2::timestamptz, $3::uuid)"
	}
	query := fmt.Sprintf(`
SELECT id, conversation_id, parent_id, author_uid, author_email, content, created_at
FROM dr_messages
WHERE %s
ORDER BY created_at DESC, id DESC
LIMIT %d`, where, limit+1)

	rows, err := h.pool.Query(ctx, query, args...)
	if err != nil {
		log.Printf("dr feedback: list messages: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load messages"})
		return
	}
	scanned := make([]scannedMessage, 0, limit+1)
	func() {
		defer rows.Close()
		for rows.Next() {
			var s scannedMessage
			if err := rows.Scan(&s.id, &s.conversationID, &s.parentID, &s.authorUID, &s.authorEmail, &s.content, &s.createdAt); err != nil {
				log.Printf("dr feedback: scan message: %v", err)
				continue
			}
			scanned = append(scanned, s)
		}
	}()

	hasMore := len(scanned) > limit
	if hasMore {
		scanned = scanned[:limit]
	}

	ids := make([]string, 0, len(scanned))
	for _, s := range scanned {
		ids = append(ids, s.id)
	}
	attByMsg := h.hydrateAttachments(ctx, ids)
	counts, lasts := h.replyAggregates(ctx, ids)

	// Reverse to oldest→newest within the page.
	msgs := make([]models.DrMessage, 0, len(scanned))
	for i := len(scanned) - 1; i >= 0; i-- {
		s := scanned[i]
		var lastPtr *time.Time
		if t, ok := lasts[s.id]; ok {
			lastPtr = &t
		}
		msgs = append(msgs, s.toDTO(claims.UID, attByMsg[s.id], counts[s.id], lastPtr))
	}

	c.JSON(http.StatusOK, models.DrMessagesPage{Messages: msgs, HasMore: hasMore})
}

// ----------------------------------------------------------------------- //
// POST /feedback/conversations/:id/messages  (send; binds attachments)
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) SendMessage(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	convoID, ok := drFeedbackConvoID(c)
	if !ok {
		return
	}
	var req models.DrSendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if _, access, err := h.conversationAccess(ctx, convoID, claims.Email); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	} else if !access {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conversation not found"})
		return
	}

	if err := validateDrMessageJSON(req.Content); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	parsed, _ := parseDrContent(req.Content)
	if len(parsed.Blocks) == 0 && len(req.AttachmentIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Message is empty"})
		return
	}
	if len(req.AttachmentIDs) > drFeedbackMaxAttachmentsPerMessage {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("At most %d attachments are allowed", drFeedbackMaxAttachmentsPerMessage)})
		return
	}

	// A reply's parent must exist, be in this conversation, and be top-level.
	var parentID *string
	if req.ParentID != nil && strings.TrimSpace(*req.ParentID) != "" {
		pid := strings.TrimSpace(*req.ParentID)
		if _, err := uuid.Parse(pid); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Replies can only target a top-level message"})
			return
		}
		var pConvo string
		var pParent *string
		err := h.pool.QueryRow(ctx, `SELECT conversation_id, parent_id FROM dr_messages WHERE id = $1`, pid).Scan(&pConvo, &pParent)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (pConvo != convoID || pParent != nil)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Replies can only target a top-level message"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
			return
		}
		parentID = &pid
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var msgID string
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
INSERT INTO dr_messages (conversation_id, parent_id, author_uid, author_email, content_format, content)
VALUES ($1, $2, $3, $4, 'dr-blocks/v1', $5)
RETURNING id, created_at`, convoID, parentID, claims.UID, claims.Email, req.Content).Scan(&msgID, &createdAt)
	if err != nil {
		log.Printf("dr feedback: insert message: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	// Bind each attachment: must be uploaded, unbound, same conversation, same
	// author. A zero-row update aborts the whole send.
	for _, rawID := range req.AttachmentIDs {
		aid := strings.TrimSpace(rawID)
		if _, err := uuid.Parse(aid); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Attachment is not ready"})
			return
		}
		tag, err := tx.Exec(ctx, `
UPDATE dr_message_attachments
SET message_id = $1
WHERE id = $2 AND conversation_id = $3 AND author_uid = $4 AND status = 'uploaded' AND message_id IS NULL`,
			msgID, aid, convoID, claims.UID)
		if err != nil {
			log.Printf("dr feedback: bind attachment: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
			return
		}
		if tag.RowsAffected() == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Attachment is not ready"})
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr feedback: commit message: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	// Broadcast the nudge only AFTER the successful commit.
	h.broadcaster.Broadcast(drFeedbackEvent{Type: "message", ConversationID: convoID, ParentID: parentID})

	atts := h.hydrateAttachments(ctx, []string{msgID})[msgID]
	dto := scannedMessage{
		id:             msgID,
		conversationID: convoID,
		parentID:       parentID,
		authorUID:      claims.UID,
		authorEmail:    claims.Email,
		content:        req.Content,
		createdAt:      createdAt,
	}.toDTO(claims.UID, atts, 0, nil)
	c.JSON(http.StatusCreated, dto)
}

// ----------------------------------------------------------------------- //
// GET /feedback/messages/:id/replies  (a thread)
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) ListReplies(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	msgID, ok := drFeedbackMessageID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var parent scannedMessage
	err := h.pool.QueryRow(ctx, `
SELECT id, conversation_id, parent_id, author_uid, author_email, content, created_at
FROM dr_messages WHERE id = $1`, msgID).
		Scan(&parent.id, &parent.conversationID, &parent.parentID, &parent.authorUID, &parent.authorEmail, &parent.content, &parent.createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Message not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load replies"})
		return
	}
	if parent.parentID != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Replies can only target a top-level message"})
		return
	}
	if _, access, aerr := h.conversationAccess(ctx, parent.conversationID, claims.Email); aerr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load replies"})
		return
	} else if !access {
		c.JSON(http.StatusNotFound, gin.H{"error": "Message not found"})
		return
	}

	rrows, err := h.pool.Query(ctx, `
SELECT id, conversation_id, parent_id, author_uid, author_email, content, created_at
FROM dr_messages WHERE parent_id = $1 ORDER BY created_at, id`, msgID)
	if err != nil {
		log.Printf("dr feedback: list replies: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load replies"})
		return
	}
	replies := make([]scannedMessage, 0)
	func() {
		defer rrows.Close()
		for rrows.Next() {
			var s scannedMessage
			if err := rrows.Scan(&s.id, &s.conversationID, &s.parentID, &s.authorUID, &s.authorEmail, &s.content, &s.createdAt); err != nil {
				continue
			}
			replies = append(replies, s)
		}
	}()

	ids := make([]string, 0, len(replies)+1)
	ids = append(ids, parent.id)
	for _, r := range replies {
		ids = append(ids, r.id)
	}
	attByMsg := h.hydrateAttachments(ctx, ids)
	counts, lasts := h.replyAggregates(ctx, []string{parent.id})

	var parentLast *time.Time
	if t, ok := lasts[parent.id]; ok {
		parentLast = &t
	}
	parentDTO := parent.toDTO(claims.UID, attByMsg[parent.id], counts[parent.id], parentLast)

	replyDTOs := make([]models.DrMessage, 0, len(replies))
	for _, r := range replies {
		replyDTOs = append(replyDTOs, r.toDTO(claims.UID, attByMsg[r.id], 0, nil))
	}

	c.JSON(http.StatusOK, models.DrRepliesResponse{Parent: parentDTO, Replies: replyDTOs})
}

// ----------------------------------------------------------------------- //
// GET /feedback/threads  (all threads visible to the caller, newest activity)
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) ListThreads(c *gin.Context) {
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

	limit := parseFeedbackLimit(c.Query("limit"))
	beforeID := strings.TrimSpace(c.Query("before"))
	var cursorLast time.Time
	if beforeID != "" {
		if _, err := uuid.Parse(beforeID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pagination cursor"})
			return
		}
		var last *time.Time
		err := h.pool.QueryRow(ctx, `
SELECT (SELECT max(created_at) FROM dr_messages r WHERE r.parent_id = m.id)
FROM dr_messages m JOIN dr_conversations c ON c.id = m.conversation_id
WHERE m.id = $1 AND m.parent_id IS NULL
  AND (c.kind = 'channel' OR EXISTS (SELECT 1 FROM dr_conversation_participants p
       WHERE p.conversation_id = c.id AND lower(p.email) = lower($2)))`, beforeID, claims.Email).Scan(&last)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && last == nil) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pagination cursor"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load threads"})
			return
		}
		cursorLast = *last
	}

	args := []any{claims.Email}
	where := `m.parent_id IS NULL
  AND (c.kind = 'channel' OR EXISTS (SELECT 1 FROM dr_conversation_participants p
       WHERE p.conversation_id = c.id AND lower(p.email) = lower($1)))`
	if beforeID != "" {
		args = append(args, cursorLast, beforeID)
		where += " AND (agg.last_reply_at, m.id) < ($2::timestamptz, $3::uuid)"
	}
	query := fmt.Sprintf(`
SELECT m.id, m.conversation_id, m.parent_id, m.author_uid, m.author_email, m.content, m.created_at,
       c.kind, c.name,
       (SELECT p2.email FROM dr_conversation_participants p2
          WHERE p2.conversation_id = c.id AND lower(p2.email) <> lower($1) LIMIT 1) AS dm_partner,
       agg.reply_count, agg.last_reply_at, agg.last_reply_content
FROM dr_messages m
JOIN dr_conversations c ON c.id = m.conversation_id
JOIN LATERAL (
  SELECT count(*) AS reply_count, max(r.created_at) AS last_reply_at,
         (SELECT rr.content FROM dr_messages rr WHERE rr.parent_id = m.id ORDER BY rr.created_at DESC, rr.id DESC LIMIT 1) AS last_reply_content
  FROM dr_messages r WHERE r.parent_id = m.id
) agg ON agg.reply_count > 0
WHERE %s
ORDER BY agg.last_reply_at DESC, m.id DESC
LIMIT %d`, where, limit+1)

	rows, err := h.pool.Query(ctx, query, args...)
	if err != nil {
		log.Printf("dr feedback: list threads: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load threads"})
		return
	}

	type threadRow struct {
		msg              scannedMessage
		kind             string
		name             *string
		dmPartner        *string
		replyCount       int
		lastReplyAt      time.Time
		lastReplyContent []byte
	}
	var scanned []threadRow
	func() {
		defer rows.Close()
		for rows.Next() {
			var t threadRow
			if err := rows.Scan(&t.msg.id, &t.msg.conversationID, &t.msg.parentID, &t.msg.authorUID, &t.msg.authorEmail, &t.msg.content, &t.msg.createdAt,
				&t.kind, &t.name, &t.dmPartner, &t.replyCount, &t.lastReplyAt, &t.lastReplyContent); err != nil {
				log.Printf("dr feedback: scan thread: %v", err)
				continue
			}
			scanned = append(scanned, t)
		}
	}()

	hasMore := len(scanned) > limit
	if hasMore {
		scanned = scanned[:limit]
	}

	ids := make([]string, 0, len(scanned))
	for _, t := range scanned {
		ids = append(ids, t.msg.id)
	}
	attByMsg := h.hydrateAttachments(ctx, ids)

	threads := make([]models.DrThreadListItem, 0, len(scanned))
	for _, t := range scanned {
		lastCopy := t.lastReplyAt
		item := models.DrThreadListItem{
			Message:          t.msg.toDTO(claims.UID, attByMsg[t.msg.id], t.replyCount, &lastCopy),
			ConversationID:   t.msg.conversationID,
			ConversationKind: t.kind,
			ConversationName: t.name,
			DMPartnerEmail:   t.dmPartner,
			LastReplyAt:      models.UTCTime{Time: t.lastReplyAt},
			ReplyCount:       t.replyCount,
			LastReplySnippet: messageSnippet(t.lastReplyContent),
		}
		threads = append(threads, item)
	}

	c.JSON(http.StatusOK, models.DrThreadsPage{Threads: threads, HasMore: hasMore})
}

// ----------------------------------------------------------------------- //
// POST /feedback/conversations/:id/read  (mark read)
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) MarkRead(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	convoID, ok := drFeedbackConvoID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if _, access, err := h.conversationAccess(ctx, convoID, claims.Email); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark read"})
		return
	} else if !access {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conversation not found"})
		return
	}

	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_conversation_reads (conversation_id, user_uid, last_read_at)
VALUES ($1, $2, now())
ON CONFLICT (conversation_id, user_uid) DO UPDATE SET last_read_at = now()`, convoID, claims.UID); err != nil {
		log.Printf("dr feedback: mark read: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark read"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ----------------------------------------------------------------------- //
// Attachments — presign / complete / delete (compose-before-send)
// ----------------------------------------------------------------------- //

func (h *DrFeedbackHandler) PresignAttachment(c *gin.Context) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	convoID, ok := drFeedbackConvoID(c)
	if !ok {
		return
	}
	var req models.DrFeedbackPresignAttachmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	ext, maxBytes, okExt := docAssetExt(req.Kind, req.ContentType)
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

	if _, access, err := h.conversationAccess(ctx, convoID, claims.Email); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	} else if !access {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conversation not found"})
		return
	}

	attachmentID := uuid.NewString()
	key := fmt.Sprintf("feedback/%s/attachments/%s.%s", convoID, attachmentID, ext)
	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_message_attachments (id, conversation_id, author_uid, kind, file_name, s3_key, content_type, size_bytes, width, height, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending')`,
		attachmentID, convoID, claims.UID, req.Kind, fileName, key, req.ContentType, req.SizeBytes, req.Width, req.Height); err != nil {
		log.Printf("dr feedback: insert attachment: %v", err)
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
		log.Printf("dr feedback: presign put: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload URL"})
		return
	}
	c.JSON(http.StatusCreated, models.DrFeedbackPresignResponse{AttachmentID: attachmentID, UploadURL: out.URL})
}

func (h *DrFeedbackHandler) CompleteAttachment(c *gin.Context) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	convoID, ok := drFeedbackConvoID(c)
	if !ok {
		return
	}
	attachmentID, ok := drFeedbackAttachmentID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var (
		aConvo, key, contentType, authorUID, status string
		declaredSize                                int64
	)
	err := h.pool.QueryRow(ctx, `
SELECT conversation_id, s3_key, content_type, size_bytes, author_uid, status
FROM dr_message_attachments WHERE id = $1`, attachmentID).
		Scan(&aConvo, &key, &contentType, &declaredSize, &authorUID, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && aConvo != convoID) {
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
		log.Printf("dr feedback: head attachment %s: %v", key, err)
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

	if _, err := h.pool.Exec(ctx, `UPDATE dr_message_attachments SET status = 'uploaded' WHERE id = $1`, attachmentID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *DrFeedbackHandler) DeleteAttachment(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	convoID, ok := drFeedbackConvoID(c)
	if !ok {
		return
	}
	attachmentID, ok := drFeedbackAttachmentID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var aConvo, key, authorUID string
	var messageID *string
	err := h.pool.QueryRow(ctx, `SELECT conversation_id, s3_key, author_uid, message_id FROM dr_message_attachments WHERE id = $1`, attachmentID).
		Scan(&aConvo, &key, &authorUID, &messageID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && aConvo != convoID) {
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
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_message_attachments WHERE id = $1`, attachmentID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete attachment"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ----------------------------------------------------------------------- //
// Reaper (daily) — unbound attachments > 24h
// ----------------------------------------------------------------------- //

// ReapUnboundAttachments deletes dr_message_attachments rows that were never
// bound to a message (message_id IS NULL) and are older than 24h (any status),
// best-effort removing their S3 objects first. Bound attachments are never
// reaped. Called on a daily ticker from cmd/api. Safe to run repeatedly; errors
// are logged, not fatal.
func (h *DrFeedbackHandler) ReapUnboundAttachments(ctx context.Context) {
	if h.pool == nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	rows, err := h.pool.Query(ctx, `
SELECT id, s3_key FROM dr_message_attachments
WHERE message_id IS NULL AND created_at < $1`, cutoff)
	if err != nil {
		log.Printf("dr feedback reaper: select unbound attachments: %v", err)
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
		if _, err := h.pool.Exec(ctx, `DELETE FROM dr_message_attachments WHERE id = ANY($1)`, ids); err != nil {
			log.Printf("dr feedback reaper: delete unbound attachments: %v", err)
		} else {
			log.Printf("dr feedback reaper: removed %d unbound message attachments", len(ids))
		}
	}
}
