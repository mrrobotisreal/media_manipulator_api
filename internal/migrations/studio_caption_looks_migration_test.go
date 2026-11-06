package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM122: the studio_caption_looks migration is written, never run against a
// real database by the agent, so these assertions keep its contract honest.
func TestStudioCaptionLooksMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261106122_studio_caption_looks.up.sql")
	down := read("20261106122_studio_caption_looks.down.sql")
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS studio_caption_looks",
		"id            uuid PRIMARY KEY DEFAULT gen_random_uuid()",
		"owner_user_id uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE",
		"CHECK (char_length(name) BETWEEN 1 AND 60)",
		"style         jsonb NOT NULL",
		"revision      int NOT NULL DEFAULT 1",
		"deleted_at    timestamptz",
		"ON studio_caption_looks (owner_user_id, updated_at DESC)",
		"WHERE deleted_at IS NULL",
		"'photo_preset', 'assistant_conversation',\n                    'studio_caption_look'));",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	del := strings.Index(down, "DELETE FROM mm_retained_objects WHERE kind = 'studio_caption_look'")
	add := strings.Index(down, "ADD CONSTRAINT mm_retained_objects_kind_check")
	if del < 0 || add < 0 || del > add {
		t.Error("down must delete studio_caption_look rows before narrowing the CHECK")
	}
	// The down list must equal the MM104 (20261104001_assistant) up list.
	prev := read("20261104001_assistant.up.sql")
	i := strings.Index(prev, "ADD CONSTRAINT mm_retained_objects_kind_check")
	j := strings.Index(prev[i:], "));")
	k := strings.Index(down, "ADD CONSTRAINT mm_retained_objects_kind_check")
	l := strings.Index(down[k:], "));")
	if i < 0 || k < 0 || strings.Join(strings.Fields(prev[i:i+j]), " ") != strings.Join(strings.Fields(down[k:k+l]), " ") {
		t.Error("down must restore the MM104 kind list exactly")
	}
	if !strings.Contains(down, "DROP TABLE IF EXISTS studio_caption_looks") {
		t.Error("down must drop studio_caption_looks")
	}
}
