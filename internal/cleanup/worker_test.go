package cleanup

import (
	"path/filepath"
	"testing"
)

func TestIsWithin(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "a"), true},
		{filepath.Join(root, "deep", "a", "b"), true},
		{filepath.Join(root, ".."), false},
		{"/etc/passwd", false},
	}
	for _, tc := range cases {
		if got := isWithin(tc.path, root); got != tc.want {
			t.Errorf("isWithin(%q, %q) = %v, want %v", tc.path, root, got, tc.want)
		}
	}
}
