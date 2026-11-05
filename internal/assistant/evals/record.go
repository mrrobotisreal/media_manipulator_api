package evals

import (
	"context"
	"fmt"
	"sort"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// RunStore is the slice of the telemetry store the recorder and the gate use
// (MM113). Tests pass a fake; the CLI passes *telemetry.Store when given
// --record or --gate.
type RunStore interface {
	InsertAssistantEvalRun(ctx context.Context, r telemetry.AssistantEvalRun) error
	LatestAssistantEvalRun(ctx context.Context, surface string) (*telemetry.AssistantEvalRun, error)
}

// RunMeta is what a recorded row carries besides the results.
type RunMeta struct {
	PromptVersion string
	GitSHA        string
	ModelGuide    string
	ModelCopilot  string
	EvidencePath  string
}

// Records builds one row per surface that had cases (run or skipped). The cost
// of a surface is the sum of its cases; median_tool_calls is over the cases
// that ran.
func Records(results []Result, meta RunMeta) []telemetry.AssistantEvalRun {
	type acc struct {
		r     telemetry.AssistantEvalRun
		calls []int
	}
	by := map[string]*acc{}
	for _, res := range results {
		a := by[res.Case.Surface]
		if a == nil {
			a = &acc{r: telemetry.AssistantEvalRun{PromptVersion: meta.PromptVersion, GitSHA: meta.GitSHA,
				ModelGuide: meta.ModelGuide, ModelCopilot: meta.ModelCopilot, Surface: res.Case.Surface, EvidencePath: meta.EvidencePath}}
			by[res.Case.Surface] = a
		}
		a.r.Cases++
		switch {
		case res.Skipped != "":
			a.r.Skipped++
		case res.Pass:
			a.r.Passed++
		default:
			a.r.Failed++
		}
		if res.Skipped == "" {
			a.calls = append(a.calls, res.Output.ToolCalls)
		}
		a.r.CostUSD += res.Output.CostUSD
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]telemetry.AssistantEvalRun, 0, len(keys))
	for _, k := range keys {
		a := by[k]
		a.r.MedianToolCalls = median(a.calls)
		out = append(out, a.r)
	}
	return out
}

func median(v []int) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	if len(s)%2 == 1 {
		return float64(s[len(s)/2])
	}
	return float64(s[len(s)/2-1]+s[len(s)/2]) / 2
}

// PassRate is passed / (passed + failed); ok is false when nothing ran.
func PassRate(r telemetry.AssistantEvalRun) (rate float64, ok bool) {
	n := r.Passed + r.Failed
	if n == 0 {
		return 0, false
	}
	return float64(r.Passed) / float64(n), true
}

// GateVerdict is the comparison of one surface against its latest recorded run.
type GateVerdict struct {
	Surface string
	// Previous is nil when the surface has no recorded run (nothing to compare:
	// the gate passes).
	Previous *telemetry.AssistantEvalRun
	Current  telemetry.AssistantEvalRun
	Regress  bool
}

// Line is the one-line report of a verdict, naming the previous prompt version.
func (g GateVerdict) Line() string {
	cur, _ := PassRate(g.Current)
	if g.Previous == nil {
		return fmt.Sprintf("gate %-6s %.0f %%: no earlier recorded run", g.Surface, 100*cur)
	}
	prev, _ := PassRate(*g.Previous)
	word := "ok"
	if g.Regress {
		word = "REGRESSION"
	}
	return fmt.Sprintf("gate %-6s %.0f %% vs %.0f %% (prompt %s, %s): %s", g.Surface, 100*cur, 100*prev,
		g.Previous.PromptVersion, g.Previous.CreatedAt.UTC().Format("2006-01-02"), word)
}

// Gate compares each surface's pass rate with the latest recorded run of that
// surface; a lower rate is a regression. A surface with no cases run, or no
// earlier run, never regresses. Call it before recording the new rows.
func Gate(ctx context.Context, store RunStore, current []telemetry.AssistantEvalRun) ([]GateVerdict, error) {
	out := make([]GateVerdict, 0, len(current))
	for _, cur := range current {
		v := GateVerdict{Surface: cur.Surface, Current: cur}
		prev, err := store.LatestAssistantEvalRun(ctx, cur.Surface)
		if err != nil {
			return out, fmt.Errorf("latest run for %s: %w", cur.Surface, err)
		}
		v.Previous = prev
		if prev != nil {
			_, curOK := PassRate(cur)
			_, prevOK := PassRate(*prev)
			// Integer cross-multiplication avoids float noise on equal rates.
			if curOK && prevOK && cur.Passed*(prev.Passed+prev.Failed) < prev.Passed*(cur.Passed+cur.Failed) {
				v.Regress = true
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// Regressed reports whether any verdict regressed.
func Regressed(vs []GateVerdict) bool {
	for _, v := range vs {
		if v.Regress {
			return true
		}
	}
	return false
}

// Record writes the rows.
func Record(ctx context.Context, store RunStore, rows []telemetry.AssistantEvalRun) error {
	for _, r := range rows {
		if err := store.InsertAssistantEvalRun(ctx, r); err != nil {
			return fmt.Errorf("record %s: %w", r.Surface, err)
		}
	}
	return nil
}
