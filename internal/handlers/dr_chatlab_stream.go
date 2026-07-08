package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// POST /api/dr/chatlab/sessions/:id/messages — the streaming send.
//
// The response is an SSE-formatted body on a POST (Content-Type:
// text/event-stream); the client consumes it via fetch + ReadableStream with a
// Bearer header (EventSource cannot send Authorization, and the DR auth model
// never changes), mirroring lib/dr/useFeedbackEvents.ts.
//
// Sequence: validate → persist the USER message in one transaction (committed
// BEFORE calling upstream, so it is never lost to an upstream failure) →
// assemble the full conversation for OpenRouter → stream, forwarding deltas
// downstream as our own SSE events → persist the assistant message on any
// terminal condition (complete / interrupted / error) using a background
// context so a client disconnect can't cancel the write.

// ---- Downstream SSE events (exact wire shapes; unit-tested) -----------------

type chatLabMetaEvent struct {
	Type               string `json:"type"` // "meta"
	UserMessageID      string `json:"userMessageId"`
	AssistantMessageID string `json:"assistantMessageId"`
}

type chatLabReasoningEvent struct {
	Type string `json:"type"` // "reasoning"
	Text string `json:"text"`
}

type chatLabDeltaEvent struct {
	Type string `json:"type"` // "delta"
	Text string `json:"text"`
}

type chatLabUsageEvent struct {
	Type             string  `json:"type"` // "usage"
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	ReasoningTokens  int     `json:"reasoningTokens"`
	CostUsd          float64 `json:"costUsd"`
}

type chatLabDoneEvent struct {
	Type   string `json:"type"` // "done"
	Status string `json:"status"`
}

type chatLabErrorEvent struct {
	Type    string `json:"type"` // "error"
	Message string `json:"message"`
}

// writeChatLabEvent emits one downstream SSE record (`data: {json}\n\n`) and
// flushes. A write error means the client is gone.
func writeChatLabEvent(w gin.ResponseWriter, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	w.Flush()
	return nil
}

// ---- Prompt assembly (pure; unit-tested with a fake fetchBody) ----------------

// storedChatMessage is the minimal view of a persisted message needed to
// rebuild the upstream conversation. Assistant REASONING is deliberately
// absent: stored reasoning is never echoed back upstream.
type storedChatMessage struct {
	ID      string
	Role    string
	Content string
}

// storedChatAttachment is the minimal view of a ready, bound attachment.
type storedChatAttachment struct {
	Kind        string
	FileName    string
	ContentType string
	S3Key       string
}

// errChatLabInlineBudget is surfaced as a 400 with a clear message.
var errChatLabInlineBudget = errors.New("Attached text files are too large to send (512 KiB total inline limit per request)")

// chatLabTextFileTypes are the attachment content types inlined into the text
// part (everything "file"-kind except PDF).
var chatLabTextFileTypes = map[string]bool{
	"text/plain":       true,
	"text/csv":         true,
	"text/markdown":    true,
	"application/json": true,
}

