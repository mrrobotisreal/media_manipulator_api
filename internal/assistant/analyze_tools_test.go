package assistant

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant/analyze"
)

func metricsBody(surface string) ChatBody {
	return ChatBody{Surface: surface, Locale: "en-US", Message: &MessageIn{Text: "brighter"},
		Context: &ContextIn{Digest: "d", Metrics: analyze.MetricSet{"meanLuma": 0.2, "shadowsClipped": 0.08}}}
}

// MM115 acceptance 3: the body limits are enforced.
func TestMetricsBodyLimits(t *testing.T) {
	ok := metricsBody("photo")
	if _, _, err := ok.Validate(300000); err != nil {
		t.Fatalf("valid metrics refused: %v", err)
	}
	guide := metricsBody("guide")
	if _, _, err := guide.Validate(300000); err == nil {
		t.Error("the guide accepted metrics")
	}
	// The 4 KB limit binds by size: 100 long-named metrics are over it.
	wide := metricsBody("photo")
	wide.Context.Metrics = analyze.MetricSet{}
	for i := 0; i < 100; i++ {
		wide.Context.Metrics[fmt.Sprintf("%s%02d", strings.Repeat("k", 29), i)] = 1234.5678
	}
	if _, _, err := wide.Validate(300000); err == nil {
		t.Error("a metric set over 4 KB was accepted")
	} else if be, _ := AsBodyError(err); be == nil || be.Code != "metrics_too_large" {
		t.Errorf("error = %v", err)
	}
	nan := metricsBody("photo")
	nan.Context.Metrics = analyze.MetricSet{"bad key!": 1}
	if _, _, err := nan.Validate(300000); err == nil {
		t.Error("a bad metric name was accepted")
	}
	er := ChatBody{Surface: "photo", Locale: "en-US", ConversationID: "00000000-0000-4000-8000-000000000001", TurnID: "00000000-0000-4000-8000-000000000002",
		EditResult: &EditResult{CallID: "e1", Applied: true, MetricsBefore: analyze.MetricSet{"meanLuma": 0.2}, MetricsAfter: analyze.MetricSet{"meanLuma": 0.4}}}
	if _, _, err := er.Validate(300000); err != nil {
		t.Fatalf("editResult metrics refused: %v", err)
	}
	er.EditResult.MetricsAfter = analyze.MetricSet{"x": 1e12}
	if _, _, err := er.Validate(300000); err == nil {
		t.Error("an out-of-range value was accepted")
	}
}

// MM115 acceptance: analyze_compare is offered to copilots only, is a server
// tool, and a continuation with before/after meters answers it from the body.
func TestAnalyzeCompareToolRound(t *testing.T) {
	v := newEnv(t,
		round{calls: []ToolCall{call("e1", ToolApplyEdits, greyOps)}},
		round{deltas: []string{"A title"}},
		round{calls: []ToolCall{call("c1", ToolAnalyzeCompare, `{}`)}},
		round{deltas: []string{"Shadows clipped 8 % to 0.4 %; done."}})
	v.run(v.turn(photoMsg("brighten"), "user-1"), nil)
	meta := v.em.events[0]
	convID, turnID := meta["conversationId"].(string), meta["turnId"].(string)
	conv, _ := v.st.GetAssistantConversation(context.Background(), convID, "user-1")
	prep, err := v.e.PrepareEditResult(context.Background(), conv, turnID, "e1")
	if err != nil {
		t.Fatal(err)
	}
	v.em = &recEmitter{}
	cont := &Turn{Body: ChatBody{Surface: "photo", Locale: "en-US", ConversationID: convID, TurnID: turnID,
		EditResult: &EditResult{CallID: "e1", Applied: true,
			MetricsBefore: analyze.MetricSet{"meanLuma": 0.2, "shadowsClipped": 0.08},
			MetricsAfter:  analyze.MetricSet{"meanLuma": 0.4, "shadowsClipped": 0.004}}}, UserID: "user-1", Conversation: conv}
	v.run(cont, prep)
	reqs := v.p.requests()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d", len(reqs))
	}
	var gotTool bool
	for _, td := range reqs[2].Tools {
		gotTool = gotTool || td.Name == ToolAnalyzeCompare
	}
	if !gotTool {
		t.Fatal("analyze_compare not offered on the continuation")
	}
	// The edit result text carries the brief; the tool result carries the table.
	msgs := reqs[3].Messages
	var edit, tool string
	for _, m := range msgs {
		switch m.ToolCallID {
		case "e1":
			edit = m.Text
		case "c1":
			tool = m.Text
		}
	}
	if !strings.Contains(edit, "Meters before to after") || !strings.Contains(edit, "shadowsClipped 0.08→0.004") {
		t.Errorf("edit result = %q", edit)
	}
	if !strings.Contains(tool, "meanLuma: 0.2 → 0.4 (+0.2)") || !strings.Contains(tool, "shadowsClipped: 0.08 → 0.004 (-0.076)") {
		t.Errorf("analyze_compare result = %q", tool)
	}
	if got := strings.Join(v.em.types(), ","); got != "meta,tool,tool,delta,usage,done" {
		t.Errorf("events = %s", got)
	}
	if IsServerTool(ToolApplyEdits) || !IsServerTool(ToolAnalyzeCompare) {
		t.Error("tool classes")
	}
	for _, td := range ToolsFor("guide", NewOpsValidator()) {
		if td.Name == ToolAnalyzeCompare {
			t.Error("the guide offers analyze_compare")
		}
	}
}

