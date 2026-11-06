package services

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

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
//   - bounce (MM122): as pop, a taller y-stretch with an overshoot settle.
//   - word_by_word (MM122): words appear at their start (alpha fade-in),
//     later words hidden.
//   - box_highlight (MM122): a BorderStyle 3 box in the highlight colour
//     behind the active word (the WordBox layer, as backgroundMode word_box).
//   - fade (MM122): \fad in and out per group (exit ~55 % of the entrance).
//   - typewriter (MM122): the active word's characters revealed across its
//     spoken span, later words hidden.
//   - glow (MM122): the active word's outline turns the highlight colour
//     and its \blur pulses.
//
// animationStrength scales every effect (sizes, durations, blur).
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
	if r.boxed {
		fmt.Fprintf(&b, "Style: WordBox,%s,%d,&HFF000000,&HFF000000,%s,&HFF000000,%d,0,0,0,100,100,0,0,3,%s,0,%d,%d,%d,%d,-1\n",
			r.family, r.fontSize, hexToASSColor(r.highlight, 1), bold, assNum(r.wordBoxPad), r.align, marginH, marginH, marginV)
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
	// MM122 effect parameters (all derived from animationStrength).
	boxed                  bool    // a WordBox layer: backgroundMode word_box or animation box_highlight
	wordBoxPad             float64 // the WordBox outline (box padding)
	bounceX, bounceY       int     // bounce peak \fscx / \fscy
	bounceDipX, bounceDipY int     // bounce overshoot dip
	fadeIn, fadeOut        int     // fade ms
	revealMs               int     // word_by_word fade-in ms
	typeFrac               float64 // typewriter: share of a word's span its characters take
	glowBord               float64 // glow outline px
	glowBlur, glowPulse    float64 // glow \blur rest / peak
	outlineTag             string  // &HBBGGRR& of the outline colour
	backAlpha              string  // &HAA& of BackColour (restores \4a after a hide)
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
	// MM122. Timings follow the motion reference (word stagger 60-120 ms,
	// exits 40-50 % shorter than entrances); sizes scale with the canvas.
	r.bounceX = 100 + int(math.Round(20*strength))
	r.bounceY = 100 + int(math.Round(45*strength))
	r.bounceDipX = 100 - int(math.Round(6*strength))
	r.bounceDipY = 100 - int(math.Round(10*strength))
	r.fadeIn = int(math.Round(120 + 180*strength))
	r.fadeOut = int(math.Round(float64(r.fadeIn) * 0.55))
	r.revealMs = int(math.Round(60 + 80*strength))
	r.typeFrac = 0.5 + 0.5*strength
	r.glowBord = math.Max(r.outlinePx, roundCentis((2+4*strength)*scale))
	r.glowBlur = roundCentis((1 + 3*strength) * scale)
	r.glowPulse = roundCentis((2 + 6*strength) * scale)
	r.primary = hexToASSColor(st.Color, 1)
	r.primaryTag = assColorTag(st.Color)
	r.highlightTag = assColorTag(r.highlight)
	r.outlineColor = hexToASSColor(firstNonEmptyString(st.OutlineColor, models.CaptionDefaultOutlineColor), 1)
	r.outlineTag = assColorTag(firstNonEmptyString(st.OutlineColor, models.CaptionDefaultOutlineColor))
	r.boxColor = hexToASSColor(st.BackgroundColor, st.BackgroundOpacity)
	r.shadowColor = r.boxColor
	r.backAlpha = "&H" + r.boxColor[2:4] + "&"
	r.boxed = r.bgMode == models.CaptionBackgroundWordBox || r.animation == models.CaptionAnimationBoxHighlight
	r.wordBoxPad = r.boxPad
	if r.bgMode != models.CaptionBackgroundWordBox {
		r.wordBoxPad = roundCentis(float64(r.fontSize) * (0.1 + 0.15*strength))
	}
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
	revealing := revealingAnimation(r.animation)
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
		perWord := captionPerWord(r.animation) || (wordBox && r.animation == models.CaptionAnimationKaraokeFill)
		pfx := prefix
		if r.animation == models.CaptionAnimationFade {
			in, out := r.fadeIn, r.fadeOut
			if dur := (ge - gs) * 10; in+out > dur {
				tot := in + out
				in, out = in*dur/tot, out*dur/tot
			}
			pfx = fmt.Sprintf(`\fad(%d,%d)`, in, out) + prefix
		}
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
		if r.boxed {
			for _, sp := range spans {
				if revealing && sp.active < 0 {
					continue
				}
				boxed := false
				for i := range gu {
					boxed = boxed || gu[i].emphasis || (i == sp.active && r.animation != models.CaptionAnimationNone)
				}
				if !boxed {
					continue
				}
				fmt.Fprintf(b, "Dialogue: 0,%s,%s,WordBox,,0,0,0,,%s\n", assCentis(sp.s), assCentis(sp.e), boxLine(gu, sp.active, r, pfx))
			}
		}
		if r.animation == models.CaptionAnimationKaraokeFill {
			fmt.Fprintf(b, "Dialogue: 1,%s,%s,Default,,0,0,0,,%s\n", assCentis(gs), assCentis(ge), karaokeLine(gu, gs, r, prefix))
			continue
		}
		for _, sp := range spans {
			if revealing && sp.active < 0 {
				continue
			}
			fmt.Fprintf(b, "Dialogue: 1,%s,%s,Default,,0,0,0,,%s\n", assCentis(sp.s), assCentis(sp.e), textLine(gu, sp.active, r, pfx))
		}
	}
}

