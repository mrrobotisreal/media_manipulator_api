package evals

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

// recorder is an assistant.Emitter that keeps every event of one request.
type recorder struct {
	out Output
	// conversationID/turnID/lastEditCallID let an adjust case continue the turn.
	conversationID, turnID, lastEditCallID string
}

func (r *recorder) Ping() error { return nil }

func (r *recorder) Send(ev any) error {
	switch e := ev.(type) {
	case assistant.MetaEvent:
		r.out.Model = e.Model
		r.out.PromptVersion = e.PromptVersion
		r.turnID = e.TurnID
		if e.ConversationID != nil {
			r.conversationID = *e.ConversationID
		}
	case assistant.ToolEvent:
		if e.Status == "running" {
			r.out.ToolCalls++
		}
	case assistant.DeltaEvent:
		r.out.Text += e.Text
	case assistant.ActionEvent:
		r.out.ToolCalls++
		r.out.Actions = append(r.out.Actions, Action{Kind: e.Kind, Label: e.Label, Params: e.Params})
	case assistant.AskEvent:
		r.out.ToolCalls++
		r.out.Asks = append(r.out.Asks, e.Question)
	case assistant.EditsEvent:
		var ops []map[string]any
		_ = json.Unmarshal(e.Ops, &ops)
		r.out.ToolCalls++
		r.lastEditCallID = e.CallID
		r.out.Edits = append(r.out.Edits, Edit{Summary: e.Summary, Reason: e.Reason, Ops: ops})
	case assistant.UsageEvent:
		r.out.CostUSD += e.CostUSD
	case assistant.ErrorEvent:
		r.out.Error = e.Code
	case assistant.DoneEvent:
		// MM107: "static" / "out_of_scope" = answered without a model call.
		if e.Outcome != "" {
			r.out.Model = ""
		}
	}
	return nil
}

// Runner drives the real engine in-process, one case at a time, guest-style
// (no store: nothing persisted, nothing charged against a user).
type Runner struct {
	Engine *assistant.Engine
	// Timeout per case (default 120 s).
	Timeout time.Duration
}

