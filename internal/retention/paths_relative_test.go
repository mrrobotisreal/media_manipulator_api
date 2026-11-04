package retention

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression (production, 2026-11-04): with the default relative PHOTO_DIR
// ("uploads/photo") IsWithin returned false for every file, so Aperture's
// original/proxy/thumb routes and project export/heal all failed.
func TestIsWithinRelativeRoot(t *testing.T) {
	// Resolve symlinks (macOS /var -> /private/var) so the absolute cases
	// compare against the same spelling Getwd returns after Chdir.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	root := filepath.Join("uploads", "photo", "p1")
	cases := []struct {
		cand, root string
		want       bool
	}{
		{filepath.Join(root, "original.jpg"), root, true},
		{filepath.Join(root, "masks", "m.png"), root, true},
		{root, root, true},
		{filepath.Join(root, "..", "p2", "original.jpg"), root, false},
		{filepath.Join(root, "..", "..", "..", "etc", "passwd"), root, false},
		{filepath.Join(dir, root, "proxy.jpg"), root, true},                // absolute candidate, relative root
		{filepath.Join(root, "thumb.jpg"), filepath.Join(dir, root), true}, // relative candidate, absolute root
		{filepath.Join("uploads", "photo", "p10", "x.jpg"), root, false},   // sibling with a shared prefix
	}
	for _, tc := range cases {
		if got := IsWithin(tc.cand, tc.root); got != tc.want {
			t.Errorf("IsWithin(%q, %q) = %v, want %v", tc.cand, tc.root, got, tc.want)
		}
	}
}
