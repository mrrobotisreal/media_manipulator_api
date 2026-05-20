package services

import (
	"testing"
)

func TestParseFractionFrameRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"0/0", 0},
		{"30000/1001", 30000.0 / 1001.0},
		{"30/1", 30},
		{"29.97", 29.97},
		{"  60/1 ", 60},
		{"abc", 0},
	}
	for _, c := range cases {
		got := parseFractionFrameRate(c.in)
		if (got == 0) != (c.want == 0) || (c.want != 0 && got != c.want && !floatClose(got, c.want, 1e-6)) {
			t.Errorf("parseFractionFrameRate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func floatClose(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

func TestClassifyMaxQuality(t *testing.T) {
	cases := []struct {
		height int
		want   string
	}{
		{0, "unknown"},
		{100, "sub-144p"},
		{144, "144p"},
		{200, "144p"},
		{240, "240p"},
		{359, "240p"},
		{360, "360p"},
		{480, "480p"},
		{720, "720p"},
		{1080, "1080p"},
		{1440, "1080p"},
		{2160, "2160p"},
		{3840, "2160p"},
	}
	for _, c := range cases {
		if got := classifyMaxQuality(c.height); got != c.want {
			t.Errorf("classifyMaxQuality(%d) = %q, want %q", c.height, got, c.want)
		}
	}
}

func TestBuildQualityRungsFreeUser(t *testing.T) {
	rungs := buildQualityRungs(1080, false)
	if len(rungs) != 7 {
		t.Fatalf("expected 7 rungs got %d", len(rungs))
	}
	for _, r := range rungs {
		switch r.Label {
		case "144p", "240p", "1080p", "2160p":
			if r.Enabled {
				t.Errorf("rung %s should be disabled for free user", r.Label)
			}
			if !r.PremiumOnly && !r.SourceTooSmall {
				t.Errorf("rung %s disabled needs a reason flag", r.Label)
			}
		case "360p", "480p", "720p":
			if !r.Enabled {
				t.Errorf("rung %s should be enabled when source is 1080p", r.Label)
			}
			if !r.Selected {
				t.Errorf("rung %s should be default-selected", r.Label)
			}
		}
	}
}

func TestBuildQualityRungsLowResSource(t *testing.T) {
	rungs := buildQualityRungs(240, false)
	for _, r := range rungs {
		if r.Label == "360p" || r.Label == "480p" || r.Label == "720p" || r.Label == "1080p" || r.Label == "2160p" {
			if r.Enabled {
				t.Errorf("rung %s should be disabled when source=240", r.Label)
			}
			if r.Label == "360p" && !r.SourceTooSmall {
				t.Errorf("360p should be marked sourceTooSmall when source=240")
			}
		}
	}
}

func TestValidateSelectedRungsBelowFreeMin(t *testing.T) {
	if _, err := ValidateSelectedRungs(240, []string{"360p"}, false); err == nil {
		t.Fatalf("expected error for source 240 free user, got nil")
	}
	if _, err := ValidateSelectedRungs(360, []string{"720p"}, false); err == nil {
		t.Fatalf("expected error when 720p > source 360, got nil")
	}
	if _, err := ValidateSelectedRungs(720, []string{"1080p"}, false); err == nil {
		t.Fatalf("expected error for premium rung 1080p, got nil")
	}
	if _, err := ValidateSelectedRungs(720, []string{}, false); err == nil {
		t.Fatalf("expected error for empty rung list, got nil")
	}
	profiles, err := ValidateSelectedRungs(1080, []string{"720p", "360p", "480p"}, false)
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if len(profiles) != 3 {
		t.Fatalf("expected 3 profiles got %d", len(profiles))
	}
	// Should be sorted by height ascending.
	if profiles[0].Label != "360p" || profiles[1].Label != "480p" || profiles[2].Label != "720p" {
		t.Errorf("expected sorted profiles, got %v %v %v", profiles[0].Label, profiles[1].Label, profiles[2].Label)
	}
}
