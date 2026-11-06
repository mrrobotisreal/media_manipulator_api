package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM122: a studio_caption_look ledger row (kept with the account) sweeps only
// when it comes due by a look delete or the account delete: the sweep
// hard-deletes exactly that look's row and nothing else.
func TestStudioCaptionLookLedgerSweepRemovesRow(t *testing.T) {
	e, _ := photoEnv(t)
	e.db.lookRows["lk-1"], e.db.lookRows["lk-2"] = true, true
	row := e.db.add(telemetry.LedgerRow{Kind: KindStudioCaptionLook, Locator: "lk-1", ProjectID: "lk-1", OwnerUserID: "u1"})
	res := e.sweep()
	if e.db.lookRows["lk-1"] || !e.db.lookRows["lk-2"] {
		t.Errorf("only lk-1's row goes: %+v", e.db.lookRows)
	}
	if row.DeletedAt == nil || res.PerKind[KindStudioCaptionLook].Deleted != 1 {
		t.Fatalf("row stamped deleted: %+v / %+v", row, res.PerKind[KindStudioCaptionLook])
	}
}

func TestStudioCaptionLookLedgerSweepRetriesOnFailure(t *testing.T) {
	e, _ := photoEnv(t)
	e.db.lookRows["lk-3"] = true
	e.db.failOn["look-row"] = errors.New("db down")
	row := e.db.add(telemetry.LedgerRow{Kind: KindStudioCaptionLook, Locator: "lk-3", ProjectID: "lk-3", OwnerUserID: "u1"})
	e.sweep()
	if row.DeletedAt != nil || !e.db.lookRows["lk-3"] {
		t.Fatal("a failed row delete must not stamp the ledger row")
	}
	if KindStudioCaptionLook != "studio_caption_look" {
		t.Error("ledger kind mirrors the migration CHECK")
	}
}

// The DELETE /api/studio/caption-looks/:id path: ExpireNow by project id (the
// look id) removes that look's row even though it never expires by time
// (keep_with_account), and leaves the owner's other looks.
func TestStudioCaptionLookExpireNowByProject(t *testing.T) {
	e := newSweepEnv(t)
	e.db.lookRows["lk-a"], e.db.lookRows["lk-b"] = true, true
	never := sweepT0.Add(24 * 365 * 100 * time.Hour)
	e.db.add(telemetry.LedgerRow{Kind: KindStudioCaptionLook, Locator: "lk-a", ProjectID: "lk-a", OwnerUserID: user1, ExpiresAt: never, KeepWithAccount: true})
	e.db.add(telemetry.LedgerRow{Kind: KindStudioCaptionLook, Locator: "lk-b", ProjectID: "lk-b", OwnerUserID: user1, ExpiresAt: never, KeepWithAccount: true})
	if _, err := e.sweeper().ExpireNow(context.Background(), telemetry.LedgerFilter{ProjectID: "lk-a"}); err != nil {
		t.Fatal(err)
	}
	if e.db.lookRows["lk-a"] || !e.db.lookRows["lk-b"] {
		t.Errorf("only lk-a goes: %+v", e.db.lookRows)
	}
	// A normal sweep never touches a keep-with-account look.
	e.sweep()
	if !e.db.lookRows["lk-b"] {
		t.Error("a kept look must survive the time-based sweep")
	}
}
