package telemetry

import (
	"strings"
	"testing"
)

// MM77: deleting one voice expires its own ledger row and the reference /
// preview objects registered with the voice id as parent, nothing else.
func TestLedgerFilterVoiceID(t *testing.T) {
	f := LedgerFilter{VoiceID: "v-1"}
	if !f.Valid() {
		t.Fatal("a voice filter is valid on its own")
	}
	where, args := f.where(1)
	want := "((o.kind = 'voice' AND o.locator = $1) OR (o.kind = 's3_object' AND o.parent_locator = $1))"
	if !strings.Contains(where, want) {
		t.Fatalf("where = %s", where)
	}
	if len(args) != 1 || args[0] != "v-1" {
		t.Fatalf("args = %v", args)
	}
	f.UserID = "u-1"
	where, args = f.where(1)
	if !strings.Contains(where, "o.owner_user_id = $1::uuid") || !strings.Contains(where, "o.locator = $2") || len(args) != 2 {
		t.Fatalf("owner + voice: %s %v", where, args)
	}
	if (LedgerFilter{VoiceID: "  "}).Valid() {
		t.Fatal("blank voice id is not a filter")
	}
}