// Run executes one case and returns its output.
func (r *Runner) Run(ctx context.Context, c Case) (Output, error) {
	if c.Adjust != nil {
		return r.runAdjust(ctx, c)
	}
	body := assistant.ChatBody{Surface: c.Surface, Locale: c.Locale, Page: assistant.PageRef{Path: c.Page},
		Message: &assistant.MessageIn{Text: c.Message}}
	if c.FixtureDoc != "" {
		raw, err := Fixture(c.FixtureDoc)
		if err != nil {
			return Output{}, err
		}
		body.Context = &assistant.ContextIn{Digest: string(raw)}
	}
	if c.Frame != "" {
		raw, err := Frame(c.Frame)
		if err != nil {
			return Output{}, err
		}
		body.Frame = &assistant.FrameIn{JPEGBase64: base64.StdEncoding.EncodeToString(raw)}
	}
	frameURL, _, err := body.Validate(r.Engine.Cfg.MaxFrameBytes)
	if err != nil {
		return Output{}, fmt.Errorf("%s: body: %w", c.ID, err)
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rec := &recorder{}
	turn := &assistant.Turn{Body: body, FrameURL: frameURL, VisitorHash: "eval",
		Live: assistant.LiveContext{Tier: "operator", TurnsLimit: -1, TurnsRemaining: -1}}
	r.Engine.Run(ctx, turn, rec, nil)
	return rec.out, nil
}

// Filter selects cases by surface and id (empty = all) and splits off the
// ones whose op schema has not landed (requires: <surface> not in schemas).
func Filter(cases []Case, surface, id string, schemas map[string]bool) (run []Case, skipped []Result) {
	for _, c := range cases {
		if surface != "" && c.Surface != surface {
			continue
		}
		if id != "" && c.ID != id {
			continue
		}
		if c.Requires != "" && !schemas[c.Requires] {
			skipped = append(skipped, Result{Case: c, Skipped: "requires " + c.Requires + " op schema (not merged yet)"})
			continue
		}
		run = append(run, c)
	}
	return run, skipped
}

// SuiteCases selects a suite: "adjust" (MM115) returns only the adjust cases,
// "routing" and "options" (MM116a) the guide cases tagged with that suite, and
// "" (the default) every other case, so the surface pass rates that --gate
// compares never mix in a named suite.
func SuiteCases(cases []Case, suite string) []Case {
	var out []Case
	for _, c := range cases {
		in := false
		switch suite {
		case "adjust":
			in = c.Adjust != nil
		case "routing", "options":
			in = c.Suite == suite
		default:
			in = c.Adjust == nil && c.Suite == ""
		}
		if in {
			out = append(out, c)
		}
	}
	return out
}

// Estimate is the printed cost estimate of a run (design §16: the session
// asks Mitch before spending).
type Estimate struct {
	Cases      int
	PerSurface map[string]int
	USD        float64
	WorstUSD   float64
}

// typical input/output tokens per case and surface (design §6 table plus the
// measured MM105 capsules): guide ≈ 6k capsule + 4 docs, copilot ≈ capsule +
// digest + frame; one or two upstream rounds.
var typical = map[string]struct{ in, out, rounds int }{
	"guide":  {in: 11000, out: 500, rounds: 2},
	"photo":  {in: 14000, out: 1500, rounds: 2},
	"studio": {in: 12000, out: 1500, rounds: 2},
	"vector": {in: 10000, out: 1500, rounds: 2},
}

// EstimateCost prices cases × repeat with the engine's models and the
// catalog prices (`assistant.DefaultPricing`): a typical figure (no cache
// hits) and a worst case (every round at the full output cap).
func EstimateCost(cases []Case, repeat int, cfg assistantModels) Estimate {
	if repeat < 1 {
		repeat = 1
	}
	e := Estimate{PerSurface: map[string]int{}}
	for _, c := range cases {
		e.Cases += repeat
		e.PerSurface[c.Surface] += repeat
		model, maxTok := cfg.ModelCopilot, cfg.MaxTokensCopilot
		if c.Surface == "guide" {
			model, maxTok = cfg.ModelGuide, cfg.MaxTokensGuide
		}
		t := typical[c.Surface]
		if c.Adjust != nil {
			// An adjust case is up to four requests (edit, edit, edit, confirm),
			// each carrying the stored turn; the cached prefix is shared.
			t.rounds *= 3
			t.out *= 2
		}
		u := assistant.Usage{InputTokens: t.in * t.rounds, OutputTokens: t.out}
		e.USD += float64(repeat) * assistant.EstimateUSD(model, u)
		e.WorstUSD += float64(repeat) * float64(t.rounds) * assistant.WorstCaseUSD(model, t.in, maxTok)
	}
	return e
}

type assistantModels struct {
	ModelGuide, ModelCopilot         string
	MaxTokensGuide, MaxTokensCopilot int
}

// ModelsOf extracts what EstimateCost needs from the engine config.
func ModelsOf(e *assistant.Engine) assistantModels {
	return assistantModels{e.Cfg.ModelGuide, e.Cfg.ModelCopilot, e.Cfg.MaxTokensGuide, e.Cfg.MaxTokensCopilot}
}

// Summary is pass/fail counts per surface plus cost.
type Summary struct {
	Pass, Fail, Skipped map[string]int
	CostUSD             float64
}

// Summarize totals results.
func Summarize(rs []Result) Summary {
	s := Summary{Pass: map[string]int{}, Fail: map[string]int{}, Skipped: map[string]int{}}
	for _, r := range rs {
		switch {
		case r.Skipped != "":
			s.Skipped[r.Case.Surface]++
		case r.Pass:
			s.Pass[r.Case.Surface]++
		default:
			s.Fail[r.Case.Surface]++
		}
		s.CostUSD += r.Output.CostUSD
	}
	return s
}

// Markdown renders the evidence file (docs/evidence/medi-eval-<date>.md).
func Markdown(date string, rs []Result, s Summary, models assistantModels) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Medi eval — %s\n\n", date)
	fmt.Fprintf(&b, "Generated by `go run ./cmd/assistant-eval` (MM106). Models: guide `%s`, copilot `%s`. Graded by code (`internal/assistant/evals`).\n\n", models.ModelGuide, models.ModelCopilot)
	for _, r := range rs {
		if r.Output.PromptVersion != "" {
			fmt.Fprintf(&b, "Prompt version: `%s` (content hash of persona, capsules, knowledge and op schemas).\n\n", r.Output.PromptVersion)
			break
		}
	}
	b.WriteString("| Surface | Pass | Fail | Skipped | Pass rate |\n|---|---|---|---|---|\n")
	surfaces := map[string]bool{}
	for _, r := range rs {
		surfaces[r.Case.Surface] = true
	}
	var keys []string
	for k := range surfaces {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ran := s.Pass[k] + s.Fail[k]
		rate := "—"
		if ran > 0 {
			rate = fmt.Sprintf("%.0f %%", 100*float64(s.Pass[k])/float64(ran))
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %s |\n", k, s.Pass[k], s.Fail[k], s.Skipped[k], rate)
	}
	fmt.Fprintf(&b, "\nTotal cost: $%.4f (OpenRouter-reported where available).\n\n", s.CostUSD)
	b.WriteString("## Cases\n\n| Case | Surface | Result | Cost | Notes |\n|---|---|---|---|---|\n")
	for _, r := range rs {
		res := "✅ pass"
		notes := ""
		switch {
		case r.Skipped != "":
			res, notes = "⏭ skipped", r.Skipped
		case !r.Pass:
			res, notes = "❌ fail", strings.Join(r.Failures, "; ")+" — "+failureExcerpt(r.Output)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | $%.4f | %s |\n", r.Case.ID, r.Case.Surface, res, r.Output.CostUSD, strings.ReplaceAll(notes, "|", "\\|"))
	}
	b.WriteString("\n## Failure triage\n\n(Filled in by the session: each failure is a knowledge gap fixed in the corpus/recipes, or logged for MM107/MM108.)\n")
	return b.String()
}

// failureExcerpt is what the model did, short enough for one table cell, so a
// failure can be triaged without re-running it (answer text, buttons, edits).
func failureExcerpt(out Output) string {
	text := strings.Join(strings.Fields(out.Text), " ")
	if r := []rune(text); len(r) > 240 {
		text = string(r[:240]) + "…"
	}
	parts := []string{fmt.Sprintf("said: %q", text), "actions: " + describeActions(out.Actions)}
	for _, ed := range out.Edits {
		parts = append(parts, fmt.Sprintf("edit %q (%d ops)", ed.Summary, len(ed.Ops)))
	}
	if out.Error != "" {
		parts = append(parts, "error: "+out.Error)
	}
	return strings.ReplaceAll(strings.Join(parts, "; "), "\n", " ")
}

// runAdjust plays the browser for an adjust case (MM115): the user's message
// carries the case's meters; each edit the model makes is answered with the
// next canned meters (editResult.metricsBefore/After) until the model stops
// editing or the edit budget is spent. Every request of the turn is one entry
// of Output.Rounds; the aggregate fields (Text, Edits, cost) cover them all.
func (r *Runner) runAdjust(ctx context.Context, c Case) (Output, error) {
	a := c.Adjust
	st := newMemStore()
	eng := *r.Engine // a copy: the case's store must not leak into the shared engine
	eng.Store = st
	eng.Background = func(func()) {} // no title or summary calls: they cost money and the eval stores nothing
	maxRounds := eng.Cfg.MaxEditRounds
	if maxRounds < 1 {
		maxRounds = 1
	}

	body := assistant.ChatBody{Surface: c.Surface, Locale: c.Locale, Page: assistant.PageRef{Path: c.Page},
		Message: &assistant.MessageIn{Text: c.Message}, Context: &assistant.ContextIn{Metrics: a.Before}}
	if c.FixtureDoc != "" {
		raw, err := Fixture(c.FixtureDoc)
		if err != nil {
			return Output{}, err
		}
		body.Context.Digest = string(raw)
	}
	var frameURL string
	if c.Frame != "" {
		raw, err := Frame(c.Frame)
		if err != nil {
			return Output{}, err
		}
		body.Frame = &assistant.FrameIn{JPEGBase64: base64.StdEncoding.EncodeToString(raw)}
	}
	frameURL, _, err := body.Validate(eng.Cfg.MaxFrameBytes)
	if err != nil {
		return Output{}, fmt.Errorf("%s: body: %w", c.ID, err)
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	total := Output{}
	live := assistant.LiveContext{Tier: "operator", TurnsLimit: -1, TurnsRemaining: -1}
	turn := &assistant.Turn{Body: body, FrameURL: frameURL, UserID: "eval", VisitorHash: "eval", Live: live}
	prev := a.Before
	edits := 0
	var convID, turnID string
	var prep *assistant.Prepared
	for {
		rec := &recorder{}
		eng.Run(ctx, turn, rec, prep)
		out := rec.out
		round := RoundOut{Text: out.Text, Failed: out.Error}
		if len(out.Edits) > 0 {
			e := out.Edits[0]
			round.Edit = &e
			edits++
		}
		total.Text += out.Text
		total.Actions = append(total.Actions, out.Actions...)
		total.Edits = append(total.Edits, out.Edits...)
		total.Asks = append(total.Asks, out.Asks...)
		total.CostUSD += out.CostUSD
		total.ToolCalls += out.ToolCalls
		if total.Model == "" {
			total.Model, total.PromptVersion = out.Model, out.PromptVersion
		}
		if out.Error != "" {
			total.Error = out.Error
		}
		if round.Edit != nil {
			after := a.After[min(edits-1, len(a.After)-1)]
			round.Shown = after
		}
		total.Rounds = append(total.Rounds, round)
		if round.Edit == nil || out.Error != "" || edits >= maxRounds+1 {
			break
		}
		if convID == "" {
			convID, turnID = rec.conversationID, rec.turnID
		}
		// The model's edit awaits its result; answer it with the canned meters.
		callID := rec.lastEditCallID
		conv, err := st.GetAssistantConversation(ctx, convID, "eval")
		if err != nil {
			return total, err
		}
		prep, err = eng.PrepareEditResult(ctx, conv, turnID, callID)
		if err != nil {
			break // edit budget spent or stale: the turn ends here
		}
		cont := assistant.ChatBody{Surface: c.Surface, Locale: c.Locale, Page: body.Page, ConversationID: convID, TurnID: turnID,
			EditResult: &assistant.EditResult{CallID: callID, Applied: true, MetricsBefore: prev, MetricsAfter: round.Shown}}
		if _, _, err := cont.Validate(eng.Cfg.MaxFrameBytes); err != nil {
			return total, fmt.Errorf("%s: editResult: %w", c.ID, err)
		}
		prev = round.Shown
		turn = &assistant.Turn{Body: cont, UserID: "eval", VisitorHash: "eval", Live: live, Conversation: conv}
	}
	return total, nil
}
