package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM124: the burst_runs migration is written, never run by the agent, so these
// assertions keep its contract honest (up/down).
func TestBurstRunsMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261108001_burst_runs.up.sql")
	down := read("20261108001_burst_runs.down.sql")
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS burst_runs",
		"id                  uuid PRIMARY KEY",
		"reserved_usd_micros bigint NOT NULL",
		"cost_usd_micros     bigint NOT NULL",
		"adopted_from        uuid REFERENCES burst_runs(id)",
		"terminated_at       timestamptz",
		"ON burst_runs (offered_at)",
		"ON burst_runs (state) WHERE terminated_at IS NULL",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	// Every design §6 state is in the CHECK.
	for _, s := range []string{"offered", "launching", "booting", "ready", "staging_in", "running", "staging_out",
		"completed", "lost_race", "adopted", "launch_failed", "boot_timeout", "worker_lost", "tool_failed", "cap_refused", "orphaned"} {
		if !strings.Contains(up, "'"+s+"'") {
			t.Errorf("state %q missing from CHECK", s)
		}
	}
	for _, secret := range []string{"authkey", "auth_key", "secret"} {
		if strings.Contains(strings.ToLower(strings.SplitN(up, "BEGIN;", 2)[1]), secret) {
			t.Errorf("up migration must not store %q", secret)
		}
	}
	if !strings.Contains(down, "DROP TABLE IF EXISTS burst_runs") {
		t.Error("down must drop burst_runs")
	}
}
