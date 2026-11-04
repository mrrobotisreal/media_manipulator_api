package assistant

import (
	"context"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// Store is the slice of *telemetry.Store Medi uses. Tests pass a fake. A nil
// Store (no database) still serves the guide; nothing is persisted.
type Store interface {
	CreateAssistantConversation(ctx context.Context, in telemetry.AssistantConversationInsert) (*telemetry.AssistantConversation, error)
	GetAssistantConversation(ctx context.Context, id, ownerUID string) (*telemetry.AssistantConversation, error)
	ListAssistantConversations(ctx context.Context, ownerUID, surface, projectID string, limit int) ([]telemetry.AssistantConversation, error)
	AppendAssistantMessage(ctx context.Context, in telemetry.AssistantMessageInsert) (string, int, error)
	RecentAssistantMessages(ctx context.Context, conversationID string, limit int) ([]telemetry.AssistantMessage, error)
	AssistantConversationMessages(ctx context.Context, conversationID string) ([]telemetry.AssistantMessage, error)
	SetAssistantConversationTitle(ctx context.Context, id, title string) error
	SoftDeleteAssistantConversation(ctx context.Context, id, ownerUID string) error
	DeleteAssistantConversationRow(ctx context.Context, id string) error
	SetAssistantFeedback(ctx context.Context, messageID, userID, rating, reason string) error
	RecordAssistantUsage(ctx context.Context, ev telemetry.AssistantUsageEvent) error
	AssistantSpendSince(ctx context.Context, since time.Time) (float64, error)
}
