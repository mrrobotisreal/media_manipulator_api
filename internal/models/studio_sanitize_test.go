package models

import (
	"encoding/json"
	"testing"
)

func f64(v float64) *float64 { return &v }
func strp(v string) *string  { return &v }
func intp(v int) *int        { return &v }

// v1Fixture is the valid-v1 track tree shared by the no-op guards. Fresh copy
// per call so tests can mutate their own instance.
func v1Fixture() []StudioTrack {
	return []StudioTrack{
		{
			ID: "v1", Kind: StudioTrackKindVideo, Index: 0, Muted: false,
			Clips: []StudioClip{
				{
					ID: "c1", AssetID: "a1", StreamIndex: 0,
					TimelineStart: 0, SourceIn: 0, SourceOut: 5,
					Opacity:             f64(1),
					TransitionInSeconds: f64(0.5),
					Adjustments:         &StudioAdjustments{Brightness: 0.1, Contrast: 1.2, Saturation: 0.8},
					TextOverlays:        []StudioTextOverlay{{ID: "o1", Text: "Reykjavík", X: 0.05, Y: 0.9, FontSize: 48, Color: "#FFCC00"}},
				},
			},
		},
		{
			ID: "a1", Kind: StudioTrackKindAudio, Index: 0, Muted: false,
			Clips: []StudioClip{
				{ID: "c2", AssetID: "music", StreamIndex: 0, TimelineStart: 0, SourceIn: 0, SourceOut: 10, Volume: f64(0.3)},
			},
		},
	}
}

// TestSanitizeTracks_V1NoOp is the backward-compat guard: a valid v1 clip
// (in-range values) must sanitize byte-identically — with the ONE documented
// v3 exception that a legacy transitionInSeconds > 0 grows a typed
// crossDissolve mirror (the legacy field itself is untouched).
func TestSanitizeTracks_V1NoOp(t *testing.T) {
	want := v1Fixture()
	want[0].Clips[0].Transition = &StudioTransition{Type: StudioTransitionCrossDissolve, DurationSeconds: 0.5}

	wantJSON, _ := json.Marshal(want)
	got, _ := json.Marshal(SanitizeTracks(v1Fixture()))
	if string(wantJSON) != string(got) {
		t.Errorf("v1 clip changed beyond the documented transition upgrade:\nwant: %s\ngot:  %s", wantJSON, got)
	}
}

