package assistant

import (
	"context"
	"strings"
	"testing"
)

// fakeJobs is a read-only estimator that counts what it was asked, and offers
// no way to start anything (that is the point of the interface).
type fakeJobs struct {
	quotaCalls, queueCalls int
	metric                 string
	view                   QuotaView
	queue                  QueueView
}

func (f *fakeJobs) QuotaMetric(family string) string {
	if f.metric != "" {
		return f.metric
	}
	if strings.HasPrefix(family, "image:ai_") {
		return "ai_ops"
	}
	return "ops"
}
func (f *fakeJobs) Quota(_ context.Context, _ *Turn, metric string) QuotaView {
	f.quotaCalls++
	v := f.view
	v.Metric = metric
	return v
}
func (f *fakeJobs) Queue(string) QueueView { f.queueCalls++; return f.queue }

func submit(t *testing.T, args string) (ClientAction, error) {
	t.Helper()
	k := optsKnowledge(t)
	tools := append(ToolsFor(SurfaceGuide, nil), SubmitJobTool())
	return validateClientCall(call("c1", ToolSubmitJob, args), SurfaceGuide, tools, nil, k.Routes(), k)
}

// submit_job validates with exactly the same check as open_tool.options.
func TestSubmitJobValidatesLikeOpenTool(t *testing.T) {
	a, err := submit(t, `{"slug":"compress-video","options":{"level":"smallest","crf":30},"label":"Compress it"}`)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "confirm_job" || a.Params["tool"] != "compress-video" || a.Family != "video:compress" {
		t.Errorf("action = %+v", a)
	}
	if o, _ := a.Params["options"].(map[string]any); o["crf"] != float64(30) {
		t.Errorf("params = %v", a.Params)
	}
	for _, bad := range []string{
		`{"slug":"compress-video","options":{"crf":99}}`,
		`{"slug":"compress-video","options":{"bogus":1}}`,
		`{"slug":"nope","options":{"crf":30}}`,
		`{"slug":"motion"}`,                       // an editor: no job to run
		`{"slug":"blur-image","options":{"a":1}}`, // no option schema in the corpus
		`{"slug":"compress-video","options":{"level":"smallest","pad":"` + strings.Repeat("x", MaxOptionsBytes) + `"}}`,
	} {
		if _, err := submit(t, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// Options are optional: a bare proposal still gets a card (the page defaults apply).
func TestSubmitJobWithoutOptions(t *testing.T) {
	a, err := submit(t, `{"slug":"compress-video"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := a.Params["options"]; has {
		t.Errorf("params = %v", a.Params)
	}
}

// End to end: the engine streams a confirm_job action with server-computed quota and queue
// numbers, tells the model nothing ran, and asks the estimator exactly once (read-only).
func TestEngineSubmitJobStreamsConfirmCard(t *testing.T) {
	v := newEnv(t, round{calls: []ToolCall{call("c1", ToolSubmitJob, `{"slug":"compress-video","options":{"level":"smallest"}}`)}})
	v.e.Knowledge = optsKnowledge(t)
	rem := 7
	fj := &fakeJobs{view: QuotaView{Tier: "anonymous", Limit: 10, Remaining: &rem}, queue: QueueView{GPU: true, Bucket: QueueShort, Waiting: 2, Running: 1}}
	v.e.Jobs = fj
	v.run(v.turn(guideMsg("just run it, as small as you can"), "user-1"), nil)

	var got map[string]any
	for _, ev := range v.em.events {
		if ev["type"] == "action" {
			got = ev
		}
	}
	if got == nil {
		t.Fatalf("no action: %v", v.em.events)
	}
	if got["kind"] != "confirm_job" {
		t.Errorf("kind = %v", got["kind"])
	}
	p, _ := got["params"].(map[string]any)
	q, _ := p["quota"].(map[string]any)
	qe, _ := p["queueEstimate"].(map[string]any)
	if p["tool"] != "compress-video" || q["metric"] != "ops" || q["remaining"] != float64(7) || q["limit"] != float64(10) || qe["bucket"] != "short" || qe["waiting"] != float64(2) {
		t.Errorf("params = %v", p)
	}
	if fj.quotaCalls != 1 || fj.queueCalls != 1 {
		t.Errorf("estimator calls = %d/%d", fj.quotaCalls, fj.queueCalls)
	}
	// The stored tool result tells the model the truth for the next turn.
	if !strings.Contains(syntheticResult(ToolSubmitJob), "Nothing has run") {
		t.Error("tool result must say nothing ran")
	}
}

// The model cannot supply the numbers: a proposal that smuggles quota/queue is refused by the schema.
func TestSubmitJobSchemaClosed(t *testing.T) {
	if err := validateArgs(SubmitJobTool().Parameters, `{"slug":"compress-video","quota":{"remaining":999}}`); err == nil {
		t.Error("submit_job accepted a quota argument")
	}
	if err := validateArgs(SubmitJobTool().Parameters, `{"slug":"compress-video","queueEstimate":{"bucket":"none"}}`); err == nil {
		t.Error("submit_job accepted a queueEstimate argument")
	}
}

// submit_job is a guide tool offered only when the engine can price a job; the copilots never get it.
func TestSubmitJobOffered(t *testing.T) {
	for _, s := range []string{SurfacePhoto, SurfaceStudio, SurfaceVector} {
		for _, td := range ToolsFor(s, NewOpsValidator()) {
			if td.Name == ToolSubmitJob {
				t.Errorf("%s offers submit_job", s)
			}
		}
	}
	v := newEnv(t, round{deltas: []string{"hi"}})
	v.run(v.turn(guideMsg("hello"), "user-1"), nil)
	for _, td := range v.p.requests()[0].Tools {
		if td.Name == ToolSubmitJob {
			t.Error("offered without an estimator")
		}
	}
	v = newEnv(t, round{deltas: []string{"hi"}})
	v.e.Jobs = &fakeJobs{}
	v.run(v.turn(guideMsg("hello"), "user-1"), nil)
	found := false
	for _, td := range v.p.requests()[0].Tools {
		found = found || td.Name == ToolSubmitJob
	}
	if !found {
		t.Error("not offered with an estimator")
	}
}

func TestPersonaRuleForJobs(t *testing.T) {
	for _, want := range []string{"submit_job", "can never start a job", "confirmation card", "Options: none"} {
		if !strings.Contains(persona, want) {
			t.Errorf("persona lacks %q", want)
		}
	}
}
