package tiers

import (
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
// would otherwise ship silently.
func TestLimitsAreMonotonic(t *testing.T) {
	r := testRegistry(t)
	anon, free, premium := r.For(TierAnonymous), r.For(TierFree), r.For(TierPremium)

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
		{"ResultRetentionHours", func(l Limits) int64 { return int64(l.ResultRetentionHours) }},
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
		a, fr, p := f.get(anon), f.get(free), f.get(premium)
		if !(a <= fr && fr <= p) {
			t.Errorf("%s is not non-decreasing across tiers: anon=%d free=%d premium=%d", f.name, a, fr, p)
		}
	}

	bools := []struct {
		name     string
		a, fr, p bool
	}{
		{"PriorityGPU", anon.PriorityGPU, free.PriorityGPU, premium.PriorityGPU},
		{"AdsRemoved", anon.AdsRemoved, free.AdsRemoved, premium.AdsRemoved},
	}
	for _, b := range bools {
		if (b.a && !b.fr) || (b.fr && !b.p) {
			t.Errorf("%s is not non-decreasing across tiers: anon=%v free=%v premium=%v", b.name, b.a, b.fr, b.p)
		}
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
		},
		TierFree: {
			OpsPerDay: 30, AIOpsPerDay: 5, ExportsPerDay: 30,
			MaxFileBytes: 1024 * 1024 * 1024, MaxVideoSeconds: 1200,
			MaxOutputHeight: 1440, MaxLadderHeight: 1080,
			StudioProjects: 5, StudioVersionAutoKeep: 20, PriorityGPU: false,
			ResultRetentionHours: 168, AdsRemoved: false,
		},
		TierPremium: {
			OpsPerDay: 300, AIOpsPerDay: 60, ExportsPerDay: 300,
			MaxFileBytes: 5 * 1024 * 1024 * 1024, MaxVideoSeconds: 7200,
			MaxOutputHeight: 2160, MaxLadderHeight: 2160,
			StudioProjects: Unlimited, StudioVersionAutoKeep: 20, PriorityGPU: true,
			ResultRetentionHours: 720, AdsRemoved: true,
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