// TestSanitizeTracks_V2NoOp: a valid v2 clip (every v2 field populated with
// in-range, already-sanitized values, no legacy transition) is byte-identical.
func TestSanitizeTracks_V2NoOp(t *testing.T) {
	v2 := []StudioTrack{
		{
			ID: "v1", Kind: StudioTrackKindVideo, Index: 0, Muted: false,
			Clips: []StudioClip{
				{
					ID: "c1", AssetID: "a1", StreamIndex: 0,
					TimelineStart: 0, SourceIn: 0, SourceOut: 5,
					Opacity:   f64(0.9),
					Transform: &StudioTransform{X: 0.1, Y: -0.1, Scale: 1.5, RotationDeg: 45},
					Crop:      &StudioCrop{Left: 0.1, Top: 0.1, Right: 0.2, Bottom: 0.2},
					BlendMode: StudioBlendMultiply,
					Effects: []StudioEffect{{
						ID: "e1", Type: StudioEffectLumetri, Enabled: true,
						Exposure: f64(0.5), Contrast: f64(1.1), Saturation: f64(0.9),
						Temperature: f64(10), Tint: f64(-5), Vibrance: f64(0.2),
					}},
					VolumeKeyframes: []StudioVolumeKeyframe{{T: 0, Gain: 1}, {T: 2, Gain: 0.5}},
					Pan:             f64(-0.5),
				},
			},
		},
	}

	before, _ := json.Marshal(v2)
	after, _ := json.Marshal(SanitizeTracks(v2))
	if string(before) != string(after) {
		t.Errorf("v2 clip not byte-identical after sanitize:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestSanitizeTracks_TransitionUpgrade covers the legacy → typed transition
// path: upgrade, idempotence, clamping, and the legacy mirror.
func TestSanitizeTracks_TransitionUpgrade(t *testing.T) {
	mk := func(c StudioClip) []StudioTrack {
		return []StudioTrack{{ID: "v1", Kind: StudioTrackKindVideo, Clips: []StudioClip{c}}}
	}

	t.Run("legacy upgrades to crossDissolve and is kept", func(t *testing.T) {
		out := SanitizeTracks(mk(StudioClip{ID: "c", AssetID: "a", SourceOut: 5, TransitionInSeconds: f64(0.5)}))
		c := out[0].Clips[0]
		if c.Transition == nil || c.Transition.Type != StudioTransitionCrossDissolve || c.Transition.DurationSeconds != 0.5 {
			t.Fatalf("upgrade: got %+v", c.Transition)
		}
		if c.TransitionInSeconds == nil || *c.TransitionInSeconds != 0.5 {
			t.Errorf("legacy field must be kept: got %v", c.TransitionInSeconds)
		}
	})

	t.Run("idempotent: second sanitize is byte-identical", func(t *testing.T) {
		once := SanitizeTracks(mk(StudioClip{ID: "c", AssetID: "a", SourceOut: 5, TransitionInSeconds: f64(0.5)}))
		twice := SanitizeTracks(once)
		a, _ := json.Marshal(once)
		b, _ := json.Marshal(twice)
		if string(a) != string(b) {
			t.Errorf("not idempotent:\nonce:  %s\ntwice: %s", a, b)
		}
	})

	t.Run("legacy duration clamps into 0.1..5 on upgrade", func(t *testing.T) {
		out := SanitizeTracks(mk(StudioClip{ID: "c", AssetID: "a", SourceOut: 5, TransitionInSeconds: f64(9)}))
		c := out[0].Clips[0]
		if c.Transition.DurationSeconds != 5 {
			t.Errorf("duration clamp: got %v want 5", c.Transition.DurationSeconds)
		}
		if *c.TransitionInSeconds != 5 {
			t.Errorf("legacy mirror after clamp: got %v want 5", *c.TransitionInSeconds)
		}
	})

	t.Run("typed transition wins and mirrors into the legacy field", func(t *testing.T) {
		out := SanitizeTracks(mk(StudioClip{
			ID: "c", AssetID: "a", SourceOut: 5,
			TransitionInSeconds: f64(0.5),
			Transition:          &StudioTransition{Type: StudioTransitionWipeLeft, DurationSeconds: 2},
		}))
		c := out[0].Clips[0]
		if c.Transition.Type != StudioTransitionWipeLeft || c.Transition.DurationSeconds != 2 {
			t.Fatalf("typed transition must win: %+v", c.Transition)
		}
		if *c.TransitionInSeconds != 2 {
			t.Errorf("legacy mirror: got %v want 2", *c.TransitionInSeconds)
		}
	})

	t.Run("unknown type is dropped, then legacy refills", func(t *testing.T) {
		out := SanitizeTracks(mk(StudioClip{
			ID: "c", AssetID: "a", SourceOut: 5,
			TransitionInSeconds: f64(1),
			Transition:          &StudioTransition{Type: "spiral", DurationSeconds: 2},
		}))
		c := out[0].Clips[0]
		if c.Transition == nil || c.Transition.Type != StudioTransitionCrossDissolve || c.Transition.DurationSeconds != 1 {
			t.Errorf("unknown type should fall back to the legacy upgrade: %+v", c.Transition)
		}
	})

	t.Run("zero legacy stays untouched with no transition", func(t *testing.T) {
		out := SanitizeTracks(mk(StudioClip{ID: "c", AssetID: "a", SourceOut: 5, TransitionInSeconds: f64(0)}))
		c := out[0].Clips[0]
		if c.Transition != nil {
			t.Errorf("no upgrade expected for 0: %+v", c.Transition)
		}
		if *c.TransitionInSeconds != 0 {
			t.Errorf("legacy 0 must survive: %v", *c.TransitionInSeconds)
		}
	})
}

func TestSanitizeTracks_ClampsAndDrops(t *testing.T) {
	tracks := []StudioTrack{
		{
			ID: "v1", Kind: StudioTrackKindVideo, Index: 0,
			Clips: []StudioClip{
				{
					ID: "c1", AssetID: "a1", SourceIn: 0, SourceOut: 5,
					Volume:    f64(3.0), // > 2 → 2
					Opacity:   f64(1.5), // > 1 → 1
					Pan:       f64(2.0), // > 1 → 1
					BlendMode: "bogus",  // unknown → ""
					Transform: &StudioTransform{X: 2, Y: -2, Scale: 0, RotationDeg: 720},
					Crop:      &StudioCrop{Left: 0.6, Right: 0.6}, // l+r >= 1 → dropped
					Effects: []StudioEffect{
						{ID: "e1", Type: StudioEffectLumetri, Enabled: true, Exposure: f64(5)}, // 5 → 3
						{ID: "e2", Type: StudioEffectLUT, Enabled: true},                       // no assetId → dropped
						{ID: "e3", Type: "bogus"},                                              // unknown → dropped
						{ID: "e4", Type: StudioEffectChromaKey, Enabled: true, KeyColor: strp("nope")},
					},
					VolumeKeyframes: []StudioVolumeKeyframe{
						{T: 5, Gain: 3}, // out of order + gain clamp
						{T: 1, Gain: 0.5},
						{T: 1, Gain: 0.9}, // dup time → last wins
					},
				},
			},
		},
	}

	out := SanitizeTracks(tracks)
	c := out[0].Clips[0]

	if *c.Volume != 2 {
		t.Errorf("volume clamp: got %v want 2", *c.Volume)
	}
	if *c.Opacity != 1 {
		t.Errorf("opacity clamp: got %v want 1", *c.Opacity)
	}
	if *c.Pan != 1 {
		t.Errorf("pan clamp: got %v want 1", *c.Pan)
	}
	if c.BlendMode != "" {
		t.Errorf("blend mode: got %q want empty", c.BlendMode)
	}
	if c.Transform.X != 1 || c.Transform.Y != -1 || c.Transform.Scale != 1 || c.Transform.RotationDeg != 360 {
		t.Errorf("transform clamp: got %+v", *c.Transform)
	}
	if c.Crop != nil {
		t.Errorf("impossible crop should be dropped, got %+v", *c.Crop)
	}
	if len(c.Effects) != 2 {
		t.Fatalf("effects: got %d want 2 (lumetri+chromakey), %+v", len(c.Effects), c.Effects)
	}
	if c.Effects[0].Type != StudioEffectLumetri || *c.Effects[0].Exposure != 3 {
		t.Errorf("lumetri exposure clamp: %+v", c.Effects[0])
	}
	if c.Effects[1].Type != StudioEffectChromaKey || *c.Effects[1].KeyColor != "#00FF00" {
		t.Errorf("chromakey bad color → default #00FF00: %+v", c.Effects[1])
	}
	if len(c.VolumeKeyframes) != 2 {
		t.Fatalf("keyframes dedupe: got %d want 2: %+v", len(c.VolumeKeyframes), c.VolumeKeyframes)
	}
	if c.VolumeKeyframes[0].T != 1 || c.VolumeKeyframes[0].Gain != 0.9 {
		t.Errorf("keyframe sort/dedupe: got %+v want {1,0.9}", c.VolumeKeyframes[0])
	}
	if c.VolumeKeyframes[1].T != 5 || c.VolumeKeyframes[1].Gain != 2 {
		t.Errorf("keyframe gain clamp: got %+v want {5,2}", c.VolumeKeyframes[1])
	}
}

func TestSanitizeTracks_ZeroCropDropped(t *testing.T) {
	tracks := []StudioTrack{{ID: "v1", Kind: StudioTrackKindVideo, Clips: []StudioClip{
		{ID: "c1", AssetID: "a1", SourceOut: 1, Crop: &StudioCrop{}},
	}}}
	if c := SanitizeTracks(tracks)[0].Clips[0]; c.Crop != nil {
		t.Errorf("all-zero crop should be dropped, got %+v", *c.Crop)
	}
}

func TestSanitizeKeyframes_Cap(t *testing.T) {
	kfs := make([]StudioVolumeKeyframe, 0, 100)
	for i := 0; i < 100; i++ {
		kfs = append(kfs, StudioVolumeKeyframe{T: float64(i), Gain: 1})
	}
	if got := len(sanitizeKeyframes(kfs)); got != studioMaxKeyframes {
		t.Errorf("keyframe cap: got %d want %d", got, studioMaxKeyframes)
	}
}

func TestSanitizeCaptions(t *testing.T) {
	cues := []StudioCaptionCue{
		{ID: "b", StartSeconds: 5, EndSeconds: 2}, // end < start → end := start
		{ID: "a", StartSeconds: 1, EndSeconds: 3},
	}
	out := SanitizeCaptions(cues)
	if len(out) != 2 || out[0].ID != "a" {
		t.Fatalf("captions should sort by start: %+v", out)
	}
	if out[1].EndSeconds < out[1].StartSeconds {
		t.Errorf("end must be >= start: %+v", out[1])
	}
	// Empty input → non-nil slice (so JSON serializes []).
	if SanitizeCaptions(nil) == nil {
		t.Errorf("SanitizeCaptions(nil) must return a non-nil slice")
	}
}

func TestSanitizeCaptionStyle(t *testing.T) {
	st := SanitizeCaptionStyle(&StudioCaptionStyle{
		FontSizePct: 99, Color: "bad", BackgroundColor: "#abcdef",
		BackgroundOpacity: 2, Position: "sideways", MaxWidthPct: 10,
	})
	if st.FontSizePct != 15 {
		t.Errorf("fontSizePct clamp: got %v want 15", st.FontSizePct)
	}
	if st.Color != "#FFFFFF" {
		t.Errorf("bad color → default #FFFFFF: got %q", st.Color)
	}
	if st.BackgroundColor != "#ABCDEF" {
		t.Errorf("hex normalized uppercase: got %q", st.BackgroundColor)
	}
	if st.BackgroundOpacity != 1 {
		t.Errorf("opacity clamp: got %v want 1", st.BackgroundOpacity)
	}
	if st.Position != "bottom" {
		t.Errorf("bad position → bottom: got %q", st.Position)
	}
	if st.MaxWidthPct != 40 {
		t.Errorf("maxWidthPct clamp: got %v want 40", st.MaxWidthPct)
	}
}

func TestSanitizeAudioConfig(t *testing.T) {
	a := SanitizeAudioConfig(&StudioAudioConfig{DuckAmountDb: 99, DuckAttackMs: 9999, DuckReleaseMs: 99999})
	if a.DuckAmountDb != 24 || a.DuckAttackMs != 2000 || a.DuckReleaseMs != 5000 {
		t.Errorf("ducking clamps: got %+v", *a)
	}
}

// --- EDL v3 -----------------------------------------------------------------

func TestSanitizeClipKeyframes(t *testing.T) {
	kf := func(ts ...float64) []StudioKeyframe {
		out := make([]StudioKeyframe, 0, len(ts))
		for _, tt := range ts {
			out = append(out, StudioKeyframe{T: tt, Value: 0.5, Ease: "linear"})
		}
		return out
	}

	t.Run("sorts, dedupes, clamps value and ease", func(t *testing.T) {
		got := sanitizeClipKeyframes(&StudioClipKeyframes{
			Opacity: []StudioKeyframe{
				{T: 5, Value: 2, Ease: "linear"},     // value > 1 → 1
				{T: 1, Value: -1, Ease: "bounce"},    // bad ease → linear, value → 0
				{T: 1, Value: 0.25, Ease: "easeIn"},  // dup t → last wins
				{T: -2, Value: 0.5, Ease: "easeOut"}, // t < 0 → 0
			},
		})
		lane := got.Opacity
		if len(lane) != 3 {
			t.Fatalf("dedupe: got %d lanes %+v", len(lane), lane)
		}
		if lane[0].T != 0 || lane[0].Ease != "easeOut" {
			t.Errorf("negative t clamps to 0: %+v", lane[0])
		}
		if lane[1].T != 1 || lane[1].Value != 0.25 || lane[1].Ease != "easeIn" {
			t.Errorf("dup t last-wins: %+v", lane[1])
		}
		if lane[2].Value != 1 {
			t.Errorf("value clamp to opacity range: %+v", lane[2])
		}
	})

	t.Run("caps each lane at 64", func(t *testing.T) {
		ts := make([]float64, 100)
		for i := range ts {
			ts[i] = float64(i)
		}
		got := sanitizeClipKeyframes(&StudioClipKeyframes{Scale: kf(ts...)})
		if len(got.Scale) != 64 {
			t.Errorf("cap: got %d want 64", len(got.Scale))
		}
	})

	t.Run("effects keys clamp by param name, unknown keys drop", func(t *testing.T) {
		got := sanitizeClipKeyframes(&StudioClipKeyframes{
			Effects: map[string][]StudioKeyframe{
				"lumetri1.exposure": {{T: 0, Value: 9, Ease: "linear"}}, // 9 → 3
				"lumetri1.bogus":    {{T: 0, Value: 1, Ease: "linear"}}, // unknown param → dropped
				"noDotKey":          {{T: 0, Value: 1, Ease: "linear"}}, // malformed key → dropped
			},
		})
		if len(got.Effects) != 1 {
			t.Fatalf("effects keys: got %+v", got.Effects)
		}
		if got.Effects["lumetri1.exposure"][0].Value != 3 {
			t.Errorf("exposure clamp: %+v", got.Effects["lumetri1.exposure"])
		}
	})

	t.Run("all-empty container drops to nil", func(t *testing.T) {
		if got := sanitizeClipKeyframes(&StudioClipKeyframes{}); got != nil {
			t.Errorf("empty keyframes should drop: %+v", got)
		}
	})
}

func TestSanitizeTracks_TitleClips(t *testing.T) {
	mk := func(c StudioClip) []StudioTrack {
		return []StudioTrack{{ID: "v1", Kind: StudioTrackKindVideo, Clips: []StudioClip{c}}}
	}
	textLayer := func() StudioTitleLayer {
		return StudioTitleLayer{
			Type: StudioTitleLayerText, Text: strp("Hello"), FontFamily: strp("Inter"),
			FontSizePx: f64(64), FontWeight: intp(700), FillColor: strp("#ffffff"),
			Align: strp("left"), X: f64(0.5), Y: f64(0.5),
		}
	}

	t.Run("assetless title clip survives; defaults fill", func(t *testing.T) {
		out := SanitizeTracks(mk(StudioClip{
			ID: "t1", AssetID: "", SourceOut: 5,
			Title: &StudioTitle{Layers: []StudioTitleLayer{
				textLayer(),
				{Type: StudioTitleLayerRect, X: f64(2), Y: f64(-1), W: f64(0.5), H: f64(0.25), Color: strp("nope")},
				{Type: StudioTitleLayerLine},
				{Type: "sticker"}, // unknown → dropped
			}},
		}))
		title := out[0].Clips[0].Title
		if title == nil || len(title.Layers) != 3 {
			t.Fatalf("layers: %+v", title)
		}
		text := title.Layers[0]
		if *text.FillColor != "#FFFFFF" || *text.Align != "left" || *text.FontWeight != 700 {
			t.Errorf("text layer sanitize: %+v", text)
		}
		rect := title.Layers[1]
		if *rect.X != 1 || *rect.Y != 0 || *rect.Color != "#FFFFFF" {
			t.Errorf("rect clamp/default: %+v", rect)
		}
		line := title.Layers[2]
		if *line.X1 != 0 || *line.X2 != 1 || *line.ThicknessPx != 4 {
			t.Errorf("line defaults: %+v", line)
		}
	})

	t.Run("title on an asset-backed clip is dropped (assetId XOR title)", func(t *testing.T) {
		out := SanitizeTracks(mk(StudioClip{
			ID: "c1", AssetID: "a1", SourceOut: 5,
			Title: &StudioTitle{Layers: []StudioTitleLayer{textLayer()}},
		}))
		if out[0].Clips[0].Title != nil {
			t.Errorf("asset-backed clip must not keep a title")
		}
	})

	t.Run("layer cap at 8", func(t *testing.T) {
		layers := make([]StudioTitleLayer, 0, 12)
		for i := 0; i < 12; i++ {
			layers = append(layers, textLayer())
		}
		out := SanitizeTracks(mk(StudioClip{ID: "t1", SourceOut: 5, Title: &StudioTitle{Layers: layers}}))
		if got := len(out[0].Clips[0].Title.Layers); got != 8 {
			t.Errorf("layer cap: got %d want 8", got)
		}
	})

	t.Run("invalid fontWeight coerces to 400, bad font falls back", func(t *testing.T) {
		l := textLayer()
		l.FontWeight = intp(600)
		l.FontFamily = strp("   ")
		out := SanitizeTracks(mk(StudioClip{ID: "t1", SourceOut: 5, Title: &StudioTitle{Layers: []StudioTitleLayer{l}}}))
		got := out[0].Clips[0].Title.Layers[0]
		if *got.FontWeight != 400 || *got.FontFamily != "Inter" {
			t.Errorf("text defaults: weight=%d font=%q", *got.FontWeight, *got.FontFamily)
		}
	})

	// Part 16: fontFamily is an enum against the shipped registry — never a
	// path ingredient. Unknown names (including traversal attempts) fall back
	// to the default; 400-only families clamp bold away.
	t.Run("fontFamily enum check", func(t *testing.T) {
		for _, bad := range []string{"Comic Sans MS", "../../etc/passwd", "Inter'; rm -rf"} {
			l := textLayer()
			l.FontFamily = strp(bad)
			out := SanitizeTracks(mk(StudioClip{ID: "t1", SourceOut: 5, Title: &StudioTitle{Layers: []StudioTitleLayer{l}}}))
			if got := *out[0].Clips[0].Title.Layers[0].FontFamily; got != StudioTitleFontDefault {
				t.Errorf("family %q: got %q want default", bad, got)
			}
		}
		known := textLayer()
		known.FontFamily = strp("Playfair Display")
		known.FontWeight = intp(700)
		out := SanitizeTracks(mk(StudioClip{ID: "t1", SourceOut: 5, Title: &StudioTitle{Layers: []StudioTitleLayer{known}}}))
		got := out[0].Clips[0].Title.Layers[0]
		if *got.FontFamily != "Playfair Display" || *got.FontWeight != 700 {
			t.Errorf("known family kept: font=%q weight=%d", *got.FontFamily, *got.FontWeight)
		}
	})

	t.Run("bold clamps to 400 for 400-only families", func(t *testing.T) {
		l := textLayer()
		l.FontFamily = strp("Bebas Neue")
		l.FontWeight = intp(700)
		out := SanitizeTracks(mk(StudioClip{ID: "t1", SourceOut: 5, Title: &StudioTitle{Layers: []StudioTitleLayer{l}}}))
		got := out[0].Clips[0].Title.Layers[0]
		if *got.FontFamily != "Bebas Neue" || *got.FontWeight != 400 {
			t.Errorf("400-only clamp: font=%q weight=%d", *got.FontFamily, *got.FontWeight)
		}
	})
}

func TestSanitizeTracks_SpeedAndFreeze(t *testing.T) {
	mk := func(c StudioClip) []StudioTrack {
		return []StudioTrack{{ID: "v1", Kind: StudioTrackKindVideo, Clips: []StudioClip{c}}}
	}

	t.Run("speed clamps into 0.1..10", func(t *testing.T) {
		out := SanitizeTracks(mk(StudioClip{ID: "c", AssetID: "a", SourceOut: 5, Speed: f64(99)}))
		if *out[0].Clips[0].Speed != 10 {
			t.Errorf("speed clamp: got %v", *out[0].Clips[0].Speed)
		}
	})

	t.Run("freeze wins over speed/reverse", func(t *testing.T) {
		out := SanitizeTracks(mk(StudioClip{
			ID: "c", AssetID: "a", SourceOut: 5,
			Speed: f64(2), Reverse: true, Freeze: &StudioFreeze{SourceTime: -3},
		}))
		c := out[0].Clips[0]
		if c.Speed != nil || c.Reverse {
			t.Errorf("freeze must clear speed/reverse: %+v", c)
		}
		if c.Freeze.SourceTime != 0 {
			t.Errorf("freeze sourceTime clamps to 0: %v", c.Freeze.SourceTime)
		}
	})
}

func TestSanitizeTracks_TrackFields(t *testing.T) {
	long := make([]rune, 0, 100)
	for i := 0; i < 100; i++ {
		long = append(long, 'x')
	}
	out := SanitizeTracks([]StudioTrack{{
		ID: "a1", Kind: StudioTrackKindAudio, Index: 0,
		Name: string(long), Gain: f64(5), Pan: f64(-3),
		Effects: &StudioTrackEffects{
			EQ:         &StudioTrackEQ{LowGainDb: -40, MidGainDb: 0, HighGainDb: 40},
			Compressor: &StudioTrackCompressor{ThresholdDb: -99, Ratio: 50, AttackMs: 0, ReleaseMs: 5, MakeupDb: 99},
		},
	}})
	tr := out[0]
	if len([]rune(tr.Name)) != 80 {
		t.Errorf("name truncation: got %d runes", len([]rune(tr.Name)))
	}
	if *tr.Gain != 2 || *tr.Pan != -1 {
		t.Errorf("gain/pan clamps: gain=%v pan=%v", *tr.Gain, *tr.Pan)
	}
	if tr.Effects.EQ.LowGainDb != -12 || tr.Effects.EQ.HighGainDb != 12 {
		t.Errorf("eq clamps: %+v", *tr.Effects.EQ)
	}
	comp := tr.Effects.Compressor
	if comp.ThresholdDb != -60 || comp.Ratio != 20 || comp.AttackMs != 1 || comp.ReleaseMs != 20 || comp.MakeupDb != 12 {
		t.Errorf("compressor clamps: %+v", *comp)
	}

	// Empty effects container drops to nil.
	out = SanitizeTracks([]StudioTrack{{ID: "a1", Kind: StudioTrackKindAudio, Effects: &StudioTrackEffects{}}})
	if out[0].Effects != nil {
		t.Errorf("empty track effects should drop: %+v", out[0].Effects)
	}
}

func TestSanitizeMarkers(t *testing.T) {
	ms := SanitizeMarkers([]StudioMarker{
		{ID: "b", T: 9, Name: "end", Color: "#abcdef"},
		{ID: "a", T: -1, Name: "start", Color: "nope", Comment: "note"},
	})
	if len(ms) != 2 || ms[0].ID != "a" {
		t.Fatalf("markers should sort by t: %+v", ms)
	}
	if ms[0].T != 0 {
		t.Errorf("negative t clamps to 0: %v", ms[0].T)
	}
	if ms[0].Color != "#22C55E" {
		t.Errorf("bad color → default: %q", ms[0].Color)
	}
	if ms[1].Color != "#ABCDEF" {
		t.Errorf("hex normalized uppercase: %q", ms[1].Color)
	}

	// Cap at 500.
	many := make([]StudioMarker, 0, 600)
	for i := 0; i < 600; i++ {
		many = append(many, StudioMarker{ID: "m", T: float64(i), Color: "#FFFFFF"})
	}
	if got := len(SanitizeMarkers(many)); got != 500 {
		t.Errorf("marker cap: got %d want 500", got)
	}
	if SanitizeMarkers(nil) != nil {
		t.Errorf("nil markers stay nil (omitempty)")
	}
}

func TestSanitizeTracksForAssets_ClearsImageSpeedReverseFreeze(t *testing.T) {
	sp := 2.0
	tracks := []StudioTrack{{ID: "v", Kind: StudioTrackKindVideo, Clips: []StudioClip{
		{ID: "img", AssetID: "i", SourceOut: 5, Speed: &sp, Reverse: true},
		{ID: "imgf", AssetID: "i", SourceOut: 5, Freeze: &StudioFreeze{SourceTime: 1}},
		{ID: "vid", AssetID: "m", SourceOut: 5, Speed: &sp, Reverse: true},
	}}}
	kinds := map[string]StudioMediaKind{"i": StudioMediaKindImage, "m": StudioMediaKindVideo}
	got := SanitizeTracksForAssets(tracks, kinds)
	c := got[0].Clips
	if c[0].Speed != nil || c[0].Reverse || c[1].Freeze != nil {
		t.Errorf("image clips not cleared: %+v %+v", c[0], c[1])
	}
	if c[2].Speed == nil || *c[2].Speed != 2 || !c[2].Reverse {
		t.Errorf("video clip must be untouched: %+v", c[2])
	}
	if tracks[0].Clips[0].Speed == nil {
		t.Errorf("input mutated")
	}
	again := SanitizeTracksForAssets(got, kinds)
	if again[0].Clips[0].Speed != nil || again[0].Clips[2].Speed == nil {
		t.Errorf("not idempotent")
	}
}

func TestSanitizeTracks_ImageHoldUnbounded7200(t *testing.T) {
	got := SanitizeTracks([]StudioTrack{{ID: "v", Kind: StudioTrackKindVideo, Clips: []StudioClip{
		{ID: "img", AssetID: "i", SourceIn: 0, SourceOut: 7200},
	}}})
	if got[0].Clips[0].SourceOut != 7200 {
		t.Errorf("sourceOut clamped: %v", got[0].Clips[0].SourceOut)
	}
}
