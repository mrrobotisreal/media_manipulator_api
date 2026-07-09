package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Response feedback: optional 👍/👎 on assistant messages with standard
// category buttons + an "Other" free-text comment. One row per (message,
// rater); changing = upsert. Both users' feedback is visible (two-person lab —
// seeing each other's steering signals is the point). In PROJECT chats a
// feedback write/delete triggers the memory updater, which distills durable
// preferences into the project memory that rides along in every system prompt
// — that is the steering mechanism. For non-project chats feedback is
// analytics-only by design (there is deliberately no per-session steering).

const (
	drChatLabMaxFeedbackCategories   = 6
	drChatLabMaxFeedbackCommentBytes = 2 << 10 // 2 KiB
)

// The standard categories — defined ONCE here as the validation allowlist and
// exposed on GET /chatlab/models so the UI never hardcodes them. Ids are the
// snake_case of the label; tuned to what this lab is for (OCR, extraction,
// general Q&A).
var drChatLabFeedbackCategoriesUp = []models.DrChatLabFeedbackCategory{
	{ID: "accurate", Label: "Accurate"},
	{ID: "perfect_format", Label: "Perfect format"},
	{ID: "followed_instructions", Label: "Followed instructions"},
	{ID: "great_ocr_transcription", Label: "Great OCR/transcription"},
	{ID: "good_structured_output", Label: "Good structured output"},
	{ID: "concise", Label: "Concise"},
}

var drChatLabFeedbackCategoriesDown = []models.DrChatLabFeedbackCategory{
	{ID: "inaccurate_hallucinated", Label: "Inaccurate / hallucinated"},
	{ID: "wrong_format", Label: "Wrong format"},
	{ID: "ignored_instructions", Label: "Ignored instructions"},
	{ID: "poor_ocr_transcription", Label: "Poor OCR/transcription"},
	{ID: "bad_structured_output", Label: "Bad structured output"},
	{ID: "too_verbose", Label: "Too verbose"},
	{ID: "incomplete_cut_off", Label: "Incomplete / cut off"},
}

// drChatLabFeedbackCategorySet maps rating → allowed category-id set.
var drChatLabFeedbackCategorySet = func() map[string]map[string]bool {
	set := map[string]map[string]bool{"up": {}, "down": {}}
	for _, c := range drChatLabFeedbackCategoriesUp {
		set["up"][c.ID] = true
	}
	for _, c := range drChatLabFeedbackCategoriesDown {
		set["down"][c.ID] = true
	}
	return set
}()

// drChatLabFeedbackCategoryLabels maps category id → label (both ratings; ids
// are disjoint) for rendering feedback into the memory prompt.
var drChatLabFeedbackCategoryLabels = func() map[string]string {
	m := map[string]string{}
	for _, c := range drChatLabFeedbackCategoriesUp {
		m[c.ID] = c.Label
	}
	for _, c := range drChatLabFeedbackCategoriesDown {
		m[c.ID] = c.Label
	}
	return m
}()

func chatLabFeedbackCategories() models.DrChatLabFeedbackCategories {
	return models.DrChatLabFeedbackCategories{
		Up:   drChatLabFeedbackCategoriesUp,
		Down: drChatLabFeedbackCategoriesDown,
	}
}

// validateFeedbackRequest checks rating/categories/comment against the
// allowlist, returning a client-facing error message ("" = valid). Pure;
// unit-tested.
func validateFeedbackRequest(rating string, categories []string, comment string) string {
	allowed, ok := drChatLabFeedbackCategorySet[rating]
	if !ok {
		return "Rating must be 'up' or 'down'"
	}
	if len(categories) > drChatLabMaxFeedbackCategories {
		return fmt.Sprintf("At most %d categories are allowed", drChatLabMaxFeedbackCategories)
	}
	for _, id := range categories {
		if !allowed[id] {
			return fmt.Sprintf("Unknown feedback category %q for rating %q", id, rating)
		}
	}
	if len(comment) > drChatLabMaxFeedbackCommentBytes {
		return "Comment is too long (2 KiB max)"
	}
	return ""
}

func drChatLabMessageID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("messageId"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message id"})
		return "", false
	}
	return id, true
}

// loadFeedbackTarget resolves the message being rated: role must be
// 'assistant'; session/project/model are denormalized into the feedback row.
type feedbackTarget struct {
	sessionID string
	projectID *string
	model     string
}

