package flywheel

import (
	"fmt"
	"strings"
	"time"
)

// Week is an ISO week in UTC: [From, To).
type Week struct {
	Label    string // 2026-W46
	From, To time.Time
}

// ParseWeek parses "2026-W46"; "" means the last complete week before now.
func ParseWeek(s string, now time.Time) (Week, error) {
	if s == "" {
		y, w := now.UTC().AddDate(0, 0, -7).ISOWeek()
		s = fmt.Sprintf("%04d-W%02d", y, w)
	}
	var y, w int
	if n, err := fmt.Sscanf(s, "%d-W%d", &y, &w); err != nil || n != 2 || w < 1 || w > 53 {
		return Week{}, fmt.Errorf("--week %q: want YYYY-Www, e.g. 2026-W46", s)
	}
	// ISO week 1 contains Jan 4th.
	jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
	mon := jan4.AddDate(0, 0, -((int(jan4.Weekday())+6)%7)).AddDate(0, 0, (w-1)*7)
	if yy, ww := mon.ISOWeek(); yy != y || ww != w {
		return Week{}, fmt.Errorf("--week %q: no such ISO week", s)
	}
	return Week{Label: fmt.Sprintf("%04d-W%02d", y, w), From: mon, To: mon.AddDate(0, 0, 7)}, nil
}

// Report is everything the markdown needs.
type Report struct {
	Week        Week
	Generated   time.Time
	TurnsInWeek int
	Flagged     int
	Clusters    []*Cluster
	// SignalGaps lists signals that had no source this run.
	SignalGaps []Signal
	LabelUSD   float64
	Labeled    bool
}

func signalList(c *Cluster) string {
	var parts []string
	for _, s := range AllSignals {
		if n := c.Counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s ×%d", s, n))
		}
	}
	return strings.Join(parts, ", ")
}

// Render writes the evidence markdown. It quotes only scrubbed, truncated text.
func Render(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Medi flywheel, %s\n\n", r.Week.Label)
	fmt.Fprintf(&b, "Window %s to %s (UTC). Generated %s by `cmd/assistant-flywheel` (MM117). Signed-in conversations only; read-only; examples are scrubbed (no emails, file names or paths, outside URLs, phone numbers or long numbers) and cut to %d characters. Nothing here is applied automatically: every proposal is a reviewed commit.\n\n",
		r.Week.From.Format("2006-01-02"), r.Week.To.Format("2006-01-02"), r.Generated.UTC().Format("2006-01-02 15:04"), ExampleMax)
	fmt.Fprintf(&b, "- Turns in the window: %d\n- Flagged turns: %d\n- Clusters: %d\n", r.TurnsInWeek, r.Flagged, len(r.Clusters))
	if r.Labeled {
		fmt.Fprintf(&b, "- Haiku labelling: on, cost $%.4f\n", r.LabelUSD)
	} else {
		b.WriteString("- Haiku labelling: off (`--label`)\n")
	}
	if len(r.SignalGaps) > 0 {
		var gs []string
		for _, s := range r.SignalGaps {
			gs = append(gs, "`"+string(s)+"`")
		}
		fmt.Fprintf(&b, "- Signals with no source this run: %s (client-side events; pass `--analytics-export <file.jsonl>`)\n", strings.Join(gs, ", "))
	}
	b.WriteString("\n## Summary\n\n| # | Surface | Docs retrieved | Turns | Signals | Proposal |\n|---|---|---|---|---|---|\n")
	props := make([]Proposal, len(r.Clusters))
	for i, c := range r.Clusters {
		props[i] = Propose(c, r.Week.Label, i+1)
		docs := strings.Join(c.Docs, " + ")
		if docs == "" {
			docs = NoDocs
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %d | %s | %s |\n", i+1, c.Surface, mdCell(docs), c.Size(), signalList(c), props[i].Kind)
	}
	for i, c := range r.Clusters {
		p := props[i]
		fmt.Fprintf(&b, "\n## Cluster %d: %s\n\n", i+1, c.Key)
		if c.Label != "" {
			fmt.Fprintf(&b, "Label: **%s**\n\n", c.Label)
		}
		fmt.Fprintf(&b, "%d turns. Signals: %s.\n", c.Size(), signalList(c))
		if len(c.Consulted) > 0 {
			fmt.Fprintf(&b, "Docs the model read or opened: %s.\n", consultedList(c))
		}
		b.WriteString("\nExamples (scrubbed):\n\n")
		shown := 0
		for _, f := range c.Turns {
			if shown == 5 {
				fmt.Fprintf(&b, "- … and %d more\n", c.Size()-shown)
				break
			}
			shown++
			fmt.Fprintf(&b, "- [%s] user: \"%s\"\n", sigNames(f.Signals), Example(f.Turn.UserText))
			if a := Example(f.Turn.AssistantText); a != "" {
				fmt.Fprintf(&b, "  Medi: \"%s\"\n", a)
			}
			if f.Turn.Reason != "" {
				fmt.Fprintf(&b, "  Reason given: \"%s\"\n", Example(f.Turn.Reason))
			}
		}
		fmt.Fprintf(&b, "\n### Proposal: %s\n\nTarget: `%s`\n\n%s\n", p.Kind, p.Target, p.Body)
		fmt.Fprintf(&b, "Eval case, paste into `%s`:\n\n```yaml\n%s```\n", p.EvalFile, p.EvalCase)
	}
	if len(r.Clusters) == 0 {
		b.WriteString("\nNo flagged turns this week.\n")
	}
	return b.String()
}

func sigNames(ss []Signal) string {
	var p []string
	for _, s := range ss {
		p = append(p, string(s))
	}
	return strings.Join(p, ", ")
}

func consultedList(c *Cluster) string {
	type kv struct {
		k string
		n int
	}
	var l []kv
	for k, n := range c.Consulted {
		l = append(l, kv{k, n})
	}
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && (l[j].n > l[j-1].n || (l[j].n == l[j-1].n && l[j].k < l[j-1].k)); j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
	var p []string
	for _, x := range l {
		p = append(p, fmt.Sprintf("`%s` ×%d", x.k, x.n))
	}
	return strings.Join(p, ", ")
}

func mdCell(s string) string { return strings.ReplaceAll(s, "|", "\\|") }
