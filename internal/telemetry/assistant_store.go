package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM104 Medi (code name `assistant`): conversations, messages, feedback and the
// content-free usage rows (migration 20261104001_assistant). Only signed-in
// users have conversations; every query on a conversation predicates on
// owner_user_id. Frames are never written: the handler strips them before any
// call here, and content is whatever JSON the handler built.

// RetainedAssistantConversation is the ledger kind of a conversation (ADR-0010):
// locator = project_id = the conversation id. Mirrored by the CHECK in
// 20261104001_assistant.
const RetainedAssistantConversation = "assistant_conversation"

// ErrAssistantConversationNotFound is a missing, not-owned or soft-deleted
// conversation (callers answer 404 for all three).
var ErrAssistantConversationNotFound = errors.New("assistant conversation not found")

// ErrAssistantMessageNotFound is a feedback target that is missing or not the
// caller's.
var ErrAssistantMessageNotFound = errors.New("assistant message not found")

// AssistantConversation is one assistant_conversations row.
type AssistantConversation struct {
	ID          string
	OwnerUserID string
	Surface     string
	ProjectKind string // "" = none
	ProjectID   string // "" = none
	Title       string
	Summary     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AssistantConversationInsert creates a conversation.
type AssistantConversationInsert struct {
	OwnerUserID string
	Surface     string
	ProjectKind string
	ProjectID   string
}

// AssistantMessage is one assistant_messages row. Content is the raw jsonb.
type AssistantMessage struct {
	ID             string
	ConversationID string
	Seq            int
	TurnID         string
	Role           string
	Content        json.RawMessage
	Model          string
	Status         string
	Usage          json.RawMessage
	CreatedAt      time.Time
}

// AssistantMessageInsert appends one message; Seq is assigned by the store.
type AssistantMessageInsert struct {
	ConversationID string
	TurnID         string
	Role           string
	Content        json.RawMessage
	Model          string
	Status         string // "" = complete
	Usage          json.RawMessage
}

// AssistantUsageEvent is one assistant_usage_events row: no content, ever.
type AssistantUsageEvent struct {
	UserID       string // "" = guest
	VisitorHash  string
	Surface      string
	Kind         string // chat | title | summary
	Model        string
	InputTokens  int
	OutputTokens int
	CachedTokens int
	CostUSD      float64
	Estimated    bool
}

// AssistantExportConversation is one conversation with its messages, for the
// account data export (assistant.json).
type AssistantExportConversation struct {
	AssistantConversation
	Messages []AssistantMessage
}

const assistantConvColumns = `id::text, owner_user_id::text, surface, coalesce(project_kind, ''), coalesce(project_id, ''),
	title, coalesce(summary, ''), created_at, updated_at`

const assistantMsgColumns = `id::text, conversation_id::text, seq, turn_id::text, role, content, coalesce(model, ''), status,
	coalesce(usage, 'null'::jsonb), created_at`

const (
	assistantConvInsertSQL = `
INSERT INTO assistant_conversations (owner_user_id, surface, project_kind, project_id)
VALUES ($1::uuid, $2, nullif($3, ''), nullif($4, ''))
RETURNING ` + assistantConvColumns

	assistantConvGetSQL = `SELECT ` + assistantConvColumns + `
FROM assistant_conversations WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL`

	// $3 surface and $4 project id filter only when non-empty.
	assistantConvListSQL = `SELECT ` + assistantConvColumns + `
FROM assistant_conversations
WHERE owner_user_id = $1::uuid AND deleted_at IS NULL
  AND ($2 = '' OR surface = $2) AND ($3 = '' OR project_id = $3)
ORDER BY updated_at DESC, id LIMIT $4`

	// The seq is assigned under a per-conversation advisory lock so two
	// concurrent appends can never collide on UNIQUE (conversation_id, seq).
	assistantMsgLockSQL   = `SELECT pg_advisory_xact_lock(hashtextextended('assistant_messages:' || $1, 0))`
	assistantMsgInsertSQL = `
INSERT INTO assistant_messages (conversation_id, seq, turn_id, role, content, model, status, usage)
SELECT $1::uuid, coalesce(max(seq), 0) + 1, $2::uuid, $3, $4::jsonb, nullif($5, ''), $6, $7::jsonb
FROM assistant_messages WHERE conversation_id = $1::uuid
RETURNING id::text, seq`
	assistantConvTouchSQL = `UPDATE assistant_conversations SET updated_at = now() WHERE id = $1::uuid`

	// The newest $2 messages, returned oldest first.
	assistantMsgRecentSQL = `SELECT * FROM (SELECT ` + assistantMsgColumns + `
FROM assistant_messages WHERE conversation_id = $1::uuid ORDER BY seq DESC LIMIT $2) m ORDER BY seq`

	assistantMsgAllSQL = `SELECT ` + assistantMsgColumns + ` FROM assistant_messages WHERE conversation_id = $1::uuid ORDER BY seq`

	assistantTitleSQL      = `UPDATE assistant_conversations SET title = $2 WHERE id = $1::uuid AND title = ''`
	assistantSoftDeleteSQL = `UPDATE assistant_conversations SET deleted_at = coalesce(deleted_at, now()) WHERE id = $1::uuid AND owner_user_id = $2::uuid`
	assistantHardDeleteSQL = `DELETE FROM assistant_conversations WHERE id = $1::uuid`

	// Feedback lands only on an assistant message of a live conversation the
	// caller owns; one rating per message (the newest wins).
	assistantFeedbackSQL = `
INSERT INTO assistant_message_feedback (message_id, user_id, rating, reason)
SELECT m.id, $2::uuid, $3, nullif($4, '')
FROM assistant_messages m JOIN assistant_conversations c ON c.id = m.conversation_id
WHERE m.id = $1::uuid AND m.role = 'assistant' AND c.owner_user_id = $2::uuid AND c.deleted_at IS NULL
ON CONFLICT (message_id) DO UPDATE SET rating = EXCLUDED.rating, reason = EXCLUDED.reason, created_at = now()`

	assistantUsageInsertSQL = `
INSERT INTO assistant_usage_events
	(user_id, visitor_hash, surface, kind, model, input_tokens, output_tokens, cached_tokens, cost_usd, estimated)
VALUES (nullif($1, '')::uuid, nullif($2, ''), $3, $4, $5, $6, $7, $8, $9, $10)`

	assistantSpendSinceSQL = `SELECT coalesce(sum(cost_usd), 0)::float8 FROM assistant_usage_events WHERE created_at >= $1`

	userAssistantConvIDsSQL = `SELECT id::text FROM assistant_conversations WHERE owner_user_id = $1::uuid`
	assistantExportSQL      = `SELECT ` + assistantConvColumns + ` FROM assistant_conversations
WHERE owner_user_id = $1::uuid AND deleted_at IS NULL ORDER BY updated_at DESC`
)

func scanAssistantConv(row pgx.Row) (*AssistantConversation, error) {
	var c AssistantConversation
	if err := row.Scan(&c.ID, &c.OwnerUserID, &c.Surface, &c.ProjectKind, &c.ProjectID, &c.Title, &c.Summary,
		&c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

func scanAssistantMsgs(rows pgx.Rows) ([]AssistantMessage, error) {
	defer rows.Close()
	out := []AssistantMessage{}
	for rows.Next() {
		var m AssistantMessage
		var content, usage []byte
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Seq, &m.TurnID, &m.Role, &content, &m.Model, &m.Status,
			&usage, &m.CreatedAt); err != nil {
			return out, err
		}
		m.Content = content
		if string(usage) != "null" {
			m.Usage = usage
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateAssistantConversation inserts a conversation for a signed-in user.
func (s *Store) CreateAssistantConversation(ctx context.Context, in AssistantConversationInsert) (*AssistantConversation, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	return scanAssistantConv(s.Pool.QueryRow(ctx, assistantConvInsertSQL, in.OwnerUserID, in.Surface, in.ProjectKind, in.ProjectID))
}

// GetAssistantConversation reads one live conversation the owner holds.
func (s *Store) GetAssistantConversation(ctx context.Context, id, ownerUID string) (*AssistantConversation, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	c, err := scanAssistantConv(s.Pool.QueryRow(ctx, assistantConvGetSQL, id, ownerUID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssistantConversationNotFound
	}
	return c, err
}

// ListAssistantConversations lists the owner's live conversations, newest first.
func (s *Store) ListAssistantConversations(ctx context.Context, ownerUID, surface, projectID string, limit int) ([]AssistantConversation, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, assistantConvListSQL, ownerUID, surface, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantConversation{}
	for rows.Next() {
		c, err := scanAssistantConv(rows)
		if err != nil {
			return out, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// AppendAssistantMessage appends one message (seq = max + 1) and touches the
// conversation. Returns the new message id and seq.
func (s *Store) AppendAssistantMessage(ctx context.Context, in AssistantMessageInsert) (string, int, error) {
	if !s.Enabled() {
		return "", 0, ErrNoStore
	}
	status := in.Status
	if status == "" {
		status = "complete"
	}
	var usage any
	if len(in.Usage) > 0 {
		usage = string(in.Usage)
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, assistantMsgLockSQL, in.ConversationID); err != nil {
		return "", 0, err
	}
	var id string
	var seq int
	if err := tx.QueryRow(ctx, assistantMsgInsertSQL, in.ConversationID, in.TurnID, in.Role, string(in.Content),
		in.Model, status, usage).Scan(&id, &seq); err != nil {
		return "", 0, err
	}
	if _, err := tx.Exec(ctx, assistantConvTouchSQL, in.ConversationID); err != nil {
		return "", 0, err
	}
	return id, seq, tx.Commit(ctx)
}

// RecentAssistantMessages returns the newest `limit` messages, oldest first.
func (s *Store) RecentAssistantMessages(ctx context.Context, conversationID string, limit int) ([]AssistantMessage, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, assistantMsgRecentSQL, conversationID, limit)
	if err != nil {
		return nil, err
	}
	return scanAssistantMsgs(rows)
}

// AssistantConversationMessages returns every message of a conversation.
func (s *Store) AssistantConversationMessages(ctx context.Context, conversationID string) ([]AssistantMessage, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, assistantMsgAllSQL, conversationID)
	if err != nil {
		return nil, err
	}
	return scanAssistantMsgs(rows)
}

// SetAssistantConversationTitle sets the title once (only while it is empty, so
// a racing second title never overwrites the first).
func (s *Store) SetAssistantConversationTitle(ctx context.Context, id, title string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, assistantTitleSQL, id, title)
	return err
}

// SoftDeleteAssistantConversation stamps deleted_at (the ledger sweep removes
// the row afterwards).
func (s *Store) SoftDeleteAssistantConversation(ctx context.Context, id, ownerUID string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, assistantSoftDeleteSQL, id, ownerUID)
	return err
}

// DeleteAssistantConversationRow hard-deletes one conversation (messages and
// feedback cascade). Idempotent; used by the retention sweeper.
func (s *Store) DeleteAssistantConversationRow(ctx context.Context, id string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, assistantHardDeleteSQL, id)
	return err
}

// SetAssistantFeedback records an up/down on one of the caller's assistant
// messages. ErrAssistantMessageNotFound when it is not theirs (or not an
// assistant message, or the conversation is deleted).
func (s *Store) SetAssistantFeedback(ctx context.Context, messageID, userID, rating, reason string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tag, err := s.Pool.Exec(ctx, assistantFeedbackSQL, messageID, userID, rating, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAssistantMessageNotFound
	}
	return nil
}

// RecordAssistantUsage inserts one content-free usage row.
func (s *Store) RecordAssistantUsage(ctx context.Context, ev AssistantUsageEvent) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, assistantUsageInsertSQL, ev.UserID, ev.VisitorHash, ev.Surface, ev.Kind, ev.Model,
		ev.InputTokens, ev.OutputTokens, ev.CachedTokens, ev.CostUSD, ev.Estimated)
	return err
}

// AssistantSpendSince sums cost_usd of every usage row at or after since (the
// spend cap reads today's UTC total).
func (s *Store) AssistantSpendSince(ctx context.Context, since time.Time) (float64, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var total float64
	err := s.Pool.QueryRow(ctx, assistantSpendSinceSQL, since).Scan(&total)
	return total, err
}

// ExportAssistantConversations lists the user's live conversations with every
// message, for the account data export (assistant.json).
func (s *Store) ExportAssistantConversations(ctx context.Context, userID string) ([]AssistantExportConversation, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, assistantExportSQL, userID)
	if err != nil {
		return nil, err
	}
	var convs []AssistantConversation
	for rows.Next() {
		c, err := scanAssistantConv(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		convs = append(convs, *c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AssistantExportConversation, 0, len(convs))
	for _, c := range convs {
		msgRows, err := s.Pool.Query(ctx, assistantMsgAllSQL, c.ID)
		if err != nil {
			return out, err
		}
		msgs, err := scanAssistantMsgs(msgRows)
		if err != nil {
			return out, err
		}
		out = append(out, AssistantExportConversation{AssistantConversation: c, Messages: msgs})
	}
	return out, nil
}

// deleteUserAssistantConversations removes every conversation of a user
// (account deletion). The mm_users fk cascade is the last backstop.
func (s *Store) deleteUserAssistantConversations(ctx context.Context, userID string) (int64, error) {
	ids, err := s.idList(ctx, userAssistantConvIDsSQL, userID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if err := s.DeleteAssistantConversationRow(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