// buildOpenRouterMessages converts the stored history (every prior message in
// order plus the new one) to upstream messages:
//   - assistant history goes in as plain text content,
//   - user messages with image attachments become multimodal parts — text first
//     (per the multimodal docs), then one image_url part per image using a
//     base64 data URL built from the S3 object bytes (the bucket is private —
//     presigned URLs are never handed upstream),
//   - PDF attachments become file parts (the caller adds the file-parser
//     plugin),
//   - text-kind files are inlined into the text part, under a 512 KiB total
//     budget across the whole request (errChatLabInlineBudget when exceeded).
//
// fetchBody loads an S3 object's bytes; the function is free of gin/context so
// it unit-tests with a fake.
func buildOpenRouterMessages(history []storedChatMessage, attachmentsByMsg map[string][]storedChatAttachment, fetchBody func(s3Key string) ([]byte, error)) ([]openrouter.Message, error) {
	out := make([]openrouter.Message, 0, len(history))
	inlineBudget := drChatLabInlineBudgetBytes
	for _, m := range history {
		if m.Role != "user" {
			out = append(out, openrouter.Message{Role: m.Role, Content: m.Content})
			continue
		}
		atts := attachmentsByMsg[m.ID]
		if len(atts) == 0 {
			out = append(out, openrouter.Message{Role: "user", Content: m.Content})
			continue
		}

		text := m.Content
		var parts []openrouter.ContentPart
		for _, a := range atts {
			switch {
			case a.Kind == "image":
				body, err := fetchBody(a.S3Key)
				if err != nil {
					return nil, fmt.Errorf("load image attachment %s: %w", a.S3Key, err)
				}
				parts = append(parts, openrouter.ContentPart{
					Type:     "image_url",
					ImageURL: &openrouter.ImageURL{URL: dataURL(a.ContentType, body)},
				})
			case a.ContentType == "application/pdf":
				body, err := fetchBody(a.S3Key)
				if err != nil {
					return nil, fmt.Errorf("load pdf attachment %s: %w", a.S3Key, err)
				}
				parts = append(parts, openrouter.ContentPart{
					Type: "file",
					File: &openrouter.FilePart{Filename: a.FileName, FileData: dataURL(a.ContentType, body)},
				})
			case chatLabTextFileTypes[a.ContentType]:
				body, err := fetchBody(a.S3Key)
				if err != nil {
					return nil, fmt.Errorf("load text attachment %s: %w", a.S3Key, err)
				}
				inlineBudget -= len(body)
				if inlineBudget < 0 {
					return nil, errChatLabInlineBudget
				}
				text += fmt.Sprintf("\n\n[Attached file: %s]\n```\n%s\n```", a.FileName, string(body))
			}
		}

		// Text part first, then the media parts (docs recommend the prompt
		// before images).
		content := append([]openrouter.ContentPart{{Type: "text", Text: text}}, parts...)
		out = append(out, openrouter.Message{Role: "user", Content: content})
	}
	return out, nil
}

// dataURL builds a base64 data URL for the given content type + bytes.
func dataURL(contentType string, body []byte) string {
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(body)
}

// hasPDFAttachment reports whether any message carries a PDF (→ the request
// needs the file-parser plugin).
func hasPDFAttachment(attachmentsByMsg map[string][]storedChatAttachment) bool {
	for _, atts := range attachmentsByMsg {
		for _, a := range atts {
			if a.ContentType == "application/pdf" {
				return true
			}
		}
	}
	return false
}

// ---- The endpoint --------------------------------------------------------------

const chatLabSafeUpstreamError = "The AI provider request failed"

