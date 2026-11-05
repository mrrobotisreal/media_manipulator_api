package evals

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

func openAct(slug string, opts map[string]any) Action {
	p := map[string]any{"slug": slug}
	if opts != nil {
		p["options"] = opts
	}
	return Action{Kind: "open_tool", Params: p}
}

func optEval(t *testing.T, expr string, acts ...Action) bool {
	t.Helper()
	c, err := ParseOptionCheck(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	ok, _ := c.Eval(acts)
	return ok
}

// MM116a: the option-check grammar.
func TestOptionChecks(t *testing.T) {
	a := openAct("compress-video", map[string]any{"level": "smallest", "crf": float64(32), "stripAudio": true, "resolution": "720"})
	for expr, want := range map[string]bool{
		"compress-video.level = smallest":                             true,
		"compress-video.level = balanced":                             false,
		"compress-video.level != balanced":                            true,
		"compress-video.crf in 28..40":                                true,
		"compress-video.crf in 33..40":                                false,
		"compress-video.crf >= 32":                                    true,
		"compress-video.crf > 32":                                     false,
		"compress-video.crf <= 32":                                    true,
		"compress-video.crf < 32":                                     false,
		"compress-video.crf = 32":                                     true,
		"compress-video.stripAudio = true":                            true,
		"compress-video.stripAudio = false":                           false,
		"compress-video.resolution in [480|720]":                      true,
		"compress-video.resolution in [480|1080]":                     false,
		"compress-video.crf present":                                  true,
		"compress-video.codec present":                                false,
		"compress-video.codec absent":                                 true,
		"compress-video.options absent":                               false,
		"compress-video.level = balanced || compress-video.crf >= 30": true,
		"compress-video.level = balanced || compress-video.crf >= 40": false,
		"image-resizer.width = 800":                                   false, // a different tool
	} {
		if got := optEval(t, expr, a); got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
	if !optEval(t, "compress-video.options absent", openAct("compress-video", nil)) {
		t.Error("a plain button should satisfy `options absent`")
	}
	if optEval(t, "compress-video.options absent", openAct("compress-video", map[string]any{})) != true {
		t.Error("an empty options object counts as absent")
	}
	// A string field that holds a number text still compares as text.
	if !optEval(t, "audio-converter.bitrate = 128", openAct("audio-converter", map[string]any{"bitrate": "128"})) {
		t.Error("string 128 should equal 128")
	}
	for _, bad := range []string{"", "compress-video", "compress-video.crf", "compress-video.crf in 40..28", "compress-video.crf in 1", "compress-video.crf >= x", "x.y ~ 1"} {
		if _, err := ParseOptionCheck(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// MM116a acceptance: Routing ≥ 120 cases with ≥ 3 in every one of the 18
// locales, Options ≥ 40, every expected slug exists, every option field
// exists in the shipped schema of its tool and every value fits it.
func TestRoutingAndOptionsSuites(t *testing.T) {
	cs, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	routing, options := SuiteCases(cs, "routing"), SuiteCases(cs, "options")
	if len(routing) < 120 || len(options) < 40 {
		t.Fatalf("routing %d (want ≥ 120), options %d (want ≥ 40)", len(routing), len(options))
	}
	perLocale := map[string]int{}
	for _, c := range routing {
		perLocale[c.Locale]++
		if c.Locale != "en-US" && c.Expect.Language == "" {
			t.Errorf("%s: a non-English routing case must check the answer language", c.ID)
		}
	}
	if len(perLocale) != 18 {
		t.Errorf("routing covers %d locales, want 18: %v", len(perLocale), perLocale)
	}
	for l, n := range perLocale {
		if n < 3 {
			t.Errorf("locale %s has %d routing cases, want ≥ 3", l, n)
		}
	}
	// The default run never includes the named suites.
	for _, c := range SuiteCases(cs, "") {
		if c.Suite != "" {
			t.Errorf("%s leaked into the default run", c.ID)
		}
	}

	k, err := assistant.NewEmbeddedKnowledge()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append(append([]Case{}, routing...), options...) {
		for _, a := range c.Expect.Actions {
			_, slug, _ := parseActionExpect(a)
			if _, ok := k.Routes().Tool(slug); !ok {
				t.Errorf("%s: unknown tool %s", c.ID, slug)
			}
		}
	}
	for _, c := range options {
		for _, src := range c.Expect.Options {
			chk, _ := ParseOptionCheck(src)
			for _, sf := range chk.Fields() {
				slug, field := sf[0], sf[1]
				o, ok := k.Corpus().ToolOptions(slug)
				if !ok {
					t.Errorf("%s: no options for %s", c.ID, slug)
					continue
				}
				if field == "options" {
					continue
				}
				if !strings.Contains(strings.Join(o.FieldNames(), ","), field) && !o.None {
					t.Errorf("%s: %s has no field %s (fields %v)", c.ID, slug, field, o.FieldNames())
				}
			}
		}
	}
}

// The grader reports an option failure with the field and the value it saw.
func TestGradeOptionFailureMessage(t *testing.T) {
	c := Case{ID: "x", Surface: "guide", Expect: Expect{Options: []string{"compress-video.crf in 28..40"}}}
	r := Grade(c, Output{Model: "m", Actions: []Action{openAct("compress-video", map[string]any{"crf": float64(18)})}})
	if r.Pass || len(r.Failures) != 1 || !strings.Contains(r.Failures[0], "crf = 18") {
		t.Errorf("failures = %v", r.Failures)
	}
	r = Grade(c, Output{Model: "m", Actions: []Action{openAct("compress-video", map[string]any{"crf": float64(30)})}})
	if !r.Pass {
		t.Errorf("a correct answer failed: %v", r.Failures)
	}
}

func TestLoadCasesRejectsBadSuites(t *testing.T) {
	for name, src := range map[string]string{
		"unknown suite":      "- {id: a, surface: guide, suite: nope, message: hi}",
		"suite on a copilot": "- {id: a, surface: photo, requires: photo, suite: routing, message: hi}",
		"bad option check":   "- {id: a, surface: guide, suite: options, message: hi, expect: {options: ['nope']}}",
	} {
		fsys := fstestMap(src)
		if _, err := LoadCasesFrom(fsys, "c"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func fstestMap(src string) fstest.MapFS { return fstest.MapFS{"c/x.yaml": {Data: []byte(src)}} }

// A button with no prose has no language to judge (route-ar-sa-2, 2026-11-05).
func TestGradeLanguageSkipsEmptyProse(t *testing.T) {
	c := Case{ID: "x", Surface: "guide", Expect: Expect{Language: "ar", Actions: []string{"tool:compress-video"}}}
	r := Grade(c, Output{Model: "m", Actions: []Action{openAct("compress-video", nil)}})
	if !r.Pass {
		t.Errorf("an action-only answer failed the language check: %v", r.Failures)
	}
	r = Grade(c, Output{Model: "m", Text: "Here is the tool.", Actions: []Action{openAct("compress-video", nil)}})
	if r.Pass {
		t.Error("English prose passed an Arabic expectation")
	}
}
