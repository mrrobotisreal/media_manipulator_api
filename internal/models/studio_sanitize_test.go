package models

import (
	"encoding/json"
	"testing"
)

func f64(v float64) *float64 { return &v }
func strp(v string) *string  { return &v }

// TestSanitizeTracks_V1NoOp is the EDL v2 backward-compat guard: a valid v1 clip
// (no v2 fields, in-range values) must sanitize to a byte-identical clip so
// existing projects load / save / export exactly as before.
func TestSanitizeTracks_V1NoOp(t *testing.T) {
	v1 := []StudioTrack{
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

	before, _ := json.Marshal(v1)
	after, _ := json.Marshal(SanitizeTracks(v1))
	if string(before) != string(after) {
		t.Errorf("v1 clip not byte-identical after sanitize:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestSanitizeTracks_ClampsAndDrops(t *testing.T) {
	tracks := []StudioTrack{
		{
			ID: "v1", Kind: StudioTrackKindVideo, Index: 0,
			Clips: []StudioClip{
				{
					ID: "c1", AssetID: "a1", SourceIn: 0, SourceOut: 5,
					Volume:    f64(3.0),  // > 2 → 2
					Opacity:   f64(1.5),  // > 1 → 1
					Pan:       f64(2.0),  // > 1 → 1
					BlendMode: "bogus",   // unknown → ""
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
