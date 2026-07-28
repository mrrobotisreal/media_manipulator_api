package retention

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteThenRead(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, 30*24*time.Hour); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, ok := Read(dir)
	if !ok {
		t.Fatal("expected a marker to be found")
	}
	if got != 30*24*time.Hour {
		t.Errorf("got %v, want 720h", got)
	}
}

// A directory the pipeline has not created yet still gets its marker — the
// handler knows the tier long before the output directory exists.
func TestWriteCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job-123")
	if err := Write(dir, 7*24*time.Hour); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, ok := Read(dir); !ok {
		t.Error("expected the marker to be readable after creating the directory")
	}
}

// No marker means "use the global retention", which is the pre-tier behaviour.
// Getting this wrong in the other direction would delete artifacts early.
func TestReadMissingReportsNotFound(t *testing.T) {
	if _, ok := Read(t.TempDir()); ok {
		t.Error("expected no marker in an empty directory")
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	for _, body := range []string{"", "  ", "abc", "0", "-5", "12.5"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, MarkerName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := Read(dir); ok {
			t.Errorf("marker %q should have been rejected", body)
		}
	}
}

// A corrupt marker must not be able to pin storage indefinitely.
func TestReadCapsAbsurdValues(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MarkerName), []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := Read(dir)
	if !ok {
		t.Fatal("expected the marker to be read")
	}
	if got != 24*365*time.Hour {
		t.Errorf("got %v, want the one-year cap", got)
	}
}

// Sub-hour windows round up rather than to zero — a marker of "0 hours" would
// be read back as absent and silently fall through to the global retention.
func TestWriteRoundsSubHourUp(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, 20*time.Minute); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, ok := Read(dir)
	if !ok || got != time.Hour {
		t.Errorf("got %v/%v, want 1h", got, ok)
	}
}

func TestWriteIgnoresEmptyInput(t *testing.T) {
	if err := Write("", time.Hour); err != nil {
		t.Errorf("empty dir should be a no-op, got %v", err)
	}
	if err := Write(t.TempDir(), 0); err != nil {
		t.Errorf("zero window should be a no-op, got %v", err)
	}
}
