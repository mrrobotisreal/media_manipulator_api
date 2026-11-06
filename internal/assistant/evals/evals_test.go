package evals

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func ops(t *testing.T, s string) []map[string]any {
	t.Helper()
	var o []map[string]any
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

func check(t *testing.T, expr, opsJSON string) (bool, string) {
	t.Helper()
	c, err := ParseCheck(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return c.Eval(ops(t, opsJSON))
}

func TestCaseSetLoadsAndIsWellFormed(t *testing.T) {
	cs, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	n := map[string]int{}
	for _, c := range cs {
		n[c.Surface]++
		if c.Surface != "guide" && c.Requires != c.Surface {
			t.Errorf("%s: copilot cases require their own surface's op schema", c.ID)
		}
	}
	t.Logf("cases: %v", n)
	if n["guide"] < 40 || n["photo"] < 15 || n["studio"] < 15 || n["vector"] < 15 {
		t.Errorf("case counts %v, want ≥ 40 guide and ≥ 15 per editor", n)
	}
	nonEnglish, offTopic := 0, 0
	for _, c := range cs {
		if c.Surface == "guide" && !strings.HasPrefix(c.Locale, "en") {
			nonEnglish++
		}
		if c.Expect.OffTopic || c.Expect.OutOfScope {
			offTopic++
		}
	}
	if nonEnglish < 5 || offTopic < 5 {
		t.Errorf("guide set needs ≥ 5 non-English and ≥ 5 off-topic cases (got %d, %d)", nonEnglish, offTopic)
	}
	// Frames are tiny committed JPEGs (decision 5: ≤ 60 KB each, ≤ 15 total).
	entries, _ := testdataFS.ReadDir("testdata/frames/photo")
	if len(entries) > 15 {
		t.Errorf("%d frames > 15", len(entries))
	}
	for _, e := range entries {
		b, _ := Frame("photo/" + e.Name())
		if len(b) > 60*1024 || len(b) < 3 || b[0] != 0xFF || b[1] != 0xD8 {
			t.Errorf("frame %s: %d bytes / not a JPEG", e.Name(), len(b))
		}
	}
}

func TestLoadCasesRejectsBadCases(t *testing.T) {
	bad := map[string]string{
		"unknown field":    "- {id: a, surface: guide, message: hi, expect: {nope: 1}}",
		"bad surface":      "- {id: a, surface: chat, message: hi}",
		"empty message":    "- {id: a, surface: guide, message: ''}",
		"bad action":       "- {id: a, surface: guide, message: hi, expect: {actions: [compress-video]}}",
		"bad op check":     "- {id: a, surface: photo, requires: photo, message: hi, expect: {ops: ['make_it_good()']}}",
		"ops need require": "- {id: a, surface: photo, message: hi, expect: {ops: ['has_op(x)']}}",
		"missing fixture":  "- {id: a, surface: photo, requires: photo, fixture_doc: nope.json, message: hi}",
		"duplicate id":     "- {id: a, surface: guide, message: hi}\n- {id: a, surface: guide, message: yo}",
	}
	for name, src := range bad {
		fsys := fstest.MapFS{"c/x.yaml": {Data: []byte(src)}}
		if _, err := LoadCasesFrom(fsys, "c"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckHasOpNoOpOpCount(t *testing.T) {
	o := `[{"op":"grey_except","ranges":["red"]},{"op":"add_layer","type":"vignette","params":{"amount":-20}}]`
	for expr, want := range map[string]bool{
		"has_op(grey_except)": true, "has_op(set_mask)": false,
		"no_op(delete_clip)": true, "no_op(grey_except)": false,
		"op_count <= 2": true, "op_count < 2": false, "op_count == 2": true, "op_count >= 3": false,
	} {
		if got, why := check(t, expr, o); got != want {
			t.Errorf("%s = %v (%s), want %v", expr, got, why, want)
		}
	}
}

func TestCheckHasLayerParamField(t *testing.T) {
	o := `[{"op":"add_layer","type":"tone","params":{"shadows":35,"highlights":-25}},
	       {"op":"add_layer","type":"hsl","params":{"ranges":[{"id":"r","saturation":-100}]}},
	       {"op":"grade","clipIds":"all","lumetri":{"saturation":0.85,"temperature":8}},
	       {"op":"apply_animator","layerIds":["a"],"animator":"drawOn","span":{"start":0,"end":1200}}]`
	for expr, want := range map[string]bool{
		"has_layer(tone)": true, "has_layer(exposure)": false,
		"param(tone, shadows) > 0": true, "param(tone, highlights) > 0": false,
		"param(hsl, ranges.0.saturation) <= -50": true,
		"param(*, shadows) ≈ 35":                 true, "param(*, shadows) ~= 50": false,
		"param(exposure, ev) > 0":              false, // no such layer
		"param(tone, whites) > 0":              false, // no such field
		"field(grade, lumetri.saturation) < 1": true, "field(grade, lumetri.temperature) > 10": false,
		"field(apply_animator, span.end) ≈ 1200": true,
	} {
		if got, why := check(t, expr, o); got != want {
			t.Errorf("%s = %v (%s), want %v", expr, got, why, want)
		}
	}
}

func TestCheckKeyframeWithin(t *testing.T) {
	vector := `[{"op":"animate","layerId":"t","property":"transform.x","keyframes":[{"t":0,"value":-40,"easing":"easeOut"},{"t":500,"value":0,"easing":"linear"}]}]`
	studio := `[{"op":"set_keyframes","clipId":"c","property":"opacity","keyframes":[{"t":0,"value":0,"ease":"easeOut"},{"t":1,"value":1,"ease":"linear"}]}]`
	cases := []struct {
		expr, ops string
		want      bool
	}{
		{"keyframe_within(transform.x, 600)", vector, true},
		{"keyframe_within(transform.x, 400)", vector, false},
		{"keyframe_within(opacity, 600)", vector, false},
		{"keyframe_within(opacity, 1000)", studio, true}, // studio t is seconds
		{"keyframe_within(opacity, 900)", studio, false},
	}
	for _, c := range cases {
		if got, why := check(t, c.expr, c.ops); got != c.want {
			t.Errorf("%s = %v (%s), want %v", c.expr, got, why, c.want)
		}
	}
	if _, err := ParseCheck("keyframe_within(opacity, soon)"); err == nil {
		t.Error("non-numeric limit accepted")
	}
}

func TestCheckTitleInRegion(t *testing.T) {
	for _, c := range []struct {
		expr, ops string
		want      bool
	}{
		{"title_in_region(lower30)", `[{"op":"add_title","template":"lowerThird","lines":["A"],"start":0,"duration":4}]`, true},
		{"title_in_region(lower30)", `[{"op":"add_title","template":"title","lines":["A"],"start":0,"duration":4}]`, false},
		{"title_in_region(center)", `[{"op":"add_title","template":"title","lines":["A"],"start":0,"duration":4}]`, true},
		{"title_in_region(lower30)", `[{"op":"template","kind":"lowerThird"}]`, true},
		{"title_in_region(lower30)", `[{"op":"add_text","text":"Jo","x":40,"y":440}]`, true},
		{"title_in_region(lower30)", `[{"op":"add_text","text":"Jo","x":40,"y":200}]`, false},
		{"title_in_region(lower30)", `[{"op":"set_canvas","height":1080},{"op":"add_layer","type":"rect","props":{"x":0,"y":800,"width":600,"height":80}}]`, true},
		{"title_in_region(upper30)", `[{"op":"add_text","text":"Jo","x":40,"y":60}]`, true},
	} {
		if got, why := check(t, c.expr, c.ops); got != c.want {
			t.Errorf("%s on %s = %v (%s), want %v", c.expr, c.ops, got, why, c.want)
		}
	}
	if _, err := ParseCheck("title_in_region(left)"); err == nil {
		t.Error("unknown region accepted")
	}
}

func TestCheckMentionsRangeAndAlternatives(t *testing.T) {
	for _, c := range []struct {
		expr, ops string
		want      bool
	}{
		{"mentions_range(red)", `[{"op":"grey_except","ranges":["red"]}]`, true},
		{"mentions_range(red)", `[{"op":"grey_except","ranges":["blue"]}]`, false},
		{"mentions_range(red)", `[{"op":"grey_except","ranges":[{"hue":350,"width":40}]}]`, true},
		{"mentions_range(aqua)", `[{"op":"grey_except","ranges":[{"hue":185,"width":40}]}]`, true},
		{"has_op(grey_except) || has_layer(hsl)", `[{"op":"add_layer","type":"hsl","params":{}}]`, true},
		{"has_op(grey_except) || has_layer(hsl)", `[{"op":"auto_tone"}]`, false},
	} {
		if got, why := check(t, c.expr, c.ops); got != c.want {
			t.Errorf("%s on %s = %v (%s), want %v", c.expr, c.ops, got, why, c.want)
		}
	}
	for _, bad := range []string{"mentions_range(chartreuse)", "has_op()", "param(tone) > 1", "has_op(x) > 1", "op_count >> 2", "make_it_pop()"} {
		if _, err := ParseCheck(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDetectLanguage(t *testing.T) {
	for text, want := range map[string]string{
		"Open Compress video and choose a smaller size for your file.":      "en",
		"Откройте «Сжатие видео» и выберите размер.":                        "ru",
		"Відкрийте інструмент і виберіть розмір файлу.":                     "uk",
		"Öffnen Sie das Werkzeug und wählen Sie die Größe, die Sie wollen.": "de",
		"Abre la herramienta y elige el tamaño que quieres para el vídeo.":  "es",
		"「動画を圧縮」を開いてください。":                                                  "ja",
		"打开压缩视频工具并选择大小。":                                                    "zh",
		"압축 도구를 여세요.":                                                       "ko",
		"افتح أداة الضغط.":                                                  "ar",
		"פתחו את הכלי.":                                                     "he",
		"संपीड़न टूल खोलें।":                                                "hi",
		// Undo labels with no function word (studio-de-grade, 2026-11-05).
		"Farben kräftiger (Sättigung, Vibrance)": "de",
		"Añadir título con animación":            "es",
		"Warm white balance":                     "en",
	} {
		if got := DetectLanguage(text); got != want {
			t.Errorf("DetectLanguage(%q) = %s, want %s", text, got, want)
		}
	}
}

func TestGradeGuideOutputs(t *testing.T) {
	c := Case{ID: "g", Surface: "guide", Locale: "ru-RU", Message: "x", Expect: Expect{
		Actions: []string{"tool:compress-video"}, MustNotInclude: []string{"$"}, Language: "ru", AnyOf: []string{"сжат"},
	}}
	pass := Output{Model: "m", Text: "Откройте «Сжатие видео».", Actions: []Action{{Kind: "open_tool", Params: map[string]any{"slug": "compress-video"}}}}
	if r := Grade(c, pass); !r.Pass {
		t.Errorf("pass graded fail: %v", r.Failures)
	}
	// navigate to a locale-prefixed tool route also counts.
	nav := pass
	nav.Actions = []Action{{Kind: "navigate", Params: map[string]any{"target": "/ru/tools/compress-video"}}}
	if r := Grade(c, nav); !r.Pass {
		t.Errorf("navigate graded fail: %v", r.Failures)
	}
	for name, o := range map[string]Output{
		"no action":    {Model: "m", Text: "Откройте «Сжатие видео»."},
		"english":      {Model: "m", Text: "Open Compress video, it is the best tool for the job.", Actions: pass.Actions},
		"price":        {Model: "m", Text: "Сжатие видео стоит $8.", Actions: pass.Actions},
		"stream error": {Model: "m", Error: "upstream"},
		"no model":     {Text: "Откройте «Сжатие видео».", Actions: pass.Actions},
	} {
		if r := Grade(c, o); r.Pass {
			t.Errorf("%s graded pass", name)
		}
	}
	page := Case{ID: "p", Surface: "guide", Message: "x", Expect: Expect{Actions: []string{"page:/contact"}}}
	if r := Grade(page, Output{Model: "m", Actions: []Action{{Kind: "navigate", Params: map[string]any{"target": "/contact"}}}}); !r.Pass {
		t.Errorf("page action: %v", r.Failures)
	}
}

func TestGradeOffTopicAndScope(t *testing.T) {
	off := Case{ID: "o", Surface: "guide", Message: "poem", Expect: Expect{OffTopic: true}}
	if r := Grade(off, Output{Text: assistant.OutOfScopeLine("en")}); !r.Pass {
		t.Errorf("gate answer: %v", r.Failures)
	}
	if r := Grade(off, Output{Model: "m", Text: "I'm Medi, I help with media editing on Media Manipulator."}); !r.Pass {
		t.Errorf("short decline: %v", r.Failures)
	}
	if r := Grade(off, Output{Model: "m", Text: "Roses are red, violets are blue, cats are soft, and so are you."}); r.Pass {
		t.Error("a poem graded as a decline")
	}
	if r := Grade(off, Output{Model: "m", Text: "Sure, media.", Actions: []Action{{Kind: "open_tool"}}}); r.Pass {
		t.Error("buttons on an off-topic answer")
	}
	strict := Case{ID: "s", Surface: "guide", Message: "poem", Expect: Expect{OutOfScope: true}}
	if r := Grade(strict, Output{Model: "m", Text: "No."}); r.Pass {
		t.Error("out_of_scope needs the gate")
	}
}

func TestGradeCopilotOutputs(t *testing.T) {
	c := Case{ID: "p", Surface: "photo", Requires: "photo", Message: "grey except red", Expect: Expect{
		Ops: []string{"has_op(grey_except) || param(hsl, ranges.0.saturation) <= -50", "mentions_range(red) || has_layer(hsl)"},
	}}
	good := Output{Model: "m", Edits: []Edit{{Summary: "Grey except red", Reason: "You asked to keep only the red", Ops: []map[string]any{{"op": "grey_except", "ranges": []any{"red"}}}}}}
	if r := Grade(c, good); !r.Pass {
		t.Errorf("good: %v", r.Failures)
	}
	if r := Grade(c, Output{Model: "m", Text: "Here is how you could do it."}); r.Pass {
		t.Error("text-only answer passed an op case")
	}
	del := Case{ID: "d", Surface: "studio", Requires: "studio", Message: "delete it", Expect: Expect{Ask: true, NoEdits: true}}
	if r := Grade(del, Output{Model: "m", Asks: []string{"Delete clipB?"}}); !r.Pass {
		t.Errorf("ask: %v", r.Failures)
	}
	if r := Grade(del, Output{Model: "m", Text: "Done.", Edits: good.Edits}); r.Pass {
		t.Error("deleting without asking passed")
	}
	sug := Case{ID: "h", Surface: "photo", Requires: "photo", Message: "heal", Expect: Expect{Actions: []string{"suggest:heal"}}}
	if r := Grade(sug, Output{Model: "m", Actions: []Action{{Kind: "heal"}}}); !r.Pass {
		t.Errorf("suggest: %v", r.Failures)
	}
}

func TestFilterSkipsMissingSchemas(t *testing.T) {
	cs, _ := LoadCases()
	run, skipped := Filter(cs, "", "", map[string]bool{"photo": true})
	for _, c := range run {
		if c.Requires == "studio" || c.Requires == "vector" {
			t.Errorf("%s ran without its schema", c.ID)
		}
	}
	if len(skipped) == 0 {
		t.Error("studio/vector cases should be skipped")
	}
	if run, _ := Filter(cs, "guide", "guide-gif-from-video", nil); len(run) != 1 {
		t.Errorf("--case filter = %d", len(run))
	}
}

func TestEstimateCost(t *testing.T) {
	cs, _ := LoadCases()
	guide, _ := Filter(cs, "guide", "", nil)
	m := assistantModels{"anthropic/claude-haiku-5.5", "anthropic/claude-sonnet-5.5", 2048, 8192}
	e := EstimateCost(guide, 1, m)
	if e.Cases != len(guide) || e.USD <= 0 || e.WorstUSD < e.USD || e.USD > 1 {
		t.Errorf("guide estimate %+v (target < $1)", e)
	}
	if e3 := EstimateCost(guide, 3, m); e3.USD < 2.9*e.USD {
		t.Errorf("repeat 3 = %v vs %v", e3.USD, e.USD)
	}
}

// scripted is a fake provider for the runner test: one text round with an
// open_tool call.
type scripted struct{}

type scriptedStream struct{ evs []assistant.Event }

func (scripted) Stream(ctx context.Context, req assistant.Request) (assistant.Stream, error) {
	return &scriptedStream{evs: []assistant.Event{
		{Kind: assistant.EventDelta, Text: "Use Compress video."},
		{Kind: assistant.EventUsage, Usage: &assistant.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.0003, Reported: true}},
		{Kind: assistant.EventDone, ToolCalls: []assistant.ToolCall{{ID: "c1", Name: assistant.ToolOpenTool, Arguments: `{"slug":"compress-video","label":"Open Compress video"}`}}},
	}}, nil
}

func (s *scriptedStream) Next() (assistant.Event, error) {
	if len(s.evs) == 0 {
		return assistant.Event{}, io.EOF
	}
	e := s.evs[0]
	s.evs = s.evs[1:]
	return e, nil
}
func (s *scriptedStream) Close() error { return nil }

func TestRunnerRecordsEngineEvents(t *testing.T) {
	k, err := assistant.NewEmbeddedKnowledge()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Load().Assistant
	e := &assistant.Engine{Provider: scripted{}, Knowledge: k, Ops: assistant.NewOpsValidator(), Cfg: cfg,
		Spend: assistant.NewSpendCap(0, nil, nil), Background: func(f func()) { f() }}
	r := &Runner{Engine: e}
	cs, _ := LoadCases()
	var c Case
	for _, x := range cs {
		if x.ID == "guide-compress-video-email" {
			c = x
		}
	}
	out, err := r.Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if out.Model == "" || out.CostUSD != 0.0003 || !strings.Contains(out.Text, "Compress video") {
		t.Fatalf("output %+v", out)
	}
	if g := Grade(c, out); !g.Pass {
		t.Errorf("graded %v", g.Failures)
	}
	// The scope gate answers without the provider; the runner marks it modelless.
	off := Case{ID: "o", Surface: "guide", Locale: "en-US", Page: "/", Message: "write me a poem about cats", Expect: Expect{OutOfScope: true}}
	out, _ = r.Run(context.Background(), off)
	if out.Model != "" || !Grade(off, out).Pass {
		t.Errorf("gate output %+v", out)
	}
}

// The harness never calls a model without the flag (decision 5).
func TestHarnessRefusesWithoutFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the command")
	}
	bin := t.TempDir() + "/assistant-eval"
	if out, err := exec.Command("go", "build", "-o", bin, "../../../cmd/assistant-eval").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "--surface", "guide")
	cmd.Env = append(os.Environ(), "ASSISTANT_OPENROUTER_API_KEY=sk-test-not-real", "OPENROUTER_BASE_URL=http://127.0.0.1:1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--i-know-this-costs-money") || !strings.Contains(string(out), "estimate") {
		t.Errorf("harness ran without the flag (err %v):\n%s", err, out)
	}
	est := exec.Command(bin, "--estimate")
	est.Env = cmd.Env
	if out, err := est.CombinedOutput(); err != nil || !strings.Contains(string(out), "typical") {
		t.Errorf("--estimate: %v\n%s", err, out)
	}
}

// 2026-11-05 photo eval: a Russian edit turn with no prose was graded "en".
// The undo label is what the user reads, so the language check counts it.
func TestGradeLanguageCountsEditLabel(t *testing.T) {
	c := Case{ID: "ru-edit", Surface: "photo", Expect: Expect{Language: "ru"}}
	out := Output{Model: "m", Edits: []Edit{{Summary: "Тёплый баланс белого", Reason: "На фото заметный холодный оттенок 4500 K", Ops: []map[string]any{{"op": "auto_tone"}}}}}
	if r := Grade(c, out); !r.Pass {
		t.Fatalf("Russian undo label with empty text should pass the language check: %v", r.Failures)
	}
	out.Edits[0].Summary = "Warm white balance"
	if r := Grade(c, out); r.Pass {
		t.Fatal("an English-only edit turn must fail a ru language check")
	}
}

func TestAnyOfReadsButtonLabels(t *testing.T) {
	c := Case{ID: "mask", Surface: "photo", Expect: Expect{AnyOf: []string{"subject", "mask"}}}
	out := Output{Model: "m", Actions: []Action{{Kind: "suggest_action", Label: "Select the person", Params: map[string]any{"kind": "subject_mask"}}}}
	if r := Grade(c, out); !r.Pass {
		t.Fatalf("a subject_mask button should satisfy any_of: %v", r.Failures)
	}
}

// MM114: reason_present and reason_cites run on every copilot answer that edited.
func TestGradeReasons(t *testing.T) {
	c := Case{ID: "r", Surface: "photo", Requires: "photo", Message: "Brighten it a bit", Expect: Expect{Ops: []string{"has_op(auto_tone)"}}}
	ops := []map[string]any{{"op": "auto_tone"}}
	edit := func(reason string) Output {
		return Output{Model: "m", Edits: []Edit{{Summary: "Auto tone", Reason: reason, Ops: ops}}}
	}
	for _, ok := range []string{
		"Shadows are clipped: 3 % of pixels sit at black",
		"You asked to brighten it, so I lift the exposure",
		"The histogram is bunched in the mids",
		"Das Foto ist zu dunkel, du wolltest es heller",
	} {
		c.Message = "Brighten it a bit"
		if strings.HasPrefix(ok, "Das") {
			c.Message = "Mach es heller, das Foto ist zu dunkel"
		}
		if r := Grade(c, edit(ok)); !r.Pass {
			t.Errorf("%q: %v", ok, r.Failures)
		}
	}
	c.Message = "Brighten it a bit"
	r := Grade(c, edit(""))
	if r.Pass || !strings.Contains(strings.Join(r.Failures, ";"), "reason_present") {
		t.Errorf("a missing reason must fail reason_present: %v", r.Failures)
	}
	r = Grade(c, edit("ok"))
	if r.Pass || !strings.Contains(strings.Join(r.Failures, ";"), "reason_present") {
		t.Errorf("a 2-character reason must fail reason_present: %v", r.Failures)
	}
	r = Grade(c, edit("Because it looks nicer this way"))
	if r.Pass || !strings.Contains(strings.Join(r.Failures, ";"), "reason_cites") {
		t.Errorf("a generic reason must fail reason_cites: %v", r.Failures)
	}
	if r := Grade(c, edit(strings.Repeat("x", 141))); r.Pass {
		t.Error("a 141-character reason passed")
	}
	// No edit, no reason needed; the guide never edits.
	if r := Grade(Case{ID: "g", Surface: "guide", Message: "hi"}, Output{Model: "m", Text: "Hello"}); len(ReasonProblems(Case{Surface: "guide"}, Output{Edits: []Edit{{}}})) != 0 || !r.Pass {
		t.Errorf("guide: %v", r.Failures)
	}
}

// MM111: an edit turn with no prose satisfies any_of through its edit summary
// (the undo label), the way the language check already reads it.
func TestAnyOfReadsEditSummaries(t *testing.T) {
	mentions := func(r Result) bool {
		for _, f := range r.Failures {
			if strings.Contains(f, "mentions none of") {
				return false
			}
		}
		return true
	}
	c := Case{ID: "x", Surface: "vector", Expect: Expect{AnyOf: []string{"gradient", "shimmer"}}}
	if !mentions(Grade(c, Output{Model: "m", Edits: []Edit{{Summary: "Gradient headline for shimmer"}}})) {
		t.Fatal("an edit summary should satisfy any_of")
	}
	if mentions(Grade(c, Output{Model: "m", Edits: []Edit{{Summary: "Spin the logo"}}})) {
		t.Fatal("an unrelated edit summary must not satisfy any_of")
	}
}

// MM121: value(op, field, a|b) compares a string field of the first op with that name.
func TestCheckValue(t *testing.T) {
	o := `[{"op":"set_caption_style","look":"bold_pop","animation":"pop"}]`
	if ok, why := check(t, "value(set_caption_style, look, bold_pop|clean_highlight)", o); !ok {
		t.Errorf("look should match: %s", why)
	}
	if ok, _ := check(t, "value(set_caption_style, animation, karaoke_fill)", o); ok {
		t.Error("animation pop must not match karaoke_fill")
	}
	if ok, _ := check(t, "value(set_caption_style, look, bold_pop)", `[{"op":"grade"}]`); ok {
		t.Error("no such op must fail")
	}
	if _, err := ParseCheck("value(set_caption_style, look)"); err == nil {
		t.Error("value needs three arguments")
	}
}
