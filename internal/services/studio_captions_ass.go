package services

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// BuildASS is the Darkroom caption generator (MM121, ADR-0019): cues + style +
// canvas → the .ass text libass draws, in ffmpeg's `subtitles=` filter for
// export burn-in and in JASSUB for the editor preview. Its TS twin is
// lib/studio/captions/ass.ts in the UI; both reproduce the golden fixtures in
// testdata/captions/*.ass byte for byte, so a change here is a change there
// (and a fixture regeneration). Neither side may special-case an effect the
// other lacks.
//
// Layout: one style ("Default") from the project style; a "WordBox" style
// when backgroundMode is word_box. Each cue splits into groups (wordsPerGroup
// units, breaking after a sentence end or before a gap ≥ 150 ms; 0 = the whole
// cue), one group on screen at a time, each Dialogue hard-ending at its group
// end. Times are whole centiseconds. Animations:
//   - none: one Dialogue per group.
//   - karaoke_fill: one Dialogue per group, \kf per word (filled = highlight).
//   - highlight: one Dialogue per word span, the active word recoloured.
//   - pop: as highlight, plus a scale-up-and-back \t on the active word.
//
// fontName is ignored (kept for the call sites); the family comes from the
// style, with a per-cue script fallback (models.CaptionFontFor).
func BuildASS(cues []models.StudioCaptionCue, style *models.StudioCaptionStyle, width, height int, fontName string) string {
	_ = fontName
	if width <= 0 {
		width = 1920
	}
	if height <= 0 {
		height = 1080
	}
	r := resolveCaptionStyle(models.SanitizeCaptionStyle(style), height)
	var b strings.Builder
	b.WriteString("[Script Info]\n")
	b.WriteString("; Media Manipulator Darkroom captions (generator v1)\n")
	b.WriteString("ScriptType: v4.00+\n")
	fmt.Fprintf(&b, "PlayResX: %d\nPlayResY: %d\n", width, height)
	b.WriteString("WrapStyle: 0\nScaledBorderAndShadow: yes\nYCbCr Matrix: None\n\n")
	b.WriteString("[V4+ Styles]\n")
	b.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n")
	marginH := int(math.Round(float64(width) * (100 - r.maxWidthPct) / 200))
	marginV := int(math.Round(float64(height) * 0.05))
	bold := 0
	if r.bold {
		bold = -1
	}
	borderStyle, shadow, back := 1, r.shadowPx, r.shadowColor
	if r.bgMode == models.CaptionBackgroundBox {
		borderStyle, shadow, back = 4, r.boxPad, r.boxColor
	}
	fmt.Fprintf(&b, "Style: Default,%s,%d,%s,%s,%s,%s,%d,0,0,0,100,100,0,0,%d,%s,%s,%d,%d,%d,%d,-1\n",
		r.family, r.fontSize, r.primary, r.primary, r.outlineColor, back, bold, borderStyle, assNum(r.outlinePx), assNum(shadow), r.align, marginH, marginH, marginV)
	if r.bgMode == models.CaptionBackgroundWordBox {
		fmt.Fprintf(&b, "Style: WordBox,%s,%d,&HFF000000,&HFF000000,%s,&HFF000000,%d,0,0,0,100,100,0,0,3,%s,0,%d,%d,%d,%d,-1\n",
			r.family, r.fontSize, hexToASSColor(r.highlight, 1), bold, assNum(r.boxPad), r.align, marginH, marginH, marginV)
	}
	b.WriteString("\n[Events]\n")
	b.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
	for _, c := range cues {
		writeCueEvents(&b, c, r)
	}
	return b.String()
}

// resolvedCaptionStyle is a sanitized style with every default filled in and
// pixel values computed for the canvas.
type resolvedCaptionStyle struct {
	family                      string
	bold, uppercase             bool
	fontSize                    int
	primary, outlineColor       string // &HAABBGGRR
	primaryTag, highlightTag    string // &HBBGGRR& for overrides
	highlight                   string // #RRGGBB
	outlinePx, shadowPx, boxPad float64
	shadowColor, boxColor       string
	bgMode, animation           string
	wordsPerGroup, align        int
	popScale                    int
	maxWidthPct                 float64
}

