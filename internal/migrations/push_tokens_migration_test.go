package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MA23: the push token table and the pushJobFinished column; down drops both.
func TestPushTokenMigration(t *testing.T) {
	up, err := os.ReadFile(filepath.Join("migrations", "20261101001_add_push_tokens.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile(filepath.Join("migrations", "20261101001_add_push_tokens.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	u, d := string(up), string(down)
	for _, want := range []string{
		"BEGIN;", "COMMIT;",
		"CREATE TABLE IF NOT EXISTS mm_push_tokens",
		"user_id         uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE",
		"token           text NOT NULL UNIQUE",
		"CHECK (platform IN ('ios', 'android'))",
		"disabled_at     timestamptz NULL", "disabled_reason text NULL",
		"WHERE disabled_at IS NULL",
		"ADD COLUMN IF NOT EXISTS push_job_finished boolean NOT NULL DEFAULT true",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("up migration missing %q", want)
		}
	}
	for _, want := range []string{"DROP COLUMN IF EXISTS push_job_finished", "DROP TABLE IF EXISTS mm_push_tokens", "BEGIN;", "COMMIT;"} {
		if !strings.Contains(d, want) {
			t.Errorf("down migration missing %q", want)
		}
	}
}
