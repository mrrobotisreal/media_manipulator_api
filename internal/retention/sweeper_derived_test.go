package retention

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM81: a derived_file row (the cached MP3 of a WAV stem) removes exactly that
// file inside outputs/<jobId>/ and is stamped; the result next to it is the
// output_dir row's business.
func TestSweepDerivedFileRemovesOnlyThatFile(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-d", "converted.wav", "derived.mp3")
	row := e.db.add(telemetry.LedgerRow{Kind: KindDerivedFile, Locator: "job-d/derived.mp3", ParentLocator: "job-d", JobID: "job-d", OwnerSession: "s"})
	res := e.sweep()
	if exists(filepath.Join(dir, "derived.mp3")) {
		t.Error("derived.mp3 should be removed")
	}
	if !exists(filepath.Join(dir, "converted.wav")) {
		t.Error("the result must be untouched")
	}
	if row.DeletedAt == nil || res.PerKind[KindDerivedFile].Deleted != 1 {
		t.Fatalf("row %+v tally %+v", row.DeletedAt, res.PerKind[KindDerivedFile])
	}
}

// The usual order: the output_dir sweep already removed the directory; the
// derived row then finds nothing and is done (idempotent).
func TestSweepDerivedFileAfterDirGone(t *testing.T) {
	e := newSweepEnv(t)
	e.jobDir(e.outputs, "job-g", "converted.wav", "derived.mp3")
	out := e.db.add(telemetry.LedgerRow{Kind: KindOutputDir, Locator: "job-g", JobID: "job-g", OwnerSession: "s"})
	der := e.db.add(telemetry.LedgerRow{Kind: KindDerivedFile, Locator: "job-g/derived.mp3", ParentLocator: "job-g", JobID: "job-g", OwnerSession: "s"})
	e.sweep()
	if exists(filepath.Join(e.outputs, "job-g")) {
		t.Error("output dir should be gone")
	}
	if out.DeletedAt == nil || der.DeletedAt == nil {
		t.Fatal("both rows must be stamped")
	}
}

func TestSweepDerivedFileUnexpiredAndUnsafe(t *testing.T) {
	e := newSweepEnv(t)
	dir := e.jobDir(e.outputs, "job-k", "derived.mp3")
	keep := e.db.add(telemetry.LedgerRow{Kind: KindDerivedFile, Locator: "job-k/derived.mp3", JobID: "job-k", OwnerSession: "s",
		ExpiresAt: sweepT0.Add(time.Hour)})
	bad := e.db.add(telemetry.LedgerRow{Kind: KindDerivedFile, Locator: "../job-k/derived.mp3", OwnerSession: "s"})
	e.sweep()
	if !exists(filepath.Join(dir, "derived.mp3")) || keep.DeletedAt != nil {
		t.Error("an unexpired derived file must stay")
	}
	if bad.DeletedAt != nil || bad.Attempts != 1 {
		t.Errorf("an unsafe locator must fail, never delete: %+v", bad)
	}
}

func TestDerivedFilePath(t *testing.T) {
	root := t.TempDir()
	if got := DerivedFilePath(root, "job/derived.mp3"); got != filepath.Join(root, "job", "derived.mp3") {
		t.Errorf("good locator: %q", got)
	}
	for _, bad := range []string{"", "job", "job/", "/derived.mp3", "../x/derived.mp3", "job/../x", "a/b/c", "job/.hidden"} {
		if got := DerivedFilePath(root, bad); got != "" {
			t.Errorf("%q -> %q, want refused", bad, got)
		}
	}
	if DerivedFilePath("", "job/derived.mp3") != "" {
		t.Error("unset root must refuse")
	}
}
