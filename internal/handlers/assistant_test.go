package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// ---- fakes ----------------------------------------------------------------------

// asstProvider replays scripted rounds: text, or one tool call.
type asstProvider struct {
	mu     sync.Mutex
	rounds []asstRound
	calls  int
}

type asstRound struct {
	text   string
	call   *assistant.ToolCall
	preErr error
}

func (p *asstProvider) Stream(_ context.Context, req assistant.Request) (assistant.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	// Title calls (64 max tokens, no tools) are answered without a script.
	if len(p.rounds) == 0 || (req.MaxTokens == 64 && len(req.Tools) == 0) {
		return &asstStream{evs: []assistant.Event{{Kind: assistant.EventDelta, Text: "Title"}, {Kind: assistant.EventDone}}}, nil
	}
	r := p.rounds[0]
	p.rounds = p.rounds[1:]
	if r.preErr != nil {
		return nil, r.preErr
	}
	var evs []assistant.Event
	if r.text != "" {
		evs = append(evs, assistant.Event{Kind: assistant.EventDelta, Text: r.text})
	}
	evs = append(evs, assistant.Event{Kind: assistant.EventUsage, Usage: &assistant.Usage{InputTokens: 50, OutputTokens: 5, CostUSD: 0.0002, Reported: true}})
	done := assistant.Event{Kind: assistant.EventDone}
	if r.call != nil {
		done.ToolCalls = []assistant.ToolCall{*r.call}
	}
	evs = append(evs, done)
	return &asstStream{evs: evs}, nil
}

type asstStream struct{ evs []assistant.Event }

func (s *asstStream) Next() (assistant.Event, error) {
	if len(s.evs) == 0 {
		return assistant.Event{}, io.EOF
	}
	ev := s.evs[0]
	s.evs = s.evs[1:]
	return ev, nil
}
func (s *asstStream) Close() error { return nil }

// asstStore is the conversation store (in memory, owner-checked) plus the
// quota counters the real DeferQuota middleware increments.
type asstStore struct {
	mu       sync.Mutex
	convs    map[string]*telemetry.AssistantConversation
	msgs     map[string][]telemetry.AssistantMessage
	usage    []telemetry.AssistantUsageEvent
	counters map[string]int64 // subject|metric -> count
	spend    float64
	seq      int
}

func newAsstStore() *asstStore {
	return &asstStore{convs: map[string]*telemetry.AssistantConversation{}, msgs: map[string][]telemetry.AssistantMessage{}, counters: map[string]int64{}}
}

func (f *asstStore) nextID() string {
	f.seq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
}

