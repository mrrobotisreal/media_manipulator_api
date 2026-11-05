package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// MM109 acceptance 2: the tracks Medi's Darkroom engine produces for a
// representative batch (testdata/medi_studio_tracks.json, written by the UI's
// lib/assistant/ops/studioSanitizerFixture.ts) survive the server sanitizer
// unchanged — every look, grade, title, keyframe, speed, freeze and audio value
// Medi writes is already inside the Go ranges, so a save never alters it.
func TestSanitizeTracks_MediStudioOutputUnchanged(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "medi_studio_tracks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tracks []StudioTrack
	if err := json.Unmarshal(raw, &tracks); err != nil {
		t.Fatal(err)
	}
	if len(tracks) < 4 {
		t.Fatalf("fixture has %d tracks, want the title track too", len(tracks))
	}
	before, _ := json.Marshal(tracks)
	after, _ := json.Marshal(SanitizeTracks(tracks))
	var a, b any
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(after, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("SanitizeTracks changed Medi's output:\nbefore %s\nafter  %s", before, after)
	}
	// The look is a plain Lumetri effect: no new effect type reaches the export.
	found := false
	for _, tr := range tracks {
		for _, c := range tr.Clips {
			for _, e := range c.Effects {
				if e.ID == "look-cinematic" && e.Type == StudioEffectLumetri {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("look-cinematic Lumetri effect missing from the fixture")
	}
}
