package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeMigrationName(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"add foo":               "add_foo",
		"  add  Some FEATURE  ": "add_some_feature",
		"add--double---dash":    "add_double_dash",
		"weird!@#$%^chars":      "weirdchars",
		"already_snake_case":    "already_snake_case",
	}
	for in, want := range cases {
		if got := normalizeMigrationName(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateMigrationFiles_Counter(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	stamp := "20260520"

	// Pre-create two same-day files to verify the counter picks 003.
	for _, p := range []string{
		filepath.Join(dir, stamp+"001_pre.up.sql"),
		filepath.Join(dir, stamp+"001_pre.down.sql"),
		filepath.Join(dir, stamp+"002_pre.up.sql"),
		filepath.Join(dir, stamp+"002_pre.down.sql"),
	} {
		if err := os.WriteFile(p, []byte("--"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	upPath, downPath, err := createMigrationFiles(dir, "Add Thing", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wantPrefix := stamp + "003"
	if !strings.HasPrefix(filepath.Base(upPath), wantPrefix) {
		t.Errorf("up file %s should start with %s", filepath.Base(upPath), wantPrefix)
	}
	if !strings.HasPrefix(filepath.Base(downPath), wantPrefix) {
		t.Errorf("down file %s should start with %s", filepath.Base(downPath), wantPrefix)
	}
	if !strings.HasSuffix(upPath, "_add_thing.up.sql") {
		t.Errorf("unexpected up suffix: %s", upPath)
	}
	if !strings.HasSuffix(downPath, "_add_thing.down.sql") {
		t.Errorf("unexpected down suffix: %s", downPath)
	}
	for _, p := range []string{upPath, downPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing created file %s: %v", p, err)
		}
	}
}

func TestParseTargetDB(t *testing.T) {
	target, host, err := parseTargetDB("postgres://postgres:postgres@localhost:5432/media_manipulator?sslmode=disable")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if target != "media_manipulator" {
		t.Errorf("unexpected target %q", target)
	}
	if host != "localhost:5432" {
		t.Errorf("unexpected host %q", host)
	}
}

func TestDeriveAdminURL(t *testing.T) {
	got, err := deriveAdminURL("postgres://postgres:postgres@localhost:5432/media_manipulator?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "/postgres") {
		t.Errorf("admin URL should target postgres db, got %q", got)
	}
}

func TestQuoteIdentifier(t *testing.T) {
	if got := quoteIdentifier(`weird"name`); got != `"weird""name"` {
		t.Errorf("quote escape failed: %q", got)
	}
}