func (f *asstStore) CreateAssistantConversation(_ context.Context, in telemetry.AssistantConversationInsert) (*telemetry.AssistantConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &telemetry.AssistantConversation{ID: f.nextID(), OwnerUserID: in.OwnerUserID, Surface: in.Surface, ProjectKind: in.ProjectKind, ProjectID: in.ProjectID}
	f.convs[c.ID] = c
	cp := *c
	return &cp, nil
}
func (f *asstStore) GetAssistantConversation(_ context.Context, id, owner string) (*telemetry.AssistantConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.convs[id]; ok && c.OwnerUserID == owner {
		cp := *c
		return &cp, nil
	}
	return nil, telemetry.ErrAssistantConversationNotFound
}
func (f *asstStore) ListAssistantConversations(_ context.Context, owner, surface, _ string, _ int) ([]telemetry.AssistantConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []telemetry.AssistantConversation{}
	for _, c := range f.convs {
		if c.OwnerUserID == owner && (surface == "" || surface == c.Surface) {
			out = append(out, *c)
		}
	}
	return out, nil
}
func (f *asstStore) AppendAssistantMessage(_ context.Context, in telemetry.AssistantMessageInsert) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := telemetry.AssistantMessage{ID: f.nextID(), ConversationID: in.ConversationID, Seq: len(f.msgs[in.ConversationID]) + 1,
		TurnID: in.TurnID, Role: in.Role, Content: in.Content, Model: in.Model, Status: in.Status, ProviderState: in.ProviderState}
	f.msgs[in.ConversationID] = append(f.msgs[in.ConversationID], m)
	return m.ID, m.Seq, nil
}
func (f *asstStore) RecentAssistantMessages(ctx context.Context, id string, _ int) ([]telemetry.AssistantMessage, error) {
	return f.AssistantConversationMessages(ctx, id)
}
func (f *asstStore) AssistantConversationMessages(_ context.Context, id string) ([]telemetry.AssistantMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telemetry.AssistantMessage(nil), f.msgs[id]...), nil
}
func (f *asstStore) SetAssistantConversationTitle(context.Context, string, string) error { return nil }
func (f *asstStore) SetAssistantConversationSummary(context.Context, string, string, int) error {
	return nil
}
func (f *asstStore) SoftDeleteAssistantConversation(_ context.Context, id, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.convs[id]; ok && c.OwnerUserID == owner {
		delete(f.convs, id)
	}
	return nil
}
func (f *asstStore) DeleteAssistantConversationRow(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.convs, id)
	return nil
}
func (f *asstStore) SetAssistantFeedback(_ context.Context, msg, user, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for cid, list := range f.msgs {
		for _, m := range list {
			if m.ID == msg && m.Role == "assistant" && f.convs[cid] != nil && f.convs[cid].OwnerUserID == user {
				return nil
			}
		}
	}
	return telemetry.ErrAssistantMessageNotFound
}
func (f *asstStore) RecordAssistantUsage(_ context.Context, ev telemetry.AssistantUsageEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, ev)
	return nil
}
func (f *asstStore) AssistantSpendSince(context.Context, time.Time) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spend, nil
}

// QuotaStore + TurnUsageStore.
func (f *asstStore) Enabled() bool { return true }
func (f *asstStore) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	for _, u := range asstUsers {
		if u.FirebaseUID == uid {
			return u, nil
		}
	}
	return nil, nil
}
func (f *asstStore) IncrementUsage(_ context.Context, _ models.UsageSubjectKind, key, metric string, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counters[key+"|"+metric]++
	return f.counters[key+"|"+metric], nil
}
func (f *asstStore) DecrementUsage(_ context.Context, _ models.UsageSubjectKind, key, metric string, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counters[key+"|"+metric] > 0 {
		f.counters[key+"|"+metric]--
	}
	return f.counters[key+"|"+metric], nil
}
func (f *asstStore) UsageForDay(_ context.Context, _ models.UsageSubjectKind, key string, _ time.Time) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]int64{models.MetricAssistantTurns: f.counters[key+"|"+models.MetricAssistantTurns]}, nil
}
func (f *asstStore) turns(user string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counters[user+"|"+models.MetricAssistantTurns]
}

var asstUsers = map[string]*models.User{
	"free":  {ID: "user-free", FirebaseUID: "uid-free", Tier: "free"},
	"free2": {ID: "user-free-2", FirebaseUID: "uid-free2", Tier: "free"},
	"pro":   {ID: "user-pro", FirebaseUID: "uid-pro", Tier: "pro"},
	"op":    {ID: "user-op", FirebaseUID: "uid-op", Tier: "free"},
}

// ---- rig ----------------------------------------------------------------------------

type asstRig struct {
	r    *gin.Engine
	st   *asstStore
	p    *asstProvider
	cfg  config.AssistantConfig
	ledg *ledgerFake
}

const asstSchema = `{"type":"array","minItems":1,"items":{"type":"object","required":["op"],"properties":{"op":{"enum":["grey_except"]},"ranges":{"type":"array"}},"additionalProperties":false}}`

// asstEngineHook lets one test adjust the engine before the handler is built (MM116b).
var asstEngineHook func(*assistant.Engine, *asstStore)

