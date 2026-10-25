package tiers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// testRegistry builds the registry from config defaults (no env set), so these
// tests assert against the published tier matrix rather than fixtures.
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	return New(config.Load())
}

func TestResolve(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(24 * time.Hour)

	cases := []struct {
		name     string
		signedIn bool
		user     *models.User
		want     Tier
	}{
		{
			name: "not signed in is anonymous",
			want: TierAnonymous,
		},
		{
			// Signed in but the sync call has not landed yet. A signed-in
			// person must never fall back to the anonymous allowance.
			name:     "signed in with no user row is free",
			signedIn: true,
			want:     TierFree,
		},
		{
			name:     "free user row is free",
			signedIn: true,
			user:     &models.User{Tier: "free"},
			want:     TierFree,
		},
		{
			name:     "premium user row is premium",
			signedIn: true,
			user:     &models.User{Tier: "premium"},
			want:     TierPremium,
		},
		{
			name:     "premium with a future expiry is still premium",
			signedIn: true,
			user:     &models.User{Tier: "premium", TierExpiresAt: &future},
			want:     TierPremium,
		},
		{
			name:     "premium with a lapsed expiry falls back to free",
			signedIn: true,
			user:     &models.User{Tier: "premium", TierExpiresAt: &past},
			want:     TierFree,
		},
		{
			name:     "pro user row is pro",
			signedIn: true,
			user:     &models.User{Tier: "pro", TierExpiresAt: &future},
			want:     TierPro,
		},
		{
			// Same expiry rule as premium: a lapsed Pro period is free.
			name:     "pro with a lapsed expiry falls back to free",
			signedIn: true,
			user:     &models.User{Tier: "pro", TierExpiresAt: &past},
			want:     TierFree,
		},
		{
			// A tier string we do not recognise must degrade, never elevate.
			name:     "unknown tier string degrades to free",
			signedIn: true,
			user:     &models.User{Tier: "enterprise"},
			want:     TierFree,
		},
		{
			name:     "anonymous stored on a user row degrades to free",
			signedIn: true,
			user:     &models.User{Tier: "anonymous"},
			want:     TierFree,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.signedIn, tc.user); got != tc.want {
				t.Fatalf("Resolve = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLimitsAreMonotonic is the guard on the whole pricing story: paying more
// must never buy less. A typo in a config default that inverted two numbers
// would otherwise ship silently. It walks every public tier in All() order
// (anonymous < free < premium < pro, MM39).
func TestLimitsAreMonotonic(t *testing.T) {
	r := testRegistry(t)
	ladder := All()
	if len(ladder) != 4 || ladder[3] != TierPro {
		t.Fatalf("All() = %v, want anonymous, free, premium, pro", ladder)
	}

	type field struct {
		name string
		get  func(Limits) int64
	}
	fields := []field{
		{"OpsPerDay", func(l Limits) int64 { return int64(l.OpsPerDay) }},
		{"AIOpsPerDay", func(l Limits) int64 { return int64(l.AIOpsPerDay) }},
		{"MaxFileBytes", func(l Limits) int64 { return l.MaxFileBytes }},
		{"MaxVideoSeconds", func(l Limits) int64 { return int64(l.MaxVideoSeconds) }},
		{"MaxOutputHeight", func(l Limits) int64 { return int64(l.MaxOutputHeight) }},
		{"MaxLadderHeight", func(l Limits) int64 { return int64(l.MaxLadderHeight) }},
		{"ExportsPerDay", func(l Limits) int64 { return int64(l.ExportsPerDay) }},
		{"StudioVersionAutoKeep", func(l Limits) int64 { return int64(l.StudioVersionAutoKeep) }},
		{"ResultRetentionHours", func(l Limits) int64 { return int64(l.ResultRetentionHours) }},
		// Motion (MM43).
		{"VectorExportsPerDay", func(l Limits) int64 { return unlimitedHigh(l.VectorExportsPerDay) }},
		{"VectorProjects", func(l Limits) int64 { return unlimitedHigh(l.VectorProjects) }},
		{
			// 0 = no duration cap, which sorts above every finite cap.
			"VectorMaxDurationMs",
			func(l Limits) int64 {
				if l.VectorMaxDurationMs == 0 {
					return int64(^uint64(0) >> 1)
				}
				return int64(l.VectorMaxDurationMs)
			},
		},
		// Raster caps are plain numbers: a tier with no raster format carries 0.
		{"VectorVideo.MaxHeight", func(l Limits) int64 { return int64(l.VectorVideo.MaxHeight) }},
		{"VectorVideo.MaxFps", func(l Limits) int64 { return int64(l.VectorVideo.MaxFps) }},
		{"VectorVideo.MaxSeconds", func(l Limits) int64 { return int64(l.VectorVideo.MaxSeconds) }},
		{
			// -1 means unlimited, which sorts above every finite value.
			"StudioProjects",
			func(l Limits) int64 {
				if l.StudioProjects == Unlimited {
					return int64(^uint64(0) >> 1)
				}
				return int64(l.StudioProjects)
			},
		},
	}

	for _, f := range fields {
		for i := 1; i < len(ladder); i++ {
			lo, hi := f.get(r.For(ladder[i-1])), f.get(r.For(ladder[i]))
			if lo > hi {
				t.Errorf("%s is not non-decreasing: %s=%d > %s=%d", f.name, ladder[i-1], lo, ladder[i], hi)
			}
		}
	}

	bools := []struct {
		name string
		get  func(Limits) bool
	}{
		{"PriorityGPU", func(l Limits) bool { return l.PriorityGPU }},
		{"AdsRemoved", func(l Limits) bool { return l.AdsRemoved }},
	}
	for _, b := range bools {
		for i := 1; i < len(ladder); i++ {
			if b.get(r.For(ladder[i-1])) && !b.get(r.For(ladder[i])) {
				t.Errorf("%s is not non-decreasing: %s has it, %s does not", b.name, ladder[i-1], ladder[i])
			}
		}
	}
}

// Pro (MM39, design PRO_TIER.md §2.3): ops 700, AI 120, exports 700, every
// other cap equal to Premium, priority GPU and no ads.
func TestProRow(t *testing.T) {
	r := testRegistry(t)
	pro, premium := r.For(TierPro), r.For(TierPremium)
	if pro.OpsPerDay != 700 || pro.AIOpsPerDay != 120 || pro.ExportsPerDay != 700 {
		t.Fatalf("pro allowances = ops %d / ai %d / exports %d, want 700 / 120 / 700", pro.OpsPerDay, pro.AIOpsPerDay, pro.ExportsPerDay)
	}
	same := premium
	same.OpsPerDay, same.AIOpsPerDay, same.ExportsPerDay = pro.OpsPerDay, pro.AIOpsPerDay, pro.ExportsPerDay
	if pro != same {
		t.Fatalf("pro must equal premium apart from ops/ai/exports:\n pro     %+v\n premium %+v", pro, premium)
	}
	if !pro.PriorityGPU || !pro.AdsRemoved || pro.RateLimitExempt {
		t.Fatalf("pro flags wrong: %+v", pro)
	}
}

// Every TIER_PRO_* cap that defaults to Premium follows a retuned Premium, so
// the ladder cannot invert when only Premium is changed.
func TestProCapsFollowPremiumAndOverride(t *testing.T) {
	t.Setenv("TIER_PREMIUM_MAX_VIDEO_SECONDS", "9000")
	t.Setenv("TIER_PREMIUM_STUDIO_PROJECTS", "40")
	t.Setenv("TIER_PRO_MAX_FILE_BYTES", "8589934592")
	t.Setenv("TIER_PRO_OPS_PER_DAY", "900")
	r := testRegistry(t)
	pro := r.For(TierPro)
	if pro.MaxVideoSeconds != 9000 || pro.StudioProjects != 40 {
		t.Fatalf("pro must inherit retuned premium caps: %+v", pro)
	}
	if pro.MaxFileBytes != 8589934592 || pro.OpsPerDay != 900 || pro.ExportsPerDay != 900 {
		t.Fatalf("pro overrides ignored: %+v", pro)
	}
}

func TestRankAndPaid(t *testing.T) {
	order := []Tier{TierAnonymous, TierFree, TierPremium, TierPro, TierOperator}
	for i := 1; i < len(order); i++ {
		if Rank(order[i-1]) >= Rank(order[i]) {
			t.Errorf("Rank(%s) >= Rank(%s)", order[i-1], order[i])
		}
	}
	if Rank(Tier("enterprise")) >= Rank(TierAnonymous) {
		t.Error("an unknown tier must rank below anonymous")
	}
	for tier, want := range map[Tier]bool{TierAnonymous: false, TierFree: false, TierPremium: true, TierPro: true, TierOperator: false} {
		if Paid(tier) != want {
			t.Errorf("Paid(%s) = %v", tier, !want)
		}
	}
	if !Valid(TierPro) {
		t.Error("Valid(pro) must be true")
	}
}

// TestPublishedMatrix pins the numbers /pricing shows. If a default changes,
// this test changes with it — deliberately, in the same commit.
func TestPublishedMatrix(t *testing.T) {
	r := testRegistry(t)

	want := map[Tier]Limits{
		TierAnonymous: {
			OpsPerDay: 5, AIOpsPerDay: 1, ExportsPerDay: 5,
			MaxFileBytes: 200 * 1024 * 1024, MaxVideoSeconds: 300,
			MaxOutputHeight: 1080, MaxLadderHeight: 720,
			StudioProjects: 1, StudioVersionAutoKeep: 20, PriorityGPU: false,
			ResultRetentionHours: 24, AdsRemoved: false,
			VectorExportsPerDay: 2, VectorFormats: "svg_animated", VectorProjects: 1,
			VectorMaxDurationMs: 10000,
		},
		TierFree: {
			OpsPerDay: 30, AIOpsPerDay: 5, ExportsPerDay: 30,
			MaxFileBytes: 1024 * 1024 * 1024, MaxVideoSeconds: 1200,
			MaxOutputHeight: 1440, MaxLadderHeight: 1080,
			StudioProjects: 5, StudioVersionAutoKeep: 20, PriorityGPU: false,
			ResultRetentionHours: 168, AdsRemoved: false,
			VectorExportsPerDay: 5, VectorProjects: 3,
			VectorFormats: "svg_animated,lottie,import_lottie,import_svg_vectorize",
		},
		TierPremium: {
			OpsPerDay: 300, AIOpsPerDay: 60, ExportsPerDay: 300,
			MaxFileBytes: 5 * 1024 * 1024 * 1024, MaxVideoSeconds: 7200,
			MaxOutputHeight: 2160, MaxLadderHeight: 2160,
			StudioProjects: Unlimited, StudioVersionAutoKeep: 20, PriorityGPU: true,
			ResultRetentionHours: 720, AdsRemoved: true,
			VectorExportsPerDay: 30, VectorProjects: 25,
			VectorFormats: "svg_animated,lottie,react_tsx,js_ts,flutter,gif,import_lottie,import_svg_vectorize",
			VectorVideo:   VectorVideo{MaxHeight: 720, MaxSeconds: 15},
		},
		TierPro: {
			OpsPerDay: 700, AIOpsPerDay: 120, ExportsPerDay: 700,
			MaxFileBytes: 5 * 1024 * 1024 * 1024, MaxVideoSeconds: 7200,
			MaxOutputHeight: 2160, MaxLadderHeight: 2160,
			StudioProjects: Unlimited, StudioVersionAutoKeep: 20, PriorityGPU: true,
			ResultRetentionHours: 720, AdsRemoved: true,
			VectorExportsPerDay: 30, VectorProjects: 25,
			VectorFormats: "svg_animated,lottie,react_tsx,js_ts,flutter,gif,import_lottie,import_svg_vectorize",
			VectorVideo:   VectorVideo{MaxHeight: 720, MaxSeconds: 15},
		},
	}

	for tier, expected := range want {
		if got := r.For(tier); got != expected {
			t.Errorf("limits for %q:\n got  %+v\n want %+v", tier, got, expected)
		}
	}
}

// An unrecognised tier must land on the most restrictive setting, never on a
// zero-value Limits (which would read as "no limit at all" to a caller that
// only checks `> 0`).
func TestForUnknownTierFallsBackToAnonymous(t *testing.T) {
	r := testRegistry(t)
	if got, want := r.For(Tier("nonsense")), r.For(TierAnonymous); got != want {
		t.Fatalf("For(unknown) = %+v, want the anonymous limits %+v", got, want)
	}
}

// By default export's allowance equals ops for every tier, so adding the
// explicit knob changed no behaviour.
func TestExportsPerDayDefaultsToOps(t *testing.T) {
	r := testRegistry(t)
	for _, tier := range All() {
		l := r.For(tier)
		if l.ExportsPerDay != l.OpsPerDay {
			t.Errorf("%s: exportsPerDay=%d, want opsPerDay=%d", tier, l.ExportsPerDay, l.OpsPerDay)
		}
	}
	if op := r.For(TierOperator); op.ExportsPerDay != Unlimited {
		t.Errorf("operator exportsPerDay = %d, want Unlimited", op.ExportsPerDay)
	}
}

func TestExportsPerDayOverride(t *testing.T) {
	t.Setenv("TIER_FREE_EXPORTS_PER_DAY", "3")
	t.Setenv("TIER_ANON_EXPORTS_PER_DAY", "2")
	t.Setenv("TIER_PREMIUM_EXPORTS_PER_DAY", "99")
	r := testRegistry(t)
	if got := r.For(TierFree); got.ExportsPerDay != 3 || got.OpsPerDay != 30 {
		t.Errorf("free: %+v", got)
	}
	if got := r.For(TierAnonymous).ExportsPerDay; got != 2 {
		t.Errorf("anon exports = %d", got)
	}
	if got := r.For(TierPremium).ExportsPerDay; got != 99 {
		t.Errorf("premium exports = %d", got)
	}
}

// unlimitedHigh sorts the -1 "unlimited" sentinel above every finite value.
func unlimitedHigh(v int) int64 {
	if v == Unlimited {
		return int64(^uint64(0) >> 1)
	}
	return int64(v)
}

// Motion formats are a cumulative ladder: tier N+1 can do everything tier N
// can (design §3.4 "+ Lottie", "+ code exports"). Operator has every format.
func TestVectorFormatsAreSupersets(t *testing.T) {
	r := testRegistry(t)
	ladder := append(All(), TierOperator)
	for i := 1; i < len(ladder); i++ {
		lo, hi := r.For(ladder[i-1]).VectorFormats, r.For(ladder[i]).VectorFormats
		if !hi.SupersetOf(lo) {
			t.Errorf("%s formats %v are not a superset of %s formats %v", ladder[i], hi.List(), ladder[i-1], lo.List())
		}
	}
	if got := r.For(TierOperator).VectorFormats.List(); len(got) != len(AllVectorFormats()) {
		t.Errorf("operator formats = %v, want all %d", got, len(AllVectorFormats()))
	}
	for _, tier := range ladder {
		if r.For(tier).VectorFormats.Has(VectorFormatSVGStatic) {
			t.Errorf("%s: svg_static is uncounted and must never be in a format set", tier)
		}
	}
}

// An env override can widen a tier but can never make a higher tier lose a
// format the lower one has (the union keeps the ladder).
func TestVectorFormatsOverrideKeepsLadder(t *testing.T) {
	t.Setenv("TIER_FREE_VECTOR_FORMATS", "svg_animated,mp4,bogus")
	t.Setenv("TIER_PREMIUM_VECTOR_FORMATS", "gif")
	r := testRegistry(t)
	free, premium := r.For(TierFree).VectorFormats, r.For(TierPremium).VectorFormats
	if !free.Has(VectorFormatMP4) || free.Has("bogus") || free.Has(VectorFormatLottie) {
		t.Fatalf("free formats = %v", free.List())
	}
	if !premium.Has(VectorFormatGIF) || !premium.Has(VectorFormatMP4) || !premium.Has(VectorFormatSVGAnimated) {
		t.Fatalf("premium must keep free's formats: %v", premium.List())
	}
}

// Pro's Motion row is MM49's; until then it mirrors Premium exactly.
func TestProVectorRowMirrorsPremium(t *testing.T) {
	t.Setenv("TIER_PREMIUM_VECTOR_EXPORTS_PER_DAY", "44")
	r := testRegistry(t)
	pro, premium := r.For(TierPro), r.For(TierPremium)
	if pro.VectorExportsPerDay != 44 || pro.VectorFormats != premium.VectorFormats ||
		pro.VectorProjects != premium.VectorProjects || pro.VectorVideo != premium.VectorVideo ||
		pro.VectorMaxDurationMs != premium.VectorMaxDurationMs {
		t.Fatalf("pro vector row must equal premium's:\n pro     %+v\n premium %+v", pro, premium)
	}
}

func TestOperatorVectorRow(t *testing.T) {
	r := testRegistry(t)
	op := r.For(TierOperator)
	if op.VectorExportsPerDay != Unlimited || op.VectorProjects != Unlimited || op.VectorMaxDurationMs != 0 {
		t.Fatalf("operator vector allowances: %+v", op)
	}
	if op.VectorVideo.MaxHeight != DeploymentMaxOutputHeight || op.VectorVideo.MaxFps != DeploymentMaxVectorFps {
		t.Fatalf("operator video cap = %+v, want the deployment ceilings", op.VectorVideo)
	}
}

func TestVectorEnvOverrides(t *testing.T) {
	t.Setenv("TIER_ANON_VECTOR_EXPORTS_PER_DAY", "4")
	t.Setenv("TIER_FREE_VECTOR_PROJECTS", "-1")
	t.Setenv("TIER_PREMIUM_VECTOR_VIDEO_MAX_FPS", "30")
	r := testRegistry(t)
	if got := r.For(TierAnonymous).VectorExportsPerDay; got != 4 {
		t.Errorf("anon vector exports = %d", got)
	}
	if got := r.For(TierFree).VectorProjects; got != Unlimited {
		t.Errorf("free vector projects = %d", got)
	}
	if got := r.For(TierPremium).VectorVideo.MaxFps; got != 30 {
		t.Errorf("premium video fps = %d", got)
	}
}

func TestFormatSetJSON(t *testing.T) {
	set := NewFormatSet("gif", "svg_animated", "gif", "svg_static")
	b, err := json.Marshal(struct {
		F FormatSet `json:"f"`
	}{set})
	if err != nil || string(b) != `{"f":["svg_animated","gif"]}` {
		t.Fatalf("marshal = %s, %v", b, err)
	}
	var back struct {
		F FormatSet `json:"f"`
	}
	if err := json.Unmarshal(b, &back); err != nil || back.F != set {
		t.Fatalf("unmarshal = %q, %v", back.F, err)
	}
	if b, _ := json.Marshal(FormatSet("")); string(b) != "[]" {
		t.Fatalf("empty set = %s, want []", b)
	}
}