func (h *DrChatLabHandler) loadFeedbackTarget(ctx context.Context, messageID string) (feedbackTarget, int, string) {
	var t feedbackTarget
	var role string
	var model *string
	err := h.pool.QueryRow(ctx, `
SELECT m.session_id, m.role, m.model, s.project_id
FROM dr_chat_messages m
JOIN dr_chat_sessions s ON s.id = m.session_id
WHERE m.id = $1`, messageID).Scan(&t.sessionID, &role, &model, &t.projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, http.StatusNotFound, "Message not found"
	}
	if err != nil {
		log.Printf("dr chatlab feedback: load message: %v", err)
		return t, http.StatusInternalServerError, "Failed to save feedback"
	}
	if role != "assistant" {
		return t, http.StatusBadRequest, "Feedback can only be left on assistant messages"
	}
	if model != nil {
		t.model = *model
	} else {
		t.model = "unknown"
	}
	return t, 0, ""
}

// ----------------------------------------------------------------------- //
// PUT /chatlab/messages/:messageId/feedback  (upsert)
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) PutMessageFeedback(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	messageID, ok := drChatLabMessageID(c)
	if !ok {
		return
	}
	var req models.DrChatFeedbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	comment := strings.TrimSpace(req.Comment)
	if req.Categories == nil {
		req.Categories = []string{}
	}
	if msg := validateFeedbackRequest(req.Rating, req.Categories, comment); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	target, status, errMsg := h.loadFeedbackTarget(ctx, messageID)
	if status != 0 {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	var dto models.DrChatMessageFeedback
	var updatedAt time.Time
	err := h.pool.QueryRow(ctx, `
INSERT INTO dr_chat_message_feedback (message_id, session_id, project_id, model, rater_uid, rater_email, rating, categories, comment)
VALUES ($1, $2, $3, $4, $5, lower($6), $7, $8, $9)
ON CONFLICT (message_id, rater_uid) DO UPDATE
SET rating = EXCLUDED.rating, categories = EXCLUDED.categories, comment = EXCLUDED.comment, updated_at = now()
RETURNING rating, categories, comment, rater_email, updated_at`,
		messageID, target.sessionID, target.projectID, target.model, claims.UID, claims.Email, req.Rating, req.Categories, comment).
		Scan(&dto.Rating, &dto.Categories, &dto.Comment, &dto.RaterEmail, &updatedAt)
	if err != nil {
		log.Printf("dr chatlab feedback: upsert: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save feedback"})
		return
	}
	dto.IsMine = true
	dto.UpdatedAt = models.UTCTime{Time: updatedAt}

	// Feedback in a project steers future responses through the memory:
	// dirty-mark the project's feedback state so the nightly job folds it in
	// (manual Refresh regenerates on demand).
	if target.projectID != nil {
		h.markFeedbackHash(*target.projectID)
	}
	c.JSON(http.StatusOK, dto)
}

// ----------------------------------------------------------------------- //
// DELETE /chatlab/messages/:messageId/feedback  (caller's row; idempotent)
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) DeleteMessageFeedback(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	messageID, ok := drChatLabMessageID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var projectID *string
	err := h.pool.QueryRow(ctx, `
DELETE FROM dr_chat_message_feedback
WHERE message_id = $1 AND rater_uid = $2
RETURNING project_id`, messageID, claims.UID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusOK, gin.H{"deleted": true}) // idempotent
		return
	}
	if err != nil {
		log.Printf("dr chatlab feedback: delete: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete feedback"})
		return
	}
	if projectID != nil {
		h.markFeedbackHash(*projectID)
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// ----------------------------------------------------------------------- //
// Hydration (GetSession)
// ----------------------------------------------------------------------- //

// hydrateMessageFeedback batch-loads all feedback rows for the given message
// ids, keyed by message id. Both users' rows are returned; IsMine flags the
// caller's.
func (h *DrChatLabHandler) hydrateMessageFeedback(ctx context.Context, messageIDs []string, callerUID string) map[string][]models.DrChatMessageFeedback {
	out := map[string][]models.DrChatMessageFeedback{}
	if len(messageIDs) == 0 {
		return out
	}
	rows, err := h.pool.Query(ctx, `
SELECT message_id, rating, categories, comment, rater_email, rater_uid, updated_at
FROM dr_chat_message_feedback
WHERE message_id = ANY($1)
ORDER BY created_at, id`, messageIDs)
	if err != nil {
		log.Printf("dr chatlab feedback: hydrate: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var f models.DrChatMessageFeedback
		var messageID, raterUID string
		var updatedAt time.Time
		if err := rows.Scan(&messageID, &f.Rating, &f.Categories, &f.Comment, &f.RaterEmail, &raterUID, &updatedAt); err != nil {
			log.Printf("dr chatlab feedback: scan: %v", err)
			continue
		}
		f.IsMine = raterUID == callerUID
		f.UpdatedAt = models.UTCTime{Time: updatedAt}
		out[messageID] = append(out[messageID], f)
	}
	return out
}