func newAsstRig(t *testing.T, mutate func(*config.Config), rounds ...asstRound) *asstRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.OperatorEmails = []string{"op@example.com"}
	cfg.Assistant.Enabled, cfg.Assistant.APIKey = true, "test-key"
	cfg.Assistant.CopilotSurfaces = []string{"photo", "studio", "vector"}
	if mutate != nil {
		mutate(cfg)
	}
	registry := tiers.New(cfg)
	st := newAsstStore()
	p := &asstProvider{rounds: rounds}
	engine := &assistant.Engine{Provider: p, Knowledge: assistant.NewStubKnowledge(),
		Ops: assistant.NewOpsValidatorFrom(map[string][]byte{"photo": []byte(asstSchema)}), Store: st,
		Spend: assistant.NewSpendCap(cfg.Assistant.DailySpendCapUSD, st, nil), Cfg: cfg.Assistant,
		Background: func(f func()) { f() }}
	if asstEngineHook != nil {
		asstEngineHook(engine, st)
	}
	h := NewAssistantHandler(cfg.Assistant, engine, st, registry, quiet)
	rig := &asstRig{st: st, p: p, cfg: cfg.Assistant, ledg: installFakeLedger(t, nil)}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if u := asstUsers[c.GetHeader("X-Test-User")]; u != nil {
			email := u.ID + "@example.com"
			if c.GetHeader("X-Test-User") == "op" {
				email = "op@example.com"
			}
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: u.FirebaseUID, Email: email, EmailVerified: true})
		}
		c.Next()
	})
	r.Use(middleware.TierContext(middleware.TierConfig{Store: st, Registry: registry}, true))
	api := r.Group("/api")
	// The production wiring: /api/assistant/chat is a DeferQuota route.
	defer_ := middleware.DeferQuota(middleware.QuotaConfig{Store: st, Registry: registry, Enabled: true, IPHashSecret: []byte("s")})
	api.Use(func(c *gin.Context) {
		if c.Request.Method == http.MethodPost && c.Request.URL.Path == AssistantChatPath {
			defer_(c)
			return
		}
		c.Next()
	})
	RegisterAssistantRoutes(api, h)
	rig.r = r
	return rig
}

func (g *asstRig) do(method, path, user, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MM-Session-ID", "sess-1")
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	w := httptest.NewRecorder()
	g.r.ServeHTTP(w, req)
	return w
}

// sse parses the recorder's body into events.
func sse(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
			t.Fatalf("bad record %q", line)
		}
		out = append(out, m)
	}
	return out
}

func types(evs []map[string]any) string {
	var t []string
	for _, e := range evs {
		t = append(t, e["type"].(string))
	}
	return strings.Join(t, ",")
}

const asstGuideBody = `{"surface":"guide","locale":"en-US","page":{"path":"/"},"message":{"text":"where is compress video?"}}`
const asstPhotoBody = `{"surface":"photo","locale":"en-US","page":{"path":"/photo/p1"},"message":{"text":"grey except red"},"context":{"digest":"L1 base","projectId":"p1"}}`
const asstPlanBody = `{"surface":"photo","locale":"en-US","page":{"path":"/photo/p1"},"plan":true,"message":{"text":"cinematic"}}`

// ---- tests ------------------------------------------------------------------------

