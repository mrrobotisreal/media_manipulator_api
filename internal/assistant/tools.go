package assistant

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Tool names (design §8.1).
const (
	ToolSearchHelp    = "search_help"
	ToolGetRecipe     = "get_recipe"
	ToolGetToolPage   = "get_tool_page"
	ToolNavigate      = "navigate"
	ToolOpenTool      = "open_tool"
	ToolApplyEdits    = "apply_edits"
	ToolSuggestAction = "suggest_action"
	ToolAskUser       = "ask_user"
	// MM115: Medi's eyes. analyze_compare diffs the browser's before/after
	// meters; the file tools measure an uploaded file (offered only with a ref).
	ToolAnalyzeCompare = "analyze_compare"
	ToolAnalyzeImage   = "analyze_image"
	ToolAnalyzeAudio   = "analyze_audio"
	ToolAnalyzeVideo   = "analyze_video"
)

// serverTools run on the API inside the upstream loop; every other tool is a
// client tool that ends the upstream stream.
var serverTools = map[string]bool{ToolSearchHelp: true, ToolGetRecipe: true, ToolGetToolPage: true,
	ToolAnalyzeCompare: true, ToolAnalyzeImage: true, ToolAnalyzeAudio: true, ToolAnalyzeVideo: true}

// IsServerTool reports whether the API executes the tool itself.
func IsServerTool(name string) bool { return serverTools[name] }

// SuggestKinds are the suggest_action kinds (design §8.1): anything that
// spends allowance or needs a human.
var SuggestKinds = []string{"generate_captions", "subject_mask", "heal", "voiceover", "derive_audio", "export", "open_tool"}

const (
	maxSummaryChars  = 60
	maxReasonChars   = 140
	maxQuestionChars = 300
	maxChoiceChars   = 60
	maxLabelChars    = 60
)

func schema(s string) json.RawMessage { return json.RawMessage(s) }

func quotedList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = fmt.Sprintf("%q", x)
	}
	return strings.Join(q, ",")
}

// ToolsFor lists the tools a surface offers. apply_edits is offered only
// where an op schema exists (decision 1); the guide gets navigate/open_tool,
// the copilots get apply_edits/suggest_action. Order is fixed (it is part of
// the cached prefix upstream).
func ToolsFor(surface string, ops *OpsValidator) []ToolDef {
	tools := []ToolDef{
		{Name: ToolSearchHelp, Description: "Search Media Manipulator's help: tool pages, site pages, editor capabilities and pro recipes. Returns up to 4 snippets with ids.",
			Parameters: schema(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":200}},"required":["query"],"additionalProperties":false}`)},
		{Name: ToolGetRecipe, Description: "Read one recipe (a professional technique with an op list) by id, e.g. recipes/photo/grey-except-red.",
			Parameters: schema(`{"type":"object","properties":{"id":{"type":"string","minLength":1,"maxLength":120}},"required":["id"],"additionalProperties":false}`)},
		{Name: ToolGetToolPage, Description: "Read one tool's full help page by slug, e.g. compress-video.",
			Parameters: schema(`{"type":"object","properties":{"slug":{"type":"string","minLength":1,"maxLength":120}},"required":["slug"],"additionalProperties":false}`)},
	}
	if surface == SurfaceGuide {
		tools = append(tools,
			ToolDef{Name: ToolNavigate, Description: "Show the user a button that opens a site page. target must be a site-relative route from the help documents, e.g. /pricing.",
				Parameters: schema(`{"type":"object","properties":{"target":{"type":"string","pattern":"^/[A-Za-z0-9/_\\-]*$","maxLength":200},"label":{"type":"string","maxLength":60}},"required":["target"],"additionalProperties":false}`)},
			ToolDef{Name: ToolOpenTool, Description: "Show the user a button that opens a tool, optionally with one of its presets.",
				Parameters: schema(`{"type":"object","properties":{"slug":{"type":"string","pattern":"^[a-z0-9\\-]+$","maxLength":120},"preset":{"type":"string","maxLength":80},"label":{"type":"string","maxLength":60}},"required":["slug"],"additionalProperties":false}`)},
		)
	} else {
		tools = append(tools, ToolDef{Name: ToolAnalyzeCompare,
			Description: "Compare the preview's meters before and after your last edit (mean luma, contrast, shadows and highlights clipped, warmth, tint). Call it after a visual edit, before you decide to stop or adjust once more. Returns each figure's before, after and change.",
			Parameters:  schema(`{"type":"object","properties":{},"additionalProperties":false}`)})
		if ops.Has(surface) {
			tools = append(tools, ToolDef{Name: ToolApplyEdits,
				Description: "Apply edits to the open document as ONE undoable step. summary (<= 60 chars) becomes the undo label. reason (4-140 chars) says why, citing what you saw in the digest or what the user asked; ops may carry their own optional reason. ops use only ids from the document digest.",
				Parameters:  applyEditsParameters(ops.Schema(surface))})
		}
		tools = append(tools, ToolDef{Name: ToolSuggestAction,
			Description: "Offer a button for something that spends the user's allowance or needs a human: captions, AI subject mask, heal, voiceover, derived audio, export, or opening a tool.",
			Parameters:  schema(`{"type":"object","properties":{"kind":{"type":"string","enum":[` + quotedList(SuggestKinds) + `]},"label":{"type":"string","minLength":1,"maxLength":60},"params":{"type":"object"}},"required":["kind","label"],"additionalProperties":false}`)})
	}
	tools = append(tools, ToolDef{Name: ToolAskUser, Description: "Ask the user one short clarifying question with up to 4 choices before a large or destructive edit.",
		Parameters: schema(`{"type":"object","properties":{"question":{"type":"string","minLength":1,"maxLength":300},"choices":{"type":"array","maxItems":4,"items":{"type":"string","minLength":1,"maxLength":60}}},"required":["question"],"additionalProperties":false}`)})
	return tools
}

// toolSchema finds a tool's parameter schema in a tool list.
func toolSchema(tools []ToolDef, name string) (json.RawMessage, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t.Parameters, true
		}
	}
	return nil, false
}

