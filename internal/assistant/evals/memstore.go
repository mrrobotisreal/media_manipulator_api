package evals

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// memStore is an in-memory assistant.Store for adjust cases (MM115): the
// engine needs stored rows to continue a turn with an editResult, but the eval
// persists nothing anywhere. One store per case.
type memStore struct {
	mu    sync.Mutex
	seq   int
	convs map[string]*telemetry.AssistantConversation
	msgs  map[string][]telemetry.AssistantMessage
}

func newMemStore() *memStore {
	return &memStore{convs: map[string]*telemetry.AssistantConversation{}, msgs: map[string][]telemetry.AssistantMessage{}}
}

func (m *memStore) id() string {
	m.seq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", m.seq)
}

func (m *memStore) CreateAssistantConversation(_ context.Context, in telemetry.AssistantConversationInsert) (*telemetry.AssistantConversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := &telemetry.AssistantConversation{ID: m.id(), OwnerUserID: in.OwnerUserID, Surface: in.Surface, ProjectKind: in.ProjectKind, ProjectID: in.ProjectID}
	m.convs[c.ID] = c
	cp := *c
	return &cp, nil
}

func (m *memStore) GetAssistantConversation(_ context.Context, id, owner string) (*telemetry.AssistantConversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok || c.OwnerUserID != owner {
		return nil, telemetry.ErrAssistantConversationNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *memStore) ListAssistantConversations(context.Context, string, string, string, int) ([]telemetry.AssistantConversation, error) {
	return nil, nil
}

func (m *memStore) AppendAssistantMessage(_ context.Context, in telemetry.AssistantMessageInsert) (string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := in.Status
	if status == "" {
		status = "complete"
	}
	row := telemetry.AssistantMessage{ID: m.id(), ConversationID: in.ConversationID, Seq: len(m.msgs[in.ConversationID]) + 1, TurnID: in.TurnID,
		Role: in.Role, Content: in.Content, Model: in.Model, Status: status, Usage: in.Usage, ProviderState: in.ProviderState,
		PromptVersion: in.PromptVersion, CreatedAt: time.Now()}
	m.msgs[in.ConversationID] = append(m.msgs[in.ConversationID], row)
	return row.ID, row.Seq, nil
}

func (m *memStore) RecentAssistantMessages(_ context.Context, id string, limit int) ([]telemetry.AssistantMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.msgs[id]
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return append([]telemetry.AssistantMessage(nil), all...), nil
}

func (m *memStore) AssistantConversationMessages(_ context.Context, id string) ([]telemetry.AssistantMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]telemetry.AssistantMessage(nil), m.msgs[id]...), nil
}

func (m *memStore) SetAssistantConversationTitle(context.Context, string, string) error { return nil }
func (m *memStore) SetAssistantConversationSummary(context.Context, string, string, int) error {
	return nil
}
func (m *memStore) SoftDeleteAssistantConversation(context.Context, string, string) error { return nil }
func (m *memStore) DeleteAssistantConversationRow(context.Context, string) error          { return nil }
func (m *memStore) SetAssistantFeedback(context.Context, string, string, string, string) error {
	return errors.New("memstore: no feedback")
}
func (m *memStore) RecordAssistantUsage(context.Context, telemetry.AssistantUsageEvent) error {
	return nil
}
func (m *memStore) AssistantSpendSince(context.Context, time.Time) (float64, error) { return 0, nil }