// Acceptance 2: ASSISTANT_ENABLED=false 404s every route except capabilities.
func TestAssistantDisabled404(t *testing.T) {
	g := newAsstRig(t, func(c *config.Config) { c.Assistant.Enabled = false })
	for _, rt := range [][2]string{{"POST", "/api/assistant/chat"}, {"GET", "/api/assistant/conversations"},
		{"GET", "/api/assistant/conversations/00000000-0000-4000-8000-000000000001"},
		{"DELETE", "/api/assistant/conversations/00000000-0000-4000-8000-000000000001"},
		{"POST", "/api/assistant/messages/00000000-0000-4000-8000-000000000001/feedback"}} {
		if w := g.do(rt[0], rt[1], "free", asstGuideBody); w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d", rt[0], rt[1], w.Code)
		}
	}
	w := g.do("GET", "/api/assistant/capabilities", "", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"enabled":false}` {
		t.Fatalf("capabilities = %d %s", w.Code, w.Body.String())
	}
	if g.p.calls != 0 {
		t.Fatal("no upstream call")
	}
}

// Acceptance 2: a missing key disables Medi (fail closed; no fallback key).
func TestAssistantMissingKeyDisables(t *testing.T) {
	g := newAsstRig(t, func(c *config.Config) { c.Assistant.APIKey = ""; c.OpenRouterAPIKey = "chat-lab-key" })
	if w := g.do("POST", "/api/assistant/chat", "", asstGuideBody); w.Code != http.StatusNotFound {
		t.Fatalf("chat = %d", w.Code)
	}
	if w := g.do("GET", "/api/assistant/capabilities", "", ""); !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("capabilities = %s", w.Body.String())
	}
}

// ASSISTANT_OPERATOR_ONLY: only the operator sees Medi.
func TestAssistantOperatorOnly(t *testing.T) {
	g := newAsstRig(t, func(c *config.Config) { c.Assistant.OperatorOnly = true }, asstRound{text: "hi"})
	if w := g.do("POST", "/api/assistant/chat", "pro", asstGuideBody); w.Code != http.StatusNotFound {
		t.Fatalf("pro = %d", w.Code)
	}
	if w := g.do("POST", "/api/assistant/chat", "op", asstGuideBody); w.Code != 200 || types(sse(t, w)) != "meta,delta,usage,done" {
		t.Fatalf("operator = %d %s", w.Code, w.Body.String())
	}
}

// Capabilities shape.
func TestAssistantCapabilities(t *testing.T) {
	g := newAsstRig(t, nil)
	w := g.do("GET", "/api/assistant/capabilities", "free", "")
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["promptVersion"] != assistant.PromptVersion() || body["promptVersion"] == "" {
		t.Errorf("promptVersion = %v", body["promptVersion"])
	}
	if body["enabled"] != true || body["frames"] != true || body["maxFrameBytes"].(float64) != 307200 || body["spendCapReached"] != false {
		t.Fatalf("capabilities = %v", body)
	}
	turns := body["turns"].(map[string]any)
	if turns["limit"].(float64) != 5 || turns["remaining"].(float64) != 5 || turns["resetsAt"] == "" {
		t.Errorf("turns = %v", turns)
	}
	cop := body["copilot"].(map[string]any)
	if cop["signedInOnly"] != true || len(cop["surfaces"].([]any)) != 3 || fmt.Sprint(cop["editSurfaces"]) != "[photo]" {
		t.Errorf("copilot = %v", cop)
	}
	if g := body["guide"].(map[string]any); g["enabled"] != true {
		t.Errorf("guide = %v", g)
	}
	w = g.do("GET", "/api/assistant/capabilities", "op", "")
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["turns"].(map[string]any)["limit"].(float64) != -1 {
		t.Errorf("operator turns = %v", body["turns"])
	}
}

// Acceptance 2: the charging matrix — guide free, copilot 1, plan 3, operator
// free, anonymous copilot 401, refund on upstream failure, continuations free.
func TestAssistantChargingMatrix(t *testing.T) {
	g := newAsstRig(t, nil,
		asstRound{text: "guide answer"},               // free guide
		asstRound{text: "copilot answer"},             // free copilot (1)
		asstRound{text: "plan"},                       // free plan (3)
		asstRound{preErr: errors.New("upstream 502")}, // refunded
		asstRound{text: "op answer"},                  // operator copilot
	)
	if w := g.do("POST", "/api/assistant/chat", "free", asstGuideBody); w.Code != 200 || g.st.turns("user-free") != 0 || !strings.Contains(w.Body.String(), "guide answer") {
		t.Fatalf("guide charged: %d turns=%d", w.Code, g.st.turns("user-free"))
	}
	if w := g.do("POST", "/api/assistant/chat", "free", asstPhotoBody); w.Code != 200 || g.st.turns("user-free") != 1 || !strings.Contains(w.Body.String(), "copilot answer") {
		t.Fatalf("copilot: %d turns=%d %s", w.Code, g.st.turns("user-free"), w.Body.String())
	}
	if w := g.do("POST", "/api/assistant/chat", "free", asstPlanBody); w.Code != 200 || g.st.turns("user-free") != 4 || !strings.Contains(w.Body.String(), "plan") {
		t.Fatalf("plan: %d turns=%d %s", w.Code, g.st.turns("user-free"), w.Body.String())
	}
	w := g.do("POST", "/api/assistant/chat", "free", asstPhotoBody)
	if evs := sse(t, w); types(evs) != "meta,error" || evs[1]["code"] != "upstream" || g.st.turns("user-free") != 4 {
		t.Fatalf("upstream failure: %s turns=%d", w.Body.String(), g.st.turns("user-free"))
	}
	if w := g.do("POST", "/api/assistant/chat", "op", asstPhotoBody); w.Code != 200 || g.st.turns("user-op") != 0 {
		t.Fatalf("operator charged: turns=%d", g.st.turns("user-op"))
	}
	if w := g.do("POST", "/api/assistant/chat", "", asstPhotoBody); w.Code != http.StatusUnauthorized ||
		!strings.Contains(w.Body.String(), "sign_in_required") || g.st.turns("sess-1") != 0 {
		t.Fatalf("anonymous copilot = %d %s", w.Code, w.Body.String())
	}
	// Over the allowance: one unit is left (free = 5) and a plan needs 3. It is
	// refused before anything is counted, so the last single turn survives.
	w = g.do("POST", "/api/assistant/chat", "free", asstPlanBody)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"metric":"assistant_turns"`) || g.st.turns("user-free") != 4 {
		t.Fatalf("plan over limit = %d %s turns=%d", w.Code, w.Body.String(), g.st.turns("user-free"))
	}
	g.p.rounds = append(g.p.rounds, asstRound{text: "last one"})
	if w := g.do("POST", "/api/assistant/chat", "free", asstPhotoBody); w.Code != 200 || g.st.turns("user-free") != 5 {
		t.Fatalf("last single turn = %d turns=%d", w.Code, g.st.turns("user-free"))
	}
	if w := g.do("POST", "/api/assistant/chat", "free", asstPhotoBody); w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th turn = %d", w.Code)
	}
}