func resolveCaptionStyle(st *models.StudioCaptionStyle, height int) resolvedCaptionStyle {
	if st == nil {
		st = &models.StudioCaptionStyle{FontSizePct: 4.5, Color: "#FFFFFF", BackgroundColor: "#000000", BackgroundOpacity: 0.55, Position: "bottom", MaxWidthPct: 90}
	}
	r := resolvedCaptionStyle{
		family:        firstNonEmptyString(st.FontFamily, models.CaptionDefaultFontFamily),
		bold:          models.CaptionDefaultBold,
		highlight:     firstNonEmptyString(st.HighlightColor, models.CaptionDefaultHighlightColor),
		bgMode:        firstNonEmptyString(st.BackgroundMode, models.CaptionDefaultBackgroundMode),
		animation:     firstNonEmptyString(st.Animation, models.CaptionDefaultAnimation),
		wordsPerGroup: st.WordsPerGroup,
		maxWidthPct:   st.MaxWidthPct,
	}
	if st.Bold != nil {
		r.bold = *st.Bold
	}
	if f, ok := models.CaptionFontByFamily(r.family); ok && f.Bold == "" {
		r.bold = false
	}
	if st.Uppercase != nil {
		r.uppercase = *st.Uppercase
	}
	scale := float64(height) / 1080
	r.fontSize = int(math.Round(float64(height) * st.FontSizePct / 100))
	if r.fontSize < 8 {
		r.fontSize = 8
	}
	outline, shadow, strength := models.CaptionDefaultOutlineWidth, models.CaptionDefaultShadow, models.CaptionDefaultAnimationStrength
	if st.OutlineWidth != nil {
		outline = *st.OutlineWidth
	}
	if st.Shadow != nil {
		shadow = *st.Shadow
	}
	if st.AnimationStrength != nil {
		strength = *st.AnimationStrength
	}
	r.outlinePx = roundCentis(outline * scale)
	r.shadowPx = roundCentis(shadow * 8 * scale)
	r.boxPad = roundCentis(float64(r.fontSize) * 0.2)
	r.popScale = 100 + int(math.Round(40*strength))
	r.primary = hexToASSColor(st.Color, 1)
	r.primaryTag = assColorTag(st.Color)
	r.highlightTag = assColorTag(r.highlight)
	r.outlineColor = hexToASSColor(firstNonEmptyString(st.OutlineColor, models.CaptionDefaultOutlineColor), 1)
	r.boxColor = hexToASSColor(st.BackgroundColor, st.BackgroundOpacity)
	r.shadowColor = r.boxColor
	switch st.Position {
	case "top":
		r.align = 8
	case "middle":
		r.align = 5
	default:
		r.align = 2
	}
	return r
}

// captionTimedUnit is a caption unit with its centisecond times.
type captionTimedUnit struct {
	sep, text  string
	start, end int
	emphasis   bool
}

func cueTimedUnits(c models.StudioCaptionCue, uppercase bool) ([]captionTimedUnit, bool) {
	units := models.CaptionUnits(c.Text)
	cs, ce := centis(c.StartSeconds), centis(c.EndSeconds)
	if ce < cs {
		ce = cs
	}
	n := len(units)
	out := make([]captionTimedUnit, n)
	hasWords := len(c.Words) == n && n > 0
	prev := cs
	for i, u := range units {
		t := captionTimedUnit{sep: u.Sep, text: u.Text}
		if uppercase {
			t.text = models.CaptionUpper(t.text)
		}
		if hasWords {
			w := c.Words[i]
			t.start = clampInt(centis(w.StartSeconds), prev, ce)
			t.end = clampInt(centis(w.EndSeconds), t.start, ce)
			t.emphasis = w.Emphasis
			prev = t.start
		} else {
			t.start = cs + (ce-cs)*i/n
			t.end = cs + (ce-cs)*(i+1)/n
		}
		out[i] = t
	}
	return out, hasWords
}

