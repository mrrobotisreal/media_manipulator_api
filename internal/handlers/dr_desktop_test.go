package handlers

import (
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func TestDesktopFileName(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"spaces in name (mac arm64)", "double-raven/desktop/mac/apple/Double Raven Portal-0.1.0-arm64.dmg", "Double Raven Portal-0.1.0-arm64.dmg"},
		{"spaces in name (windows)", "double-raven/desktop/windows/Double Raven Portal Setup 0.1.0.exe", "Double Raven Portal Setup 0.1.0.exe"},
		{"nested prefixes", "a/b/c/d/file.dmg", "file.dmg"},
		{"no slash at all", "file.exe", "file.exe"},
		{"trailing slash", "double-raven/desktop/mac/apple/", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := desktopFileName(tc.key); got != tc.want {
				t.Errorf("desktopFileName(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestDesktopPlatformKeys(t *testing.T) {
	cfg := &config.Config{
		DRDesktopMacArm64Key: "double-raven/desktop/mac/apple/Double Raven Portal-0.1.0-arm64.dmg",
		DRDesktopMacIntelKey: "double-raven/desktop/mac/intel/Double Raven Portal-0.1.0.dmg",
		DRDesktopWindowsKey:  "double-raven/desktop/windows/Double Raven Portal Setup 0.1.0.exe",
	}
	keys := desktopPlatformKeys(cfg)

	want := map[string]string{
		"mac-arm64": cfg.DRDesktopMacArm64Key,
		"mac-intel": cfg.DRDesktopMacIntelKey,
		"windows":   cfg.DRDesktopWindowsKey,
	}
	if len(keys) != len(want) {
		t.Fatalf("platform map has %d entries, want %d", len(keys), len(want))
	}
	for platform, key := range want {
		got, ok := keys[platform]
		if !ok {
			t.Errorf("platform %q missing from allowlist", platform)
			continue
		}
		if got != key {
			t.Errorf("platform %q resolves to %q, want %q", platform, got, key)
		}
	}

	// Unknown platforms must not resolve.
	for _, unknown := range []string{"", "linux", "mac", "windows-arm64", "MAC-ARM64"} {
		if _, ok := keys[unknown]; ok {
			t.Errorf("unknown platform %q unexpectedly resolves", unknown)
		}
	}
}