// Acceptance 2: an edits turn then its editResult continuation, not charged.
func TestAssistantEditsAndContinuation(t *testing.T) {
	g := newAsstRig(t, nil,
		asstRound{text: "Done.", call: &assistant.ToolCall{ID: "e1", Name: "apply_edits", Arguments: `{"summary":"Grey except red","reason":"You asked to keep only the red","ops":[{"op":"grey_except","ranges":["red"]}]}`}},
		asstRound{text: "The jacket stays red."})
	w := g.do("POST", "/api/assistant/chat", "free", asstPhotoBody)
	evs := sse(t, w)
	if types(evs) != "meta,delta,edits,usage,done" || evs[4]["awaiting"] != "edit_result" {
		t.Fatalf("edits turn = %s", w.Body.String())
	}
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type = %s", w.Header().Get("Content-Type"))
	}
	conv, turn := evs[0]["conversationId"].(string), evs[0]["turnId"].(string)
	body := fmt.Sprintf(`{"surface":"photo","locale":"en-US","page":{"path":"/photo/p1"},"conversationId":%q,"turnId":%q,"editResult":{"callId":"e1","applied":true}}`, conv, turn)
	w = g.do("POST", "/api/assistant/chat", "free", body)
	if evs := sse(t, w); types(evs) != "meta,delta,usage,done" || evs[0]["turnId"] != turn {
		t.Fatalf("continuation = %s", w.Body.String())
	}
	if g.st.turns("user-free") != 1 {
		t.Fatalf("turns = %d, want 1 (the continuation is free)", g.st.turns("user-free"))
	}
	// Replaying the same result is stale.
	if w := g.do("POST", "/api/assistant/chat", "free", body); w.Code != http.StatusConflict {
		t.Fatalf("replay = %d", w.Code)
	}
	// Another user cannot continue it.
	if w := g.do("POST", "/api/assistant/chat", "free2", body); w.Code != http.StatusNotFound {
		t.Fatalf("stranger = %d", w.Code)
	}
	// Ledger: one assistant_conversation row for the new conversation.
	found := false
	for _, r := range g.ledg.rows {
		if r.Kind == retention.KindAssistantConversation && r.Locator == conv && r.ProjectID == conv && r.OwnerUserID == "user-free" {
			found = true
		}
	}
	if !found {
		t.Errorf("ledger rows = %+v", g.ledg.rows)
	}
}

