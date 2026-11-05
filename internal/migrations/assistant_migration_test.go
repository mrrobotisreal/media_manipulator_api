package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM104: the assistant migration is written, never run on this machine, so
// these assertions keep its contract honest.
func TestAssistantMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261104001_assistant.up.sql")
	down := read("20261104001_assistant.down.sql")

	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS assistant_conversations",
		"owner_user_id uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE",
		"CHECK (surface IN ('guide', 'studio', 'vector', 'photo'))",
		"CREATE TABLE IF NOT EXISTS assistant_messages",
		"conversation_id uuid NOT NULL REFERENCES assistant_conversations(id) ON DELETE CASCADE",
		"CHECK (role IN ('user', 'assistant', 'tool'))",
		"CHECK (status IN ('complete', 'interrupted', 'error'))",
		"UNIQUE (conversation_id, seq)",
		"provider_state  jsonb,",
		"CREATE TABLE IF NOT EXISTS assistant_message_feedback",
		"CHECK (rating IN ('up', 'down'))",
		"CREATE TABLE IF NOT EXISTS assistant_usage_events",
		"cost_usd      numeric(14,6) NOT NULL DEFAULT 0",
		"CHECK (kind IN ('chat', 'title', 'summary'))",
		"ON assistant_usage_events (created_at)",
		"'export', 'vector_export', 'assistant_turns'));",
		"'derived_file', 'photo_project', 'photo_preset', 'assistant_conversation'));",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	// Frames are never a column: content is jsonb and the handler strips them.
	if strings.Contains(strings.ToLower(up), "frame ") || strings.Contains(strings.ToLower(up), "bytea") {
		t.Error("no frame / binary column may exist")
	}
	for _, pair := range [][2]string{
		{"DELETE FROM mm_retained_objects WHERE kind = 'assistant_conversation'", "ADD CONSTRAINT mm_retained_objects_kind_check"},
		{"DELETE FROM mm_usage_counters WHERE metric = 'assistant_turns'", "ADD CONSTRAINT mm_usage_counters_metric_check"},
	} {
		del, add := strings.Index(down, pair[0]), strings.Index(down, pair[1])
		if del < 0 || add < 0 || del > add {
			t.Errorf("down must run %q before %q", pair[0], pair[1])
		}
	}
	for _, want := range []string{
		"'derived_file', 'photo_project', 'photo_preset'));",
		"'export', 'vector_export'));",
		"DROP TABLE IF EXISTS assistant_usage_events",
		"DROP TABLE IF EXISTS assistant_conversations",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("down is missing %q", want)
		}
	}
}

// MM113: the version/eval-runs migration (written, never run here).
func TestAssistantVersionsMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261105001_assistant_versions.up.sql")
	down := read("20261105001_assistant_versions.down.sql")
	for _, want := range []string{
		"ALTER TABLE assistant_messages ADD COLUMN IF NOT EXISTS prompt_version text",
		"ALTER TABLE assistant_usage_events ADD COLUMN IF NOT EXISTS prompt_version text",
		"ADD COLUMN IF NOT EXISTS summary_upto int NOT NULL DEFAULT 0",
		"CREATE TABLE IF NOT EXISTS assistant_eval_runs",
		"median_tool_calls", "evidence_path", "model_guide", "model_copilot", "cost_usd",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	for _, want := range []string{"DROP TABLE IF EXISTS assistant_eval_runs", "DROP COLUMN IF EXISTS prompt_version", "DROP COLUMN IF EXISTS summary_upto"} {
		if !strings.Contains(down, want) {
			t.Errorf("down is missing %q", want)
		}
	}
}
