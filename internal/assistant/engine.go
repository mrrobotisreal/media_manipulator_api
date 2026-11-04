package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// Engine runs one Medi request end to end: prompt, the bounded upstream loop
// with server tools, client-tool validation (one retry), persistence exactly
// once, usage rows, spend reservations and refunds. The HTTP handler owns
// gating, charging and the SSE writer.
type Engine struct {
	Provider  Provider
	Knowledge Knowledge
	Ops       *OpsValidator
	Store     Store // nil = nothing persisted (no database)
	Spend     *SpendCap
	Cfg       config.AssistantConfig
	Logger    *slog.Logger
	// Shutdown ends in-flight streams as interrupted (the app context).
	Shutdown <-chan struct{}
	// Keepalive is the `: ping` interval (default 15 s).
	Keepalive time.Duration
	// Background runs fire-and-forget work (titles); tests run it inline.
	Background func(func())
	// OnTurnCounted is told once per completed upstream request (MM107 turns it
	// into the server analytics event).
	OnTurnCounted func(t TurnReport)
}

// Refunder gives back allowance units (*middleware.QuotaCharge).
type Refunder interface {
	Refund(reason string) bool
}

// Turn is one validated request plus what the handler resolved.
type Turn struct {
	Body          ChatBody
	FrameURL      string
	FrameAfterURL string
	// UserID is the mm_users id ("" = guest: nothing is stored).
	UserID      string
	VisitorHash string
	Live        LiveContext
	// Charges are the allowance units spent on this request (refunded when the
	// upstream fails before any content reached the user).
	Charges []Refunder
	// Conversation is the loaded conversation of an editResult continuation.
	Conversation *telemetry.AssistantConversation
	// RegisterConversation records a new conversation in the retention ledger
	// (kind assistant_conversation) under the caller's window.
	RegisterConversation func(ctx context.Context, conversationID string)
	// AnalyticsVisitorID / AnalyticsSessionID are the raw X-MM-Visitor-ID and
	// X-MM-Session-ID headers, passed through to TurnReport for the server
	// analytics event only (MM107); never stored.
	AnalyticsVisitorID string
	AnalyticsSessionID string
}

// TurnReport summarises a finished request for analytics (no content).
type TurnReport struct {
	Surface      string
	ModelTier    string // guide | copilot | plan
	Outcome      string // text | edits | action | ask | error | interrupted | static | out_of_scope
	ToolRounds   int
	CostUSD      float64
	InputTokens  int
	CachedTokens int
	Latency      time.Duration
	// Tier and the analytics ids come from the Turn (MM107); the ids are ""
	// when the caller sent none (the emitter then drops the event).
	Tier      string
	VisitorID string
	SessionID string
}

// historyWindow is how many stored rows feed the model (design §7.3: the
// last 12 messages; tool rows ride along).
const historyWindow = 24

// digestStoreBytes is how much of a digest is stored, for debugging only.
const digestStoreBytes = 2048

// ErrStaleEditResult is an editResult that does not answer the turn's latest
// apply_edits call.
var ErrStaleEditResult = errors.New("assistant: editResult does not match the latest edit")

// ErrEditRoundsExceeded is an editResult past ASSISTANT_MAX_EDIT_ROUNDS.
var ErrEditRoundsExceeded = errors.New("assistant: edit rounds exceeded")

// modelFor picks model, effort and output cap (design §6).
func (e *Engine) modelFor(surface string, plan bool) (model, effort, tier string, maxTokens int) {
	switch {
	case surface == SurfaceGuide:
		return e.Cfg.ModelGuide, "low", "guide", e.Cfg.MaxTokensGuide
	case plan:
		return e.Cfg.ModelPlan, "high", "plan", e.Cfg.MaxTokensPlan
	default:
		return e.Cfg.ModelCopilot, "medium", "copilot", e.Cfg.MaxTokensCopilot
	}
}

func (e *Engine) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

func (e *Engine) bg(f func()) {
	if e.Background != nil {
		e.Background(f)
		return
	}
	go f()
}

