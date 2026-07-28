package cleanup

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
)

// makeAgedJobDir creates <root>/<id>/ containing one file, then backdates the
// directory so the sweeper sees it as `age` old.
func makeAgedJobDir(t *testing.T, root, id string, age time.Duration, window time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if window > 0 {
		if err := retention.Write(dir, window); err != nil {
			t.Fatal(err)
		}
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
	return dir
}

func quietWorker(t *testing.T, outputDir string, globalRetention time.Duration) *Worker {
	t.Helper()
	return NewWorker(&config.Config{
		OutputDir:                  outputDir,
		OutputRetention:            globalRetention,
		CleanupAuditMaxPathsPerRun: 100,
	}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

// The point of §4.6: with a 24h global window, a Premium job carrying a 30-day
// marker survives a sweep that removes an unmarked job of the same age.
func TestSweepHonoursPerJobRetention(t *testing.T) {
	outputs := t.TempDir()
	premium := makeAgedJobDir(t, outputs, "premium-job", 48*time.Hour, 30*24*time.Hour)
	free := makeAgedJobDir(t, outputs, "free-job", 48*time.Hour, 7*24*time.Hour)
	anonymous := makeAgedJobDir(t, outputs, "anon-job", 48*time.Hour, 24*time.Hour)
	unmarked := makeAgedJobDir(t, outputs, "unmarked-job", 48*time.Hour, 0)

	quietWorker(t, outputs, 24*time.Hour).tick(context.Background())

	for _, dir := range []string{premium, free} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s should have survived: %v", filepath.Base(dir), err)
		}
	}
	for _, dir := range []string{anonymous, unmarked} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s should have been swept, stat err = %v", filepath.Base(dir), err)
		}
	}
}

// Once a marked job passes its own window it is swept like anything else —
// the marker extends retention, it does not grant immortality.
func TestSweepRemovesMarkedJobPastItsOwnWindow(t *testing.T) {
	outputs := t.TempDir()
	stale := makeAgedJobDir(t, outputs, "stale-premium", 40*24*time.Hour, 30*24*time.Hour)

	quietWorker(t, outputs, 24*time.Hour).tick(context.Background())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a 40-day-old job with a 30-day window should have been swept, got %v", err)
	}
}

// A marker shorter than the global window must still be honoured, otherwise
// anonymous artifacts would linger past what the privacy copy promises.
func TestSweepHonoursShorterThanGlobalWindow(t *testing.T) {
	outputs := t.TempDir()
	short := makeAgedJobDir(t, outputs, "short-job", 30*time.Hour, 24*time.Hour)

	quietWorker(t, outputs, 7*24*time.Hour).tick(context.Background())

	if _, err := os.Stat(short); !os.IsNotExist(err) {
		t.Errorf("a 30-hour-old job with a 24-hour window should have been swept, got %v", err)
	}
}
