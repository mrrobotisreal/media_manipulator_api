package evals

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func TestAdjustSuiteWellFormed(t *testing.T) {
	cs, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	adj := SuiteCases(cs, "adjust")
	if len(adj) < 15 {
		t.Fatalf("adjust cases = %d, want >= 15", len(adj))
	}
	for _, c := range adj {
		if c.Adjust.Rounds.Max > 3 || !c.Adjust.CitesMetric {
			t.Errorf("%s: rounds/cites", c.ID)
		}
		// The canned "after" of the last edit must satisfy the case's final
		// ranges, or no model could pass it.
		last := c.Adjust.After[len(c.Adjust.After)-1]
		if c.Adjust.Rounds.Min == 1 {
			last = c.Adjust.After[0]
		}
		for _, f := range c.Adjust.Final {
			v := last[f.Metric]
			if (f.Min != nil && v < *f.Min) || (f.Max != nil && v > *f.Max) {
				t.Errorf("%s: canned last meters %s=%v are outside the final range", c.ID, f.Metric, v)
			}
		}
	}
	for _, c := range SuiteCases(cs, "") {
		if c.Adjust != nil {
			t.Errorf("%s leaked into the default suite", c.ID)
		}
	}
	// The multi-round cases cost more than a single-turn one in the estimate.
	one, _ := Filter(SuiteCases(cs, ""), "photo", "photo-brighter", map[string]bool{"photo": true})
	m := assistantModels{"anthropic/claude-haiku-5.5", "anthropic/claude-sonnet-5.5", 2048, 8192}
	if EstimateCost(adj[:1], 1, m).USD <= EstimateCost(one, 1, m).USD {
		t.Error("adjust estimate not above a single-turn case")
	}
}

func f64(v float64) *float64 { return &v }

func adjustCase() Case {
	return Case{ID: "a", Surface: "photo", Requires: "photo", Message: "too dark", Adjust: &Adjust{
		Before: map[string]float64{"meanLuma": 0.18, "shadowsClipped": 0.09},
		After:  []map[string]float64{{"meanLuma": 0.3, "shadowsClipped": 0.05}, {"meanLuma": 0.43, "shadowsClipped": 0.008}},
		Rounds: struct {
			Min int `yaml:"min"`
			Max int `yaml:"max"`
		}{2, 3},
		Final:       []FinalMetric{{Metric: "meanLuma", Min: f64(0.35), Max: f64(0.55)}},
		CitesMetric: true}}
}

func rounds(r ...RoundOut) Output {
	return Output{Model: "m", Rounds: r}
}

// MM115: cites_metric, rounds <= 3 and final_metric in range on canned traces.
func TestGradeAdjust(t *testing.T) {
	c := adjustCase()
	edit := &Edit{Summary: "Brighter", Reason: "mean luma is 0.18"}
	good := rounds(
		RoundOut{Text: "Lifting exposure: mean luma is 0.18.", Edit: edit, Shown: c.Adjust.After[0]},
		RoundOut{Text: "Still dark at 0.3, raising shadows.", Edit: &Edit{Summary: "Open shadows", Reason: "luma 0.3"}, Shown: c.Adjust.After[1]},
		RoundOut{Text: "Done: mean luma 0.43, shadows clipped 0.8 %."},
	)
	if r := Grade(c, good); !r.Pass {
		t.Fatalf("good trace failed: %v", r.Failures)
	}

	oneShot := rounds(RoundOut{Text: "Brighter.", Edit: edit, Shown: c.Adjust.After[0]}, RoundOut{Text: "Done."})
	if r := Grade(c, oneShot); r.Pass || !strings.Contains(strings.Join(r.Failures, ";"), "1 edit round") {
		t.Errorf("one edit passed a 2-3 round case: %v", r.Failures)
	}

	mute := rounds(
		RoundOut{Text: "Brightening.", Edit: edit, Shown: c.Adjust.After[0]},
		RoundOut{Text: "Brightening more.", Edit: &Edit{Summary: "More", Reason: "to make it nicer"}, Shown: c.Adjust.After[1]},
		RoundOut{Text: "All done, looks great."},
	)
	r := Grade(c, mute)
	if r.Pass || !strings.Contains(strings.Join(r.Failures, ";"), "without citing a metric") || !strings.Contains(strings.Join(r.Failures, ";"), "closing message cites no metric") {
		t.Errorf("an adjustment with no figure passed: %v", r.Failures)
	}

	four := rounds(
		RoundOut{Text: "luma 0.18", Edit: edit, Shown: c.Adjust.After[0]},
		RoundOut{Text: "luma 0.3", Edit: edit, Shown: c.Adjust.After[1]},
		RoundOut{Text: "luma 0.43", Edit: edit, Shown: c.Adjust.After[1]},
		RoundOut{Text: "luma 0.43", Edit: edit, Shown: c.Adjust.After[1]},
	)
	if r := Grade(c, four); r.Pass || !strings.Contains(strings.Join(r.Failures, ";"), "4 edit round") {
		t.Errorf("4 edits passed: %v", r.Failures)
	}

	wrong := c
	a := *c.Adjust
	a.After = []map[string]float64{{"meanLuma": 0.3}, {"meanLuma": 0.31}}
	wrong.Adjust = &a
	bad := rounds(
		RoundOut{Text: "luma 0.18", Edit: edit, Shown: a.After[0]},
		RoundOut{Text: "luma 0.3", Edit: edit, Shown: a.After[1]},
		RoundOut{Text: "Done at 0.31"},
	)
	if r := Grade(wrong, bad); r.Pass || !strings.Contains(strings.Join(r.Failures, ";"), "final_metric(meanLuma) = 0.31") {
		t.Errorf("final out of range passed: %v", r.Failures)
	}
}