// The live block shows the browser's meters; the persona carries the rule.
func TestMetricsInLiveBlockAndPersona(t *testing.T) {
	v := newEnv(t, round{deltas: []string{"ok"}})
	body := metricsBody("photo")
	body.Context.Digest = "layer L1"
	v.run(v.turn(body, "user-1"), nil)
	sys := v.p.requests()[0].System[1].Text
	if !strings.Contains(sys, "Meters of the current preview") || !strings.Contains(sys, "meanLuma 0.2") {
		t.Errorf("block 2 lacks the meters:\n%s", sys)
	}
	if !strings.Contains(persona, "Numbers first") || !strings.Contains(persona, "Never adjust without citing a figure") {
		t.Error("persona lacks the cite-a-metric rule")
	}
}

type fakeFiles struct {
	path string
	err  error
}

func (f fakeFiles) ResolveFile(_ context.Context, _, _ string) (string, func(), error) {
	return f.path, func() {}, f.err
}

// The file tools are offered only when the turn has a file ref and the
// resolver exists; the model can only reach the turn's own ref.
func TestFileToolsOnlyWithFileRef(t *testing.T) {
	has := func(rq Request) bool {
		for _, td := range rq.Tools {
			if td.Name == ToolAnalyzeImage {
				return true
			}
		}
		return false
	}
	v := newEnv(t, round{deltas: []string{"a"}})
	v.run(v.turn(photoMsg("x"), "user-1"), nil)
	if has(v.p.requests()[0]) {
		t.Fatal("file tools offered with no file ref")
	}
	v = newEnv(t, round{deltas: []string{"a"}})
	v.e.Files = fakeFiles{path: "x"}
	tn := v.turn(photoMsg("x"), "user-1")
	v.run(tn, nil)
	if has(v.p.requests()[0]) {
		t.Fatal("file tools offered with a resolver but no ref")
	}
	v = newEnv(t, round{deltas: []string{"a"}})
	tn = v.turn(photoMsg("x"), "user-1")
	tn.FileRef = "job-1/in.png"
	v.run(tn, nil)
	if has(v.p.requests()[0]) {
		t.Fatal("file tools offered with a ref but no resolver")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	if err := os.WriteFile(p, mustPNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	v = newEnv(t, round{calls: []ToolCall{call("i1", ToolAnalyzeImage, `{"fileRef":"job-1/in.png"}`)}}, round{deltas: []string{"measured"}})
	v.e.Files = fakeFiles{path: p}
	tn = v.turn(photoMsg("how is my picture"), "user-1")
	tn.FileRef = "job-1/in.png"
	v.run(tn, nil)
	reqs := v.p.requests()
	if !has(reqs[0]) || len(reqs) < 2 {
		t.Fatalf("offered=%v requests=%d", has(reqs[0]), len(reqs))
	}
	var res string
	for _, m := range reqs[1].Messages {
		if m.ToolCallID == "i1" {
			res = m.Text
		}
	}
	if !strings.Contains(res, "shadowsClipped") || !strings.Contains(res, "Picture 64x64") {
		t.Errorf("analyze_image result = %q", res)
	}
	// Another ref is refused.
	v = newEnv(t, round{calls: []ToolCall{call("i1", ToolAnalyzeImage, `{"fileRef":"someone-elses"}`)}}, round{deltas: []string{"no"}})
	v.e.Files = fakeFiles{path: p}
	tn = v.turn(photoMsg("x"), "user-1")
	tn.FileRef = "job-1/in.png"
	v.run(tn, nil)
	for _, m := range v.p.requests()[1].Messages {
		if m.ToolCallID == "i1" && !strings.Contains(m.Text, "no such file") {
			t.Errorf("foreign ref answered: %q", m.Text)
		}
	}
	v = newEnv(t, round{calls: []ToolCall{call("i1", ToolAnalyzeImage, `{"fileRef":"job-1/in.png"}`)}}, round{deltas: []string{"no"}})
	v.e.Files = fakeFiles{err: errors.New("gone")}
	tn = v.turn(photoMsg("x"), "user-1")
	tn.FileRef = "job-1/in.png"
	v.run(tn, nil)
	for _, m := range v.p.requests()[1].Messages {
		if m.ToolCallID == "i1" && !strings.Contains(m.Text, "could not be opened") {
			t.Errorf("resolver error answered: %q", m.Text)
		}
	}
}

func TestVolatileBlockMetricsAbsentWhenNone(t *testing.T) {
	if strings.Contains(VolatileBlock(LiveContext{Surface: "photo"}).Text, "Meters of the current preview") {
		t.Error("meters line without metrics")
	}
}

func mustPNG(t *testing.T) []byte {
	t.Helper()
	// 64x64 half black, half white: clipped at both ends.
	img := image.NewGray(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 32; x < 64; x++ {
			img.SetGray(x, y, color.Gray{255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
