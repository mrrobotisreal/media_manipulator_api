package retention

import (
	"errors"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM104: an expired assistant_conversation row hard-deletes that conversation
// only, touches no disk or S3, and a store failure is retried, not lost.
func TestAssistantConversationLedgerSweep(t *testing.T) {
	e := newSweepEnv(t)
	e.db.convRows["c-1"], e.db.convRows["c-2"] = true, true
	row := e.db.add(telemetry.LedgerRow{Kind: KindAssistantConversation, Locator: "c-1", ProjectID: "c-1", OwnerUserID: "u1"})
	res := e.sweep()
	if e.db.convRows["c-1"] || !e.db.convRows["c-2"] {
		t.Fatalf("only c-1 goes: %+v", e.db.convRows)
	}
	if row.DeletedAt == nil || res.PerKind[KindAssistantConversation].Deleted != 1 {
		t.Fatalf("row must be stamped deleted: %+v", res.PerKind[KindAssistantConversation])
	}
	if len(e.disk.removed) != 0 || len(e.s3.calls) != 0 {
		t.Fatal("a conversation has no files or objects")
	}

	e.db.convRows["c-3"] = true
	e.db.failOn["assistant-row"] = errors.New("db down")
	bad := e.db.add(telemetry.LedgerRow{Kind: KindAssistantConversation, Locator: "c-3", OwnerUserID: "u1"})
	e.sweep()
	if bad.DeletedAt != nil || !e.db.convRows["c-3"] {
		t.Fatal("a failed delete keeps the row for a retry")
	}
}
