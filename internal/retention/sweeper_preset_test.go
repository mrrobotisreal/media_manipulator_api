package retention

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM103: an expired photo_preset row removes the thumbnail file, then the row.
func TestPhotoPresetLedgerSweepRemovesThumbnailAndRow(t *testing.T) {
	e, root := photoEnv(t)
	thumb := filepath.Join(root, "presets", "u1", "pr-1.png")
	other := filepath.Join(root, "presets", "u1", "pr-2.png")
	for _, f := range []string{thumb, other} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.db.presetRows["pr-1"], e.db.presetRows["pr-2"] = true, true
	row := e.db.add(telemetry.LedgerRow{Kind: KindPhotoPreset, Locator: "pr-1", ProjectID: "pr-1", OwnerUserID: "u1"})
	res := e.sweep()
	if exists(thumb) || !exists(other) {
		t.Error("only pr-1's thumbnail goes")
	}
	if e.db.presetRows["pr-1"] || !e.db.presetRows["pr-2"] {
		t.Errorf("only pr-1's row goes: %+v", e.db.presetRows)
	}
	if row.DeletedAt == nil || res.PerKind[KindPhotoPreset].Deleted != 1 {
		t.Fatal("row stamped deleted")
	}
}

func TestPhotoPresetLedgerSweepWithoutThumbnailAndUnsafeLocator(t *testing.T) {
	e, _ := photoEnv(t)
	e.db.presetRows["pr-3"] = true
	row := e.db.add(telemetry.LedgerRow{Kind: KindPhotoPreset, Locator: "pr-3", OwnerUserID: "u1"})
	e.sweep()
	if row.DeletedAt == nil || e.db.presetRows["pr-3"] {
		t.Error("a preset with no thumbnail still deletes its row")
	}
	bad := e.db.add(telemetry.LedgerRow{Kind: KindPhotoPreset, Locator: "../x", OwnerUserID: "u1"})
	e.sweep()
	if bad.DeletedAt != nil {
		t.Error("an unsafe locator must fail, not delete")
	}
}

func TestPhotoPresetThumbPathIsConfined(t *testing.T) {
	if PhotoPresetThumbPath("/r", "../u", "a") != "" || PhotoPresetThumbPath("/r", "u", "a/b") != "" || PhotoPresetThumbPath("", "u", "a") != "" {
		t.Error("unsafe segments or unset root must resolve to empty")
	}
	if got := PhotoPresetThumbPath("/r", "u", "a"); got != "/r/presets/u/a.png" {
		t.Errorf("got %s", got)
	}
}