// captionGroups splits units into [from, to) groups.
func captionGroups(units []captionTimedUnit, perGroup int, hasWords bool) [][2]int {
	var out [][2]int
	if perGroup <= 0 {
		return [][2]int{{0, len(units)}}
	}
	from := 0
	for i := range units {
		if i > from {
			brk := i-from >= perGroup || models.CaptionSentenceEnd(units[i-1].text) ||
				(hasWords && units[i].start-units[i-1].end >= 15)
			if brk {
				out = append(out, [2]int{from, i})
				from = i
			}
		}
	}
	return append(out, [2]int{from, len(units)})
}

func writeCueEvents(b *strings.Builder, c models.StudioCaptionCue, r resolvedCaptionStyle) {
	units, hasWords := cueTimedUnits(c, r.uppercase)
	if len(units) == 0 {
		return
	}
	cs, ce := centis(c.StartSeconds), centis(c.EndSeconds)
	if ce <= cs {
		return
	}
	prefix := ""
	if fam := models.CaptionFontFor(r.family, c.Text); fam != r.family {
		prefix = `\fn` + fam
		if f, ok := models.CaptionFontByFamily(fam); ok && f.Bold == "" && r.bold {
			prefix += `\b0`
		}
	}
	groups := captionGroups(units, r.wordsPerGroup, hasWords)
	wordBox := r.bgMode == models.CaptionBackgroundWordBox
	for gi, g := range groups {
		gs, ge := cs, ce
		if gi > 0 {
			gs = units[g[0]].start
		}
		if gi < len(groups)-1 {
			ge = units[groups[gi+1][0]].start
		}
		if ge <= gs {
			continue
		}
		gu := units[g[0]:g[1]]
		perWord := r.animation == models.CaptionAnimationHighlight || r.animation == models.CaptionAnimationPop || (wordBox && r.animation == models.CaptionAnimationKaraokeFill)
		type span struct{ s, e, active int }
		spans := []span{{gs, ge, -1}}
		if perWord {
			spans = spans[:0]
			if gu[0].start > gs {
				spans = append(spans, span{gs, gu[0].start, -1})
			}
			for i := range gu {
				s, e := gu[i].start, ge
				if i == 0 {
					s = maxInt(gs, gu[0].start)
				}
				if i+1 < len(gu) {
					e = gu[i+1].start
				}
				if e > s {
					spans = append(spans, span{s, e, i})
				}
			}
		}
		if wordBox {
			for _, sp := range spans {
				boxed := false
				for i := range gu {
					boxed = boxed || gu[i].emphasis || (i == sp.active && r.animation != models.CaptionAnimationNone)
				}
				if !boxed {
					continue
				}
				fmt.Fprintf(b, "Dialogue: 0,%s,%s,WordBox,,0,0,0,,%s\n", assCentis(sp.s), assCentis(sp.e), boxLine(gu, sp.active, r, prefix))
			}
		}
		if r.animation == models.CaptionAnimationKaraokeFill {
			fmt.Fprintf(b, "Dialogue: 1,%s,%s,Default,,0,0,0,,%s\n", assCentis(gs), assCentis(ge), karaokeLine(gu, gs, r, prefix))
			continue
		}
		for _, sp := range spans {
			fmt.Fprintf(b, "Dialogue: 1,%s,%s,Default,,0,0,0,,%s\n", assCentis(sp.s), assCentis(sp.e), textLine(gu, sp.active, r, prefix))
		}
	}
}

// popTags is the scale-up-and-back transform on the active word.
func popTags(r resolvedCaptionStyle) string {
	return fmt.Sprintf(`\t(0,80,\fscx%d\fscy%d)\t(80,180,\fscx100\fscy100)`, r.popScale, r.popScale)
}

func openTags(prefix string) string {
	if prefix == "" {
		return ""
	}
	return "{" + prefix + "}"
}