// ClientAction is a validated client tool call, ready to stream.
type ClientAction struct {
	CallID string
	Name   string
	// apply_edits
	Summary string
	Reason  string
	Ops     json.RawMessage
	// navigate / open_tool / suggest_action
	Kind   string
	Label  string
	Params map[string]any
	// ask_user
	Question string
	Choices  []string
	// Awaits is true when the client must answer with editResult (apply_edits).
	Awaits bool
}

// applyEditsParameters wraps a surface's ops schema as the apply_edits
// parameters. The ops schema's shared definitions ($defs, referenced as
// "#/$defs/…") move to the parameters' root so the references resolve there,
// and its own $schema keyword (only valid at a root) is dropped.
func applyEditsParameters(opsSchema json.RawMessage) json.RawMessage {
	var ops map[string]json.RawMessage
	if err := json.Unmarshal(opsSchema, &ops); err != nil {
		return json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","minLength":1,"maxLength":60},"reason":{"type":"string","minLength":4,"maxLength":140},"ops":` +
			string(opsSchema) + `},"required":["summary","reason","ops"],"additionalProperties":false}`)
	}
	defs := ops["$defs"]
	delete(ops, "$defs")
	delete(ops, "$schema")
	inner, _ := json.Marshal(ops)
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{"summary":{"type":"string","minLength":1,"maxLength":60},"reason":{"type":"string","minLength":4,"maxLength":140},"ops":`)
	b.Write(inner)
	b.WriteString(`},"required":["summary","reason","ops"],"additionalProperties":false`)
	if len(defs) > 0 {
		b.WriteString(`,"$defs":`)
		b.Write(defs)
	}
	b.WriteString(`}`)
	return json.RawMessage(b.String())
}

// validateClientCall checks one client tool call: the offered tool's schema,
// then the semantic rules (op schema, routes.json allowlist). The error text
// goes back to the model as the tool result of its one retry.
func validateClientCall(call ToolCall, surface string, tools []ToolDef, ops *OpsValidator, routes *Routes) (ClientAction, error) {
	sch, offered := toolSchema(tools, call.Name)
	if !offered {
		return ClientAction{}, fmt.Errorf("tool %q is not available here", call.Name)
	}
	if err := validateArgs(sch, call.Arguments); err != nil {
		return ClientAction{}, fmt.Errorf("invalid arguments for %s: %v", call.Name, err)
	}
	a := ClientAction{CallID: call.ID, Name: call.Name}
	switch call.Name {
	case ToolApplyEdits:
		var args struct {
			Summary string          `json:"summary"`
			Reason  string          `json:"reason"`
			Ops     json.RawMessage `json:"ops"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return a, fmt.Errorf("invalid apply_edits arguments: %v", err)
		}
		if err := ops.Validate(surface, args.Ops); err != nil {
			return a, fmt.Errorf("ops do not match the %s op schema: %v", surface, err)
		}
		a.Summary, a.Reason, a.Ops, a.Awaits = clip(args.Summary, maxSummaryChars), clip(args.Reason, maxReasonChars), args.Ops, true
		a.Kind = ToolApplyEdits
	case ToolNavigate:
		var args struct{ Target, Label string }
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		if strings.Contains(args.Target, "//") || !routes.HasRoute(args.Target) {
			return a, fmt.Errorf("unknown route %q: use a route from the help documents", args.Target)
		}
		a.Kind, a.Label = "navigate", clip(firstNonEmpty(args.Label, args.Target), maxLabelChars)
		a.Params = map[string]any{"target": args.Target}
	case ToolOpenTool:
		var args struct{ Slug, Preset, Label string }
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		entry, ok := routes.Tool(args.Slug)
		if !ok {
			return a, fmt.Errorf("unknown tool %q: use a tool slug from the help documents", args.Slug)
		}
		if args.Preset != "" && entry.Presets != nil && !contains(entry.Presets, args.Preset) {
			return a, fmt.Errorf("tool %q has no preset %q (presets: %s)", args.Slug, args.Preset, strings.Join(entry.Presets, ", "))
		}
		a.Kind, a.Label = "open_tool", clip(firstNonEmpty(args.Label, args.Slug), maxLabelChars)
		a.Params = map[string]any{"slug": args.Slug}
		if entry.Route != "" {
			a.Params["route"] = entry.Route
		}
		if args.Preset != "" {
			a.Params["preset"] = args.Preset
		}
	case ToolSuggestAction:
		var args struct {
			Kind   string         `json:"kind"`
			Label  string         `json:"label"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		if args.Kind == "open_tool" {
			slug, _ := args.Params["slug"].(string)
			if _, ok := routes.Tool(slug); !ok {
				return a, fmt.Errorf("unknown tool %q in open_tool", slug)
			}
		}
		a.Kind, a.Label, a.Params = args.Kind, clip(args.Label, maxLabelChars), args.Params
		if a.Params == nil {
			a.Params = map[string]any{}
		}
	case ToolAskUser:
		var args struct {
			Question string   `json:"question"`
			Choices  []string `json:"choices"`
		}
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		a.Question = clip(args.Question, maxQuestionChars)
		for _, c := range args.Choices {
			a.Choices = append(a.Choices, clip(c, maxChoiceChars))
		}
		if a.Choices == nil {
			a.Choices = []string{}
		}
	default:
		return a, fmt.Errorf("tool %q is not a client tool", call.Name)
	}
	return a, nil
}

// executeServerTool runs search_help / get_recipe / get_tool_page and returns
// the tool-result text plus the doc ids it surfaced.
func executeServerTool(call ToolCall, surface string, k Knowledge) (string, []string) {
	switch call.Name {
	case ToolSearchHelp:
		var args struct{ Query string }
		if json.Unmarshal([]byte(call.Arguments), &args) != nil || strings.TrimSpace(args.Query) == "" {
			return "Error: search_help needs a query.", nil
		}
		hits := k.Search(args.Query, surface, 4)
		if len(hits) == 0 {
			return "No results. Answer from what you know about the site, or say you are not sure.", nil
		}
		var b strings.Builder
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.Doc.ID)
			fmt.Fprintf(&b, "[%s] %s", h.Doc.ID, h.Doc.Title)
			if h.Doc.URL != "" {
				fmt.Fprintf(&b, " (%s)", h.Doc.URL)
			}
			fmt.Fprintf(&b, "\n%s\n\n", h.Snippet)
		}
		return strings.TrimSpace(b.String()), ids
	case ToolGetRecipe, ToolGetToolPage:
		var args struct{ ID, Slug string }
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		id := args.ID
		if call.Name == ToolGetToolPage {
			id = "tools/" + strings.TrimPrefix(args.Slug, "tools/")
		}
		doc, ok := k.Get(id)
		if !ok {
			return fmt.Sprintf("No document %q.", id), nil
		}
		return docText(doc, 24000), []string{doc.ID}
	}
	return fmt.Sprintf("Error: unknown tool %q.", call.Name), nil
}

func docText(d Doc, limit int) string {
	head := "# " + d.Title
	if d.URL != "" {
		head += "\nURL: " + d.URL
	}
	return clip(head+"\n\n"+d.Body, limit)
}

// clip truncates to n runes.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n]))
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// FileToolsFor lists the analyze_image / analyze_audio / analyze_video tools.
// They measure a file the user already uploaded for a job, so they are
// offered only when the turn carries a file ref (MM116 wires it); the model
// passes that ref back and it is checked against the turn's own.
func FileToolsFor() []ToolDef {
	arg := schema(`{"type":"object","properties":{"fileRef":{"type":"string","minLength":1,"maxLength":200}},"required":["fileRef"],"additionalProperties":false}`)
	return []ToolDef{
		{Name: ToolAnalyzeImage, Description: "Measure the user's uploaded picture: histogram, clipped shadows and highlights, mean luma, white balance, sharpness, noise, dominant hues.", Parameters: arg},
		{Name: ToolAnalyzeAudio, Description: "Measure the user's uploaded audio: integrated and short-term loudness (LUFS), true peak, loudness range, noise floor, clipping.", Parameters: arg},
		{Name: ToolAnalyzeVideo, Description: "Measure the user's uploaded video: size, duration, frame rate, scene brightness, black frames and the audio loudness.", Parameters: arg},
	}
}
