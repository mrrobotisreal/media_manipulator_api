package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM95: the Aperture tables migration is written, never run on this machine, so
// these assertions keep its contract honest.
func TestPhotoProjectsMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261103001_photo_projects.up.sql")
	down := read("20261103001_photo_projects.down.sql")

	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS photo_projects",
		"id              uuid PRIMARY KEY DEFAULT gen_random_uuid()",
		"owner_user_id   uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE",
		"doc             jsonb NOT NULL",
		"revision        int NOT NULL DEFAULT 1",
		"original_ext    text NOT NULL",
		"original_width  int NOT NULL",
		"original_height int NOT NULL",
		"original_bytes  bigint NOT NULL",
		"has_alpha       boolean NOT NULL DEFAULT false",
		"CHECK (status IN ('ingesting', 'ready', 'failed'))",
		"error           text,",
		"deleted_at      timestamptz",
		"ON photo_projects (owner_user_id, updated_at DESC)",
		"WHERE deleted_at IS NULL",
		"CREATE TABLE IF NOT EXISTS photo_derivatives",
		"project_id  uuid NOT NULL REFERENCES photo_projects(id) ON DELETE CASCADE",
		"CHECK (kind IN ('subject_mask', 'heal_base'))",
		"UNIQUE (project_id, kind, params_hash)",
		"'job_row', 'vector_project', 'voice',\n                    'derived_file', 'photo_project'));",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	del := strings.Index(down, "DELETE FROM mm_retained_objects WHERE kind = 'photo_project'")
	add := strings.Index(down, "ADD CONSTRAINT mm_retained_objects_kind_check")
	if del < 0 || add < 0 || del > add {
		t.Error("down must delete photo_project rows before narrowing the CHECK")
	}
	if !strings.Contains(down, "'job_row', 'vector_project', 'voice',\n                    'derived_file'));") {
		t.Error("down must restore the MM81 kind list exactly")
	}
	// Derivatives are dropped before projects (the fk).
	if a, b := strings.Index(down, "DROP TABLE IF EXISTS photo_derivatives"), strings.Index(down, "DROP TABLE IF EXISTS photo_projects"); a < 0 || b < 0 || a > b {
		t.Error("down must drop photo_derivatives before photo_projects")
	}
}
