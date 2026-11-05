package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// ---- fake provider: scripted rounds, records every request ------------------

type round struct {
	deltas   []string
	calls    []ToolCall
	usage    *Usage
	preErr   error // Stream() fails
	midErr   error // after the deltas
	state    json.RawMessage
	blockOn  chan struct{} // Next blocks until closed (interrupt tests)
	afterOne bool          // block after the first delta
}

type fakeProvider struct {
	mu     sync.Mutex
	rounds []round
	reqs   []Request
}

func (p *fakeProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	if len(p.rounds) == 0 {
		return nil, errors.New("fake: no scripted round")
	}
	r := p.rounds[0]
	p.rounds = p.rounds[1:]
	if r.preErr != nil {
		return nil, r.preErr
	}
	var evs []Event
	for _, d := range r.deltas {
		evs = append(evs, Event{Kind: EventDelta, Text: d})
	}
	return &fakeStream{ctx: ctx, evs: evs, r: r}, nil
}

func (p *fakeProvider) requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Request(nil), p.reqs...)
}

type fakeStream struct {
	ctx  context.Context
	evs  []Event
	r    round
	i    int
	tail bool
}

func (s *fakeStream) Next() (Event, error) {
	if s.r.blockOn != nil && (!s.r.afterOne || s.i >= 1) {
		select {
		case <-s.r.blockOn:
		case <-s.ctx.Done():
			return Event{}, s.ctx.Err()
		}
	}
	if s.i < len(s.evs) {
		ev := s.evs[s.i]
		s.i++
		return ev, nil
	}
	if s.r.midErr != nil {
		return Event{}, s.r.midErr
	}
	if !s.tail {
		s.tail = true
		u := s.r.usage
		if u == nil {
			u = &Usage{InputTokens: 100, OutputTokens: 20, CachedTokens: 80, CostUSD: 0.001, Reported: true}
		}
		s.evs = append(s.evs, Event{Kind: EventUsage, Usage: u}, Event{Kind: EventDone, ToolCalls: s.r.calls, ProviderState: s.r.state})
		return s.Next()
	}
	return Event{}, io.EOF
}

func (s *fakeStream) Close() error { return nil }

// ---- fake store ---------------------------------------------------------------

type fakeStore struct {
	mu     sync.Mutex
	convs  map[string]*telemetry.AssistantConversation
	msgs   map[string][]telemetry.AssistantMessage
	usage  []telemetry.AssistantUsageEvent
	titles map[string]string
	sums   map[string]string
	fb     map[string]string
	seq    int
	spend  float64
}

func newFakeStore() *fakeStore {
	return &fakeStore{convs: map[string]*telemetry.AssistantConversation{}, msgs: map[string][]telemetry.AssistantMessage{},
		titles: map[string]string{}, sums: map[string]string{}, fb: map[string]string{}}
}

func (f *fakeStore) id() string {
	f.seq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
}

func (f *fakeStore) CreateAssistantConversation(_ context.Context, in telemetry.AssistantConversationInsert) (*telemetry.AssistantConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &telemetry.AssistantConversation{ID: f.id(), OwnerUserID: in.OwnerUserID, Surface: in.Surface, ProjectKind: in.ProjectKind,
		ProjectID: in.ProjectID, CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0)}
	f.convs[c.ID] = c
	cp := *c
	return &cp, nil
}

func (f *fakeStore) GetAssistantConversation(_ context.Context, id, owner string) (*telemetry.AssistantConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.convs[id]
	if !ok || c.OwnerUserID != owner {
		return nil, telemetry.ErrAssistantConversationNotFound
	}
	cp := *c
	return &cp, nil
}

