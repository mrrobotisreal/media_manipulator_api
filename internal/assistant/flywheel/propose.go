package flywheel

import (
	"fmt"
	"sort"
	"strings"
)

// ProposalKind is what a cluster should become.
type ProposalKind string

const (
	// ProposeRecipe: nothing (or only a recipe) covers the ask; write a recipe.
	ProposeRecipe ProposalKind = "new recipe stub"
	// ProposeWhenToUse: a tool page is retrieved but users still go astray; fix
	// its content/medi/tools/<slug>.md "When to use" sidecar.
	ProposeWhenToUse ProposalKind = "whenToUse sidecar fix"
)

// Proposal is the reviewable output for one cluster. Every change it suggests
// is a human-reviewed commit; nothing is applied automatically.
type Proposal struct {
	Kind     ProposalKind
	Target   string // the file the change goes in
	Body     string // markdown/stub text
	EvalCase string // ready-to-paste YAML for internal/assistant/evals/cases/<surface>.yaml
	EvalFile string
}

// Propose builds a cluster's proposal from its scrubbed examples. n is the
// cluster's rank in the report (stable eval case ids).
func Propose(c *Cluster, week string, n int) Proposal {
	msgs := distinctExamples(c, 3)
	top := ""
	if len(c.Docs) > 0 {
		top = c.Docs[0]
	}
	toolDoc := ""
	for _, d := range c.Docs {
		if strings.HasPrefix(d, "tools/") {
			toolDoc = d
			break
		}
	}
	p := Proposal{}
	switch {
	case c.Surface == "guide" && toolDoc != "":
		slug := strings.TrimPrefix(toolDoc, "tools/")
		p.Kind = ProposeWhenToUse
		p.Target = "media-manipulator-ui/content/medi/tools/" + slug + ".md (then `npm run export:medi`)"
		var b strings.Builder
		b.WriteString("Add these asks to the \"## When to use\" paragraph of `" + slug + "` (or, if another tool is the right answer, to that tool's sidecar and a \"When not to use\" line here):\n\n")
		for _, m := range msgs {
			b.WriteString("- Typical ask: \"" + m + "\"\n")
		}
		p.Body = b.String()
	default:
		p.Kind = ProposeRecipe
		dir := map[string]string{"guide": "workflow", "studio": "studio", "vector": "vector", "photo": "photo"}[c.Surface]
		if dir == "" {
			dir = "workflow"
		}
		id := slugify(strings.Join(firstWords(msgs, 4), " "))
		if id == "" {
			id = fmt.Sprintf("flywheel-%s-%d", week, n)
		}
		editor := c.Surface
		if editor == "guide" {
			editor = "workflow"
		}
		p.Target = fmt.Sprintf("media-manipulator-ui/content/medi/recipes/%s/%s.md (then `npm run export:medi`)", dir, id)
		var b strings.Builder
		b.WriteString("```markdown\n---\n")
		fmt.Fprintf(&b, "id: %q\neditor: %q\ntitle: \"TODO name this technique\"\nintents: [", id, editor)
		for i, m := range msgs {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", strings.ReplaceAll(m, "\"", "'"))
		}
		b.WriteString("]\ndifficulty: \"basic\"\n---\n\n## When to use\n\nTODO: the situation behind these asks.\n\n## What a pro does and why\n\nTODO\n")
		if c.Surface != "guide" {
			b.WriteString("\n## Ops\n\nTODO: a schema-valid `ops` example (it must pass the op validator for `" + c.Surface + "`).\n")
		}
		b.WriteString("```\n")
		p.Body = b.String()
	}

	// The eval case, ready to paste.
	caseID := fmt.Sprintf("flywheel-%s-%02d", strings.ToLower(strings.ReplaceAll(week, "-", "")), n)
	var y strings.Builder
	fmt.Fprintf(&y, "# MM117 flywheel %s, cluster %d (%s): review the expectation before merging.\n", week, n, c.Key)
	fmt.Fprintf(&y, "- id: %s\n  surface: %s\n", caseID, c.Surface)
	if c.Surface != "guide" {
		fmt.Fprintf(&y, "  requires: %s\n", c.Surface)
	}
	msg := "TODO write the ask"
	if len(msgs) > 0 {
		msg = msgs[0]
	}
	fmt.Fprintf(&y, "  message: %q\n  expect:\n", msg)
	switch {
	case c.Surface != "guide":
		y.WriteString("    ops: [\"op_count <= 6\"] # TODO: tighten to the op the recipe should produce\n")
	case toolDoc != "":
		fmt.Fprintf(&y, "    actions: [tool:%s] # TODO: confirm this is the right tool\n", strings.TrimPrefix(toolDoc, "tools/"))
	case top != "":
		fmt.Fprintf(&y, "    any_of: [%q] # TODO: name what a good answer mentions\n", top)
	default:
		y.WriteString("    any_of: [\"TODO\"] # nothing covers this ask yet: name what a good answer mentions\n")
	}
	p.EvalCase = y.String()
	p.EvalFile = "media_manipulator_api/internal/assistant/evals/cases/" + c.Surface + ".yaml"
	return p
}

func distinctExamples(c *Cluster, n int) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range c.Turns {
		m := Example(f.Turn.UserText)
		if m == "" || seen[strings.ToLower(m)] {
			continue
		}
		seen[strings.ToLower(m)] = true
		out = append(out, m)
		if len(out) == n {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) < len(out[j]) })
	return out
}

func firstWords(msgs []string, n int) []string {
	if len(msgs) == 0 {
		return nil
	}
	w := strings.Fields(msgs[0])
	if len(w) > n {
		w = w[:n]
	}
	return w
}

func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
