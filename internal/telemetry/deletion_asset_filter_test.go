package telemetry

import (
	"strings"
	"testing"
)

func TestLedgerFilterAssetID(t *testing.T) {
	f := LedgerFilter{AssetID: "a1"}
	if !f.Valid() {
		t.Fatal("an asset id alone is a valid filter")
	}
	where, args := f.where(2)
	if !strings.Contains(where, "o.kind = 'studio_asset'") || !strings.Contains(where, "o.locator = $2") {
		t.Errorf("where = %q", where)
	}
	if len(args) != 1 || args[0] != "a1" {
		t.Errorf("args = %v", args)
	}
}