func (f *fakeStore) ListAssistantConversations(_ context.Context, owner, surface, project string, limit int) ([]telemetry.AssistantConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []telemetry.AssistantConversation
	for _, c := range f.convs {
		if c.OwnerUserID == owner && (surface == "" || c.Surface == surface) && (project == "" || c.ProjectID == project) {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (f *fakeStore) AppendAssistantMessage(_ context.Context, in telemetry.AssistantMessageInsert) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !json.Valid(in.Content) {
		return "", 0, errors.New("content is not JSON")
	}
	status := in.Status
	if status == "" {
		status = "complete"
	}
	m := telemetry.AssistantMessage{ID: f.id(), ConversationID: in.ConversationID, Seq: len(f.msgs[in.ConversationID]) + 1,
		TurnID: in.TurnID, Role: in.Role, Content: in.Content, Model: in.Model, Status: status, Usage: in.Usage, ProviderState: in.ProviderState, PromptVersion: in.PromptVersion}
	f.msgs[in.ConversationID] = append(f.msgs[in.ConversationID], m)
	return m.ID, m.Seq, nil
}

func (f *fakeStore) RecentAssistantMessages(_ context.Context, id string, limit int) ([]telemetry.AssistantMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.msgs[id]
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return append([]telemetry.AssistantMessage(nil), all...), nil
}

func (f *fakeStore) AssistantConversationMessages(_ context.Context, id string) ([]telemetry.AssistantMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telemetry.AssistantMessage(nil), f.msgs[id]...), nil
}

func (f *fakeStore) SetAssistantConversationTitle(_ context.Context, id, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titles[id] = title
	return nil
}

func (f *fakeStore) SetAssistantConversationSummary(_ context.Context, id, summary string, upto int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.convs[id]
	if !ok || c.SummaryUpto >= upto {
		return nil
	}
	if c.Summary == "" {
		c.Summary = summary
	} else {
		c.Summary += "\n\n" + summary
	}
	c.SummaryUpto = upto
	f.sums[id] = c.Summary
	return nil
}

func (f *fakeStore) SoftDeleteAssistantConversation(_ context.Context, id, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.convs[id]; ok && c.OwnerUserID == owner {
		delete(f.convs, id)
	}
	return nil
}

func (f *fakeStore) DeleteAssistantConversationRow(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.convs, id)
	delete(f.msgs, id)
	return nil
}

func (f *fakeStore) SetAssistantFeedback(_ context.Context, msg, user, rating, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for cid, list := range f.msgs {
		for _, m := range list {
			if m.ID == msg && m.Role == "assistant" && f.convs[cid] != nil && f.convs[cid].OwnerUserID == user {
				f.fb[msg] = rating
				return nil
			}
		}
	}
	return telemetry.ErrAssistantMessageNotFound
}

func (f *fakeStore) RecordAssistantUsage(_ context.Context, ev telemetry.AssistantUsageEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, ev)
	return nil
}

func (f *fakeStore) AssistantSpendSince(context.Context, time.Time) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spend, nil
}

// rows returns every stored message of every conversation.
func (f *fakeStore) rows() []telemetry.AssistantMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []telemetry.AssistantMessage
	for _, l := range f.msgs {
		out = append(out, l...)
	}
	return out
}

// ---- fake knowledge ----------------------------------------------------------------

type fakeKnowledge struct {
	docs   map[string]Doc
	hits   []Hit
	routes *Routes
}

func (k fakeKnowledge) Search(string, string, int) []Hit { return k.hits }
func (k fakeKnowledge) Get(id string) (Doc, bool)        { d, ok := k.docs[id]; return d, ok }
func (fakeKnowledge) Capsule(s string) string            { return "CAPSULE:" + s }
func (k fakeKnowledge) Routes() *Routes                  { return k.routes }
func (fakeKnowledge) InScope(string) bool                { return true }

// ---- recording emitter -----------------------------------------------------------------

type recEmitter struct {
	mu     sync.Mutex
	events []map[string]any
	failAt int // fail the Nth Send (1-based); 0 = never
	pings  int
}

func (r *recEmitter) Send(ev any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failAt > 0 && len(r.events)+1 >= r.failAt {
		return errors.New("client gone")
	}
	b, _ := json.Marshal(ev)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	r.events = append(r.events, m)
	return nil
}

func (r *recEmitter) Ping() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pings++
	return nil
}

func (r *recEmitter) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i], _ = e["type"].(string)
	}
	return out
}

func (r *recEmitter) last() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return nil
	}
	return r.events[len(r.events)-1]
}

// ---- refund recorder --------------------------------------------------------------

type fakeCharge struct {
	mu       sync.Mutex
	refunded []string
}

func (c *fakeCharge) Refund(reason string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.refunded) > 0 {
		return false
	}
	c.refunded = append(c.refunded, reason)
	return true
}

func (c *fakeCharge) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.refunded)
}