// textLine is a group's text with the active (-1 = none) and emphasised words
// decorated: recoloured to the highlight (not under word_box, where the box
// carries the highlight) and, for pop, scaled.
func textLine(gu []captionTimedUnit, active int, r resolvedCaptionStyle, prefix string) string {
	var b strings.Builder
	b.WriteString(openTags(prefix))
	wordBox := r.bgMode == models.CaptionBackgroundWordBox
	for i, u := range gu {
		if i > 0 {
			b.WriteString(assSep(u.sep))
		}
		isActive := i == active
		color := !wordBox && (isActive || u.emphasis)
		pop := isActive && r.animation == models.CaptionAnimationPop
		if !color && !pop {
			b.WriteString(assEscape(u.text))
			continue
		}
		b.WriteString("{")
		if color {
			b.WriteString(`\1c` + r.highlightTag)
		}
		if pop {
			b.WriteString(popTags(r))
		}
		b.WriteString("}" + assEscape(u.text) + "{")
		if color {
			b.WriteString(`\1c` + r.primaryTag)
		}
		if pop {
			b.WriteString(`\fscx100\fscy100`)
		}
		b.WriteString("}")
	}
	return b.String()
}

// boxLine is the WordBox layer: the same text, invisible, with the box (the
// BorderStyle 3 outline) shown behind the active and emphasised words.
func boxLine(gu []captionTimedUnit, active int, r resolvedCaptionStyle, prefix string) string {
	var b strings.Builder
	b.WriteString("{" + prefix + `\3a&HFF&}`)
	for i, u := range gu {
		if i > 0 {
			b.WriteString(assSep(u.sep))
		}
		isActive := i == active && r.animation != models.CaptionAnimationNone
		pop := i == active && r.animation == models.CaptionAnimationPop
		if !isActive && !u.emphasis {
			b.WriteString(assEscape(u.text))
			continue
		}
		b.WriteString(`{\3a&H00&`)
		if pop {
			b.WriteString(popTags(r))
		}
		b.WriteString("}" + assEscape(u.text) + `{\3a&HFF&`)
		if pop {
			b.WriteString(`\fscx100\fscy100`)
		}
		b.WriteString("}")
	}
	return b.String()
}

// karaokeLine is a group as one \kf line: filled = highlight (\1c), unfilled
// = the text colour (\2c); gaps between words are instant \k syllables.
func karaokeLine(gu []captionTimedUnit, gs int, r resolvedCaptionStyle, prefix string) string {
	var b strings.Builder
	b.WriteString("{" + prefix + `\1c` + r.highlightTag + `\2c` + r.primaryTag)
	if lead := gu[0].start - gs; lead > 0 {
		fmt.Fprintf(&b, `\k%d`, lead)
	}
	b.WriteString("}")
	for i, u := range gu {
		if i > 0 {
			fmt.Fprintf(&b, `{\k%d}%s`, maxInt(0, u.start-gu[i-1].end), assSep(u.sep))
		}
		if u.emphasis {
			fmt.Fprintf(&b, `{\kf%d\2c%s}%s{\2c%s}`, u.end-u.start, r.highlightTag, assEscape(u.text), r.primaryTag)
			continue
		}
		fmt.Fprintf(&b, `{\kf%d}%s`, u.end-u.start, assEscape(u.text))
	}
	return b.String()
}

// assSep renders the whitespace before a unit: a line break stays a line
// break, any other run of whitespace is one space, none stays none.
func assSep(sep string) string {
	switch {
	case sep == "":
		return ""
	case strings.ContainsAny(sep, "\n\r"):
		return `\N`
	default:
		return " "
	}
}

// assColorTag → &HBBGGRR& for \1c-style overrides.
func assColorTag(hex string) string {
	r, g, b := parseHexRGB(hex)
	return fmt.Sprintf("&H%02X%02X%02X&", b, g, r)
}

// assNum formats a non-negative number with at most two decimals and no
// trailing zeros (the TS twin: String(Math.round(x * 100) / 100)).
func assNum(v float64) string {
	return strconv.FormatFloat(roundCentis(v), 'f', -1, 64)
}

func roundCentis(v float64) float64 { return math.Round(v*100) / 100 }

func centis(sec float64) int { return int(math.Round(math.Max(sec, 0) * 100)) }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
