// Package safety converts heterogeneous scan output into the canonical
// shape mm_tool_scans + mm_safety_incidents expect.
//
// The classifier deliberately leans conservative: marginal cases are still
// surfaced for review rather than silently dismissed.
package safety

import (
	"strings"
	"time"
)

// ScanInput is the union of fields we can extract from a VLM/transcript
// analysis JSON blob (analysis.json).
type ScanInput struct {
	Tool                  string
	ScanType              string // metadata, ai_summary, ai_safety, transcript_review, visual_review, …
	ScannerName           string
	ScannerVersion        string
	ModelName             string
	ModelVersion          string
	Summary               string
	Description           string
	DetectedLanguage      string
	Labels                []any
	SafetyRating          string  // safe | moderate | unsafe | unknown
	SafetyScore           float64 // 0..1 confidence
	HarmfulContent        bool
	HarmfulContentReasons []any
	TOSViolation          bool
	TOSCategories         []any
	Warnings              []any
	StartedAt             time.Time
	CompletedAt           time.Time
	RawResult             map[string]any
}

// Classification is the result of mapping ScanInput into incident
// severity/status.
type Classification struct {
	ShouldCreateIncident bool
	Severity             string // low | medium | high | critical
	IncidentStatus       string // open
	SafetyRating         string // normalized lower-case
	TOSViolation         bool
}

// Classify returns a Classification given the raw scan input.
//
// Heuristics:
//   - TOS categories naming exploitation/CSAM bypass → critical.
//   - safety_rating=unsafe OR explicit harmful_content=true → high.
//   - safety_rating=moderate with concerning labels → medium.
//   - Any flagged warning but otherwise safe → low.
func Classify(in *ScanInput) Classification {
	if in == nil {
		return Classification{}
	}
	rating := strings.ToLower(strings.TrimSpace(in.SafetyRating))
	c := Classification{SafetyRating: rating, TOSViolation: in.TOSViolation}

	// Critical: anything that looks like CSAM / explicit-illegal /
	// child exploitation indicators. Keep the keyword list short and
	// conservative — false positives are acceptable here.
	criticalKeywords := []string{
		"csam", "child_sexual", "child_exploitation", "minor_sexual",
		"underage_sexual",
	}
	if anyMatches(in.TOSCategories, criticalKeywords) ||
		anyMatches(in.HarmfulContentReasons, criticalKeywords) ||
		anyMatches(in.Labels, criticalKeywords) {
		c.ShouldCreateIncident = true
		c.Severity = "critical"
		c.IncidentStatus = "open"
		return c
	}

	// High: explicit unsafe rating or harmful_content=true with serious
	// reason.
	if in.HarmfulContent || rating == "unsafe" || in.TOSViolation {
		c.ShouldCreateIncident = true
		c.Severity = "high"
		c.IncidentStatus = "open"
		return c
	}

	// Medium: moderate rating with concerning labels.
	if rating == "moderate" && (len(in.HarmfulContentReasons) > 0 || hasConcerningLabel(in.Labels)) {
		c.ShouldCreateIncident = true
		c.Severity = "medium"
		c.IncidentStatus = "open"
		return c
	}

	// Low: explicit warnings even when rating is safe.
	if len(in.Warnings) > 0 && rating == "safe" {
		c.ShouldCreateIncident = true
		c.Severity = "low"
		c.IncidentStatus = "open"
		return c
	}

	return c
}

func anyMatches(values []any, needles []string) bool {
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			continue
		}
		lower := strings.ToLower(s)
		for _, n := range needles {
			if strings.Contains(lower, n) {
				return true
			}
		}
	}
	return false
}

// hasConcerningLabel scans labels for words that justify a manual review
// even when the overall rating is "moderate".
func hasConcerningLabel(values []any) bool {
	concerning := []string{
		"weapon", "violence", "graphic", "drug",
		"self_harm", "selfharm", "extremism", "hate",
	}
	return anyMatches(values, concerning)
}
