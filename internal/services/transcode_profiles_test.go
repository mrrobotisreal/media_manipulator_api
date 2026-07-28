package services

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
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

// testRegistry builds the registry from the shipped config defaults, so these
// tests pin the published matrix rather than a set of literals invented here.
func testRegistry(t *testing.T) *tiers.Registry {
	t.Helper()
	return tiers.New(config.Load())
}

func accessFor(t *testing.T, tier tiers.Tier) LadderAccess {
	t.Helper()
	return LadderAccessFor(tier, testRegistry(t))
}

// TestLadderAccessThreeTierMatrix is the exact pin of §4.1's ladder row:
// anonymous 360p–720p, free 360p–1080p, premium the full 144p–2160p.
func TestLadderAccessThreeTierMatrix(t *testing.T) {
	want := map[tiers.Tier][]string{
		tiers.TierAnonymous: {"360p", "480p", "720p"},
		tiers.TierFree:      {"360p", "480p", "720p", "1080p"},
		tiers.TierPremium:   {"144p", "240p", "360p", "480p", "720p", "1080p", "2160p"},
	}
	for tier, expected := range want {
		access := accessFor(t, tier)
		// Source tall enough for every rung, so the only thing filtering is tier.
		got := []string{}
		for _, r := range buildQualityRungs(4320, access) {
			if r.Enabled {
				got = append(got, r.Label)
			}
		}
		if strings.Join(got, ",") != strings.Join(expected, ",") {
			t.Errorf("%s: selectable rungs = %v, want %v", tier, got, expected)
		}
	}
}

// A disabled rung must say which tier unlocks it, not just "premium" —
// telling an anonymous visitor that 1080p needs Premium when a free account
// would do is both wrong and a worse conversion prompt.
func TestBuildQualityRungsDisabledReasonNamesTheUnlockingTier(t *testing.T) {
	rungs := buildQualityRungs(4320, accessFor(t, tiers.TierAnonymous))
	byLabel := map[string]models.TranscodeQualityRung{}
	for _, r := range rungs {
		byLabel[r.Label] = r
	}
	if len(rungs) != 7 {
		t.Fatalf("expected 7 rungs got %d", len(rungs))
	}
	if got := byLabel["1080p"].DisabledReason; !strings.Contains(got, "free account") {
		t.Errorf("1080p reason = %q, want it to mention a free account", got)
	}
	if got := byLabel["2160p"].DisabledReason; !strings.Contains(got, "Premium") {
		t.Errorf("2160p reason = %q, want it to mention Premium", got)
	}
	if got := byLabel["144p"].DisabledReason; !strings.Contains(got, "Premium") {
		t.Errorf("144p reason = %q, want it to mention Premium", got)
	}
	for _, label := range []string{"144p", "240p", "1080p", "2160p"} {
		if byLabel[label].Enabled {
			t.Errorf("rung %s should be disabled for an anonymous caller", label)
		}
		if !byLabel[label].PremiumOnly {
			t.Errorf("rung %s should carry the locked flag", label)
		}
	}
	for _, label := range []string{"360p", "480p", "720p"} {
		if !byLabel[label].Enabled || !byLabel[label].Selected {
			t.Errorf("rung %s should be enabled and default-selected", label)
		}
	}
}

func TestBuildQualityRungsLowResSource(t *testing.T) {
	rungs := buildQualityRungs(240, accessFor(t, tiers.TierAnonymous))
	for _, r := range rungs {
		switch r.Label {
		case "360p", "480p", "720p", "1080p", "2160p":
			if r.Enabled {
				t.Errorf("rung %s should be disabled when source=240", r.Label)
			}
			if r.Label == "360p" && !r.SourceTooSmall {
				t.Errorf("360p should be marked sourceTooSmall when source=240")
			}
		}
	}
	// Premium can take the sub-360p source and use the sub-360p rungs.
	premium := buildQualityRungs(240, accessFor(t, tiers.TierPremium))
	for _, r := range premium {
		if (r.Label == "144p" || r.Label == "240p") && !r.Enabled {
			t.Errorf("rung %s should be selectable for Premium at source=240", r.Label)
		}
	}
}

