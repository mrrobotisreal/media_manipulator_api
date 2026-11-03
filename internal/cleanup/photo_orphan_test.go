package cleanup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func photoWorker(uploads, photo string, orphans OrphanLedger) *Worker {
	w := NewWorker(&config.Config{
		UploadDir: uploads, UploadRetention: 24 * time.Hour, PhotoDir: photo,
		CleanupAuditMaxPathsPerRun: 100, RetentionSweepDryRun: false,
	}, nil, nil, quiet(), nil)
	w.Orphans = orphans
	return w
}

// MM95: a PHOTO_DIR project directory with no live photo_project row is an
// orphan after 24 h; one with a live row survives however old it is.
func TestPhotoOrphanWalkerKeepsLiveAndReapsOrphans(t *testing.T) {
	photo := t.TempDir()
	live := agedDir(t, photo, "live-project", 72*time.Hour)
	orphan := agedDir(t, photo, "orphan-project", 72*time.Hour)
	young := agedDir(t, photo, "young-project", time.Hour)

	photoWorker(t.TempDir(), photo, &fakeOrphans{live: map[string]bool{"live-project": true}}).tick(context.Background())

	if !exists(live) {
		t.Error("a project with a live ledger row must survive, however old")
	}
	if exists(orphan) {
		t.Error("a project directory with no live row is reaped after 24 h")
	}
	if !exists(young) {
		t.Error("a young directory (ingest in flight) is never reaped")
	}
}

// PHOTO_DIR defaults to uploads/photo, inside the upload root: the upload walker
// must not reap the photo root itself, and without the ledger the photo root is
// skipped altogether (fail safe).
func TestPhotoOrphanWalkerProtectsRootInsideUploads(t *testing.T) {
	uploads := t.TempDir()
	photo := filepath.Join(uploads, "photo")
	proj := agedDir(t, photo, "p1", 72*time.Hour)
	// Age the photo root itself past the upload window.
	old := time.Now().Add(-72 * time.Hour)
	if err := chtimes(photo, old); err != nil {
		t.Fatal(err)
	}

	photoWorker(uploads, photo, &fakeOrphans{live: map[string]bool{"p1": true}}).tick(context.Background())
	if !exists(photo) || !exists(proj) {
		t.Fatal("the upload walker must never reap PHOTO_DIR itself")
	}

	// No ledger: nothing under the photo root may be touched.
	w := photoWorker(uploads, photo, nil)
	w.tick(context.Background())
	if !exists(proj) {
		t.Fatal("without a ledger the photo root is skipped, not reaped")
	}
}

func chtimes(path string, when time.Time) error { return os.Chtimes(path, when, when) }
