package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM77: the voices migration is written, never run on this machine, so these
// assertions keep its contract honest.
func TestVoicesMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261028001_mm_voices.up.sql")
	down := read("20261028001_mm_voices.down.sql")
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS mm_voices",
		"user_id              uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE",
		"CHECK (kind IN ('cloned', 'designed', 'preset'))",
		"seed                 bigint",
		"reference_s3_key     text",
		"preview_s3_key       text",
		"consent_at           timestamptz",
		"consent_text_version text",
		"deleted_at           timestamptz",
		"CHECK (kind <> 'cloned' OR (consent_at IS NOT NULL AND consent_text_version IS NOT NULL))",
		"ON mm_voices (user_id) WHERE deleted_at IS NULL",
		"'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice'",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	del := strings.Index(down, "DELETE FROM mm_retained_objects WHERE kind = 'voice'")
	add := strings.Index(down, "ADD CONSTRAINT mm_retained_objects_kind_check")
	if del < 0 || add < 0 || del > add {
		t.Error("down must delete the voice ledger rows before narrowing the CHECK")
	}
	if !strings.Contains(down, "'job_row', 'vector_project'));") || strings.Contains(down, "'voice'))") {
		t.Error("down must restore the MM43 kind list exactly")
	}
	if !strings.Contains(down, "DROP TABLE IF EXISTS mm_voices") {
		t.Error("down must drop mm_voices")
	}
}
