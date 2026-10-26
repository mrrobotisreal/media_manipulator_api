package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM69: the currency columns are nullable, allowlisted to usd|eur, and the
// down migration drops exactly what the up added.
func TestBillingCurrencyMigration(t *testing.T) {
	up, err := os.ReadFile(filepath.Join("migrations", "20261026001_add_billing_currency.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile(filepath.Join("migrations", "20261026001_add_billing_currency.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	u, d := string(up), string(down)
	for _, want := range []string{
		"BEGIN;", "COMMIT;",
		"ALTER TABLE mm_user_settings", "ADD COLUMN IF NOT EXISTS currency text NULL",
		"CHECK (currency IN ('usd', 'eur'))",
		"ALTER TABLE mm_users", "ADD COLUMN IF NOT EXISTS billing_currency text NULL",
		"CHECK (billing_currency IN ('usd', 'eur'))",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("up migration missing %q", want)
		}
	}
	if strings.Contains(u, "NOT NULL") || strings.Contains(u, "DEFAULT") {
		t.Error("currency columns must be nullable with no default: NULL means not chosen / not known")
	}
	for _, want := range []string{"DROP COLUMN IF EXISTS billing_currency", "DROP COLUMN IF EXISTS currency"} {
		if !strings.Contains(d, want) {
			t.Errorf("down migration missing %q", want)
		}
	}
}