// ValidateSelectedRungs is the enforcement point: the probe response's tooltips
// are UX, but a client can post any label it likes.
func TestValidateSelectedRungsPerTier(t *testing.T) {
	anon := accessFor(t, tiers.TierAnonymous)
	free := accessFor(t, tiers.TierFree)
	premium := accessFor(t, tiers.TierPremium)

	if _, err := ValidateSelectedRungs(240, []string{"360p"}, anon); err == nil {
		t.Error("expected error for a sub-360p source on the anonymous ladder")
	}
	if _, err := ValidateSelectedRungs(360, []string{"720p"}, anon); err == nil {
		t.Error("expected error when 720p exceeds a 360p source")
	}
	if _, err := ValidateSelectedRungs(1080, []string{"1080p"}, anon); err == nil {
		t.Error("expected anonymous to be refused 1080p")
	}
	if _, err := ValidateSelectedRungs(1080, []string{"1080p"}, free); err != nil {
		t.Errorf("expected free to be allowed 1080p, got %v", err)
	}
	if _, err := ValidateSelectedRungs(2160, []string{"2160p"}, free); err == nil {
		t.Error("expected free to be refused 2160p")
	}
	if _, err := ValidateSelectedRungs(2160, []string{"2160p", "144p"}, premium); err != nil {
		t.Errorf("expected premium to be allowed the full ladder, got %v", err)
	}
	if _, err := ValidateSelectedRungs(240, []string{"144p"}, premium); err != nil {
		t.Errorf("expected premium to accept a 240p source, got %v", err)
	}
	if _, err := ValidateSelectedRungs(720, []string{}, anon); err == nil {
		t.Error("expected error for an empty rung list")
	}

	profiles, err := ValidateSelectedRungs(1080, []string{"720p", "360p", "480p"}, anon)
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if len(profiles) != 3 {
		t.Fatalf("expected 3 profiles got %d", len(profiles))
	}
	if profiles[0].Label != "360p" || profiles[1].Label != "480p" || profiles[2].Label != "720p" {
		t.Errorf("expected sorted profiles, got %v %v %v", profiles[0].Label, profiles[1].Label, profiles[2].Label)
	}
}

// AnonymousLadderAccess is the fallback every un-resolved path uses, so it must
// agree with the registry-derived anonymous access exactly. If the two ever
// drift, an unauthenticated code path silently offers the wrong rungs.
func TestAnonymousLadderAccessMatchesRegistry(t *testing.T) {
	fallback := AnonymousLadderAccess()
	derived := accessFor(t, tiers.TierAnonymous)
	if fallback.MinHeight != derived.MinHeight || fallback.MaxHeight != derived.MaxHeight {
		t.Fatalf("fallback bounds %d-%d != derived %d-%d",
			fallback.MinHeight, fallback.MaxHeight, derived.MinHeight, derived.MaxHeight)
	}
	for _, p := range ladderProfiles() {
		if fallback.unlockedBy[p.Label] != derived.unlockedBy[p.Label] {
			t.Errorf("%s: fallback unlockedBy=%q, derived=%q",
				p.Label, fallback.unlockedBy[p.Label], derived.unlockedBy[p.Label])
		}
	}
}

// ApplyLadderAccess is applied more than once to the same report (probe, then
// again in the pipeline). Re-applying must replace the previous answer, never
// accumulate warnings.
func TestApplyLadderAccessIsIdempotentAndReversible(t *testing.T) {
	report := &models.VideoProbeResponse{Height: 240}
	anon := accessFor(t, tiers.TierAnonymous)

	ApplyLadderAccess(report, anon)
	ApplyLadderAccess(report, anon)
	if !report.SourceTooSmall {
		t.Error("expected a 240p source to be too small for the anonymous ladder")
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("expected exactly 1 warning after re-applying, got %d: %v", len(report.Warnings), report.Warnings)
	}

	ApplyLadderAccess(report, accessFor(t, tiers.TierPremium))
	if report.SourceTooSmall {
		t.Error("premium has no 360p floor; the source is not too small")
	}
	if len(report.Warnings) != 0 {
		t.Errorf("expected the stale minimum-height warning to be dropped, got %v", report.Warnings)
	}
}
