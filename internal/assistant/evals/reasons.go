package evals

import (
	"strings"
	"unicode"
)

// MM114: every apply_edits call of a copilot answer carries a reason that
// cites what the model saw or what the user asked.
//
//	reason_present  each edit has a reason of 4-140 characters (the schema
//	                demands it, so this also catches a reason that is padding).
//	reason_cites    each reason names a figure, a meter or digest word, or a
//	                content word of the user's message (so "Because it looks
//	                better" fails).
//
// Both run on every copilot case that applied an edit; neither needs a case
// to opt in. A reason that fails either check fails the case.

var reasonWords = []string{
	"histogram", "clip", "clipped", "preview", "frame", "digest", "layer", "track", "clip", "histogramm",
	"shadow", "highlight", "exposure", "contrast", "saturation", "luma", "warm", "cool", "dark", "bright",
	"flat", "noisy", "tilt", "horizon", "pause", "silence", "volume", "loud", "quiet", "pace", "keyframe",
}

// ReasonProblems lists what is wrong with the reasons of out's edits (empty = fine).
func ReasonProblems(c Case, out Output) []string {
	if c.Surface == "guide" || len(out.Edits) == 0 {
		return nil
	}
	var fails []string
	for i, ed := range out.Edits {
		r := strings.TrimSpace(ed.Reason)
		n := len([]rune(r))
		if n < 4 || n > 140 {
			fails = append(fails, "reason_present: edit "+itoa(i+1)+" has a reason of "+itoa(n)+" characters")
			continue
		}
		if !reasonCites(r, c.Message) {
			fails = append(fails, "reason_cites: edit "+itoa(i+1)+" reason cites neither a figure, a meter nor the ask: "+quote(r))
		}
	}
	return fails
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func quote(s string) string { return "\"" + s + "\"" }

func reasonCites(reason, ask string) bool {
	low := strings.ToLower(reason)
	if digitRe.MatchString(low) {
		return true
	}
	for _, w := range reasonWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	for name := range metricWords {
		if strings.Contains(low, strings.ToLower(name)) {
			return true
		}
	}
	rt := wordsOf(low)
	for _, a := range wordsOf(strings.ToLower(ask)) {
		for _, w := range rt {
			if sameStem(a, w) {
				return true
			}
		}
	}
	return false
}

var stopWords = map[string]bool{"the": true, "and": true, "you": true, "for": true, "with": true, "that": true, "this": true, "can": true, "make": true, "bit": true, "its": true, "was": true, "are": true, "but": true, "not": true, "all": true, "how": true, "why": true}

func wordsOf(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

// sameStem matches words of 4+ letters that share their first five letters
// (or are equal): "brighten" ~ "brighter", "тёмное" ~ "тёмным". CJK words have
// no spaces, so any shared run of three or more characters counts for them.
func sameStem(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	if a == b && len(ra) >= 3 && !stopWords[a] {
		return true
	}
	if len(ra) < 4 || len(rb) < 4 {
		return false
	}
	if a == b {
		return true
	}
	k := 5
	if len(ra) < k {
		k = len(ra)
	}
	if len(rb) < k {
		k = len(rb)
	}
	return string(ra[:k]) == string(rb[:k])
}
