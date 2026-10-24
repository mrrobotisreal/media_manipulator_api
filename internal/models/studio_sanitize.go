package models

import (
	"math"
	"sort"
	"strings"
)

// EDL v2/v3 sanitation. Ranges mirror the Zod schema in lib/studioTypes.ts and
// STUDIO_V3_RANGES in lib/studio/effectRegistry.ts. SanitizeTracks /
// SanitizeCaptions / SanitizeCaptionStyle / SanitizeAudioConfig /
// SanitizeMarkers run on save AND before export so the export compiler can
// trust the plan without re-validating client input.
//
// Invariant: a valid v1/v2 clip (in-range values) sanitizes byte-identically,
// so existing projects export exactly as before — with ONE documented
// exception: a legacy `transitionInSeconds > 0` grows a typed
// `transition: {type: "crossDissolve"}` mirror (idempotently; the legacy field
// is kept and stays authoritative for the v2 export path until part 14).

const (
	studioMaxVolume         = 2.0
	studioMaxKeyframes      = 64 // per property — v3 lanes reuse the volumeKeyframes cap
	studioMaxTextOverlayLen = 200
	studioMaxCaptionLen     = 500
	studioMaxCaptionCues    = 2000

	// EDL v3 caps/ranges — mirror STUDIO_V3_RANGES in effectRegistry.ts.
	studioTransitionMinSeconds = 0.1
	studioTransitionMaxSeconds = 5.0
	studioSpeedMin             = 0.1
	studioSpeedMax             = 10.0
	studioTrackNameMaxLen      = 80
	studioEQGainRangeDb        = 12.0
	studioMaxTitleLayers       = 8
	studioMaxTitleTextLen      = 500
	studioMaxMarkers           = 500
	studioMarkerNameMaxLen     = 80
	studioMarkerCommentMaxLen  = 500
	studioDefaultFontFamily    = "Inter"
	studioMarkerDefaultColor   = "#22C55E"
)

// studioTransitionTypes mirrors studioTransitionTypeSchema.
var studioTransitionTypes = map[string]bool{
	StudioTransitionCrossDissolve: true,
	StudioTransitionDipToBlack:    true,
	StudioTransitionDipToWhite:    true,
	StudioTransitionWipeLeft:      true,
	StudioTransitionWipeRight:     true,
	StudioTransitionWipeUp:        true,
	StudioTransitionWipeDown:      true,
	StudioTransitionPushLeft:      true,
	StudioTransitionPushRight:     true,
	StudioTransitionPushUp:        true,
	StudioTransitionPushDown:      true,
	StudioTransitionSlideLeft:     true,
	StudioTransitionSlideRight:    true,
	StudioTransitionSlideUp:       true,
	StudioTransitionSlideDown:     true,
}

// studioKeyframeEases mirrors studioKeyframeEaseSchema.
var studioKeyframeEases = map[string]bool{
	"linear": true, "hold": true, "easeIn": true, "easeOut": true, "easeBoth": true,
}

// studioEffectParamRanges is the per-param clamp table for keyframed effect
// params ("<effectId>.<param>" keys). Mirrors the v2 effect ranges above /
// LUMETRI_PARAMS + LUT_PARAMS + CHROMAKEY_PARAMS in effectRegistry.ts.
var studioEffectParamRanges = map[string][2]float64{
	// lumetri
	"exposure": {-3, 3}, "contrast": {0, 2}, "saturation": {0, 2},
	"temperature": {-100, 100}, "tint": {-100, 100}, "vibrance": {-2, 2},
	// lut
	"intensity": {0, 1},
	// chromakey
	"similarity": {0.01, 1}, "blend": {0, 1}, "despill": {0, 1},
}

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
		// EDL v3 track-level fields.
		t.Name = truncateRunes(t.Name, studioTrackNameMaxLen)
		if t.Gain != nil {
			t.Gain = ptrF(clampF(*t.Gain, 0, studioMaxVolume))
		}
		if t.Pan != nil {
			t.Pan = ptrF(clampF(*t.Pan, -1, 1))
		}
		t.Effects = sanitizeTrackEffects(t.Effects)
		out = append(out, t)
	}
	return out
}

