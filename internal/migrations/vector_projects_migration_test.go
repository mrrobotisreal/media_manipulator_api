package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM43: the Motion tables migration is written, never run on this machine, so
// these assertions are what keeps its contract honest.
func TestVectorProjectsMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261025002_vector_projects.up.sql")
	down := read("20261025002_vector_projects.down.sql")

	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS vector_projects",
		"owner_uid     TEXT,",
		"anon_owner    TEXT,",
		"document      JSONB NOT NULL",
		"revision      INT NOT NULL DEFAULT 1",
		"thumbnail_key TEXT",
		"CHECK (owner_uid IS NOT NULL OR anon_owner IS NOT NULL)",
		"CREATE TABLE IF NOT EXISTS vector_exports",
		"project_id  TEXT,",
		"ON vector_exports (owner, created_at)",
		"'studio_project', 'metadata_report', 'job_row', 'vector_project'",
		"CHECK (metric IN ('ops', 'ai_ops', 'bytes_in', 'export', 'vector_export'))",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	// No FK from exports to projects: deleting a project keeps export history.
	if strings.Contains(up, "REFERENCES vector_projects") {
		t.Error("vector_exports must not reference vector_projects")
	}
	for _, f := range []string{"svg_animated", "lottie", "react_tsx", "js_ts", "flutter", "gif",
		"png_sequence", "mp4", "webm", "import_lottie", "import_svg_vectorize"} {
		if !strings.Contains(up, "'"+f+"'") {
			t.Errorf("format CHECK is missing %q", f)
		}
	}
	if strings.Contains(up, "'svg_static'") {
		t.Error("svg_static is uncounted and must not be a vector_exports format")
	}

	// Down removes the rows the narrower CHECKs would reject BEFORE re-adding them.
	for _, pair := range [][2]string{
		{"DELETE FROM mm_retained_objects WHERE kind = 'vector_project'", "ADD CONSTRAINT mm_retained_objects_kind_check"},
		{"DELETE FROM mm_usage_counters WHERE metric = 'vector_export'", "ADD CONSTRAINT mm_usage_counters_metric_check"},
	} {
		del, add := strings.Index(down, pair[0]), strings.Index(down, pair[1])
		if del < 0 || add < 0 || del > add {
			t.Errorf("down must run %q before %q", pair[0], pair[1])
		}
	}
	for _, want := range []string{"DROP TABLE IF EXISTS vector_exports", "DROP TABLE IF EXISTS vector_projects"} {
		if !strings.Contains(down, want) {
			t.Errorf("down migration is missing %q", want)
		}
	}
}
