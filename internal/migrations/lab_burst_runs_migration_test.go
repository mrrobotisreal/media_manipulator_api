package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM138: the Lab migration is written, never run on this machine, so these
// assertions keep its contract honest.
func TestLabBurstRunsMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261111001_lab_burst_runs_purpose.up.sql")
	down := read("20261111001_lab_burst_runs_purpose.down.sql")
	for _, want := range []string{
		"ALTER TABLE burst_runs",
		"ADD COLUMN IF NOT EXISTS purpose text DEFAULT 'burst'",
		"CHECK (purpose IS NULL OR purpose IN ('burst', 'lab'))",
		"ADD COLUMN IF NOT EXISTS session_until timestamptz",
		"WHERE purpose = 'lab'",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	for _, bad := range []string{"DROP ", "NOT NULL", "DELETE", "UPDATE "} {
		if strings.Contains(up, bad) {
			t.Errorf("up migration must be additive; found %q", bad)
		}
	}
	for _, want := range []string{"DROP COLUMN IF EXISTS session_until", "DROP COLUMN IF EXISTS purpose", "DROP INDEX IF EXISTS idx_burst_runs_lab_offered_at"} {
		if !strings.Contains(down, want) {
			t.Errorf("down migration is missing %q", want)
		}
	}
}
