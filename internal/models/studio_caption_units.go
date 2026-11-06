package models

import (
	"strings"
	"unicode"
)

// Caption units (MM121): the token a word timing attaches to. Mirrored rune
// for rune by lib/studio/captions/units.ts and held identical by the golden
// .ass fixtures. Spaced scripts split on whitespace; Han and kana split per
// character (CJK punctuation sticks to the preceding unit, opening brackets to
// the following one), so Whisper words for zh/ja spread by character count.

// captionUnit is one unit of a cue's text. Sep is the whitespace that preceded
// it in the text ("" between CJK characters or at the start).
type CaptionUnit struct {
	Sep  string
	Text string
	CJK  bool
}

func isCaptionSep(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v', '　':
		return true
	}
	return false
}

const captionOpeners = "「『（【〈《〔［｛〖〘〚"

func isCaptionOpen(r rune) bool { return strings.ContainsRune(captionOpeners, r) }

func isCaptionCJKPunct(r rune) bool {
	if isCaptionOpen(r) {
		return false
	}
	return (r >= 0x3001 && r <= 0x303F) || r == 0x30FB || (r >= 0xFF01 && r <= 0xFF0F) ||
		(r >= 0xFF1A && r <= 0xFF20) || (r >= 0xFF3B && r <= 0xFF40) || (r >= 0xFF5B && r <= 0xFF65)
}

func isCaptionCJK(r rune) bool {
	return (r >= 0x3040 && r <= 0x30FF) || (r >= 0x31F0 && r <= 0x31FF) || (r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x4E00 && r <= 0x9FFF) || (r >= 0xF900 && r <= 0xFAFF) || (r >= 0xFF66 && r <= 0xFF9F) ||
		(r >= 0x20000 && r <= 0x2FA1F)
}

// captionTrailPunct is Latin punctuation that sticks to a preceding CJK unit.
const captionTrailPunct = "!\"#$%&')*+,-./:;<=>?@\\]^_`|}~…—–”’"

func isCaptionTrailPunct(r rune) bool { return strings.ContainsRune(captionTrailPunct, r) }

// CaptionUnits splits text into units.
func CaptionUnits(text string) []CaptionUnit {
	var out []CaptionUnit
	sep, open := "", ""
	cur := -1
	start := func(s string, cjk bool) {
		out = append(out, CaptionUnit{Sep: sep, Text: open + s, CJK: cjk})
		cur = len(out) - 1
		sep, open = "", ""
	}
	for _, r := range text {
		switch {
		case isCaptionSep(r):
			cur = -1
			sep += string(r)
		case isCaptionOpen(r):
			cur = -1
			open += string(r)
		case isCaptionCJKPunct(r):
			if cur >= 0 {
				out[cur].Text += string(r)
			} else {
				start(string(r), true)
			}
		case isCaptionCJK(r):
			start(string(r), true)
		default:
			if cur >= 0 && (!out[cur].CJK || isCaptionTrailPunct(r)) {
				out[cur].Text += string(r)
			} else {
				start(string(r), false)
			}
		}
	}
	if open != "" {
		if len(out) > 0 {
			out[len(out)-1].Text += open
		} else {
			out = append(out, CaptionUnit{Sep: sep, Text: open})
		}
	}
	return out
}

// CaptionSentenceEnd reports whether a unit ends a sentence (closing quotes
// and brackets after the mark are ignored).
func CaptionSentenceEnd(text string) bool {
	t := strings.TrimRight(text, "\"'”’)]」』）")
	if t == "" {
		return false
	}
	r := []rune(t)
	return strings.ContainsRune(".!?。！？…", r[len(r)-1])
}

// CaptionScriptOf returns the caption script of a rune ("" = Latin/common).
func CaptionScriptOf(r rune) string {
	switch {
	case (r >= 0x0100 && r <= 0x024F) || (r >= 0x1E00 && r <= 0x1EFF):
		return CaptionScriptLatinExt
	case r < 0x0370:
		return ""
	case (r >= 0x0370 && r <= 0x03FF) || (r >= 0x1F00 && r <= 0x1FFF):
		return CaptionScriptGreek
	case (r >= 0x0400 && r <= 0x052F) || (r >= 0x1C80 && r <= 0x1C8F) || (r >= 0x2DE0 && r <= 0x2DFF) || (r >= 0xA640 && r <= 0xA69F):
		return CaptionScriptCyrillic
	case (r >= 0x0590 && r <= 0x05FF) || (r >= 0xFB1D && r <= 0xFB4F):
		return CaptionScriptHebrew
	case (r >= 0x0600 && r <= 0x06FF) || (r >= 0x0750 && r <= 0x077F) || (r >= 0x08A0 && r <= 0x08FF) || (r >= 0xFB50 && r <= 0xFDFF) || (r >= 0xFE70 && r <= 0xFEFF):
		return CaptionScriptArabic
	case (r >= 0x0900 && r <= 0x097F) || (r >= 0xA8E0 && r <= 0xA8FF):
		return CaptionScriptDevanagari
	case (r >= 0x1100 && r <= 0x11FF) || (r >= 0x3130 && r <= 0x318F) || (r >= 0xA960 && r <= 0xA97F) || (r >= 0xAC00 && r <= 0xD7FF):
		return CaptionScriptHangul
	case (r >= 0x3000 && r <= 0x30FF) || (r >= 0x31F0 && r <= 0x31FF) || (r >= 0x3400 && r <= 0x4DBF) || (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0xF900 && r <= 0xFAFF) || (r >= 0xFF00 && r <= 0xFFEF) || (r >= 0x20000 && r <= 0x2FA1F):
		return CaptionScriptHan
	}
	return ""
}

func captionFontCovers(f StudioCaptionFont, need map[string]bool) bool {
	for s := range need {
		ok := false
		for _, fs := range f.Scripts {
			if fs == s {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// CaptionFontFor returns the family a cue renders in: the style's family when
// it covers every script in the text, else the first fallback covering them
// all, else the fallback of the highest-priority script. A generator rule so
// both renderers pick the same file (never fontconfig's guess).
func CaptionFontFor(family, text string) string {
	need := map[string]bool{}
	for _, r := range text {
		if s := CaptionScriptOf(r); s != "" {
			need[s] = true
		}
	}
	if len(need) == 0 {
		return family
	}
	if f, ok := CaptionFontByFamily(family); ok && captionFontCovers(f, need) {
		return family
	}
	for _, fb := range CaptionFallbackFonts {
		if f, ok := CaptionFontByFamily(fb); ok && captionFontCovers(f, need) {
			return fb
		}
	}
	for _, p := range CaptionScriptPriority {
		if need[p.Script] {
			return p.Family
		}
	}
	return family
}

// captionUpperSkip lists runes whose full uppercase mapping is more than one
// character (SpecialCasing.txt): uppercasing keeps them as they are, so Go's
// simple mapping and the browser's toUpperCase agree rune for rune.
const captionUpperSkip = "ßŉǰΐΰևẖẗẘẙẚὐὒὔὖﬀﬁﬂﬃﬄﬅﬆﬓﬔﬕﬖﬗ"

// CaptionUpper uppercases caption text rune by rune (MM121 uppercase style).
// The Greek Extended iota-subscript block U+1F80–1FFF and captionUpperSkip
// are left alone; lib/studio/captions/units.ts mirrors this exactly.
func CaptionUpper(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 0x1F80 && r <= 0x1FFF) || strings.ContainsRune(captionUpperSkip, r) {
			b.WriteRune(r)
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}