// SanitizeTracksForAssets is the asset-aware companion to SanitizeTracks
// (which has no asset knowledge). A still image has no time axis to remap, so
// clips whose asset is an image lose speed/reverse/freeze. Idempotent; returns
// a copy and never mutates the input. Unknown assets are left untouched. The
// hold length is unbounded by design: only sourceIn >= 0 and
// sourceOut >= sourceIn are enforced (SanitizeTracks).
func SanitizeTracksForAssets(tracks []StudioTrack, assetKinds map[string]StudioMediaKind) []StudioTrack {
	out := make([]StudioTrack, len(tracks))
	for i, t := range tracks {
		clips := make([]StudioClip, len(t.Clips))
		for j, c := range t.Clips {
			if assetKinds[c.AssetID] == StudioMediaKindImage && c.AssetID != "" {
				c.Speed = nil
				c.Reverse = false
				c.Freeze = nil
			}
			clips[j] = c
		}
		t.Clips = clips
		out[i] = t
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

	// --- EDL v3 ---
	c.Transition = sanitizeTransition(c.Transition)
	// Upgrade legacy transitionInSeconds > 0 → typed crossDissolve (idempotent:
	// once Transition exists the branch no longer fires).
	if c.Transition == nil && c.TransitionInSeconds != nil && *c.TransitionInSeconds > 0 {
		c.Transition = &StudioTransition{
			Type:            StudioTransitionCrossDissolve,
			DurationSeconds: clampF(*c.TransitionInSeconds, studioTransitionMinSeconds, studioTransitionMaxSeconds),
		}
	}
	// Keep writing the legacy field alongside the typed one (dropped in a
	// future contract) so pre-v3 readers still see the overlap duration.
	if c.Transition != nil {
		c.TransitionInSeconds = ptrF(c.Transition.DurationSeconds)
	}

	c.Keyframes = sanitizeClipKeyframes(c.Keyframes)

	// AssetID XOR Title: a clip that references media keeps its media — a title
	// on an asset-backed clip is malformed input and is dropped.
	c.Title = sanitizeTitle(c.Title)
	if c.Title != nil && strings.TrimSpace(c.AssetID) != "" {
		c.Title = nil
	}

	// Title clips are generated, not sourced: no rate, direction or held frame.
	if c.Title != nil {
		c.Speed = nil
		c.Reverse = false
		c.Freeze = nil
	}

	// Freeze is mutually exclusive with speed/reverse; freeze wins.
	if c.Freeze != nil {
		if c.Freeze.SourceTime < 0 {
			c.Freeze = &StudioFreeze{SourceTime: 0}
		}
		c.Speed = nil
		c.Reverse = false
	} else if c.Speed != nil {
		c.Speed = ptrF(clampF(*c.Speed, studioSpeedMin, studioSpeedMax))
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

// --- EDL v3 helpers --------------------------------------------------------

func sanitizeTransition(tr *StudioTransition) *StudioTransition {
	if tr == nil {
		return nil
	}
	if !studioTransitionTypes[tr.Type] {
		return nil // unknown type — drop (the legacy-upgrade branch may refill)
	}
	out := &StudioTransition{
		Type:            tr.Type,
		DurationSeconds: clampF(tr.DurationSeconds, studioTransitionMinSeconds, studioTransitionMaxSeconds),
	}
	if len(tr.Params) > 0 {
		out.Params = tr.Params // per-type ranges land with part 14; inert pass-through
	}
	return out
}

// sanitizeKeyframeLane is the v3 generalization of sanitizeKeyframes: clamp
// values to the property's range, sort by T, dedupe (last wins), cap at 64.
func sanitizeKeyframeLane(kfs []StudioKeyframe, lo, hi float64) []StudioKeyframe {
	if len(kfs) == 0 {
		return nil
	}
	cleaned := make([]StudioKeyframe, 0, len(kfs))
	for _, k := range kfs {
		if k.T < 0 {
			k.T = 0
		}
		if !studioKeyframeEases[k.Ease] {
			k.Ease = "linear"
		}
		cleaned = append(cleaned, StudioKeyframe{T: k.T, Value: clampF(k.Value, lo, hi), Ease: k.Ease})
	}
	sort.SliceStable(cleaned, func(i, j int) bool { return cleaned[i].T < cleaned[j].T })
	deduped := make([]StudioKeyframe, 0, len(cleaned))
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

func sanitizeClipKeyframes(k *StudioClipKeyframes) *StudioClipKeyframes {
	if k == nil {
		return nil
	}
	// Value bounds reuse each property's own range (STUDIO_V3_RANGES.keyframeValue).
	out := &StudioClipKeyframes{
		PositionX: sanitizeKeyframeLane(k.PositionX, -1, 1),
		PositionY: sanitizeKeyframeLane(k.PositionY, -1, 1),
		Scale:     sanitizeKeyframeLane(k.Scale, 0.01, 10),
		Rotation:  sanitizeKeyframeLane(k.Rotation, -360, 360),
		Opacity:   sanitizeKeyframeLane(k.Opacity, 0, 1),
		Volume:    sanitizeKeyframeLane(k.Volume, 0, studioMaxVolume),
		Pan:       sanitizeKeyframeLane(k.Pan, -1, 1),
	}
	if len(k.Effects) > 0 {
		eff := make(map[string][]StudioKeyframe, len(k.Effects))
		for key, lane := range k.Effects {
			// Keys are "<effectId>.<param>"; the param name picks the clamp range.
			dot := strings.LastIndex(key, ".")
			if dot <= 0 || dot == len(key)-1 {
				continue // malformed key — drop
			}
			r, ok := studioEffectParamRanges[key[dot+1:]]
			if !ok {
				continue // unknown param — drop
			}
			if cleaned := sanitizeKeyframeLane(lane, r[0], r[1]); cleaned != nil {
				eff[key] = cleaned
			}
		}
		if len(eff) > 0 {
			out.Effects = eff
		}
	}
	if out.PositionX == nil && out.PositionY == nil && out.Scale == nil && out.Rotation == nil &&
		out.Opacity == nil && out.Volume == nil && out.Pan == nil && out.Effects == nil {
		return nil // all lanes empty — drop the container
	}
	return out
}

func sanitizeTitle(title *StudioTitle) *StudioTitle {
	if title == nil {
		return nil
	}
	layers := make([]StudioTitleLayer, 0, len(title.Layers))
	for _, l := range title.Layers {
		if len(layers) >= studioMaxTitleLayers {
			break
		}
		switch l.Type {
		case StudioTitleLayerText:
			layers = append(layers, sanitizeTitleTextLayer(l))
		case StudioTitleLayerRect:
			layers = append(layers, sanitizeTitleRectLayer(l))
		case StudioTitleLayerLine:
			layers = append(layers, sanitizeTitleLineLayer(l))
		default:
			continue // unknown layer type — drop
		}
	}
	if len(layers) == 0 {
		return nil
	}
	return &StudioTitle{Layers: layers}
}

// sanitizeTitleTextLayer fills every field the Zod text-layer schema requires
// (so the serialized JSON always parses client-side) and clamps the rest.
func sanitizeTitleTextLayer(l StudioTitleLayer) StudioTitleLayer {
	text := ""
	if l.Text != nil {
		text = truncateRunes(*l.Text, studioMaxTitleTextLen)
	}
	// Part 16: fontFamily is an ENUM against the shipped registry — an unknown
	// name silently becomes the default so no user text can ever influence the
	// export's font-file resolution.
	font := studioDefaultFontFamily
	if l.FontFamily != nil && IsKnownStudioTitleFont(strings.TrimSpace(*l.FontFamily)) {
		font = strings.TrimSpace(*l.FontFamily)
	}
	weight := 400
	if l.FontWeight != nil && *l.FontWeight == 700 && StudioTitleFontSupportsBold(font) {
		weight = 700
	}
	fill := "#FFFFFF"
	if l.FillColor != nil && isHexColor(*l.FillColor) {
		fill = normalizeHexColor(*l.FillColor)
	}
	align := "center"
	if l.Align != nil && (*l.Align == "left" || *l.Align == "right") {
		align = *l.Align
	}
	out := StudioTitleLayer{
		Type:       StudioTitleLayerText,
		Text:       &text,
		FontFamily: &font,
		FontSizePx: ptrF(clampOpt(l.FontSizePx, 64, 8, 500)),
		FontWeight: &weight,
		FillColor:  &fill,
		Align:      &align,
		X:          ptrF(clampOpt(l.X, 0.5, 0, 1)),
		Y:          ptrF(clampOpt(l.Y, 0.5, 0, 1)),
	}
	if l.StrokeColor != nil && isHexColor(*l.StrokeColor) {
		sc := normalizeHexColor(*l.StrokeColor)
		out.StrokeColor = &sc
		out.StrokeWidthPx = ptrF(clampOpt(l.StrokeWidthPx, 0, 0, 50))
	}
	if l.Shadow != nil {
		color := "#000000"
		if isHexColor(l.Shadow.Color) {
			color = normalizeHexColor(l.Shadow.Color)
		}
		out.Shadow = &StudioTitleShadow{
			XPx:    clampF(l.Shadow.XPx, -100, 100),
			YPx:    clampF(l.Shadow.YPx, -100, 100),
			BlurPx: clampF(l.Shadow.BlurPx, 0, 100),
			Color:  color,
		}
	}
	if l.Background != nil {
		color := "#000000"
		if isHexColor(l.Background.Color) {
			color = normalizeHexColor(l.Background.Color)
		}
		out.Background = &StudioTitleBackground{
			Color:      color,
			Opacity:    clampF(l.Background.Opacity, 0, 1),
			PaddingXPx: clampF(l.Background.PaddingXPx, 0, 200),
			PaddingYPx: clampF(l.Background.PaddingYPx, 0, 200),
			RadiusPx:   clampF(l.Background.RadiusPx, 0, 200),
		}
	}
	if l.LetterSpacingPx != nil {
		out.LetterSpacingPx = ptrF(clampF(*l.LetterSpacingPx, -20, 100))
	}
	if l.LineHeight != nil {
		out.LineHeight = ptrF(clampF(*l.LineHeight, 0.5, 3))
	}
	return out
}

func sanitizeTitleRectLayer(l StudioTitleLayer) StudioTitleLayer {
	color := "#FFFFFF"
	if l.Color != nil && isHexColor(*l.Color) {
		color = normalizeHexColor(*l.Color)
	}
	out := StudioTitleLayer{
		Type:  StudioTitleLayerRect,
		X:     ptrF(clampOpt(l.X, 0, 0, 1)),
		Y:     ptrF(clampOpt(l.Y, 0, 0, 1)),
		W:     ptrF(clampOpt(l.W, 0, 0, 1)),
		H:     ptrF(clampOpt(l.H, 0, 0, 1)),
		Color: &color,
	}
	if l.Opacity != nil {
		out.Opacity = ptrF(clampF(*l.Opacity, 0, 1))
	}
	if l.RadiusPx != nil {
		out.RadiusPx = ptrF(clampF(*l.RadiusPx, 0, 200))
	}
	return out
}

func sanitizeTitleLineLayer(l StudioTitleLayer) StudioTitleLayer {
	color := "#FFFFFF"
	if l.Color != nil && isHexColor(*l.Color) {
		color = normalizeHexColor(*l.Color)
	}
	return StudioTitleLayer{
		Type:        StudioTitleLayerLine,
		X1:          ptrF(clampOpt(l.X1, 0, 0, 1)),
		Y1:          ptrF(clampOpt(l.Y1, 0, 0, 1)),
		X2:          ptrF(clampOpt(l.X2, 1, 0, 1)),
		Y2:          ptrF(clampOpt(l.Y2, 1, 0, 1)),
		Color:       &color,
		ThicknessPx: ptrF(clampOpt(l.ThicknessPx, 4, 1, 100)),
	}
}

func sanitizeTrackEffects(fx *StudioTrackEffects) *StudioTrackEffects {
	if fx == nil {
		return nil
	}
	out := &StudioTrackEffects{}
	if fx.EQ != nil {
		out.EQ = &StudioTrackEQ{
			LowGainDb:  clampF(fx.EQ.LowGainDb, -studioEQGainRangeDb, studioEQGainRangeDb),
			MidGainDb:  clampF(fx.EQ.MidGainDb, -studioEQGainRangeDb, studioEQGainRangeDb),
			HighGainDb: clampF(fx.EQ.HighGainDb, -studioEQGainRangeDb, studioEQGainRangeDb),
		}
	}
	if fx.Compressor != nil {
		out.Compressor = &StudioTrackCompressor{
			ThresholdDb: clampF(fx.Compressor.ThresholdDb, -60, 0),
			Ratio:       clampF(fx.Compressor.Ratio, 1, 20),
			AttackMs:    clampF(fx.Compressor.AttackMs, 1, 200),
			ReleaseMs:   clampF(fx.Compressor.ReleaseMs, 20, 1000),
			MakeupDb:    clampF(fx.Compressor.MakeupDb, 0, 12),
		}
	}
	if out.EQ == nil && out.Compressor == nil {
		return nil
	}
	return out
}

// SanitizeMarkers clamps, sorts, and caps the project marker list.
func SanitizeMarkers(ms []StudioMarker) []StudioMarker {
	if len(ms) == 0 {
		return nil
	}
	out := make([]StudioMarker, 0, len(ms))
	for _, m := range ms {
		if m.T < 0 {
			m.T = 0
		}
		m.Name = truncateRunes(m.Name, studioMarkerNameMaxLen)
		m.Comment = truncateRunes(m.Comment, studioMarkerCommentMaxLen)
		if isHexColor(m.Color) {
			m.Color = normalizeHexColor(m.Color)
		} else {
			m.Color = studioMarkerDefaultColor
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	if len(out) > studioMaxMarkers {
		out = out[:studioMaxMarkers]
	}
	return out
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
	var master *float64
	if a.MasterGain != nil {
		master = ptrF(clampF(*a.MasterGain, 0, studioMaxVolume))
	}
	return &StudioAudioConfig{
		MasterGain:       master,
		DuckingEnabled:   a.DuckingEnabled,
		DuckVoiceTrackID: a.DuckVoiceTrackID,
		DuckAmountDb:     clampF(a.DuckAmountDb, 0, 24),
		DuckAttackMs:     clampF(a.DuckAttackMs, 0, 2000),
		DuckReleaseMs:    clampF(a.DuckReleaseMs, 0, 5000),
	}
}
