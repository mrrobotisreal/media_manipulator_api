package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM103: the photo_presets migration is written, never run on this machine, so
// these assertions keep its contract honest.
func TestPhotoPresetsMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261103002_photo_presets.up.sql")
	down := read("20261103002_photo_presets.down.sql")

	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS photo_presets",
		"id            uuid PRIMARY KEY DEFAULT gen_random_uuid()",
		"owner_user_id uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE",
		"CHECK (char_length(name) BETWEEN 1 AND 60)",
		"CHECK (category IN ('color', 'bw', 'film', 'fix', 'custom'))",
		"layers        jsonb NOT NULL",
		"thumbnail_key text,",
		"revision      int NOT NULL DEFAULT 1",
		"deleted_at    timestamptz",
		"ON photo_presets (owner_user_id, updated_at DESC)",
		"WHERE deleted_at IS NULL",
		"'derived_file', 'photo_project', 'photo_preset'));",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	del := strings.Index(down, "DELETE FROM mm_retained_objects WHERE kind = 'photo_preset'")
	add := strings.Index(down, "ADD CONSTRAINT mm_retained_objects_kind_check")
	if del < 0 || add < 0 || del > add {
		t.Error("down must delete photo_preset rows before narrowing the CHECK")
	}
	if !strings.Contains(down, "'derived_file', 'photo_project'));") {
		t.Error("down must restore the MM95 kind list exactly")
	}
	if !strings.Contains(down, "DROP TABLE IF EXISTS photo_presets") {
		t.Error("down must drop photo_presets")
	}
}
