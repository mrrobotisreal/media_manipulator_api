package telemetry

import (
	"strings"
	"testing"
)

// MM71: retiring a rebuilt-over proxy expires exactly that object's ledger row.
func TestLedgerFilterS3Key(t *testing.T) {
	f := LedgerFilter{S3Key: "studio/d/s/a_proxy.mp4"}
	if !f.Valid() {
		t.Fatal("an S3 key alone is a valid filter")
	}
	where, args := f.where(2)
	if !strings.Contains(where, "o.kind = 's3_object'") || !strings.Contains(where, "o.locator = $2") || !strings.Contains(where, "o.deleted_at IS NULL") {
		t.Errorf("where = %q", where)
	}
	if len(args) != 1 || args[0] != "studio/d/s/a_proxy.mp4" {
		t.Errorf("args = %v", args)
	}
	if (LedgerFilter{S3Key: "   "}).Valid() {
		t.Error("a blank key must not make a filter valid")
	}
}