func (h *DrChatLabHandler) SendChatMessage(c *gin.Context) {
	if !h.dbReady(c) || !h.orReady(c) {
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
	var req models.DrChatSendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// -- Validation (step 1) --------------------------------------------------
	content := req.Content
	if len(content) > drChatLabMaxContentBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Message is too long (64 KiB max)"})
		return
	}
	if strings.TrimSpace(content) == "" && len(req.AttachmentIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Message is empty"})
		return
	}
	if len(req.AttachmentIDs) > drChatLabMaxAttachments {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("At most %d attachments are allowed", drChatLabMaxAttachments)})
		return
	}
	for _, rawID := range req.AttachmentIDs {
		if _, err := uuid.Parse(strings.TrimSpace(rawID)); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Attachment is not ready"})
			return
		}
	}
	if !drChatLabEfforts[req.ReasoningEffort] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid reasoning effort"})
		return
	}

	setupCtx, setupCancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer setupCancel()

	if _, err := h.loadSession(setupCtx, sessionID); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Chat not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	// Model must be in the CURRENT filtered catalog (cache refreshes if stale).
	model, err := h.catalogModel(setupCtx, req.Model)
	if err != nil {
		log.Printf("dr chatlab: catalog lookup for send: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to load model catalog"})
		return
	}
	if model == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown or unsupported model"})
		return
	}
	effort := req.ReasoningEffort
	if !model.SupportsReasoning {
		effort = "" // silently ignore effort for models without reasoning
	}

	// -- Persist the user message (step 2) — committed BEFORE upstream ---------
	tx, err := h.pool.Begin(setupCtx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}
	defer func() { _ = tx.Rollback(setupCtx) }()

	var userMessageID string
	err = tx.QueryRow(setupCtx, `
INSERT INTO dr_chat_messages (session_id, role, author_uid, author_email, content, model, reasoning_effort)
VALUES ($1, 'user', $2, lower($3), $4, $5, $6)
RETURNING id`, sessionID, claims.UID, claims.Email, content, model.ID, effort).Scan(&userMessageID)
	if err != nil {
		log.Printf("dr chatlab: insert user message: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	// Bind each attachment: must be ready, unbound, same session, caller-owned.
	// A zero-row update aborts the whole send.
	for _, rawID := range req.AttachmentIDs {
		aid := strings.TrimSpace(rawID)
		tag, err := tx.Exec(setupCtx, `
UPDATE dr_chat_attachments
SET message_id = $1
WHERE id = $2 AND session_id = $3 AND author_uid = $4 AND status = 'ready' AND message_id IS NULL`,
			userMessageID, aid, sessionID, claims.UID)
		if err != nil {
			log.Printf("dr chatlab: bind attachment: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
			return
		}
		if tag.RowsAffected() == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Attachment is not ready"})
			return
		}
	}

	if _, err := tx.Exec(setupCtx, `
UPDATE dr_chat_sessions
SET updated_at = now(), last_model = $1, last_reasoning_effort = $2
WHERE id = $3`, model.ID, effort, sessionID); err != nil {
		log.Printf("dr chatlab: bump session: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}
	if err := tx.Commit(setupCtx); err != nil {
		log.Printf("dr chatlab: commit user message: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	// Auto-title after the first exchange (fire-and-forget; guarded by
	// title_source='default' so a racing manual rename wins).
	go h.autoTitleSession(sessionID, content)

	// -- Assemble the upstream conversation (step 3) ---------------------------
	history, attachmentsByMsg, err := h.loadConversation(setupCtx, sessionID)
	if err != nil {
		log.Printf("dr chatlab: load conversation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}
	fetchBody := func(s3Key string) ([]byte, error) { return h.fetchObject(setupCtx, s3Key) }
	upstreamMessages, err := buildOpenRouterMessages(history, attachmentsByMsg, fetchBody)
	if errors.Is(err, errChatLabInlineBudget) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errChatLabInlineBudget.Error()})
		return
	}
	if err != nil {
		log.Printf("dr chatlab: build upstream messages: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare attachments"})
		return
	}

	// -- Upstream request (step 4) ---------------------------------------------
	chatReq := openrouter.ChatRequest{
		Model:     model.ID,
		Messages:  upstreamMessages,
		MaxTokens: h.cfg.DRChatLabMaxOutputTokens,
	}
	if effort != "" {
		chatReq.Reasoning = &openrouter.Reasoning{Effort: effort}
	}
	if hasPDFAttachment(attachmentsByMsg) {
		chatReq.Plugins = []openrouter.Plugin{{ID: "file-parser"}}
	}

	// The upstream stream lives on the REQUEST context so a client disconnect
	// (including the Stop button, which just aborts the fetch) cancels it.
	streamCtx, streamCancel := context.WithCancel(c.Request.Context())
	defer streamCancel()

	stream, err := h.or.StreamChat(streamCtx, chatReq)
	if err != nil {
		// Failure before ANY bytes → plain 502 JSON, not an SSE body. Upstream
		// details go to the logs only.
		log.Printf("dr chatlab: upstream request failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": chatLabSafeUpstreamError})
		return
	}
	defer stream.Close()

	// -- Downstream SSE (step 5) ------------------------------------------------
	assistantMessageID := uuid.NewString()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	if err := writeChatLabEvent(c.Writer, chatLabMetaEvent{Type: "meta", UserMessageID: userMessageID, AssistantMessageID: assistantMessageID}); err != nil {
		streamCancel()
		h.persistAssistantMessage(assistantMessageID, sessionID, model.ID, effort, "", "", "interrupted", "", nil)
		return
	}

	type streamItem struct {
		chunk *openrouter.StreamChunk
		err   error
	}
	items := make(chan streamItem, 8)
	go func() {
		defer close(items)
		for {
			chunk, err := stream.Next()
			select {
			case items <- streamItem{chunk: chunk, err: err}:
			case <-streamCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// -- Accumulate + persist (step 6) --------------------------------------------
	var contentB, reasoningB strings.Builder
	var usage *openrouter.Usage

	persist := func(status string, errorMessage ...string) {
		msg := ""
		if len(errorMessage) > 0 {
			msg = errorMessage[0]
		}
		h.persistAssistantMessage(assistantMessageID, sessionID, model.ID, effort, contentB.String(), reasoningB.String(), status, msg, usage)
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			// Client disconnected / Stop button: cancel upstream, persist the
			// partial as interrupted.
			streamCancel()
			persist("interrupted")
			return
		case <-h.shutdownChan():
			streamCancel()
			persist("interrupted")
			return
		case <-keepalive.C:
			if _, err := io.WriteString(c.Writer, ": ping\n\n"); err != nil {
				streamCancel()
				persist("interrupted")
				return
			}
			c.Writer.Flush()
		case item, chOpen := <-items:
			if !chOpen {
				// Reader exited without a terminal item (cancelled) — treat as
				// interrupted.
				persist("interrupted")
				return
			}
			if item.err != nil {
				if errors.Is(item.err, io.EOF) {
					persist("complete", "")
					_ = writeChatLabEvent(c.Writer, chatLabDoneEvent{Type: "done", Status: "complete"})
					return
				}
				// Mid-stream upstream/transport error: persist the partial with
				// a safe message; details go to the logs only.
				log.Printf("dr chatlab: upstream stream error: %v", item.err)
				persist("error", chatLabSafeUpstreamError)
				_ = writeChatLabEvent(c.Writer, chatLabErrorEvent{Type: "error", Message: chatLabSafeUpstreamError})
				return
			}
			chunk := item.chunk
			clientGone := false
			for _, choice := range chunk.Choices {
				if rt := choice.Delta.ReasoningText(); rt != "" {
					reasoningB.WriteString(rt)
					if err := writeChatLabEvent(c.Writer, chatLabReasoningEvent{Type: "reasoning", Text: rt}); err != nil {
						clientGone = true
						break
					}
				}
				if choice.Delta.Content != "" {
					contentB.WriteString(choice.Delta.Content)
					if err := writeChatLabEvent(c.Writer, chatLabDeltaEvent{Type: "delta", Text: choice.Delta.Content}); err != nil {
						clientGone = true
						break
					}
				}
			}
			if !clientGone && chunk.Usage != nil {
				usage = chunk.Usage
				if err := writeChatLabEvent(c.Writer, chatLabUsageEvent{
					Type:             "usage",
					PromptTokens:     usage.PromptTokens,
					CompletionTokens: usage.CompletionTokens,
					ReasoningTokens:  usage.ReasoningTokens(),
					CostUsd:          usage.Cost,
				}); err != nil {
					clientGone = true
				}
			}
			if clientGone {
				streamCancel()
				persist("interrupted")
				return
			}
		}
	}
}

// shutdownChan returns the app context's Done channel (nil-safe; a nil channel
// blocks forever in select — behaves like "no shutdown signal").
func (h *DrChatLabHandler) shutdownChan() <-chan struct{} {
	if h.appCtx == nil {
		return nil
	}
	return h.appCtx.Done()
}

// fetchObject downloads one S3 object's bytes (attachment bodies for prompt
// assembly). Bounded by the largest allowed attachment plus slack.
func (h *DrChatLabHandler) fetchObject(ctx context.Context, key string) ([]byte, error) {
	if h.s3Client == nil || h.cfg == nil {
		return nil, errors.New("attachment storage unavailable")
	}
	out, err := h.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(io.LimitReader(out.Body, drChatLabMaxPDFBytes+(1<<20)))
}

// loadConversation loads the full ordered message history plus the ready,
// bound attachments of user messages.
func (h *DrChatLabHandler) loadConversation(ctx context.Context, sessionID string) ([]storedChatMessage, map[string][]storedChatAttachment, error) {
	rows, err := h.pool.Query(ctx, `
SELECT id, role, content
FROM dr_chat_messages
WHERE session_id = $1
ORDER BY created_at, seq`, sessionID)
	if err != nil {
		return nil, nil, err
	}
	var history []storedChatMessage
	func() {
		defer rows.Close()
		for rows.Next() {
			var m storedChatMessage
			if err := rows.Scan(&m.ID, &m.Role, &m.Content); err != nil {
				return
			}
			history = append(history, m)
		}
	}()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	attRows, err := h.pool.Query(ctx, `
SELECT message_id, kind, file_name, content_type, s3_key
FROM dr_chat_attachments
WHERE session_id = $1 AND message_id IS NOT NULL AND status = 'ready'
ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, nil, err
	}
	attachmentsByMsg := map[string][]storedChatAttachment{}
	func() {
		defer attRows.Close()
		for attRows.Next() {
			var msgID string
			var a storedChatAttachment
			if err := attRows.Scan(&msgID, &a.Kind, &a.FileName, &a.ContentType, &a.S3Key); err != nil {
				continue
			}
			attachmentsByMsg[msgID] = append(attachmentsByMsg[msgID], a)
		}
	}()
	return history, attachmentsByMsg, nil
}

// persistAssistantMessage inserts the assistant row on a terminal condition
// and bumps the session's updated_at. Runs on context.Background() with a
// short timeout so a client disconnect can't cancel the write.
func (h *DrChatLabHandler) persistAssistantMessage(id, sessionID, model, effort, content, reasoning, status, errorMessage string, usage *openrouter.Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var reasoningPtr, errPtr *string
	if reasoning != "" {
		reasoningPtr = &reasoning
	}
	if errorMessage != "" {
		errPtr = &errorMessage
	}
	var promptTokens, completionTokens, reasoningTokens *int
	var costUsd *float64
	if usage != nil {
		pt, ct, rt := usage.PromptTokens, usage.CompletionTokens, usage.ReasoningTokens()
		cost := usage.Cost
		promptTokens, completionTokens, reasoningTokens, costUsd = &pt, &ct, &rt, &cost
	}

	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_chat_messages (id, session_id, role, content, reasoning, model, reasoning_effort,
                              status, error_message, prompt_tokens, completion_tokens, reasoning_tokens, total_cost_usd)
VALUES ($1, $2, 'assistant', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		id, sessionID, content, reasoningPtr, model, effort, status, errPtr, promptTokens, completionTokens, reasoningTokens, costUsd); err != nil {
		log.Printf("dr chatlab: persist assistant message: %v", err)
		return
	}
	if _, err := h.pool.Exec(ctx, `UPDATE dr_chat_sessions SET updated_at = now() WHERE id = $1`, sessionID); err != nil {
		log.Printf("dr chatlab: bump session after assistant message: %v", err)
	}
}

// ---- Auto-title (step 7) ---------------------------------------------------------

// autoTitleSession sets the session title after the first exchange. When
// DR_CHATLAB_TITLE_MODEL is configured it makes ONE non-streaming completion
// call; on any failure — or when unset — it falls back to a truncation of the
// first user message. Guarded by WHERE title_source='default' so a manual
// rename racing this goroutine wins.
func (h *DrChatLabHandler) autoTitleSession(sessionID, firstUserMessage string) {
	if h.pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Cheap pre-check so repeat sends don't burn a title-model call: only a
	// still-default title is eligible.
	var titleSource string
	if err := h.pool.QueryRow(ctx, `SELECT title_source FROM dr_chat_sessions WHERE id = $1`, sessionID).Scan(&titleSource); err != nil || titleSource != "default" {
		return
	}

	title := ""
	source := "derived"
	if h.or != nil && strings.TrimSpace(h.cfg.DRChatLabTitleModel) != "" {
		excerpt := firstUserMessage
		if len(excerpt) > drChatLabTitlePromptTruncate {
			excerpt = strings.ToValidUTF8(excerpt[:drChatLabTitlePromptTruncate], "")
		}
		resp, err := h.or.Complete(ctx, openrouter.ChatRequest{
			Model: h.cfg.DRChatLabTitleModel,
			Messages: []openrouter.Message{
				{Role: "system", Content: "Generate a concise 3–6 word title for this conversation. Reply with the title only."},
				{Role: "user", Content: excerpt},
			},
			MaxTokens: 64,
		})
		if err != nil {
			log.Printf("dr chatlab: title generation failed (falling back to derived): %v", err)
		} else if t := sanitizeGeneratedTitle(resp.FirstText()); t != "" {
			title, source = t, "generated"
		}
	}
	if title == "" {
		title = deriveChatTitle(firstUserMessage)
	}

	if _, err := h.pool.Exec(ctx, `
UPDATE dr_chat_sessions SET title = $1, title_source = $2
WHERE id = $3 AND title_source = 'default'`, title, source, sessionID); err != nil {
		log.Printf("dr chatlab: save auto title: %v", err)
	}
}
