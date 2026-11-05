// Package evals is Medi's code-graded eval harness (MM106, design §16).
//
// A case (cases/*.yaml, embedded) is one user message on one surface plus
// expectations checked by code, never by a model:
//
//	id: guide-gif-from-video
//	surface: guide            # guide | photo | studio | vector
//	locale: en-US
//	page: /tools              # optional: the page the user is on
//	requires: photo           # optional: skipped until that surface's op schema exists (MM108–MM110)
//	fixture_doc: photo/backlit.json   # optional: digest source (testdata/fixtures/…)
//	frame: photo/backlit.jpg          # optional: preview frame (testdata/frames/…)
//	message: "how do I make a gif from a video"
//	expect:
//	  actions: [tool:convert-video-to-animated-gif]   # an action button for each (any order)
//	  ops: ["has_op(grey_except)", "param(hsl, saturation) < -50"]
//	  must_include: ["GIF"]                             # case-insensitive substrings of the answer
//	  must_not_include: ["$"]                           # never in the answer
//	  any_of: ["Compress video", "compress-video"]      # at least one
//	  language: ru                                      # the answer's script/language
//	  out_of_scope: true                                # answered by the scope gate, no model call
//	  off_topic: true                                   # the gate, or a short decline without buttons
//	  no_edits: true                                    # must not apply edits (e.g. ask before deleting)
//	  ask: true                                         # must ask a clarifying question
//
// The runner (`cmd/assistant-eval`) drives the real engine in-process with the
// real provider; grading (`Grade`) is a pure function of the recorded events,
// so every check primitive is unit-tested on canned outputs without a network.
package evals

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed cases/*.yaml
var casesFS embed.FS

//go:embed testdata
var testdataFS embed.FS

// Case is one eval case.
type Case struct {
	ID         string `yaml:"id"`
	Surface    string `yaml:"surface"`
	Locale     string `yaml:"locale"`
	Page       string `yaml:"page"`
	Requires   string `yaml:"requires"`
	FixtureDoc string `yaml:"fixture_doc"`
	Frame      string `yaml:"frame"`
	Message    string `yaml:"message"`
	Expect     Expect `yaml:"expect"`
	// Source is the file the case came from (set by the loader).
	Source string `yaml:"-"`
}

// Expect is what a passing answer shows.
type Expect struct {
	Actions        []string `yaml:"actions"`
	Ops            []string `yaml:"ops"`
	MustInclude    []string `yaml:"must_include"`
	MustNotInclude []string `yaml:"must_not_include"`
	AnyOf          []string `yaml:"any_of"`
	Language       string   `yaml:"language"`
	OutOfScope     bool     `yaml:"out_of_scope"`
	// NoEdits: the answer must not apply edits (e.g. "ask before deleting").
	NoEdits bool `yaml:"no_edits"`
	// Ask: the answer must be (or include) a clarifying question.
	Ask bool `yaml:"ask"`
	// OffTopic: answered by the scope gate, or a short decline from the model
	// with no buttons or edits that steers back to the site.
	OffTopic bool `yaml:"off_topic"`
}

// Surfaces are the valid case surfaces.
var Surfaces = []string{"guide", "photo", "studio", "vector"}

// LoadCases parses every embedded case file, sorted by id.
func LoadCases() ([]Case, error) { return LoadCasesFrom(casesFS, "cases") }

// LoadCasesFrom parses every *.yaml under dir of fsys (each file holds a list).
func LoadCasesFrom(fsys fs.FS, dir string) ([]Case, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []Case
	seen := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var cs []Case
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&cs); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		for _, c := range cs {
			c.Source = e.Name()
			if err := c.validate(); err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			if prev, dup := seen[c.ID]; dup {
				return nil, fmt.Errorf("%s: duplicate case id %s (also in %s)", e.Name(), c.ID, prev)
			}
			seen[c.ID] = e.Name()
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *Case) validate() error {
	if c.ID == "" || strings.ContainsAny(c.ID, " /") {
		return fmt.Errorf("case id %q", c.ID)
	}
	if !contains(Surfaces, c.Surface) {
		return fmt.Errorf("%s: surface %q", c.ID, c.Surface)
	}
	if strings.TrimSpace(c.Message) == "" {
		return fmt.Errorf("%s: empty message", c.ID)
	}
	if c.Locale == "" {
		c.Locale = "en-US"
	}
	if c.Page == "" {
		c.Page = "/"
	}
	if c.Requires != "" && !contains(Surfaces[1:], c.Requires) {
		return fmt.Errorf("%s: requires %q", c.ID, c.Requires)
	}
	if c.Surface != "guide" && c.Requires == "" && len(c.Expect.Ops) > 0 {
		return fmt.Errorf("%s: op checks need `requires: <surface>`", c.ID)
	}
	for _, a := range c.Expect.Actions {
		if _, _, err := parseActionExpect(a); err != nil {
			return fmt.Errorf("%s: %w", c.ID, err)
		}
	}
	for _, o := range c.Expect.Ops {
		if _, err := ParseCheck(o); err != nil {
			return fmt.Errorf("%s: %w", c.ID, err)
		}
	}
	if c.FixtureDoc != "" {
		if _, err := fs.Stat(testdataFS, path.Join("testdata/fixtures", c.FixtureDoc)); err != nil {
			return fmt.Errorf("%s: fixture %s: %w", c.ID, c.FixtureDoc, err)
		}
	}
	if c.Frame != "" {
		if _, err := fs.Stat(testdataFS, path.Join("testdata/frames", c.Frame)); err != nil {
			return fmt.Errorf("%s: frame %s: %w", c.ID, c.Frame, err)
		}
	}
	return nil
}

// Fixture returns a committed fixture document (the digest source).
func Fixture(name string) ([]byte, error) {
	return fs.ReadFile(testdataFS, path.Join("testdata/fixtures", name))
}

// Frame returns a committed preview JPEG.
func Frame(name string) ([]byte, error) {
	return fs.ReadFile(testdataFS, path.Join("testdata/frames", name))
}

// Output is what one case produced: the answer text, the client-tool events
// and the cost, recorded by the runner (or written by hand in grader tests).
type Output struct {
	Text    string
	Actions []Action
	Edits   []Edit
	Asks    []string
	// Error is the stream's error code ("" = none).
	Error   string
	CostUSD float64
	// ToolCalls counts the server and client tool calls of the request
	// (MM113: the median per surface is recorded with the run).
	ToolCalls int
	// PromptVersion is the meta event's build hash.
	PromptVersion string
	// Model is the meta event's model; "" means no model ran (scope gate or
	// static answer).
	Model string
}

// Action is one action button (navigate / open_tool / suggest_action).
type Action struct {
	Kind   string
	Label  string
	Params map[string]any
}

// Edit is one apply_edits call.
type Edit struct {
	Summary string
	Ops     []map[string]any
}

// Result is one graded case.
type Result struct {
	Case     Case
	Pass     bool
	Skipped  string // non-empty = not run (reason)
	Failures []string
	Output   Output
}

// Grade checks an output against the case's expectations. Pure: the grader
// tests feed canned outputs.
func Grade(c Case, out Output) Result {
	r := Result{Case: c, Output: out}
	fail := func(f string, a ...any) { r.Failures = append(r.Failures, fmt.Sprintf(f, a...)) }
	if out.Error != "" {
		fail("stream error %s", out.Error)
	}
	e := c.Expect
	low := strings.ToLower(out.Text)
	switch {
	case e.OutOfScope:
		if out.Model != "" || len(out.Edits) > 0 || len(out.Actions) > 0 {
			fail("expected the scope gate to answer without a model call")
		}
	case e.OffTopic:
		if out.Model != "" {
			if len(out.Edits) > 0 || len(out.Actions) > 0 {
				fail("off-topic request got buttons or edits")
			}
			if n := len([]rune(out.Text)); n > 700 {
				fail("off-topic answer is %d characters; expected a short decline", n)
			}
			if !strings.Contains(low, "media") && !strings.Contains(low, "medi") && !strings.Contains(low, "tool") && !strings.Contains(low, "edit") {
				fail("off-topic decline does not steer back to the site")
			}
		}
	case out.Model == "" && out.Error == "":
		fail("expected a model answer, got the scope gate or a static answer")
	}
	for _, s := range e.MustInclude {
		if !strings.Contains(low, strings.ToLower(s)) {
			fail("answer lacks %q", s)
		}
	}
	for _, s := range e.MustNotInclude {
		if strings.Contains(low, strings.ToLower(s)) {
			fail("answer contains forbidden %q", s)
		}
	}
	if len(e.AnyOf) > 0 {
		hit := false
		for _, s := range e.AnyOf {
			if strings.Contains(low, strings.ToLower(s)) || actionsMention(out.Actions, s) {
				hit = true
			}
		}
		if !hit {
			fail("answer mentions none of %q", e.AnyOf)
		}
	}
	for _, a := range e.Actions {
		kind, target, _ := parseActionExpect(a)
		if !hasAction(out.Actions, kind, target) {
			fail("no %s action for %s (got %s)", kind, target, describeActions(out.Actions))
		}
	}
	if e.Language != "" {
		// An edit turn may carry no prose: its undo label is the user-visible
		// text then (photo-ru-moody, 2026-11-05: Russian label, empty text).
		said := out.Text
		for _, ed := range out.Edits {
			said += " " + ed.Summary
		}
		said += " " + strings.Join(out.Asks, " ")
		if l := DetectLanguage(strings.TrimSpace(said)); l != e.Language {
			fail("answer language %s, want %s", l, e.Language)
		}
	}
	if e.NoEdits && len(out.Edits) > 0 {
		fail("applied %d edit(s); expected none", len(out.Edits))
	}
	if e.Ask && len(out.Asks) == 0 && !strings.Contains(out.Text, "?") {
		fail("expected a clarifying question")
	}
	if len(e.Ops) > 0 {
		var ops []map[string]any
		for _, ed := range out.Edits {
			ops = append(ops, ed.Ops...)
		}
		for _, src := range e.Ops {
			chk, _ := ParseCheck(src)
			if ok, why := chk.Eval(ops); !ok {
				fail("op check %s: %s", src, why)
			}
		}
	}
	r.Pass = len(r.Failures) == 0
	return r
}

// parseActionExpect splits "tool:<slug>" / "page:<route>" / "suggest:<kind>".
func parseActionExpect(s string) (kind, target string, err error) {
	k, t, ok := strings.Cut(s, ":")
	if !ok || t == "" || (k != "tool" && k != "page" && k != "suggest") {
		return "", "", fmt.Errorf("action expectation %q: use tool:<slug>, page:<route> or suggest:<kind>", s)
	}
	return k, t, nil
}

func hasAction(acts []Action, kind, target string) bool {
	for _, a := range acts {
		switch kind {
		case "tool":
			if a.Kind == "open_tool" && strParam(a.Params, "slug") == target {
				return true
			}
			if a.Kind == "navigate" && stripLocale(strParam(a.Params, "target")) == "/tools/"+target {
				return true
			}
		case "page":
			if a.Kind == "navigate" && stripLocale(strParam(a.Params, "target")) == target {
				return true
			}
		case "suggest":
			if a.Kind == target {
				return true
			}
		}
	}
	return false
}

func actionsMention(acts []Action, s string) bool {
	s = strings.ToLower(s)
	for _, a := range acts {
		if strings.Contains(strings.ToLower(a.Kind), s) || strings.Contains(strings.ToLower(a.Label), s) {
			return true
		}
		for _, v := range a.Params {
			if str, ok := v.(string); ok && strings.Contains(strings.ToLower(str), s) {
				return true
			}
		}
	}
	return false
}

func describeActions(acts []Action) string {
	if len(acts) == 0 {
		return "none"
	}
	var parts []string
	for _, a := range acts {
		b, _ := json.Marshal(a.Params)
		parts = append(parts, a.Kind+string(b))
	}
	return strings.Join(parts, ", ")
}

func strParam(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return s
}

// stripLocale drops a leading locale prefix ("/ru/tools/x" → "/tools/x").
func stripLocale(route string) string {
	parts := strings.SplitN(strings.TrimPrefix(route, "/"), "/", 2)
	if len(parts) == 2 && localePrefix(parts[0]) {
		return "/" + parts[1]
	}
	return route
}

func localePrefix(s string) bool {
	switch s {
	case "uk", "ru", "he", "de", "es", "es-mx", "fr", "it", "ja", "zh-cn", "zh-tw", "ko", "ar", "hi", "pt-pt", "pt-br", "vi":
		return true
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
