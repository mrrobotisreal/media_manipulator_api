package cleanup

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func TestIsWithin(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "a"), true},
		{filepath.Join(root, "deep", "a", "b"), true},
		{filepath.Join(root, ".."), false},
		{"/etc/passwd", false},
	}
	for _, tc := range cases {
		if got := isWithin(tc.path, root); got != tc.want {
			t.Errorf("isWithin(%q, %q) = %v, want %v", tc.path, root, got, tc.want)
		}
	}
}

// TestSweepRootReapsWholeJobDirIncludingKeptResults pins the behaviour MM05
// relies on: an expired job dir is removed whole, so the kept per-model
// restoration results (<job>/results/...) go away with the tarball.
func TestSweepRootReapsWholeJobDirIncludingKeptResults(t *testing.T) {
	root := t.TempDir()
	job := filepath.Join(root, "job-1")
	if err := os.MkdirAll(filepath.Join(job, "results", "realesrgan"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"restoration_results.tar.gz", "results/manifest.json", "results/realesrgan/realesrgan_x4.mp4"} {
		if err := os.WriteFile(filepath.Join(job, filepath.FromSlash(f)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(job, old, old); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Cfg: &config.Config{}, Logger: slog.Default()}
	if _, _, _, _, err := w.sweepRoot(context.Background(), root, time.Hour, true, map[string]struct{}{}, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(job); !os.IsNotExist(err) {
		t.Fatalf("job dir (with results/) should be reaped, stat err=%v", err)
	}
}