// captionPerWord reports whether an animation emits one Dialogue per word
// span (the active word changes over the group).
func captionPerWord(animation string) bool {
	switch animation {
	case models.CaptionAnimationHighlight, models.CaptionAnimationPop, models.CaptionAnimationBounce,
		models.CaptionAnimationWordByWord, models.CaptionAnimationBoxHighlight,
		models.CaptionAnimationTypewriter, models.CaptionAnimationGlow:
		return true
	}
	return false
}

// popTags is the scale-up-and-back transform on the active word.
func popTags(r resolvedCaptionStyle) string {
	return fmt.Sprintf(`\t(0,80,\fscx%d\fscy%d)\t(80,180,\fscx100\fscy100)`, r.popScale, r.popScale)
}

// bounceTags is a tall stretch, an overshoot dip and the settle (MM122).
func bounceTags(r resolvedCaptionStyle) string {
	return fmt.Sprintf(`\t(0,90,\fscx%d\fscy%d)\t(90,180,\fscx%d\fscy%d)\t(180,260,\fscx100\fscy100)`,
		r.bounceX, r.bounceY, r.bounceDipX, r.bounceDipY)
}

// scaleTags is the active word's scale transform (pop, bounce), or "".
func scaleTags(r resolvedCaptionStyle) string {
	switch r.animation {
	case models.CaptionAnimationPop:
		return popTags(r)
	case models.CaptionAnimationBounce:
		return bounceTags(r)
	}
	return ""
}

// glowTags lights the active word's outline in the highlight colour and
// pulses its blur (MM122); glowReset restores the style's outline.
func glowTags(r resolvedCaptionStyle) string {
	return fmt.Sprintf(`\3c%s\bord%s\blur%s\t(0,150,\blur%s)\t(150,300,\blur%s)`,
		r.highlightTag, assNum(r.glowBord), assNum(r.glowBlur), assNum(r.glowPulse), assNum(r.glowBlur))
}

func glowReset(r resolvedCaptionStyle) string {
	return `\3c` + r.outlineTag + `\bord` + assNum(r.outlinePx) + `\blur0`
}

// hideTags / showTags hide and restore a run's fill, outline and shadow
// alpha (not \alpha, which would also make a translucent shadow opaque).
// Under backgroundMode box the back colour is the line's box, so it stays.
func hideTags(r resolvedCaptionStyle) string {
	if r.bgMode == models.CaptionBackgroundBox {
		return `\1a&HFF&\3a&HFF&`
	}
	return `\1a&HFF&\3a&HFF&\4a&HFF&`
}

func showTags(r resolvedCaptionStyle) string {
	if r.bgMode == models.CaptionBackgroundBox {
		return `\1a&H00&\3a&H00&`
	}
	return `\1a&H00&\3a&H00&\4a` + r.backAlpha
}

// revealingAnimation: word_by_word and typewriter show nothing before a
// group's first word and hide every word after the active one.
func revealingAnimation(animation string) bool {
	return animation == models.CaptionAnimationWordByWord || animation == models.CaptionAnimationTypewriter
}

