package models

import (
	"math"
	"sort"
	"strings"
)

// EDL v2 sanitation. Ranges mirror the Zod schema in lib/studioTypes.ts and the
// registry in lib/studio/effectRegistry.ts. SanitizeTracks / SanitizeCaptions /
// SanitizeCaptionStyle / SanitizeAudioConfig run on save AND before export so
// the export compiler can trust the plan without re-validating client input.
//
// Invariant: a valid v1 clip (no v2 fields, in-range values) sanitizes to a
// byte-identical clip, so existing projects export exactly as before.

const (
	studioMaxVolume         = 2.0
	studioMaxKeyframes      = 64
	studioMaxTextOverlayLen = 200
	studioMaxCaptionLen     = 500
	studioMaxCaptionCues    = 2000
)

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func ptrF(v float64) *float64 { return &v }

// clampOpt returns the clamped pointer value, or def when the pointer is nil.
func clampOpt(p *float64, def, lo, hi float64) float64 {
	if p == nil {
		return def
	}
	return clampF(*p, lo, hi)
}

func orDefaultPositive(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func isHexColor(s string) bool {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func normalizeHexColor(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	return "#" + strings.ToUpper(s)
}

// SanitizeTracks returns a deep-cleaned copy of the track tree: every numeric
// field clamped to its valid range, malformed effects dropped, keyframes sorted
// and capped.
func SanitizeTracks(tracks []StudioTrack) []StudioTrack {
	out := make([]StudioTrack, 0, len(tracks))
	for _, t := range tracks {
		clips := make([]StudioClip, 0, len(t.Clips))
		for _, c := range t.Clips {
			clips = append(clips, sanitizeClip(c))
		}
		t.Clips = clips
		out = append(out, t)
	}
	return out
}

func sanitizeClip(c StudioClip) StudioClip {
	if c.SourceIn < 0 {
		c.SourceIn = 0
	}
	if c.SourceOut < c.SourceIn {
		c.SourceOut = c.SourceIn
	}
	if c.TimelineStart < 0 {
		c.TimelineStart = 0
	}
	if c.Volume != nil {
		c.Volume = ptrF(clampF(*c.Volume, 0, studioMaxVolume))
	}
	if c.Opacity != nil {
		c.Opacity = ptrF(clampF(*c.Opacity, 0, 1))
	}
	if c.TransitionInSeconds != nil && *c.TransitionInSeconds < 0 {
		c.TransitionInSeconds = ptrF(0)
	}
	if c.Adjustments != nil {
		c.Adjustments = &StudioAdjustments{
			Brightness: clampF(c.Adjustments.Brightness, -1, 1),
			Contrast:   clampF(c.Adjustments.Contrast, 0, 2),
			Saturation: clampF(c.Adjustments.Saturation, 0, 2),
		}
	}
	c.Transform = sanitizeTransform(c.Transform)
	c.Crop = sanitizeCrop(c.Crop)
	c.BlendMode = sanitizeBlendMode(c.BlendMode)
	c.Effects = sanitizeEffects(c.Effects)
	c.VolumeKeyframes = sanitizeKeyframes(c.VolumeKeyframes)
	if c.Pan != nil {
		c.Pan = ptrF(clampF(*c.Pan, -1, 1))
	}
	if len(c.TextOverlays) > 0 {
		ovs := make([]StudioTextOverlay, 0, len(c.TextOverlays))
		for _, ov := range c.TextOverlays {
			ov.X = clampF(ov.X, 0, 1)
			ov.Y = clampF(ov.Y, 0, 1)
			ov.Text = truncateRunes(ov.Text, studioMaxTextOverlayLen)
			ovs = append(ovs, ov)
		}
		c.TextOverlays = ovs
	}
	return c
}

func sanitizeTransform(tr *StudioTransform) *StudioTransform {
	if tr == nil {
		return nil
	}
	return &StudioTransform{
		X:           clampF(tr.X, -1, 1),
		Y:           clampF(tr.Y, -1, 1),
		Scale:       clampF(orDefaultPositive(tr.Scale, 1), 0.01, 10),
		RotationDeg: clampF(tr.RotationDeg, -360, 360),
	}
}

func sanitizeCrop(cr *StudioCrop) *StudioCrop {
	if cr == nil {
		return nil
	}
	l := clampF(cr.Left, 0, 0.99)
	t := clampF(cr.Top, 0, 0.99)
	r := clampF(cr.Right, 0, 0.99)
	b := clampF(cr.Bottom, 0, 0.99)
	// Drop a no-op or impossible crop so it emits no `crop=` filter.
	if (l == 0 && t == 0 && r == 0 && b == 0) || l+r >= 1 || t+b >= 1 {
		return nil
	}
	return &StudioCrop{Left: l, Top: t, Right: r, Bottom: b}
}

func sanitizeBlendMode(mode string) string {
	switch mode {
	case StudioBlendMultiply, StudioBlendScreen, StudioBlendOverlay, StudioBlendLighten,
		StudioBlendDarken, StudioBlendAddition, StudioBlendDifference:
		return mode
	default:
		// normal / unknown → default source-over, stored as empty (omitempty).
		return ""
	}
}

func sanitizeEffects(effects []StudioEffect) []StudioEffect {
	if len(effects) == 0 {
		return nil
	}
	out := make([]StudioEffect, 0, len(effects))
	for _, e := range effects {
		switch e.Type {
		case StudioEffectLumetri:
			out = append(out, StudioEffect{
				ID: e.ID, Type: e.Type, Enabled: e.Enabled,
				Exposure:    ptrF(clampOpt(e.Exposure, 0, -3, 3)),
				Contrast:    ptrF(clampOpt(e.Contrast, 1, 0, 2)),
				Saturation:  ptrF(clampOpt(e.Saturation, 1, 0, 2)),
				Temperature: ptrF(clampOpt(e.Temperature, 0, -100, 100)),
				Tint:        ptrF(clampOpt(e.Tint, 0, -100, 100)),
				Vibrance:    ptrF(clampOpt(e.Vibrance, 0, -2, 2)),
			})
		case StudioEffectLUT:
			id := ""
			if e.LutAssetID != nil {
				id = strings.TrimSpace(*e.LutAssetID)
			}
			if id == "" {
				continue // no LUT to apply — malformed, drop
			}
			out = append(out, StudioEffect{
				ID: e.ID, Type: e.Type, Enabled: e.Enabled,
				LutAssetID: &id,
				Intensity:  ptrF(clampOpt(e.Intensity, 1, 0, 1)),
			})
		case StudioEffectChromaKey:
			key := "#00FF00"
			if e.KeyColor != nil && isHexColor(*e.KeyColor) {
				key = normalizeHexColor(*e.KeyColor)
			}
			out = append(out, StudioEffect{
				ID: e.ID, Type: e.Type, Enabled: e.Enabled,
				KeyColor:   &key,
				Similarity: ptrF(clampOpt(e.Similarity, 0.1, 0.01, 1)),
				Blend:      ptrF(clampOpt(e.Blend, 0.1, 0, 1)),
				Despill:    ptrF(clampOpt(e.Despill, 0.5, 0, 1)),
			})
		default:
			continue // unknown effect type — drop
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeKeyframes(kfs []StudioVolumeKeyframe) []StudioVolumeKeyframe {
	if len(kfs) == 0 {
		return nil
	}
	cleaned := make([]StudioVolumeKeyframe, 0, len(kfs))
	for _, k := range kfs {
		t := k.T
		if t < 0 {
			t = 0
		}
		cleaned = append(cleaned, StudioVolumeKeyframe{T: t, Gain: clampF(k.Gain, 0, studioMaxVolume)})
	}
	sort.SliceStable(cleaned, func(i, j int) bool { return cleaned[i].T < cleaned[j].T })
	deduped := make([]StudioVolumeKeyframe, 0, len(cleaned))
	for _, k := range cleaned {
		if n := len(deduped); n > 0 && math.Abs(deduped[n-1].T-k.T) < 1e-4 {
			deduped[n-1] = k // same time → last wins
			continue
		}
		deduped = append(deduped, k)
	}
	if len(deduped) > studioMaxKeyframes {
		deduped = deduped[:studioMaxKeyframes]
	}
	return deduped
}

// SanitizeCaptions clamps + sorts caption cues. Returns a non-nil slice so the
// JSON response serializes `[]` rather than null (the client expects an array).
func SanitizeCaptions(cues []StudioCaptionCue) []StudioCaptionCue {
	out := make([]StudioCaptionCue, 0, len(cues))
	for _, c := range cues {
		if c.StartSeconds < 0 {
			c.StartSeconds = 0
		}
		if c.EndSeconds < c.StartSeconds {
			c.EndSeconds = c.StartSeconds
		}
		c.Text = truncateRunes(c.Text, studioMaxCaptionLen)
		out = append(out, c)
		if len(out) >= studioMaxCaptionCues {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartSeconds < out[j].StartSeconds })
	return out
}

// SanitizeCaptionStyle clamps caption appearance, defaulting bad values.
func SanitizeCaptionStyle(st *StudioCaptionStyle) *StudioCaptionStyle {
	if st == nil {
		return nil
	}
	pos := st.Position
	if pos != "top" {
		pos = "bottom"
	}
	color := "#FFFFFF"
	if isHexColor(st.Color) {
		color = normalizeHexColor(st.Color)
	}
	bg := "#000000"
	if isHexColor(st.BackgroundColor) {
		bg = normalizeHexColor(st.BackgroundColor)
	}
	return &StudioCaptionStyle{
		FontSizePct:       clampF(orDefaultPositive(st.FontSizePct, 4.5), 1, 15),
		Color:             color,
		BackgroundColor:   bg,
		BackgroundOpacity: clampF(st.BackgroundOpacity, 0, 1),
		Position:          pos,
		MaxWidthPct:       clampF(orDefaultPositive(st.MaxWidthPct, 90), 40, 100),
	}
}

// SanitizeAudioConfig clamps the ducking config to safe ranges.
func SanitizeAudioConfig(a *StudioAudioConfig) *StudioAudioConfig {
	if a == nil {
		return nil
	}
	return &StudioAudioConfig{
		DuckingEnabled:   a.DuckingEnabled,
		DuckVoiceTrackID: a.DuckVoiceTrackID,
		DuckAmountDb:     clampF(a.DuckAmountDb, 0, 24),
		DuckAttackMs:     clampF(a.DuckAttackMs, 0, 2000),
		DuckReleaseMs:    clampF(a.DuckReleaseMs, 0, 5000),
	}
}
