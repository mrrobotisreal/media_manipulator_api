package flywheel

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// RoutingPair is one graded guide routing example for a later local-model
// distillation (design Q1). The file stays on the host that wrote it.
type RoutingPair struct {
	Week          string   `json:"week"`
	Message       string   `json:"message"` // scrubbed, not truncated
	Targets       []string `json:"targets"` // tool slugs / routes the guide sent the visitor to
	Grade         string   `json:"grade"`   // good | bad | neutral
	GradedBy      []string `json:"gradedBy"`
	PromptVersion string   `json:"promptVersion,omitempty"`
}

// RoutingPairs extracts the guide turns that routed (open_tool or navigate) in
// the window. Grade: bad on thumbs-down, invalid_ops, or a rephrase within
// RephraseWindow; good on thumbs-up; otherwise neutral (no feedback at all is
// not evidence).
func RoutingPairs(turns []Turn, an *Analytics, w Week) []RoutingPair {
	var out []RoutingPair
	for _, t := range turns {
		if t.Surface != "guide" || t.AssistantID == "" || t.Continuation || t.At.Before(w.From) || !t.At.Before(w.To) {
			continue
		}
		targets := routeTargets(t)
		if len(targets) == 0 {
			continue
		}
		p := RoutingPair{Week: w.Label, Message: Scrub(t.UserText), Targets: targets, Grade: "neutral", PromptVersion: t.PromptVersion}
		bad := func(why string) { p.Grade = "bad"; p.GradedBy = append(p.GradedBy, why) }
		if t.Rating == "down" {
			bad("thumbs_down")
		}
		if t.ErrorCode != "" {
			bad(t.ErrorCode)
		}
		if t.NextUserText != "" && t.NextUserAt.Sub(t.At) <= RephraseWindow {
			bad("rephrased")
		}
		for _, s := range an.signals(t) {
			if s == SigNoClick {
				bad(string(s))
			}
		}
		if p.Grade == "neutral" && t.Rating == "up" {
			p.Grade, p.GradedBy = "good", []string{"thumbs_up"}
		}
		if strings.TrimSpace(p.Message) == "" {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Message < out[j].Message })
	return out
}

func routeTargets(t Turn) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range t.ToolCalls {
		var a struct{ Slug, Target string }
		_ = json.Unmarshal([]byte(c.Args), &a)
		var v string
		switch c.Name {
		case "open_tool":
			v = a.Slug
		case "navigate":
			v = a.Target
		default:
			continue
		}
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// WriteRoutingJSONL writes one JSON object per line.
func WriteRoutingJSONL(w io.Writer, pairs []RoutingPair) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, p := range pairs {
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	return bw.Flush()
}
