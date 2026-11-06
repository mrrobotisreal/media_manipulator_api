package tiers

import (
	"encoding/json"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func TestStudioFormatSet(t *testing.T) {
	s := NewStudioFormatSet("HEVC", " mp4 ", "av1", "mp4", "aac")
	if s != "mp4,hevc,aac" {
		t.Fatalf("canonical set = %q", s)
	}
	if !s.Has("hevc") || s.Has("mov") || s.Has("av1") {
		t.Error("Has")
	}
	b, _ := json.Marshal(s)
	if string(b) != `["mp4","hevc","aac"]` {
		t.Errorf("json = %s", b)
	}
	var back StudioFormatSet
	if err := json.Unmarshal(b, &back); err != nil || back != s {
		t.Errorf("round trip %q %v", back, err)
	}
	if NewStudioFormatSet().List() == nil || len(StudioFormatSet("").List()) != 0 {
		t.Error("empty list must be non-nil and empty")
	}
}

// The env can add tokens to a tier but never make a higher tier lose one; an
// env Advanced on Free carries up the ladder.
func TestStudioFormatsUnionUpTheLadder(t *testing.T) {
	t.Setenv("TIER_PREMIUM_STUDIO_FORMATS", "mov")
	t.Setenv("TIER_PRO_STUDIO_FORMATS", "")
	t.Setenv("TIER_FREE_STUDIO_ADVANCED", "true")
	t.Setenv("TIER_PREMIUM_STUDIO_ADVANCED", "false")
	r := New(config.Load())
	if !r.For(TierPro).StudioFormats.Has("mov") || r.For(TierPro).StudioFormats.Has("mkv") {
		t.Errorf("pro = %q", r.For(TierPro).StudioFormats)
	}
	if !r.For(TierPremium).StudioAdvanced || !r.For(TierPro).StudioAdvanced {
		t.Error("Advanced granted on Free must carry up the ladder")
	}
	if r.For(TierAnonymous).StudioAdvanced {
		t.Error("anonymous never inherits from above")
	}
}

func TestStudioOperatorHasEverything(t *testing.T) {
	r := New(config.Load())
	op := r.For(TierOperator)
	if op.StudioFormats != NewStudioFormatSet(AllStudioFormats()...) || !op.StudioAdvanced {
		t.Errorf("operator = %q advanced=%v", op.StudioFormats, op.StudioAdvanced)
	}
}

// The decided matrix: anonymous HEVC 403 required free; free MOV 403 required
// premium; premium Advanced passes; free Advanced 403 required premium.
func TestCheckStudioOutputMatrix(t *testing.T) {
	r := New(config.Load())
	cases := []struct {
		tier     Tier
		tokens   []string
		advanced bool
		option   string
		required string
		path     string
	}{
		{TierAnonymous, []string{"mp4", "h264", "aac"}, false, "", "", ""},
		{TierAnonymous, []string{"mp3"}, false, "", "", ""},
		{TierAnonymous, []string{"wav", "pcm_s16le"}, false, "", "", ""},
		{TierAnonymous, []string{"mp4", "hevc", "aac"}, false, "hevc", "free", "signup"},
		{TierFree, []string{"mp4", "hevc", "aac"}, false, "", "", ""},
		{TierFree, []string{"mov", "h264", "aac"}, false, "mov", "premium", "premium"},
		{TierFree, []string{"mkv", "hevc", "aac"}, false, "mkv", "premium", "premium"},
		{TierFree, []string{"mp4", "h264", "aac"}, true, "advanced", "premium", "premium"},
		{TierAnonymous, []string{"mp4", "h264", "aac"}, true, "advanced", "premium", "signup"},
		{TierAnonymous, []string{"mov", "h264", "aac"}, false, "mov", "premium", "signup"},
		{TierPremium, []string{"mkv", "h264", "pcm_s16le"}, true, "", "", ""},
		{TierPro, []string{"mov", "hevc", "pcm_s16le"}, true, "", "", ""},
		{TierOperator, []string{"mkv", "hevc", "aac"}, true, "", "", ""},
	}
	for _, c := range cases {
		ref := CheckStudioOutput(c.tier, r.For(c.tier), c.tokens, c.advanced, r.RequiredFor)
		if c.option == "" {
			if ref != nil {
				t.Errorf("%s %v adv=%v: refused %+v", c.tier, c.tokens, c.advanced, ref)
			}
			continue
		}
		if ref == nil {
			t.Errorf("%s %v adv=%v: allowed, want 403 on %s", c.tier, c.tokens, c.advanced, c.option)
			continue
		}
		if ref.Error != "format_not_in_plan" || ref.LimitKey != "studio_format" || ref.Option != c.option || ref.Required != c.required || ref.UpgradePath != c.path || ref.Message == "" {
			t.Errorf("%s %v: %+v", c.tier, c.tokens, ref)
		}
	}
}
