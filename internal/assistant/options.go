package assistant

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Per-tool option schemas (MM116a, design §8.4). The UI exports
// knowledge/tools/<slug>.options.json from the code that builds each tool's
// request (`npm run export:medi`); this file validates open_tool.options against
// it before the action is emitted, and renders the fields for the model.

// MaxOptionsBytes bounds the JSON of one open_tool.options (the UI refuses a
// `?o=` link larger than this too: lib/assistant/toolOptions/codec.ts).
const MaxOptionsBytes = 1536

// ToolOptions is one tool's options file: either an explicit "none" with the
// reason the page cannot be prefilled, or a strict JSON Schema.
type ToolOptions struct {
	Slug   string
	Family string
	// None: the page has no prefillable options (editors, multi-step flows).
	None   bool
	Reason string
	Schema json.RawMessage

	once     sync.Once
	compiled *jsonschema.Schema
	err      error
}

// optionsFile is the on-disk shape of tools/<slug>.options.json.
type optionsFile struct {
	Slug    string          `json:"slug"`
	Family  string          `json:"family"`
	Options json.RawMessage `json:"options"`
	Reason  string          `json:"reason"`
}

// ParseToolOptions parses one tools/<slug>.options.json (the corpus loader and tests in other packages).
func ParseToolOptions(b []byte) (*ToolOptions, error) { return parseOptionsFile(b) }

func parseOptionsFile(b []byte) (*ToolOptions, error) {
	var f optionsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Slug == "" || len(f.Options) == 0 {
		return nil, fmt.Errorf("options file needs a slug and options")
	}
	o := &ToolOptions{Slug: f.Slug, Family: f.Family}
	if bytes.Equal(bytes.TrimSpace(f.Options), []byte(`"none"`)) {
		o.None, o.Reason = true, f.Reason
		return o, nil
	}
	var probe struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(f.Options, &probe); err != nil || probe.Type != "object" || len(probe.Properties) == 0 {
		return nil, fmt.Errorf("options schema of %s is not an object schema with properties", f.Slug)
	}
	o.Schema = f.Options
	return o, nil
}

func (o *ToolOptions) compile() (*jsonschema.Schema, error) {
	o.once.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(o.Schema))
		if err != nil {
			o.err = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("mem://options.json", doc); err != nil {
			o.err = err
			return
		}
		o.compiled, o.err = c.Compile("mem://options.json")
	})
	return o.compiled, o.err
}

// Validate checks an options object against the tool's schema. The caller has
// already decided options are wanted; a "none" tool refuses every object.
func (o *ToolOptions) Validate(raw json.RawMessage) error {
	if o == nil {
		return fmt.Errorf("this tool has no option schema")
	}
	if o.None {
		return fmt.Errorf("tool %q cannot be prefilled (%s); open it without options", o.Slug, o.Reason)
	}
	if len(raw) > MaxOptionsBytes {
		return fmt.Errorf("options are larger than %d bytes", MaxOptionsBytes)
	}
	s, err := o.compile()
	if err != nil {
		return fmt.Errorf("the option schema of %q could not be compiled", o.Slug)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("options are not JSON: %v", err)
	}
	if m, ok := inst.(map[string]any); !ok || len(m) == 0 {
		return fmt.Errorf("options must be a non-empty object")
	}
	if err := s.Validate(inst); err != nil {
		return fmt.Errorf("options do not match the %s option schema: %v", o.Slug, err)
	}
	return nil
}

// FieldNames lists the schema's property names, sorted (error messages, tests).
func (o *ToolOptions) FieldNames() []string {
	names, _ := orderedProperties(o.Schema)
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

// Render is the model-facing text of the options: one line per field with its
// label, type or choices, range, unit and default, in schema order.
func (o *ToolOptions) Render() string {
	if o == nil {
		return ""
	}
	if o.None {
		return "Options: none. " + o.Reason + " Open it without options."
	}
	order, props := orderedProperties(o.Schema)
	var b strings.Builder
	b.WriteString("Options for open_tool.options (use only these fields; every field is optional; ranges are enforced):\n")
	for _, name := range order {
		var p struct {
			Label       string   `json:"x-uiLabel"`
			Description string   `json:"description"`
			Unit        string   `json:"unit"`
			Type        string   `json:"type"`
			Enum        []any    `json:"enum"`
			Const       any      `json:"const"`
			Min         *float64 `json:"minimum"`
			Max         *float64 `json:"maximum"`
			MaxLen      *int     `json:"maxLength"`
			Default     any      `json:"default"`
			AnyOf       []struct {
				Const any `json:"const"`
			} `json:"anyOf"`
		}
		if json.Unmarshal(props[name], &p) != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s (%s): ", name, p.Label)
		switch {
		case len(p.Enum) > 0:
			parts := make([]string, len(p.Enum))
			for i, e := range p.Enum {
				parts[i] = fmt.Sprint(e)
			}
			b.WriteString("one of " + strings.Join(parts, " | "))
		case len(p.AnyOf) > 0:
			parts := make([]string, 0, len(p.AnyOf))
			for _, e := range p.AnyOf {
				parts = append(parts, fmt.Sprint(e.Const))
			}
			b.WriteString("one of " + strings.Join(parts, " | "))
		case p.Type == "boolean":
			b.WriteString("true | false")
		case p.Type == "string":
			b.WriteString("text")
			if p.MaxLen != nil {
				fmt.Fprintf(&b, " (<= %d chars)", *p.MaxLen)
			}
		default:
			b.WriteString(p.Type)
			if p.Min != nil && p.Max != nil {
				fmt.Fprintf(&b, " %v to %v", *p.Min, *p.Max)
			}
		}
		if p.Unit != "" {
			b.WriteString(" " + p.Unit)
		}
		if p.Default != nil {
			fmt.Fprintf(&b, "; default %v", p.Default)
		}
		if p.Description != "" {
			b.WriteString(". " + p.Description)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// orderedProperties returns a schema's top-level properties in document order.
func orderedProperties(schema json.RawMessage) (names []string, raw map[string]json.RawMessage) {
	raw = map[string]json.RawMessage{}
	dec := json.NewDecoder(bytes.NewReader(schema))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, raw
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return names, raw
		}
		key, _ := kt.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return names, raw
		}
		if key != "properties" {
			continue
		}
		pd := json.NewDecoder(bytes.NewReader(val))
		if t, err := pd.Token(); err != nil || t != json.Delim('{') {
			return names, raw
		}
		for pd.More() {
			nt, err := pd.Token()
			if err != nil {
				return names, raw
			}
			name, _ := nt.(string)
			var pv json.RawMessage
			if err := pd.Decode(&pv); err != nil {
				return names, raw
			}
			names = append(names, name)
			raw[name] = pv
		}
	}
	return names, raw
}
