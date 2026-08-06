package services

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// The B13 ownership rules live in SQL and need Postgres to exercise end to end
// (see the CHECKPOINT 4 server steps). What IS unit-testable is the parameter
// marshalling those predicates depend on, and it is the part most likely to be
// broken by a careless refactor: every ownership query is written as
// `$n::uuid IS NOT NULL AND user_id = $n::uuid`, so an anonymous owner MUST
// bind SQL NULL. Binding the empty string instead would make the cast fail —
// or worse, in a rewritten query, match rows it should not.
func TestStudioOwnerUserParam(t *testing.T) {
	cases := []struct {
		name  string
		owner StudioOwner
		want  any
	}{
		{"anonymous", StudioOwner{SessionID: "sess-1"}, nil},
		{"blank user id", StudioOwner{UserID: "", SessionID: "sess-1"}, nil},
		{"whitespace user id", StudioOwner{UserID: "   ", SessionID: "sess-1"}, nil},
		{
			"signed in",
			StudioOwner{UserID: "0f6c8e2a-1111-4222-8333-444455556666", SessionID: "sess-1"},
			"0f6c8e2a-1111-4222-8333-444455556666",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.owner.userParam(); got != tc.want {
				t.Errorf("userParam() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A signed-out owner must never be able to produce a non-NULL user parameter,
// no matter what the session id contains — the session id is client-supplied
// and must not be able to leak into the account-scoped half of the predicate.
func TestStudioOwnerSessionCannotForgeUserID(t *testing.T) {
	owner := StudioOwner{SessionID: "0f6c8e2a-1111-4222-8333-444455556666"}
	if got := owner.userParam(); got != nil {
		t.Fatalf("a session id must never bind as a user id, got %#v", got)
	}
}

// fp / sp pointer helpers come from studio_export_test.go (same package).
func ip(v int) *int   { return &v }
func bp(v bool) *bool { return &v }

// fullV3Tracks is a track tree with EVERY EDL v3 field populated, using
// in-range values in already-sanitized form (colors uppercased, lanes sorted,
// required title-layer fields present) so sanitize is a no-op on it.
func fullV3Tracks() []models.StudioTrack {
	kf := func(t, v float64, ease string) models.StudioKeyframe {
		return models.StudioKeyframe{T: t, Value: v, Ease: ease}
	}
	return []models.StudioTrack{
		{
			ID: "v1", Kind: models.StudioTrackKindVideo, Index: 0, Muted: false,
			Name: "Main video", Locked: true, Hidden: false,
			Clips: []models.StudioClip{
				{
					ID: "c1", AssetID: "a1", StreamIndex: 0,
					TimelineStart: 0, SourceIn: 0, SourceOut: 5,
					Opacity:             fp(0.9),
					Transition:          &models.StudioTransition{Type: models.StudioTransitionWipeLeft, DurationSeconds: 1.5, Params: map[string]float64{"feather": 0.2}},
					TransitionInSeconds: fp(1.5), // legacy mirror, kept in sync by the sanitizer
					Keyframes: &models.StudioClipKeyframes{
						PositionX: []models.StudioKeyframe{kf(0, -0.5, "linear"), kf(2, 0.5, "easeBoth")},
						PositionY: []models.StudioKeyframe{kf(0, 0.1, "hold")},
						Scale:     []models.StudioKeyframe{kf(0, 1, "easeIn"), kf(3, 2, "easeOut")},
						Rotation:  []models.StudioKeyframe{kf(0, 0, "linear"), kf(4, 180, "linear")},
						Opacity:   []models.StudioKeyframe{kf(0, 1, "linear"), kf(5, 0, "linear")},
						Effects: map[string][]models.StudioKeyframe{
							"lumetri1.exposure": {kf(0, 0, "linear"), kf(2, 1.5, "easeBoth")},
						},
					},
					Speed:         fp(2),
					Reverse:       true,
					MaintainPitch: bp(false),
					GroupID:       "g1",
				},
				{
					ID: "t1", AssetID: "", StreamIndex: 0,
					TimelineStart: 5, SourceIn: 0, SourceOut: 3,
					Title: &models.StudioTitle{Layers: []models.StudioTitleLayer{
						{
							Type: models.StudioTitleLayerText, Text: sp("Darkroom"), FontFamily: sp("Inter"),
							FontSizePx: fp(96), FontWeight: ip(700), FillColor: sp("#FFFFFF"),
							StrokeColor: sp("#000000"), StrokeWidthPx: fp(2),
							Shadow:          &models.StudioTitleShadow{XPx: 2, YPx: 2, BlurPx: 8, Color: "#000000"},
							Background:      &models.StudioTitleBackground{Color: "#111111", Opacity: 0.8, PaddingXPx: 16, PaddingYPx: 8, RadiusPx: 6},
							LetterSpacingPx: fp(1.5), LineHeight: fp(1.2),
							Align: sp("center"), X: fp(0.5), Y: fp(0.4),
						},
						{Type: models.StudioTitleLayerRect, X: fp(0.1), Y: fp(0.7), W: fp(0.8), H: fp(0.1), Color: sp("#FF0000"), Opacity: fp(0.5), RadiusPx: fp(4)},
						{Type: models.StudioTitleLayerLine, X1: fp(0.1), Y1: fp(0.85), X2: fp(0.9), Y2: fp(0.85), Color: sp("#00FF00"), ThicknessPx: fp(3)},
					}},
				},
				{
					ID: "c2", AssetID: "a1", StreamIndex: 0,
					TimelineStart: 8, SourceIn: 10, SourceOut: 12,
					Freeze: &models.StudioFreeze{SourceTime: 10.5},
				},
			},
		},
		{
			ID: "a1t", Kind: models.StudioTrackKindAudio, Index: 0, Muted: false,
			Name: "Music", Solo: true, Gain: fp(0.8), Pan: fp(-0.25),
			Effects: &models.StudioTrackEffects{
				EQ:         &models.StudioTrackEQ{LowGainDb: -3, MidGainDb: 0, HighGainDb: 2},
				Compressor: &models.StudioTrackCompressor{ThresholdDb: -24, Ratio: 4, AttackMs: 10, ReleaseMs: 250, MakeupDb: 3},
			},
			Clips: []models.StudioClip{
				{
					ID: "c3", AssetID: "music", StreamIndex: 0,
					TimelineStart: 0, SourceIn: 0, SourceOut: 10,
					Volume: fp(0.7), Pan: fp(0.25),
					Keyframes: &models.StudioClipKeyframes{
						Volume: []models.StudioKeyframe{kf(0, 1, "linear"), kf(8, 0, "easeOut")},
						Pan:    []models.StudioKeyframe{kf(0, -1, "linear"), kf(8, 1, "linear")},
					},
					// legacy lane kept alongside the upgraded one
					VolumeKeyframes: []models.StudioVolumeKeyframe{{T: 0, Gain: 1}, {T: 8, Gain: 0}},
				},
			},
		},
	}
}

// TestStudioV3RoundTrip drives a project with every v3 field populated through
// the exact transform SaveProject → GetProject applies (SanitizeTracks /
// SanitizeMarkers → json.Marshal into the JSONB columns → json.Unmarshal in
// scanProject) and requires it to come back intact. This is the unit-testable
// core of the PUT→GET acceptance check (the SQL around it is exercised on the
// server).
func TestStudioV3RoundTrip(t *testing.T) {
	clean := models.SanitizeTracks(fullV3Tracks())

	// Sanitize must be a no-op on the already-valid fixture — otherwise the
	// fixture is out of sync with the sanitizer's ranges.
	before, _ := json.Marshal(fullV3Tracks())
	after, _ := json.Marshal(clean)
	if string(before) != string(after) {
		t.Fatalf("fixture not sanitize-stable:\nbefore: %s\nafter:  %s", before, after)
	}

	// Save side: what SaveProject writes into studio_projects.tracks.
	stored, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Load side: what scanProject reads back.
	var loaded []models.StudioTrack
	if err := json.Unmarshal(stored, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(clean, loaded) {
		t.Fatalf("tracks did not survive the round trip:\nsaved:  %+v\nloaded: %+v", clean, loaded)
	}
	// And the reloaded document re-marshals byte-identically (JSONB fidelity).
	restored, _ := json.Marshal(loaded)
	if string(stored) != string(restored) {
		t.Fatalf("re-marshal not byte-identical:\nstored:   %s\nrestored: %s", stored, restored)
	}

	// Second sanitize is idempotent over the full v3 surface.
	twice, _ := json.Marshal(models.SanitizeTracks(loaded))
	if string(stored) != string(twice) {
		t.Fatalf("sanitize not idempotent over v3 fields:\nonce:  %s\ntwice: %s", stored, twice)
	}
}

// TestStudioSidecarMarkersRoundTrip covers the project-level v3 field: markers
// ride the captions sidecar (same JSONB envelope SaveProject writes and
// scanProject reads) without disturbing the v2 keys.
func TestStudioSidecarMarkersRoundTrip(t *testing.T) {
	enabled := true
	sidecar := studioProjectSidecar{
		SchemaVersion: models.StudioSchemaVersionCurrent,
		Cues:          []models.StudioCaptionCue{{ID: "cue1", StartSeconds: 1, EndSeconds: 2, Text: "Hi"}},
		Enabled:       &enabled,
		Markers: models.SanitizeMarkers([]models.StudioMarker{
			{ID: "m2", T: 12, Name: "Outro", Color: "#FF0000"},
			{ID: "m1", T: 3.5, Name: "Intro ends", Color: "#22C55E", Comment: "tighten this cut"},
		}),
	}
	stored, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var loaded studioProjectSidecar
	if err := json.Unmarshal(stored, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(sidecar, loaded) {
		t.Fatalf("sidecar did not survive the round trip:\nsaved:  %+v\nloaded: %+v", sidecar, loaded)
	}
	if len(loaded.Markers) != 2 || loaded.Markers[0].ID != "m1" {
		t.Fatalf("markers must be sorted by t: %+v", loaded.Markers)
	}
	if loaded.SchemaVersion != 3 {
		t.Fatalf("sidecar schema version: got %d want 3", loaded.SchemaVersion)
	}
}
