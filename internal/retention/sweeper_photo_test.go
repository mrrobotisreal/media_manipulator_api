package retention

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

func photoEnv(t *testing.T) (*sweepEnv, string) {
	e := newSweepEnv(t)
	e.cfg.PhotoDir = t.TempDir()
	return e, e.cfg.PhotoDir
}

func photoProjectDir(t *testing.T, root, id string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	for _, sub := range []string{"masks", "bases"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"original.heic", "proxy.jpg", "thumb.jpg", "masks/m1.png"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// MM95: an expired photo_project row removes the whole project directory, then
// the project row (derivatives cascade in SQL), and is stamped.
func TestPhotoLedgerSweepRemovesDirectoryAndRow(t *testing.T) {
	e, root := photoEnv(t)
	dir := photoProjectDir(t, root, "p-1")
	other := photoProjectDir(t, root, "p-2")
	e.db.photoRows["p-1"] = true
	e.db.photoRows["p-2"] = true
	row := e.db.add(telemetry.LedgerRow{Kind: KindPhotoProject, Locator: "p-1", ProjectID: "p-1", OwnerSession: "s", SizeBytes: 99})
	res := e.sweep()
	if exists(dir) {
		t.Error("the project directory should be removed")
	}
	if !exists(other) {
		t.Error("another project must be untouched")
	}
	if e.db.photoRows["p-1"] || !e.db.photoRows["p-2"] {
		t.Errorf("only p-1's row goes: %+v", e.db.photoRows)
	}
	if row.DeletedAt == nil || res.PerKind[KindPhotoProject].Deleted != 1 {
		t.Fatalf("row %+v tally %+v", row.DeletedAt, res.PerKind[KindPhotoProject])
	}
}

// A retry after the directory is already gone still deletes the row.
func TestPhotoLedgerSweepIsIdempotent(t *testing.T) {
	e, _ := photoEnv(t)
	e.db.photoRows["p-9"] = true
	row := e.db.add(telemetry.LedgerRow{Kind: KindPhotoProject, Locator: "p-9", OwnerSession: "s"})
	e.sweep()
	if row.DeletedAt == nil || e.db.photoRows["p-9"] {
		t.Fatalf("missing directory must not block the row: %+v %+v", row.DeletedAt, e.db.photoRows)
	}
}

func TestPhotoLedgerSweepUnexpiredStays(t *testing.T) {
	e, root := photoEnv(t)
	dir := photoProjectDir(t, root, "p-k")
	keep := e.db.add(telemetry.LedgerRow{Kind: KindPhotoProject, Locator: "p-k", OwnerSession: "s", ExpiresAt: sweepT0.Add(3600e9)})
	e.sweep()
	if !exists(dir) || keep.DeletedAt != nil {
		t.Error("an unexpired project must stay")
	}
}

// The deleter refuses locators with more than one segment (and unsafe ones):
// nothing outside PHOTO_DIR/<id> can ever be named.
func TestPhotoLedgerDeleterRefusesUnsafeLocators(t *testing.T) {
	e, root := photoEnv(t)
	victim := photoProjectDir(t, root, "p-v")
	var rows []*fakeRow
	for _, loc := range []string{"p-v/masks", "../p-v", "p-v/../p-v", "a/b/c", ".hidden"} {
		rows = append(rows, e.db.add(telemetry.LedgerRow{Kind: KindPhotoProject, Locator: loc, OwnerSession: "s"}))
	}
	e.sweep()
	if !exists(victim) || !exists(filepath.Join(victim, "masks", "m1.png")) {
		t.Fatal("an unsafe locator must never delete anything")
	}
	for _, r := range rows {
		if r.DeletedAt != nil || r.Attempts != 1 {
			t.Errorf("unsafe locator %q must fail, not delete: %+v", r.Locator, r)
		}
	}
}

func TestPhotoLedgerUnconfiguredRootRefuses(t *testing.T) {
	e := newSweepEnv(t) // PhotoDir unset
	row := e.db.add(telemetry.LedgerRow{Kind: KindPhotoProject, Locator: "p-1", OwnerSession: "s"})
	e.sweep()
	if row.DeletedAt != nil || row.Attempts != 1 {
		t.Fatalf("unset PHOTO_DIR must fail the row: %+v", row)
	}
}

func TestPhotoLedgerRowFailureKeepsRowForRetry(t *testing.T) {
	e, root := photoEnv(t)
	photoProjectDir(t, root, "p-f")
	e.db.failOn["photo-rows"] = errors.New("db down")
	row := e.db.add(telemetry.LedgerRow{Kind: KindPhotoProject, Locator: "p-f", OwnerSession: "s"})
	e.sweep()
	if row.DeletedAt != nil || row.Attempts != 1 {
		t.Fatalf("a failed row delete must be retried: %+v", row)
	}
}

func TestPhotoProjectPath(t *testing.T) {
	root := t.TempDir()
	if got := PhotoProjectPath(root, "0b6f"); got != filepath.Join(root, "0b6f") {
		t.Errorf("good locator: %q", got)
	}
	for _, bad := range []string{"", "a/b", "../x", "..", ".", "/abs", "x/", ".h"} {
		if got := PhotoProjectPath(root, bad); got != "" {
			t.Errorf("%q -> %q, want refused", bad, got)
		}
	}
	if PhotoProjectPath("", "p") != "" {
		t.Error("unset root must refuse")
	}
}
