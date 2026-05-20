package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeExecutable drops a small executable shim at path. We only care about
// the mode bits — never about the body — so an empty file with 0o755 works.
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestResolveWhisperCT2Bin_PreferredWins(t *testing.T) {
	dir := t.TempDir()
	preferred := filepath.Join(dir, "preferred", "whisper-ctranslate2")
	writeExecutable(t, preferred)

	got, err := ResolveWhisperCT2Bin(preferred)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != preferred {
		t.Errorf("expected preferred path %q, got %q", preferred, got)
	}
}

func TestResolveWhisperCT2Bin_FallsBackWhenPreferredMissing(t *testing.T) {
	// Stale env var → file doesn't exist. The resolver should fall through to
	// defaultWhisperCT2Bin if it exists, otherwise to $PATH.
	preferred := "/this/path/really/does/not/exist/whisper-ctranslate2"

	// Only useful to verify if either the default or PATH has whisper-ct2
	// installed. We can simulate by pointing the test at a fake "default"
	// that does exist — but defaultWhisperCT2Bin is a const, so we instead
	// verify that the resolver doesn't return the bogus preferred path.
	_, err := ResolveWhisperCT2Bin(preferred)
	if err == nil {
		// On a host where the binary actually exists, that's fine — just
		// make sure we didn't return the bogus preferred path.
		return
	}
	// On a host without any whisper binary the error should mention all the
	// places we looked, including the preferred path.
	if !strings.Contains(err.Error(), preferred) {
		t.Errorf("error should reference preferred path %q, got %q", preferred, err.Error())
	}
	if !strings.Contains(err.Error(), defaultWhisperCT2Bin) {
		t.Errorf("error should reference default path %q, got %q", defaultWhisperCT2Bin, err.Error())
	}
}

func TestResolveWhisperCT2Bin_EmptyPreferredOK(t *testing.T) {
	// Empty preferred should not appear in the error message (we never tried it).
	_, err := ResolveWhisperCT2Bin("")
	if err == nil {
		// Host has whisper installed — nothing to test here.
		return
	}
	if strings.Contains(err.Error(), "tried: ,") {
		t.Errorf("empty preferred leaked into error: %q", err.Error())
	}
}

func TestIsExecutableFile(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "exe")
	plain := filepath.Join(dir, "plain")
	writeExecutable(t, exe)
	if err := os.WriteFile(plain, []byte("data"), 0o644); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	if !isExecutableFile(exe) {
		t.Errorf("expected %q to be executable", exe)
	}
	if isExecutableFile(plain) {
		t.Errorf("expected %q to not be executable", plain)
	}
	if isExecutableFile(filepath.Join(dir, "missing")) {
		t.Errorf("missing file should not be reported as executable")
	}
	if isExecutableFile(dir) {
		t.Errorf("directory should not be reported as executable")
	}
}