// Acceptance 2: conversations are owner-only (404 otherwise); delete; feedback.
func TestAssistantConversationsOwnerOnly(t *testing.T) {
	g := newAsstRig(t, nil, asstRound{text: "hello"})
	evs := sse(t, g.do("POST", "/api/assistant/chat", "free", asstGuideBody))
	conv := evs[0]["conversationId"].(string)
	msgID := evs[len(evs)-1]["messageId"].(string)

	if w := g.do("GET", "/api/assistant/conversations?surface=guide", "free", ""); w.Code != 200 || !strings.Contains(w.Body.String(), conv) {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	if w := g.do("GET", "/api/assistant/conversations", "free2", ""); !strings.Contains(w.Body.String(), `"conversations":[]`) {
		t.Fatalf("stranger list = %s", w.Body.String())
	}
	w := g.do("GET", "/api/assistant/conversations/"+conv, "free", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hello") || strings.Contains(w.Body.String(), "provider") {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
	for _, u := range []string{"free2", ""} {
		code := g.do("GET", "/api/assistant/conversations/"+conv, u, "").Code
		if (u == "" && code != http.StatusUnauthorized) || (u != "" && code != http.StatusNotFound) {
			t.Errorf("get as %q = %d", u, code)
		}
	}
	if w := g.do("POST", "/api/assistant/messages/"+msgID+"/feedback", "free2", `{"rating":"up"}`); w.Code != http.StatusNotFound {
		t.Errorf("stranger feedback = %d", w.Code)
	}
	if w := g.do("POST", "/api/assistant/messages/"+msgID+"/feedback", "free", `{"rating":"meh"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad rating = %d", w.Code)
	}
	if w := g.do("POST", "/api/assistant/messages/"+msgID+"/feedback", "free", `{"rating":"down","reason":"wrong link"}`); w.Code != 200 {
		t.Errorf("feedback = %d %s", w.Code, w.Body.String())
	}
	if w := g.do("DELETE", "/api/assistant/conversations/"+conv, "free2", ""); w.Code != http.StatusNotFound {
		t.Errorf("stranger delete = %d", w.Code)
	}
	if w := g.do("DELETE", "/api/assistant/conversations/"+conv, "free", ""); w.Code != 200 {
		t.Errorf("delete = %d", w.Code)
	}
	if w := g.do("GET", "/api/assistant/conversations/"+conv, "free", ""); w.Code != http.StatusNotFound {
		t.Errorf("after delete = %d", w.Code)
	}
}

// Acceptance 2: guests are answered and never persisted.
func TestAssistantGuestNotPersisted(t *testing.T) {
	g := newAsstRig(t, nil, asstRound{text: "hi"})
	w := g.do("POST", "/api/assistant/chat", "", asstGuideBody)
	if evs := sse(t, w); types(evs) != "meta,delta,usage,done" || evs[0]["conversationId"] != nil {
		t.Fatalf("guest = %s", w.Body.String())
	}
	if len(g.st.convs) != 0 || len(g.st.msgs) != 0 || len(g.ledg.rows) != 0 {
		t.Fatal("nothing stored for a guest")
	}
	if len(g.st.usage) != 1 || g.st.usage[0].UserID != "" || g.st.usage[0].VisitorHash == "" {
		t.Fatalf("guest usage = %+v", g.st.usage)
	}
	// A guest cannot name a conversation.
	b := `{"surface":"guide","conversationId":"00000000-0000-4000-8000-000000000001","message":{"text":"x"}}`
	if w := g.do("POST", "/api/assistant/chat", "", b); w.Code != http.StatusUnauthorized {
		t.Errorf("guest conversation = %d", w.Code)
	}
}

// Acceptance 2: spend cap — copilot 503 assistant_paused (not charged), guide static.
func TestAssistantSpendCapPaused(t *testing.T) {
	g := newAsstRig(t, nil)
	g.st.spend = 10
	w := g.do("POST", "/api/assistant/chat", "free", asstPhotoBody)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "assistant_paused") || g.st.turns("user-free") != 0 {
		t.Fatalf("copilot at cap = %d %s turns=%d", w.Code, w.Body.String(), g.st.turns("user-free"))
	}
	w = g.do("POST", "/api/assistant/chat", "", asstGuideBody)
	if types(sse(t, w)) != "meta,delta,usage,done" || g.p.calls != 0 {
		t.Fatalf("guide at cap = %s calls=%d", w.Body.String(), g.p.calls)
	}
	cw := g.do("GET", "/api/assistant/capabilities", "", "")
	if !strings.Contains(cw.Body.String(), `"spendCapReached":true`) {
		t.Errorf("capabilities = %s", cw.Body.String())
	}
}

// Body limits answer before any charge.
func TestAssistantBodyLimits(t *testing.T) {
	g := newAsstRig(t, nil)
	long := fmt.Sprintf(`{"surface":"photo","message":{"text":%q}}`, strings.Repeat("a", 4001))
	if w := g.do("POST", "/api/assistant/chat", "free", long); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "message_too_long") {
		t.Errorf("long = %d %s", w.Code, w.Body.String())
	}
	huge := fmt.Sprintf(`{"surface":"photo","message":{"text":"x"},"context":{"digest":%q}}`, strings.Repeat("d", 600<<10))
	if w := g.do("POST", "/api/assistant/chat", "free", huge); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge body = %d", w.Code)
	}
	if w := g.do("POST", "/api/assistant/chat", "free", `{"surface":"photo","message":{"text":"x"},"frame":{"jpegBase64":"iVBORw0KGgo="}}`); w.Code != http.StatusBadRequest {
		t.Errorf("png frame = %d", w.Code)
	}
	if g.st.turns("user-free") != 0 {
		t.Error("refused bodies are never charged")
	}
}

// The DR chat lab and Medi share the OpenRouter client; the chat lab's own
// client keeps its attribution (services/openrouter client_test pins the title).
func TestAssistantAttributionTitle(t *testing.T) {
	if assistant.AttributionTitle != "Media Manipulator" {
		t.Fatal("Medi requests are attributed to Media Manipulator")
	}
}

// The wire format: `data: {json}\n\n` records with exactly the design §10
// field names. (The ticket's sample transcript is this test's output.)
func TestAssistantSSETranscript(t *testing.T) {
	g := newAsstRig(t, nil, asstRound{text: "Open Compress video."},
		asstRound{text: "Done.", call: &assistant.ToolCall{ID: "e1", Name: "apply_edits", Arguments: `{"summary":"Grey except red","reason":"You asked to keep only the red","ops":[{"op":"grey_except","ranges":["red"]}]}`}})
	text := g.do("POST", "/api/assistant/chat", "", asstGuideBody).Body.String()
	edits := g.do("POST", "/api/assistant/chat", "free", asstPhotoBody).Body.String()
	for _, want := range []string{
		`data: {"type":"meta","conversationId":null,"turnId":"`,
		`data: {"type":"delta","text":"Open Compress video."}` + "\n\n",
		`data: {"type":"usage","inputTokens":50,"outputTokens":5,"cachedTokens":0,"costUsd":0.0002}` + "\n\n",
		`data: {"type":"done"}` + "\n\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text transcript missing %q:\n%s", want, text)
		}
	}
	for _, want := range []string{
		`"model":"anthropic/claude-sonnet-5.5","promptVersion":"` + assistant.PromptVersion() + `"}`,
		`data: {"type":"edits","callId":"e1","summary":"Grey except red","reason":"You asked to keep only the red","ops":[{"op":"grey_except","ranges":["red"]}]}` + "\n\n",
		`data: {"type":"done","awaiting":"edit_result","messageId":"`,
	} {
		if !strings.Contains(edits, want) {
			t.Errorf("edits transcript missing %q:\n%s", want, edits)
		}
	}
	if testing.Verbose() {
		t.Logf("text turn:\n%s\nedits turn:\n%s", text, edits)
	}
}
