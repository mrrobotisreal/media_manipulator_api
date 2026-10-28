package tiers

import (
	"encoding/json"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// wantVoices is the published voices matrix (design VOICES.md §4.3):
// saved 0/1/5/25, seconds per request 30/60/300/900, AI conversion
// no / 60 s / 5 min / 15 min.
var wantVoices = map[Tier]Limits{
	TierAnonymous: {VoicesSaved: 0, VoicesMaxSeconds: 30, VoicesConvert: false, VoicesConvertMaxSeconds: 0},
	TierFree:      {VoicesSaved: 1, VoicesMaxSeconds: 60, VoicesConvert: true, VoicesConvertMaxSeconds: 60},
	TierPremium:   {VoicesSaved: 5, VoicesMaxSeconds: 300, VoicesConvert: true, VoicesConvertMaxSeconds: 300},
	TierPro:       {VoicesSaved: 25, VoicesMaxSeconds: 900, VoicesConvert: true, VoicesConvertMaxSeconds: 900},
}

// MM77 acceptance 3: the text-length cap is VoicesMaxSeconds × 18 per tier.
func TestVoicesTextCapPerTier(t *testing.T) {
	r := testRegistry(t)
	for tier, want := range map[Tier]int{TierAnonymous: 540, TierFree: 1080, TierPremium: 5400, TierPro: 16200} {
		if got := r.For(tier).VoicesTextCap(); got != want {
			t.Errorf("%s text cap = %d want %d", tier, got, want)
		}
	}
	if (Limits{}).VoicesTextCap() != 0 {
		t.Error("no seconds, no cap value")
	}
	op := r.For(TierOperator)
	if op.VoicesTextCap() != DeploymentMaxVoiceSeconds*VoiceCharsPerSecond || op.VoicesTextCap() > 20000 {
		t.Errorf("operator text cap %d must stay under the service's 20000-char backstop", op.VoicesTextCap())
	}
}

func TestVoicesLimitsAreMonotonic(t *testing.T) {
	r := testRegistry(t)
	ladder := All()
	for i := 1; i < len(ladder); i++ {
		lo, hi := r.For(ladder[i-1]), r.For(ladder[i])
		if lo.VoicesSaved > hi.VoicesSaved || lo.VoicesMaxSeconds > hi.VoicesMaxSeconds ||
			lo.VoicesConvertMaxSeconds > hi.VoicesConvertMaxSeconds || (lo.VoicesConvert && !hi.VoicesConvert) {
			t.Errorf("voices not non-decreasing between %s and %s: %+v / %+v", ladder[i-1], ladder[i], lo, hi)
		}
	}
}

func TestOperatorVoicesRow(t *testing.T) {
	op := testRegistry(t).For(TierOperator)
	if op.VoicesSaved != Unlimited || !op.VoicesConvert || op.VoicesMaxSeconds != DeploymentMaxVoiceSeconds {
		t.Fatalf("operator voices: %+v", op)
	}
	if op.VoicesConvertMaxSeconds != config.Load().MaxVideoDurationSeconds {
		t.Fatalf("operator convert ceiling: %d", op.VoicesConvertMaxSeconds)
	}
}

// Env overrides land, and anonymous can never be given saved voices.
func TestVoicesEnvOverrides(t *testing.T) {
	t.Setenv("TIER_FREE_VOICES_SAVED", "2")
	t.Setenv("TIER_FREE_VOICES_MAX_SECONDS", "90")
	t.Setenv("TIER_FREE_VOICES_CONVERT", "false")
	t.Setenv("TIER_PRO_VOICES_CONVERT_MAX_SECONDS", "1200")
	t.Setenv("TIER_ANON_VOICES_SAVED", "3")
	t.Setenv("TIER_ANON_VOICES_MAX_SECONDS", "15")
	r := New(config.Load())
	free := r.For(TierFree)
	if free.VoicesSaved != 2 || free.VoicesMaxSeconds != 90 || free.VoicesConvert || free.VoicesConvertMaxSeconds != 0 {
		t.Errorf("free overrides: %+v", free)
	}
	if r.For(TierPro).VoicesConvertMaxSeconds != 1200 {
		t.Errorf("pro convert seconds: %+v", r.For(TierPro))
	}
	anon := r.For(TierAnonymous)
	if anon.VoicesSaved != 0 || anon.VoicesMaxSeconds != 15 {
		t.Errorf("anonymous saved must stay 0, seconds overridable: %+v", anon)
	}
}

// /api/tiers serialises Limits directly: the four fields must be in the JSON.
func TestVoicesLimitsJSON(t *testing.T) {
	raw, err := json.Marshal(testRegistry(t).For(TierFree))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for k, want := range map[string]any{"voicesSaved": 1.0, "voicesMaxSeconds": 60.0, "voicesConvert": true, "voicesConvertMaxSeconds": 60.0} {
		if m[k] != want {
			t.Errorf("%s = %v want %v (%s)", k, m[k], want, raw)
		}
	}
}
