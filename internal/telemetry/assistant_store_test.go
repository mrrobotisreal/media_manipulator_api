package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// MM104: SQL contract checks (no database on this machine).
func TestAssistantStoreSQL(t *testing.T) {
	for name, q := range map[string]string{
		"get":      assistantConvGetSQL,
		"list":     assistantConvListSQL,
		"softdel":  assistantSoftDeleteSQL,
		"feedback": assistantFeedbackSQL,
	} {
		if !strings.Contains(q, "owner_user_id = $") {
			t.Errorf("%s must predicate on the owner", name)
		}
	}
	for name, q := range map[string]string{"get": assistantConvGetSQL, "list": assistantConvListSQL, "export": assistantExportSQL} {
		if !strings.Contains(q, "deleted_at IS NULL") {
			t.Errorf("%s must skip soft-deleted conversations", name)
		}
	}
	if !strings.Contains(assistantFeedbackSQL, "m.role = 'assistant'") {
		t.Error("feedback lands on assistant messages only")
	}
	if !strings.Contains(assistantMsgInsertSQL, "coalesce(max(seq), 0) + 1") || !strings.Contains(assistantMsgLockSQL, "pg_advisory_xact_lock") {
		t.Error("seq must be assigned under the per-conversation lock")
	}
	if !strings.Contains(assistantTitleSQL, "AND title = ''") {
		t.Error("the title is set once")
	}
	// provider_state (reasoning) is read only for the upstream continuation.
	if strings.Contains(assistantMsgAllSQL, "provider_state") || strings.Contains(assistantExportSQL, "provider_state") ||
		!strings.Contains(assistantMsgRecentSQL, "provider_state") {
		t.Error("provider_state is never exported or returned")
	}
	// Usage rows carry no content column.
	for _, banned := range []string{"content", "text", "digest", "frame"} {
		if strings.Contains(assistantUsageInsertSQL, banned) {
			t.Errorf("usage insert must not carry %q", banned)
		}
	}
}

func TestAssistantStoreDisabled(t *testing.T) {
	var s *Store
	ctx := context.Background()
	if _, err := s.CreateAssistantConversation(ctx, AssistantConversationInsert{}); !errors.Is(err, ErrNoStore) {
		t.Errorf("create = %v", err)
	}
	if _, err := s.AssistantSpendSince(ctx, time.Now()); !errors.Is(err, ErrNoStore) {
		t.Errorf("spend = %v", err)
	}
	if err := s.RecordAssistantUsage(ctx, AssistantUsageEvent{}); !errors.Is(err, ErrNoStore) {
		t.Errorf("usage = %v", err)
	}
}
