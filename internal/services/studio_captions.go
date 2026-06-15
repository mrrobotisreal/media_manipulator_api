package services

import (
	"fmt"
	"math"
	"strings"

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
	cues := make([]models.StudioCaptionCue, 0, len(segments))
	for _, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
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

// buildASS serializes cues + style to an Advanced SubStation Alpha subtitle for
// export burn-in via the `subtitles` filter. fontName should resolve through
// libass/fontconfig on the server (defaults to "Sans").
func BuildASS(cues []models.StudioCaptionCue, style *models.StudioCaptionStyle, width, height int, fontName string) string {
	st := models.SanitizeCaptionStyle(style)
	if st == nil {
		st = &models.StudioCaptionStyle{FontSizePct: 4.5, Color: "#FFFFFF", BackgroundColor: "#000000", BackgroundOpacity: 0.55, Position: "bottom", MaxWidthPct: 90}
	}
	if width <= 0 {
		width = 1920
	}
	if height <= 0 {
		height = 1080
	}
	if fontName == "" {
		fontName = "Sans"
	}
	fontSize := int(math.Round(float64(height) * st.FontSizePct / 100))
	if fontSize < 8 {
		fontSize = 8
	}
	primary := hexToASSColor(st.Color, 1.0)
	back := hexToASSColor(st.BackgroundColor, st.BackgroundOpacity)
	align := 2 // bottom-center
	if st.Position == "top" {
		align = 8 // top-center
	}
	marginV := int(math.Round(float64(height) * 0.05))

	var b strings.Builder
	b.WriteString("[Script Info]\n")
	b.WriteString("ScriptType: v4.00+\n")
	b.WriteString(fmt.Sprintf("PlayResX: %d\n", width))
	b.WriteString(fmt.Sprintf("PlayResY: %d\n", height))
	b.WriteString("WrapStyle: 0\n\n")
	b.WriteString("[V4+ Styles]\n")
	b.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n")
	b.WriteString(fmt.Sprintf("Style: Default,%s,%d,%s,&H000000FF,&H00000000,%s,0,0,0,0,100,100,0,0,3,0,0,%d,40,40,%d,1\n\n",
		fontName, fontSize, primary, back, align, marginV))
	b.WriteString("[Events]\n")
	b.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
	for _, c := range cues {
		b.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,Default,,0,0,0,,%s\n", assTimestamp(c.StartSeconds), assTimestamp(c.EndSeconds), assEscape(c.Text)))
	}
	return b.String()
}

// hexToASSColor → ASS &HAABBGGRR (AA: 00 opaque .. FF transparent).
func hexToASSColor(hex string, opacity float64) string {
	r, g, b := parseHexRGB(hex)
	a := int(math.Round((1 - clamp01(opacity)) * 255))
	return fmt.Sprintf("&H%02X%02X%02X%02X", a, b, g, r)
}

func assTimestamp(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	cs := int(math.Round((sec - math.Floor(sec)) * 100))
	if cs >= 100 {
		cs = 0
		s++
	}
	return fmt.Sprintf("%d:%02d:%02d.%02d", h, m, s, cs)
}

func assEscape(s string) string {
	s = strings.ReplaceAll(s, "{", "(")
	s = strings.ReplaceAll(s, "}", ")")
	s = strings.ReplaceAll(s, "\r\n", "\\N")
	s = strings.ReplaceAll(s, "\n", "\\N")
	s = strings.ReplaceAll(s, "\r", "\\N")
	return s
}
