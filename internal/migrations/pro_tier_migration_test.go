package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM39: the Pro migration widens BOTH tier CHECKs in one file, and its down
// maps pro rows to premium before restoring the narrower CHECKs.
func TestProTierMigration(t *testing.T) {
	dir := filepath.Join("migrations")
	up, err := os.ReadFile(filepath.Join(dir, "20261025001_add_pro_tier.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile(filepath.Join(dir, "20261025001_add_pro_tier.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	u := string(up)
	for _, want := range []string{
		"BEGIN;", "COMMIT;",
		"CHECK (tier IN ('free', 'premium', 'pro'))",
		"CHECK (owner_tier IS NULL OR owner_tier IN ('anonymous', 'free', 'premium', 'pro', 'operator'))",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("up migration missing %q", want)
		}
	}
	if strings.Contains(u, "CHECK (tier IN ('free', 'premium', 'pro', 'operator'))") {
		t.Error("operator must never become storable on mm_users")
	}
	d := string(down)
	for _, pair := range [][2]string{
		{"UPDATE mm_users SET tier = 'premium'", "ADD CONSTRAINT mm_users_tier_check"},
		{"UPDATE mm_conversion_jobs SET owner_tier = 'premium' WHERE owner_tier = 'pro'", "ADD CONSTRAINT mm_conversion_jobs_owner_tier_check"},
	} {
		i, j := strings.Index(d, pair[0]), strings.Index(d, pair[1])
		if i < 0 || j < 0 || i > j {
			t.Errorf("down must run %q before %q", pair[0], pair[1])
		}
	}
	if !strings.Contains(d, "CHECK (tier IN ('free', 'premium'))") ||
		!strings.Contains(d, "CHECK (owner_tier IS NULL OR owner_tier IN ('anonymous', 'free', 'premium', 'operator'))") {
		t.Error("down must restore the pre-Pro CHECKs")
	}
}
