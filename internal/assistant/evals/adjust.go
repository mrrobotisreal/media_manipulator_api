package evals

import (
	"fmt"
	"regexp"
	"strings"
)

// MM115 adjust grading. Pure functions of the recorded rounds, unit-tested on
// canned traces (adjust_test.go).

// metricWords are the words a reader accepts as naming a meter, besides the
// meter's own name. Matching is case-insensitive on the answer text.
var metricWords = map[string][]string{
	"meanLuma":          {"luma", "brightness", "exposure", "mean"},
	"medianLuma":        {"median", "luma", "midtone"},
	"lumaStd":           {"contrast", "spread", "std"},
	"shadowsClipped":    {"shadow", "black"},
	"highlightsClipped": {"highlight", "white", "blown"},
	"warmth":            {"warm", "cool", "temperature", "cast"},
	"tint":              {"tint", "green", "magenta"},
}

var digitRe = regexp.MustCompile(`[0-9]`)

// citesMetric reports whether text names any meter (its name or a word for
// it) and carries a figure.
func citesMetric(text string, metrics map[string]float64) bool {
	low := strings.ToLower(text)
	if !digitRe.MatchString(low) {
		return false
	}
	// A figure that is one of the shown values counts in any language.
	for _, v := range metrics {
		for _, cand := range figureForms(v) {
			if strings.Contains(low, cand) {
				return true
			}
		}
	}
	for name := range metrics {
		if strings.Contains(low, strings.ToLower(name)) {
			return true
		}
		for _, w := range metricWords[name] {
			if strings.Contains(low, w) {
				return true
			}
		}
	}
	return false
}

// finalMetrics are the last meters the model saw: the Shown of the last round
// that had an edit, else the case's Before.
func finalMetrics(c Case, out Output) map[string]float64 {
	m := c.Adjust.Before
	for _, r := range out.Rounds {
		if r.Edit != nil && r.Shown != nil {
			m = r.Shown
		}
	}
	return m
}

func gradeAdjust(c Case, out Output) []string {
	a := c.Adjust
	var fails []string
	edits := 0
	for _, r := range out.Rounds {
		if r.Edit != nil {
			edits++
		}
	}
	if edits < a.Rounds.Min || edits > a.Rounds.Max {
		fails = append(fails, fmt.Sprintf("adjust: %d edit round(s), want %d-%d", edits, a.Rounds.Min, a.Rounds.Max))
	}
	final := finalMetrics(c, out)
	for _, f := range a.Final {
		v, ok := final[f.Metric]
		switch {
		case !ok:
			fails = append(fails, fmt.Sprintf("final_metric(%s): never measured", f.Metric))
		case f.Min != nil && v < *f.Min, f.Max != nil && v > *f.Max:
			fails = append(fails, fmt.Sprintf("final_metric(%s) = %v outside %s", f.Metric, v, rangeText(f)))
		}
	}
	if a.CitesMetric && edits > 0 {
		// Every adjusting edit (the 2nd and later) cites the figure it reacts
		// to, from the previous round's meters; and the last message cites
		// the figure that says it is done (or what is still short).
		prev := a.Before
		for i, r := range out.Rounds {
			if i > 0 && r.Edit != nil && !citesMetric(r.Text+" "+r.Edit.Reason+" "+r.Edit.Summary, prev) {
				fails = append(fails, fmt.Sprintf("round %d adjusts without citing a metric (said %q, reason %q)", i+1, clipText(r.Text, 80), clipText(r.Edit.Reason, 80)))
			}
			if r.Edit != nil && r.Shown != nil {
				prev = r.Shown
			}
		}
		if last := out.Rounds[len(out.Rounds)-1]; last.Edit == nil && !citesMetric(last.Text, final) {
			fails = append(fails, fmt.Sprintf("the closing message cites no metric (said %q)", clipText(last.Text, 120)))
		}
	}
	return fails
}

func rangeText(f FinalMetric) string {
	switch {
	case f.Min != nil && f.Max != nil:
		return fmt.Sprintf("%v..%v", *f.Min, *f.Max)
	case f.Min != nil:
		return fmt.Sprintf(">= %v", *f.Min)
	default:
		return fmt.Sprintf("<= %v", *f.Max)
	}
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// figureForms are the ways a model writes a shown value: 0.43, 0.4, 43 % / 43%.
func figureForms(v float64) []string {
	forms := []string{trimNum(v, 3), trimNum(v, 2)}
	if v >= -1 && v <= 1 {
		forms = append(forms, trimNum(v*100, 0), trimNum(v*100, 1))
	}
	var out []string
	for _, f := range forms {
		f = strings.TrimPrefix(f, "-")
		if len(f) >= 2 && f != "0" {
			out = append(out, f)
		}
	}
	return out
}

func trimNum(v float64, places int) string {
	s := fmt.Sprintf("%.*f", places, v)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
