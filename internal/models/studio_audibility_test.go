package models

import (
	"encoding/json"
	"os"
	"testing"
)

type audibilityTrack struct {
	Kind   StudioTrackKind `json:"kind"`
	Muted  bool            `json:"muted"`
	Solo   bool            `json:"solo"`
	Hidden bool            `json:"hidden"`
}

func (a audibilityTrack) track() StudioTrack {
	return StudioTrack{Kind: a.Kind, Muted: a.Muted, Solo: a.Solo, Hidden: a.Hidden}
}

func TestTrackAudibilityTruthTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/track_audibility.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		AnySolo []struct {
			Name   string            `json:"name"`
			Tracks []audibilityTrack `json:"tracks"`
			Expect bool              `json:"expect"`
		} `json:"anySolo"`
		Audible []struct {
			Name       string          `json:"name"`
			Track      audibilityTrack `json:"track"`
			SoloActive bool            `json:"soloActive"`
			Expect     bool            `json:"expect"`
		} `json:"audible"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.AnySolo {
		tracks := make([]StudioTrack, 0, len(c.Tracks))
		for _, a := range c.Tracks {
			tracks = append(tracks, a.track())
		}
		if got := AnySolo(tracks); got != c.Expect {
			t.Errorf("AnySolo %q: got %v want %v", c.Name, got, c.Expect)
		}
	}
	for _, c := range fx.Audible {
		if got := IsTrackAudible(c.Track.track(), c.SoloActive); got != c.Expect {
			t.Errorf("IsTrackAudible %q: got %v want %v", c.Name, got, c.Expect)
		}
	}
}

func TestSanitizeAudioConfig_MasterGain(t *testing.T) {
	if a := SanitizeAudioConfig(&StudioAudioConfig{}); a.MasterGain != nil {
		t.Errorf("absent masterGain must stay absent")
	}
	g := 5.0
	if a := SanitizeAudioConfig(&StudioAudioConfig{MasterGain: &g}); *a.MasterGain != 2 {
		t.Errorf("masterGain clamp: got %v", *a.MasterGain)
	}
	g = -1
	if a := SanitizeAudioConfig(&StudioAudioConfig{MasterGain: &g}); *a.MasterGain != 0 {
		t.Errorf("masterGain floor: got %v", *a.MasterGain)
	}
}