// storedContent is the jsonb shape of assistant_messages.content.
type storedContent struct {
	Text       string        `json:"text,omitempty"`
	Page       string        `json:"page,omitempty"`
	HasFrame   bool          `json:"hasFrame,omitempty"`
	Digest     string        `json:"digest,omitempty"`
	Plan       bool          `json:"plan,omitempty"`
	ToolCalls  []ToolCall    `json:"toolCalls,omitempty"`
	Actions    []storedEvent `json:"actions,omitempty"`
	ToolCallID string        `json:"toolCallId,omitempty"`
	Name       string        `json:"name,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// storedEvent is the human summary of one client action ("op summaries").
type storedEvent struct {
	CallID  string `json:"callId"`
	Kind    string `json:"kind"`
	Summary string `json:"summary,omitempty"`
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// truncateBytes cuts s to at most n bytes on a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// historyMessages rebuilds provider messages from stored rows: starts at a user
// message, and gives every tool call a result (a synthetic one for client
// calls the user answered with a new message), as the API requires. openCall
// (an editResult continuation's call id) is left open: the request itself
// carries its real result.
func historyMessages(rows []telemetry.AssistantMessage, openCall ...string) []Message {
	keepOpen := ""
	if len(openCall) > 0 {
		keepOpen = openCall[0]
	}
	start := 0
	for start < len(rows) && rows[start].Role != "user" {
		start++
	}
	var out []Message
	pending := map[string]string{} // call id -> tool name, awaiting a result
	var pendingOrder []string
	flush := func() {
		for _, id := range pendingOrder {
			name, open := pending[id]
			if !open || id == keepOpen {
				continue
			}
			out = append(out, Message{Role: "tool", ToolCallID: id, Text: syntheticResult(name)})
			delete(pending, id)
		}
		pendingOrder = nil
	}
	for _, r := range rows[start:] {
		var c storedContent
		if json.Unmarshal(r.Content, &c) != nil {
			continue
		}
		switch r.Role {
		case "user":
			flush()
			out = append(out, Message{Role: "user", Text: c.Text})
		case "assistant":
			flush()
			if c.Text == "" && len(c.ToolCalls) == 0 {
				continue
			}
			out = append(out, Message{Role: "assistant", Text: c.Text, ToolCalls: c.ToolCalls, ProviderState: r.ProviderState})
			for _, tc := range c.ToolCalls {
				pending[tc.ID] = tc.Name
				pendingOrder = append(pendingOrder, tc.ID)
			}
		case "tool":
			if _, open := pending[c.ToolCallID]; !open {
				continue
			}
			delete(pending, c.ToolCallID)
			out = append(out, Message{Role: "tool", ToolCallID: c.ToolCallID, Text: c.Text})
		}
	}
	flush()
	return out
}

func syntheticResult(name string) string {
	switch name {
	case ToolApplyEdits:
		return "No result was reported for this edit."
	case ToolAskUser:
		return "The question was shown; the user's next message answers it."
	case ToolNavigate, ToolOpenTool, ToolSuggestAction:
		return "Shown to the user as a button."
	default:
		return "Not run."
	}
}

// editResultText is the tool result the model reads after an edit.
func editResultText(r *EditResult, hasFrame bool) string {
	var b strings.Builder
	if r.Applied {
		b.WriteString("Applied as one undo step.")
	} else {
		b.WriteString("Not applied (nothing changed).")
	}
	if len(r.Errors) > 0 {
		b.WriteString(" Errors: ")
		b.WriteString(strings.Join(r.Errors, "; "))
	}
	if hasFrame {
		b.WriteString(" The preview after the edit is attached in the next message.")
	}
	return b.String()
}

// countApplyEdits counts apply_edits calls already made in a turn.
func countApplyEdits(rows []telemetry.AssistantMessage, turnID string) int {
	n := 0
	for _, r := range rows {
		if r.Role != "assistant" || r.TurnID != turnID {
			continue
		}
		var c storedContent
		if json.Unmarshal(r.Content, &c) != nil {
			continue
		}
		for _, tc := range c.ToolCalls {
			if tc.Name == ToolApplyEdits {
				n++
			}
		}
	}
	return n
}

// latestEdit finds the newest assistant row; it must belong to turnID and hold
// the apply_edits call callID, with no result stored yet.
func latestEdit(rows []telemetry.AssistantMessage, turnID, callID string) (telemetry.AssistantMessage, bool) {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Role == "tool" {
			var c storedContent
			if json.Unmarshal(r.Content, &c) == nil && c.ToolCallID == callID {
				return r, false // already answered
			}
			continue
		}
		if r.Role != "assistant" {
			return r, false
		}
		if r.TurnID != turnID {
			return r, false
		}
		var c storedContent
		if json.Unmarshal(r.Content, &c) != nil {
			return r, false
		}
		for _, tc := range c.ToolCalls {
			if tc.ID == callID && tc.Name == ToolApplyEdits {
				return r, true
			}
		}
		return r, false
	}
	return telemetry.AssistantMessage{}, false
}

// Prepared is what PrepareEditResult loads for a continuation.
type Prepared struct {
	Rows      []telemetry.AssistantMessage
	EditCount int
	Model     string
}

// PrepareEditResult checks a continuation before anything is streamed:
// ErrStaleEditResult when callId is not the turn's latest open edit,
// ErrEditRoundsExceeded when the turn already used every edit round's result.
func (e *Engine) PrepareEditResult(ctx context.Context, conv *telemetry.AssistantConversation, turnID, callID string) (*Prepared, error) {
	rows, err := e.Store.RecentAssistantMessages(ctx, conv.ID, historyWindow)
	if err != nil {
		return nil, err
	}
	row, ok := latestEdit(rows, turnID, callID)
	if !ok {
		return nil, ErrStaleEditResult
	}
	n := countApplyEdits(rows, turnID)
	if n > e.Cfg.MaxEditRounds {
		return nil, ErrEditRoundsExceeded
	}
	return &Prepared{Rows: rows, EditCount: n, Model: row.Model}, nil
}

// run state of one request.
type run struct {
	e        *Engine
	t        *Turn
	em       Emitter
	ctx      context.Context
	surface  string
	model    string
	effort   string
	tier     string
	maxTok   int
	turnID   string
	convID   string
	started  time.Time
	text     strings.Builder
	content  bool // anything reached the user (delta, edits, action, ask)
	usage    Usage
	cost     float64
	rounds   int
	persist  bool
	saved    bool
	msgID    string
	newConv  bool
	userText string
}

// Run executes one request. prep is non-nil for an editResult continuation.
// It writes every event (meta first) and returns when the stream is over.
func (e *Engine) Run(ctx context.Context, t *Turn, em Emitter, prep *Prepared) {
	model, effort, tier, maxTok := e.modelFor(t.Body.Surface, t.Body.Plan)
	if prep != nil && prep.Model != "" {
		model = prep.Model
		if model == e.Cfg.ModelPlan {
			effort, tier, maxTok = "high", "plan", e.Cfg.MaxTokensPlan
		}
	}
	r := &run{e: e, t: t, em: em, ctx: ctx, surface: t.Body.Surface, model: model, effort: effort, tier: tier,
		maxTok: maxTok, started: time.Now(), persist: t.UserID != "" && e.Store != nil}
	r.turnID = t.Body.TurnID
	if prep == nil {
		r.turnID = uuid.NewString()
	}
	r.exec(prep)
}

func (r *run) refund(reason string) {
	for _, c := range r.t.Charges {
		if c != nil {
			c.Refund(reason)
		}
	}
}

func (r *run) send(ev any) bool {
	if err := r.em.Send(ev); err != nil {
		return false
	}
	return true
}

func (r *run) bgCtx() context.Context { return context.WithoutCancel(r.ctx) }

func (r *run) exec(prep *Prepared) {
	b := &r.t.Body
	e := r.e
	var history []Message
	var current []Message

	// Conversation + the user's message, persisted BEFORE upstream (chat lab rule).
	if r.persist {
		conv := r.t.Conversation
		if conv == nil && b.ConversationID != "" {
			c, err := e.Store.GetAssistantConversation(r.ctx, b.ConversationID, r.t.UserID)
			if err != nil {
				r.failPre(err)
				return
			}
			conv = c
		}
		if conv == nil {
			kind, pid := "", ""
			if b.Surface != SurfaceGuide && b.Context != nil && b.Context.ProjectID != "" {
				kind, pid = b.Surface, clip(b.Context.ProjectID, 100)
			}
			c, err := e.Store.CreateAssistantConversation(r.ctx, telemetry.AssistantConversationInsert{
				OwnerUserID: r.t.UserID, Surface: b.Surface, ProjectKind: kind, ProjectID: pid})
			if err != nil {
				r.failPre(err)
				return
			}
			conv, r.newConv = c, true
			if r.t.RegisterConversation != nil {
				r.t.RegisterConversation(r.bgCtx(), c.ID)
			}
		}
		r.convID = conv.ID
		rows := []telemetry.AssistantMessage(nil)
		if prep != nil {
			rows = prep.Rows
		} else if !r.newConv {
			var err error
			if rows, err = e.Store.RecentAssistantMessages(r.ctx, conv.ID, historyWindow); err != nil {
				r.failPre(err)
				return
			}
		}
		open := ""
		if b.EditResult != nil {
			open = b.EditResult.CallID
		}
		history = historyMessages(rows, open)
	}

	if b.Message != nil {
		r.userText = b.Message.Text
		um := Message{Role: "user", Text: b.Message.Text}
		if r.t.FrameURL != "" {
			um.Images = []string{r.t.FrameURL}
		}
		current = append(current, um)
		if r.persist {
			sc := storedContent{Text: b.Message.Text, Page: b.Page.Path, HasFrame: r.t.FrameURL != "", Plan: b.Plan}
			if b.Context != nil && b.Context.Digest != "" {
				sc.Digest = truncateBytes(b.Context.Digest, digestStoreBytes)
			}
			if _, _, err := e.Store.AppendAssistantMessage(r.ctx, telemetry.AssistantMessageInsert{
				ConversationID: r.convID, TurnID: r.turnID, Role: "user", Content: mustJSON(sc)}); err != nil {
				r.failPre(err)
				return
			}
		}
	} else {
		er := b.EditResult
		resText := editResultText(er, r.t.FrameAfterURL != "")
		current = append(current, Message{Role: "tool", ToolCallID: er.CallID, Text: resText})
		if r.t.FrameAfterURL != "" {
			current = append(current, Message{Role: "user", Text: "Preview after the edit:", Images: []string{r.t.FrameAfterURL}})
		}
		if r.persist {
			if _, _, err := e.Store.AppendAssistantMessage(r.ctx, telemetry.AssistantMessageInsert{
				ConversationID: r.convID, TurnID: r.turnID, Role: "tool",
				Content: mustJSON(storedContent{ToolCallID: er.CallID, Name: ToolApplyEdits, Text: resText, HasFrame: r.t.FrameAfterURL != ""})}); err != nil {
				r.failPre(err)
				return
			}
		}
	}

	var convPtr *string
	if r.convID != "" {
		id := r.convID
		convPtr = &id
	}
	if !r.send(MetaEvent{Type: "meta", ConversationID: convPtr, TurnID: r.turnID, Model: r.model}) {
		r.finish("interrupted", "")
		return
	}

	// Retrieval (design §7.3: k = 4 guide, 3 copilot) on the new message only.
	live := r.t.Live
	live.Surface, live.Plan = b.Surface, b.Plan
	live.Locale, live.LocaleName = b.Locale, LocaleName(b.Locale)
	live.PagePath, live.PageTitle = b.Page.Path, b.Page.Title
	live.HasFrame = r.t.FrameURL != ""
	if b.Context != nil {
		live.Digest = b.Context.Digest
	}
	if b.Message != nil {
		k := 3
		if b.Surface == SurfaceGuide {
			k = 4
		}
		live.Hits = e.Knowledge.Search(b.Message.Text, b.Surface, k)
		live.LocalNames = localNames(e.Knowledge, live.Hits, b.Locale)
	}

	// The guide's scope gate (MM105 decision 7): clearly off-topic messages get
	// the localized out-of-scope line without a model call (not charged; the
	// guide never is). Edit-result continuations never reach here as messages.
	if b.Surface == SurfaceGuide && b.Message != nil && !e.Knowledge.InScope(b.Message.Text) {
		r.outOfScope()
		return
	}

	// At the cap before any call: guide answers from retrieval, copilot pauses.
	if e.Spend.Reached(r.ctx) {
		r.paused(live.Hits)
		return
	}

	tools := ToolsFor(b.Surface, e.Ops)
	if prep != nil && prep.EditCount >= e.Cfg.MaxEditRounds {
		// The last edit round's result: the model may only confirm in words.
		tools = withoutTool(tools, ToolApplyEdits)
	}
	req := Request{Model: r.model, MaxTokens: r.maxTok, Effort: r.effort, Tools: tools,
		System: []Block{CachedBlock(b.Surface, e.Knowledge), VolatileBlock(live)}}
	msgs := append(history, current...)
	retried := false
	maxRounds := e.Cfg.ToolMaxRounds
	if maxRounds < 1 {
		maxRounds = 1
	}

	for {
		r.rounds++
		req.Messages = msgs
		inTok := EstimateTokens(requestChars(req), imageCount(req))
		res, ok := e.Spend.Reserve(r.ctx, WorstCaseUSD(r.model, inTok, r.maxTok))
		if !ok {
			r.paused(live.Hits)
			return
		}
		out := r.pump(req, res)
		switch out.kind {
		case pumpGone:
			r.finish("interrupted", "")
			return
		case pumpError:
			e.logger().Warn("assistant: upstream failed", "surface", r.surface, "model", r.model, "error", out.err.Error())
			r.fail(CodeUpstream, "Medi could not answer right now. Please try again in a moment.")
			return
		}
		calls := out.calls
		if len(calls) == 0 {
			r.complete(nil, out.state, "text")
			return
		}
		var server, client []ToolCall
		for _, c := range calls {
			if IsServerTool(c.Name) {
				server = append(server, c)
			} else {
				client = append(client, c)
			}
		}
		if len(client) == 0 {
			if r.rounds >= maxRounds {
				r.fail(CodeToolCap, "Medi looked things up too many times for one answer. Please ask again more specifically.")
				return
			}
			msgs = append(msgs, Message{Role: "assistant", Text: out.text, ToolCalls: calls, ProviderState: out.state})
			for _, c := range server {
				if !r.send(ToolEvent{Type: "tool", Name: c.Name, Status: "running"}) {
					r.finish("interrupted", "")
					return
				}
				result, _ := executeServerTool(c, r.surface, e.Knowledge)
				msgs = append(msgs, Message{Role: "tool", ToolCallID: c.ID, Text: result})
				if !r.send(ToolEvent{Type: "tool", Name: c.Name, Status: "ok"}) {
					r.finish("interrupted", "")
					return
				}
			}
			continue
		}

		// Client tools end the upstream stream. Validate every call first.
		actions := make([]ClientAction, 0, len(client))
		var errs []string
		invalid := map[string]string{}
		for _, c := range client {
			a, err := validateClientCall(c, r.surface, tools, e.Ops, e.Knowledge.Routes())
			if err != nil {
				invalid[c.ID] = err.Error()
				errs = append(errs, err.Error())
				continue
			}
			actions = append(actions, a)
		}
		if len(invalid) > 0 {
			if retried || r.rounds >= maxRounds {
				r.fail(CodeInvalid, "Medi could not produce a valid edit for that. Try rephrasing, or ask for a smaller change.")
				return
			}
			retried = true
			msgs = append(msgs, Message{Role: "assistant", Text: out.text, ToolCalls: calls, ProviderState: out.state})
			for _, c := range calls {
				text := "Not run: another call in this round was invalid; send all calls again."
				switch {
				case invalid[c.ID] != "":
					text = "Error: " + invalid[c.ID] + ". Fix the arguments and call again."
				case IsServerTool(c.Name):
					text, _ = executeServerTool(c, r.surface, e.Knowledge)
				}
				msgs = append(msgs, Message{Role: "tool", ToolCallID: c.ID, Text: text})
			}
			continue
		}
		// Server calls in the same round: run them now and store their results,
		// so the history stays well-formed.
		var serverResults []Message
		for _, c := range server {
			result, _ := executeServerTool(c, r.surface, e.Knowledge)
			serverResults = append(serverResults, Message{Role: "tool", ToolCallID: c.ID, Text: result})
		}
		outcome := ""
		for _, a := range actions {
			var ev any
			switch a.Name {
			case ToolApplyEdits:
				ev, outcome = EditsEvent{Type: "edits", CallID: a.CallID, Summary: a.Summary, Ops: a.Ops}, "edits"
			case ToolAskUser:
				ev = AskEvent{Type: "ask", CallID: a.CallID, Question: a.Question, Choices: a.Choices}
				if outcome == "" {
					outcome = "ask"
				}
			default:
				ev = ActionEvent{Type: "action", CallID: a.CallID, Kind: a.Kind, Label: a.Label, Params: a.Params}
				if outcome == "" || outcome == "ask" {
					outcome = "action"
				}
			}
			if !r.send(ev) {
				r.finish("interrupted", "")
				return
			}
			r.content = true
		}
		r.complete(&roundCalls{calls: calls, actions: actions, server: serverResults}, out.state, outcome)
		return
	}
}

func withoutTool(tools []ToolDef, name string) []ToolDef {
	out := make([]ToolDef, 0, len(tools))
	for _, t := range tools {
		if t.Name != name {
			out = append(out, t)
		}
	}
	return out
}

func requestChars(req Request) int {
	n := 0
	for _, b := range req.System {
		n += len(b.Text)
	}
	for _, m := range req.Messages {
		n += len(m.Text)
		for _, tc := range m.ToolCalls {
			n += len(tc.Arguments) + len(tc.Name)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Parameters) + len(t.Description)
	}
	return n
}

func imageCount(req Request) int {
	n := 0
	for _, m := range req.Messages {
		n += len(m.Images)
	}
	return n
}

type pumpKind int

const (
	pumpDone pumpKind = iota
	pumpGone
	pumpError
)

type pumpResult struct {
	kind  pumpKind
	text  string
	calls []ToolCall
	state json.RawMessage
	err   error
}

// pump drains one upstream call, forwarding deltas, and settles its spend
// reservation and usage row whatever happens.
func (r *run) pump(req Request, res *Reservation) pumpResult {
	e := r.e
	var callUsage *Usage
	settle := func() {
		cost, estimated := 0.0, false
		if callUsage != nil {
			cost = callUsage.CostUSD
			if !callUsage.Reported {
				cost, estimated = EstimateUSD(r.model, *callUsage), true
			}
			r.usage.InputTokens += callUsage.InputTokens
			r.usage.OutputTokens += callUsage.OutputTokens
			r.usage.CachedTokens += callUsage.CachedTokens
			r.cost += cost
			r.recordUsage("chat", r.model, *callUsage, cost, estimated)
		}
		res.Settle(cost)
	}
	defer settle()

	stream, err := e.Provider.Stream(r.ctx, req)
	if err != nil {
		if r.ctx.Err() != nil {
			return pumpResult{kind: pumpGone}
		}
		return pumpResult{kind: pumpError, err: err}
	}
	defer stream.Close()

	type item struct {
		ev  Event
		err error
	}
	items := make(chan item, 8)
	go func() {
		defer close(items)
		for {
			ev, err := stream.Next()
			select {
			case items <- item{ev, err}:
			case <-r.ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	every := e.Keepalive
	if every <= 0 {
		every = 15 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	var text strings.Builder
	var out pumpResult
	for {
		select {
		case <-r.ctx.Done():
			return pumpResult{kind: pumpGone}
		case <-e.Shutdown:
			return pumpResult{kind: pumpGone}
		case <-tick.C:
			if r.em.Ping() != nil {
				return pumpResult{kind: pumpGone}
			}
		case it, open := <-items:
			if !open {
				return pumpResult{kind: pumpGone}
			}
			if it.err != nil {
				if errors.Is(it.err, io.EOF) {
					out.kind, out.text = pumpDone, text.String()
					return out
				}
				if r.ctx.Err() != nil {
					return pumpResult{kind: pumpGone}
				}
				return pumpResult{kind: pumpError, err: it.err}
			}
			switch it.ev.Kind {
			case EventDelta:
				text.WriteString(it.ev.Text)
				r.text.WriteString(it.ev.Text)
				r.content = true
				if !r.send(DeltaEvent{Type: "delta", Text: it.ev.Text}) {
					return pumpResult{kind: pumpGone}
				}
			case EventUsage:
				callUsage = it.ev.Usage
			case EventDone:
				out.calls, out.state = it.ev.ToolCalls, it.ev.ProviderState
			}
		}
	}
}

func (r *run) recordUsage(kind, model string, u Usage, cost float64, estimated bool) {
	if r.e.Store == nil {
		return
	}
	ev := telemetry.AssistantUsageEvent{UserID: r.t.UserID, VisitorHash: r.t.VisitorHash, Surface: r.surface, Kind: kind,
		Model: model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.CachedTokens,
		CostUSD: cost, Estimated: estimated}
	if err := r.e.Store.RecordAssistantUsage(r.bgCtx(), ev); err != nil && !errors.Is(err, telemetry.ErrNoStore) {
		r.e.logger().Warn("assistant: usage row failed", "error", err.Error())
	}
}

type roundCalls struct {
	calls   []ToolCall
	actions []ClientAction
	server  []Message
}

// complete persists the assistant message, then sends usage and done.
func (r *run) complete(rc *roundCalls, state json.RawMessage, outcome string) {
	var awaiting string
	sc := storedContent{Text: r.text.String()}
	if rc != nil {
		sc.ToolCalls = rc.calls
		for _, a := range rc.actions {
			se := storedEvent{CallID: a.CallID, Kind: a.Kind, Summary: a.Summary}
			switch a.Name {
			case ToolAskUser:
				se.Kind, se.Summary = "ask", a.Question
			case ToolNavigate, ToolOpenTool, ToolSuggestAction:
				se.Summary = a.Label
			}
			sc.Actions = append(sc.Actions, se)
			if a.Awaits {
				awaiting = "edit_result"
			}
		}
	}
	r.save("complete", sc, state)
	if r.persist && rc != nil {
		for _, m := range rc.server {
			_, _, _ = r.e.Store.AppendAssistantMessage(r.bgCtx(), telemetry.AssistantMessageInsert{
				ConversationID: r.convID, TurnID: r.turnID, Role: "tool",
				Content: mustJSON(storedContent{ToolCallID: m.ToolCallID, Text: m.Text})})
		}
	}
	r.send(UsageEvent{Type: "usage", InputTokens: r.usage.InputTokens, OutputTokens: r.usage.OutputTokens,
		CachedTokens: r.usage.CachedTokens, CostUSD: r.cost})
	done := DoneEvent{Type: "done", Awaiting: awaiting}
	if r.msgID != "" {
		id := r.msgID
		done.MessageID = &id
	}
	r.send(done)
	r.report(outcome)
	if r.newConv && r.persist {
		r.e.bg(func() { r.e.title(r.bgCtx(), r.t, r.convID, r.userText, r.text.String()) })
	}
}

// save writes the assistant row exactly once (background context: a client
// disconnect must not cancel it).
func (r *run) save(status string, sc storedContent, state json.RawMessage) {
	if r.saved {
		return
	}
	r.saved = true
	if !r.persist || r.convID == "" {
		return
	}
	usage := mustJSON(map[string]any{"inputTokens": r.usage.InputTokens, "outputTokens": r.usage.OutputTokens,
		"cachedTokens": r.usage.CachedTokens, "costUsd": r.cost, "rounds": r.rounds})
	id, _, err := r.e.Store.AppendAssistantMessage(r.bgCtx(), telemetry.AssistantMessageInsert{
		ConversationID: r.convID, TurnID: r.turnID, Role: "assistant", Content: mustJSON(sc), Model: r.model,
		Status: status, Usage: usage, ProviderState: state})
	if err != nil {
		r.e.logger().Warn("assistant: persist reply failed", "error", err.Error())
		return
	}
	r.msgID = id
}

// finish persists a partial (interrupted) reply; nothing more can be sent.
func (r *run) finish(status, errMsg string) {
	r.save(status, storedContent{Text: r.text.String(), Error: errMsg}, nil)
	r.report(status)
}

// fail ends the stream with an error event. Nothing delivered yet: refund.
func (r *run) fail(code, msg string) {
	if !r.content {
		r.refund("assistant_" + code)
	}
	r.save("error", storedContent{Text: r.text.String(), Error: code}, nil)
	r.send(ErrorEvent{Type: "error", Code: code, Message: msg})
	r.report("error")
}

// failPre is a store failure before meta: an error event only.
func (r *run) failPre(err error) {
	r.e.logger().Warn("assistant: store failed", "error", err.Error())
	r.refund("assistant_internal")
	r.send(ErrorEvent{Type: "error", Code: CodeInternal, Message: "Medi is unavailable right now. Please try again."})
}

// paused handles the spend cap: the guide answers from retrieval without a
// model call, the copilot pauses (refunded).
func (r *run) paused(hits []Hit) {
	if r.surface == SurfaceGuide && !r.content {
		answer := StaticAnswer(r.t.Body.Locale, hits)
		r.text.WriteString(answer)
		r.send(DeltaEvent{Type: "delta", Text: answer})
		r.content = true
		r.save("complete", storedContent{Text: answer}, nil)
		r.send(UsageEvent{Type: "usage"})
		done := DoneEvent{Type: "done", Outcome: "static"}
		if r.msgID != "" {
			id := r.msgID
			done.MessageID = &id
		}
		r.send(done)
		r.report("static")
		return
	}
	r.fail(CodePaused, "Medi has reached today's limit and is paused until 00:00 UTC.")
}

// outOfScope answers an off-topic guide message with the localized line (no
// model call). It is stored like any reply so the conversation reads naturally.
func (r *run) outOfScope() {
	answer := OutOfScopeLine(r.t.Body.Locale)
	r.text.WriteString(answer)
	r.send(DeltaEvent{Type: "delta", Text: answer})
	r.content = true
	r.save("complete", storedContent{Text: answer}, nil)
	r.send(UsageEvent{Type: "usage"})
	done := DoneEvent{Type: "done", Outcome: "out_of_scope"}
	if r.msgID != "" {
		id := r.msgID
		done.MessageID = &id
	}
	r.send(done)
	r.report("out_of_scope")
}

// localNames maps retrieved docs to their name + path in the user's locale
// (decision 8); nil for English or a Knowledge without names.
func localNames(k Knowledge, hits []Hit, locale string) map[string]LocalName {
	ln, ok := k.(LocalNamer)
	if !ok || len(hits) == 0 || strings.HasPrefix(strings.ToLower(locale), "en") {
		return nil
	}
	out := map[string]LocalName{}
	for _, h := range hits {
		if n, ok := ln.LocalName(h.Doc.ID, locale); ok {
			out[h.Doc.ID] = n
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *run) report(outcome string) {
	if r.e.OnTurnCounted == nil {
		return
	}
	r.e.OnTurnCounted(TurnReport{Surface: r.surface, ModelTier: r.tier, Outcome: outcome, ToolRounds: r.rounds,
		CostUSD: r.cost, InputTokens: r.usage.InputTokens, CachedTokens: r.usage.CachedTokens, Latency: time.Since(r.started),
		Tier: r.t.Live.Tier, VisitorID: r.t.AnalyticsVisitorID, SessionID: r.t.AnalyticsSessionID})
}

// title generates a conversation title with the title model (best-effort;
// never blocks the reply; usage row kind "title").
func (e *Engine) title(ctx context.Context, t *Turn, convID, userText, reply string) {
	if e.Store == nil || strings.TrimSpace(userText) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	prompt := fmt.Sprintf("Write a title of 2 to 6 words for this conversation, in the language of the user's message. Reply with the title only, no quotes.\n\nUser: %s\n\nAssistant: %s",
		clip(userText, 600), clip(reply, 600))
	req := Request{Model: e.Cfg.ModelTitle, MaxTokens: 64, Messages: []Message{{Role: "user", Text: prompt}}}
	res, ok := e.Spend.Reserve(ctx, WorstCaseUSD(req.Model, EstimateTokens(len(prompt), 0), req.MaxTokens))
	if !ok {
		return
	}
	var u *Usage
	var b strings.Builder
	defer func() {
		cost, est := 0.0, false
		if u != nil {
			cost = u.CostUSD
			if !u.Reported {
				cost, est = EstimateUSD(req.Model, *u), true
			}
			_ = e.Store.RecordAssistantUsage(context.WithoutCancel(ctx), telemetry.AssistantUsageEvent{UserID: t.UserID,
				Surface: t.Body.Surface, Kind: "title", Model: req.Model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
				CachedTokens: u.CachedTokens, CostUSD: cost, Estimated: est})
		}
		res.Settle(cost)
	}()
	stream, err := e.Provider.Stream(ctx, req)
	if err != nil {
		return
	}
	defer stream.Close()
	for {
		ev, err := stream.Next()
		if err != nil {
			break
		}
		switch ev.Kind {
		case EventDelta:
			b.WriteString(ev.Text)
		case EventUsage:
			u = ev.Usage
		}
	}
	title := clip(strings.Trim(strings.TrimSpace(strings.SplitN(b.String(), "\n", 2)[0]), `"'.`), 80)
	if title != "" {
		_ = e.Store.SetAssistantConversationTitle(context.WithoutCancel(ctx), convID, title)
	}
}
