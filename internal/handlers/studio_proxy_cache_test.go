package handlers

import "testing"

// MM70: preview proxies + filmstrips are write-once per asset and re-read on
// every Darkroom seek, so they cache for a week (private); originals keep 1 h.
func TestStudioDerivativeCacheControl(t *testing.T) {
	cases := map[string]string{
		"proxy":          "private, max-age=604800",
		"proxy_reversed": "private, max-age=604800",
		"sprite":         "private, max-age=604800",
		"file":           "private, max-age=3600",
		"":               "private, max-age=3600",
	}
	for which, want := range cases {
		if got := studioDerivativeCacheControl(which); got != want {
			t.Errorf("studioDerivativeCacheControl(%q) = %q, want %q", which, got, want)
		}
	}
}