func TestCitesMetric(t *testing.T) {
	m := map[string]float64{"meanLuma": 0.43, "shadowsClipped": 0.008}
	for text, want := range map[string]bool{
		"mean luma went from 0.18 to 0.43":     true,
		"shadows were clipped at 9 %":          true,
		"Яркость выросла до 43 %":              true, // a shown figure, any language
		"Теперь 0.43 в среднем":                true,
		"it looks much better now":             false,
		"I made it brighter":                   false,
		"The brightness is better, around ten": false, // a word but no figure
	} {
		if got := citesMetric(text, m); got != want {
			t.Errorf("citesMetric(%q) = %v, want %v", text, got, want)
		}
	}
}

// scriptedAdjust drives runAdjust end to end with a fake provider: two edits
// answered by canned meters, then a closing message.
type adjustProvider struct {
	mu   sync.Mutex
	n    int
	reqs []assistant.Request
}

type adjustStream struct{ evs []assistant.Event }

func (s *adjustStream) Next() (assistant.Event, error) {
	if len(s.evs) == 0 {
		return assistant.Event{}, io.EOF
	}
	e := s.evs[0]
	s.evs = s.evs[1:]
	return e, nil
}
func (s *adjustStream) Close() error { return nil }

func (p *adjustProvider) Stream(_ context.Context, req assistant.Request) (assistant.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	p.n++
	usage := assistant.Event{Kind: assistant.EventUsage, Usage: &assistant.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.001, Reported: true}}
	edit := func(id string, ev float64) assistant.Event {
		args, _ := json.Marshal(map[string]any{"summary": "Brighter", "reason": "mean luma is low",
			"ops": []map[string]any{{"op": "add_layer", "type": "exposure", "params": map[string]any{"ev": ev}}}})
		return assistant.Event{Kind: assistant.EventDone, ToolCalls: []assistant.ToolCall{{ID: id, Name: assistant.ToolApplyEdits, Arguments: string(args)}}}
	}
	switch p.n {
	case 1:
		return &adjustStream{[]assistant.Event{{Kind: assistant.EventDelta, Text: "Mean luma is 0.18, lifting exposure."}, usage, edit("e1", 0.5)}}, nil
	case 2:
		return &adjustStream{[]assistant.Event{{Kind: assistant.EventDelta, Text: "Still 0.3, more."}, usage, edit("e2", 0.3)}}, nil
	}
	return &adjustStream{[]assistant.Event{{Kind: assistant.EventDelta, Text: "Done: mean luma 0.43."}, usage, {Kind: assistant.EventDone}}}, nil
}

func TestRunAdjustPlaysTheBrowser(t *testing.T) {
	k, err := assistant.NewEmbeddedKnowledge()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Load().Assistant
	p := &adjustProvider{}
	e := &assistant.Engine{Provider: p, Knowledge: k, Ops: assistant.NewOpsValidator(), Cfg: cfg, Spend: assistant.NewSpendCap(0, nil, nil)}
	r := &Runner{Engine: e}
	cs, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	var c Case
	for _, x := range cs {
		if x.ID == "adjust-photo-underexposed" {
			c = x
		}
	}
	out, err := r.Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rounds) != 3 || out.Rounds[0].Edit == nil || out.Rounds[2].Edit != nil {
		t.Fatalf("rounds = %+v", out.Rounds)
	}
	if out.CostUSD < 0.0029 || out.Error != "" {
		t.Errorf("cost %v error %q", out.CostUSD, out.Error)
	}
	// The first request carried the meters in block 2; the continuation carried both readings.
	if sys := p.reqs[0].System[1].Text; !strings.Contains(sys, "Meters of the current preview") || !strings.Contains(sys, "meanLuma 0.18") {
		t.Errorf("first request lacks meters:\n%s", sys)
	}
	last := p.reqs[1].Messages[len(p.reqs[1].Messages)-1].Text
	if !strings.Contains(last, "Meters before to after") || !strings.Contains(last, "meanLuma 0.18→0.3") {
		t.Errorf("continuation result = %q", last)
	}
	if res := Grade(c, out); !res.Pass {
		t.Errorf("scripted trace should grade as a pass: %v", res.Failures)
	}
}