// recolorsActive: the animations that paint the active word's fill in the
// highlight colour (the others mark it by scale, box, glow or reveal).
func recolorsActive(animation string) bool {
	return animation == models.CaptionAnimationHighlight || animation == models.CaptionAnimationPop || animation == models.CaptionAnimationBounce
}

// typewriterText reveals a word's characters (combining marks stay with their
// base) across typeFrac of its spoken span, each character its own run.
func typewriterText(u captionTimedUnit, r resolvedCaptionStyle) string {
	clusters := captionClusters(u.text)
	d := int(math.Round(float64((u.end-u.start)*10) * r.typeFrac))
	var b strings.Builder
	for i, cl := range clusters {
		t := i * d / len(clusters)
		fmt.Fprintf(&b, `{%s\t(%d,%d,%s)}%s`, hideTags(r), t, t+1, showTags(r), assEscape(cl))
	}
	return b.String()
}

// captionClusters splits text into characters, a combining mark joining the
// character before it (unicode Mn / Me), as the TS twin does.
func captionClusters(s string) []string {
	var out []string
	for _, ru := range s {
		if len(out) > 0 && unicode.In(ru, unicode.Mn, unicode.Me) {
			out[len(out)-1] += string(ru)
			continue
		}
		out = append(out, string(ru))
	}
	return out
}

func openTags(prefix string) string {
	if prefix == "" {
		return ""
	}
	return "{" + prefix + "}"
}

// textLine is a group's text with the active (-1 = none) and emphasised words
// decorated: recoloured to the highlight (not when a WordBox layer carries the
// highlight), scaled (pop, bounce), glowing, revealed (word_by_word fades the
// active word in, typewriter types it; both hide every later word).
func textLine(gu []captionTimedUnit, active int, r resolvedCaptionStyle, prefix string) string {
	var b strings.Builder
	b.WriteString(openTags(prefix))
	revealing := revealingAnimation(r.animation)
	for i, u := range gu {
		if revealing && active >= 0 && i == active+1 {
			b.WriteString("{" + hideTags(r) + "}")
		}
		if i > 0 {
			b.WriteString(assSep(u.sep))
		}
		isActive := i == active
		var open, closing string
		if !r.boxed && (u.emphasis || (isActive && recolorsActive(r.animation))) {
			open += `\1c` + r.highlightTag
			closing += `\1c` + r.primaryTag
		}
		if isActive {
			if sc := scaleTags(r); sc != "" {
				open += sc
				closing += `\fscx100\fscy100`
			}
			switch r.animation {
			case models.CaptionAnimationGlow:
				open += glowTags(r)
				closing += glowReset(r)
			case models.CaptionAnimationWordByWord:
				open += fmt.Sprintf(`%s\t(0,%d,%s)`, hideTags(r), r.revealMs, showTags(r))
			}
		}
		typing := isActive && r.animation == models.CaptionAnimationTypewriter
		if open == "" && closing == "" && !typing {
			b.WriteString(assEscape(u.text))
			continue
		}
		if open != "" {
			b.WriteString("{" + open + "}")
		}
		if typing {
			b.WriteString(typewriterText(u, r))
		} else {
			b.WriteString(assEscape(u.text))
		}
		if closing != "" {
			b.WriteString("{" + closing + "}")
		}
	}
	return b.String()
}

// boxLine is the WordBox layer: the same text, invisible, with the box (the
// BorderStyle 3 outline) shown behind the active and emphasised words (not
// behind a word a revealing animation has not shown yet).
func boxLine(gu []captionTimedUnit, active int, r resolvedCaptionStyle, prefix string) string {
	var b strings.Builder
	b.WriteString("{" + prefix + `\3a&HFF&}`)
	revealing := revealingAnimation(r.animation)
	for i, u := range gu {
		if i > 0 {
			b.WriteString(assSep(u.sep))
		}
		isActive := i == active && r.animation != models.CaptionAnimationNone
		sc := ""
		if i == active {
			sc = scaleTags(r)
		}
		if (!isActive && !u.emphasis) || (revealing && i > active) {
			b.WriteString(assEscape(u.text))
			continue
		}
		b.WriteString(`{\3a&H00&` + sc)
		b.WriteString("}" + assEscape(u.text) + `{\3a&HFF&`)
		if sc != "" {
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
