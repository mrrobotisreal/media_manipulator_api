package services

import (
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Caption helpers: whisper segment → cue conversion and the .ass serializer for
// export burn-in. Pure + unit-tested (no ffmpeg / whisper invocation here).

const (
	studioMaxCueSeconds = 7.0
	studioMaxCueChars   = 84
)

// segmentsToCues converts whisper segments into caption cues, splitting long
// segments (> ~7s or > ~84 chars) into balanced lines at word boundaries with
// proportional timing. idGen supplies cue ids (injected for deterministic tests).
func SegmentsToCues(segments []TranscribeSegmentDTO, idGen func() string) []models.StudioCaptionCue {
	return SegmentsToCuesUntil(segments, idGen, 0)
}

// SegmentsToCuesUntil is SegmentsToCues for word-timed segments (MM121): a
// segment carrying whisper words becomes cues with a `words` array, split by
// the grouping rules (design §6.2) — a sentence end or a gap ≥ 150 ms breaks a
// cue, as do the ~7 s / 84 char / 64 word caps — and Han/kana words spread
// over their characters by count. silenceStart > 0 is where a trailing
// silence begins (DetectTrailingSilence): words starting inside it are
// dropped (whisper's invented "Thank you." over a silent tail). Segments
// without words convert exactly as before.
func SegmentsToCuesUntil(segments []TranscribeSegmentDTO, idGen func() string, silenceStart float64) []models.StudioCaptionCue {
	cues := make([]models.StudioCaptionCue, 0, len(segments))
	for _, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		if len(seg.Words) > 0 {
			cues = append(cues, wordCues(seg.Words, idGen, silenceStart)...)
			continue
		}
		if silenceStart > 0 && seg.Start >= silenceStart {
			continue
		}
		dur := seg.End - seg.Start
		if dur <= studioMaxCueSeconds && len([]rune(text)) <= studioMaxCueChars {
			cues = append(cues, models.StudioCaptionCue{ID: idGen(), StartSeconds: seg.Start, EndSeconds: seg.End, Text: text})
			continue
		}
		parts := splitBalanced(text, studioMaxCueChars)
		n := len(parts)
		if n == 0 {
			continue
		}
		for i, p := range parts {
			s := seg.Start + dur*float64(i)/float64(n)
			e := seg.Start + dur*float64(i+1)/float64(n)
			cues = append(cues, models.StudioCaptionCue{ID: idGen(), StartSeconds: s, EndSeconds: e, Text: p})
		}
	}
	return cues
}

const (
	studioCueGapSeconds = 0.15
	studioCueMaxWords   = models.StudioMaxCaptionWords
)

// timedUnit is a caption unit with its seconds.
type timedUnit struct {
	models.CaptionUnit
	start, end float64
}

// wordCues turns one segment's whisper words into word-timed cues.
func wordCues(words []TranscribeWordDTO, idGen func() string, silenceStart float64) []models.StudioCaptionCue {
	units := wordsToUnits(words, silenceStart)
	var out []models.StudioCaptionCue
	var cur []timedUnit
	chars := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		var b strings.Builder
		ws := make([]models.StudioCaptionWord, len(cur))
		for i, u := range cur {
			if i > 0 && u.Sep != "" {
				b.WriteString(" ")
			}
			b.WriteString(u.Text)
			ws[i] = models.StudioCaptionWord{Text: u.Text, StartSeconds: roundMilli(u.start), EndSeconds: roundMilli(u.end)}
		}
		out = append(out, models.StudioCaptionCue{
			ID: idGen(), StartSeconds: ws[0].StartSeconds, EndSeconds: ws[len(ws)-1].EndSeconds, Text: b.String(), Words: ws,
		})
		cur, chars = nil, 0
	}
	for _, u := range units {
		if n := len(cur); n > 0 {
			prev := cur[n-1]
			if models.CaptionSentenceEnd(prev.Text) || u.start-prev.end >= studioCueGapSeconds ||
				chars+len([]rune(u.Text))+1 > studioMaxCueChars || u.end-cur[0].start > studioMaxCueSeconds || n >= studioCueMaxWords {
				flush()
			}
		}
		cur = append(cur, u)
		chars += len([]rune(u.Text)) + 1
	}
	flush()
	return out
}

// wordsToUnits maps whisper words onto caption units of their concatenated
// text: a unit spanning several words takes the first start and last end, and
// a word spanning several units (a Han/kana run) splits its time by
// character count. Times come out non-decreasing.
func wordsToUnits(words []TranscribeWordDTO, silenceStart float64) []timedUnit {
	type wspan struct {
		a, b       int // rune offsets in text, [a, b)
		start, end float64
	}
	var text []rune
	var spans []wspan
	for _, w := range words {
		if silenceStart > 0 && w.Start >= silenceStart {
			break
		}
		r := []rune(w.Word)
		a := len(text)
		text = append(text, r...)
		// The span is the word's non-space runes.
		lead := 0
		for lead < len(r) && unicode.IsSpace(r[lead]) {
			lead++
		}
		e := w.End
		if e < w.Start {
			e = w.Start
		}
		spans = append(spans, wspan{a: a + lead, b: a + len(r), start: w.Start, end: e})
	}
	units := models.CaptionUnits(string(text))
	out := make([]timedUnit, 0, len(units))
	idx := 0
	prevStart := 0.0
	for _, u := range units {
		first, last := -1, -1
		for _, ch := range u.Text {
			for idx < len(text) && text[idx] != ch {
				idx++
			}
			if idx >= len(text) {
				break
			}
			if first < 0 {
				first = idx
			}
			last = idx
			idx++
		}
		if first < 0 {
			continue
		}
		start, end, found := 0.0, 0.0, false
		for _, sp := range spans {
			lo, hi := maxInt(first, sp.a), minInt(last+1, sp.b)
			if hi <= lo {
				continue
			}
			n := float64(sp.b - sp.a)
			s := sp.start + (sp.end-sp.start)*float64(lo-sp.a)/n
			e := sp.start + (sp.end-sp.start)*float64(hi-sp.a)/n
			if !found {
				start, found = s, true
			}
			end = e
		}
		if !found {
			continue
		}
		if start < prevStart {
			start = prevStart
		}
		if end < start {
			end = start
		}
		prevStart = start
		out = append(out, timedUnit{CaptionUnit: u, start: start, end: end})
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// splitBalanced splits text into roughly equal word-boundary chunks each within
// maxChars.
func splitBalanced(text string, maxChars int) []string {
	words := strings.Fields(text)
	if len(words) <= 1 {
		return []string{text}
	}
	total := len([]rune(text))
	parts := (total + maxChars - 1) / maxChars
	if parts < 1 {
		parts = 1
	}
	per := (len(words) + parts - 1) / parts
	if per < 1 {
		per = 1
	}
	out := make([]string, 0, parts)
	for i := 0; i < len(words); i += per {
		end := i + per
		if end > len(words) {
			end = len(words)
		}
		out = append(out, strings.Join(words[i:end], " "))
	}
	return out
}

// hexToASSColor → ASS &HAABBGGRR (AA: 00 opaque .. FF transparent).
func hexToASSColor(hex string, opacity float64) string {
	r, g, b := parseHexRGB(hex)
	a := int(math.Round((1 - clamp01(opacity)) * 255))
	return fmt.Sprintf("&H%02X%02X%02X%02X", a, b, g, r)
}

func assTimestamp(sec float64) string {
	return assCentis(int(math.Round(math.Max(sec, 0) * 100)))
}

// assCentis formats whole centiseconds as H:MM:SS.CC.
func assCentis(cs int) string {
	if cs < 0 {
		cs = 0
	}
	return fmt.Sprintf("%d:%02d:%02d.%02d", cs/360000, (cs/6000)%60, (cs/100)%60, cs%100)
}

func assEscape(s string) string {
	s = strings.ReplaceAll(s, "{", "(")
	s = strings.ReplaceAll(s, "}", ")")
	s = strings.ReplaceAll(s, "\r\n", "\\N")
	s = strings.ReplaceAll(s, "\n", "\\N")
	s = strings.ReplaceAll(s, "\r", "\\N")
	return s
}
