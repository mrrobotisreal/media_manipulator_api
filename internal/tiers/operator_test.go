package tiers

import (
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func operatorRegistry(t *testing.T) *Registry {
	t.Helper()
	cfg := config.Load()
	cfg.OperatorEmails = []string{"owner@example.com", "partner@example.com"}
	return New(cfg)
}

// The allowlist is the only door, and it has two locks: the address must be on
// the list AND the token must assert the address is verified. Either one alone
// is not enough — that is what stops a stranger registering the owner's
// address with a password and inheriting the tier.
func TestIsOperator(t *testing.T) {
	r := operatorRegistry(t)

	cases := []struct {
		name     string
		email    string
		verified bool
		want     bool
	}{
		{"listed and verified", "owner@example.com", true, true},
		{"case-insensitive", "Owner@Example.COM", true, true},
		{"surrounding whitespace", "  partner@example.com ", true, true},
		{"listed but unverified", "owner@example.com", false, false},
		{"verified but not listed", "stranger@example.com", true, false},
		{"empty email", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.IsOperator(tc.email, tc.verified); got != tc.want {
				t.Fatalf("IsOperator(%q, %v) = %v, want %v", tc.email, tc.verified, got, tc.want)
			}
		})
	}
}

// With no allowlist configured the tier does not exist at all: nothing can
// resolve to it, verified or otherwise.
func TestIsOperatorEmptyAllowlist(t *testing.T) {
	r := New(config.Load())
	if r.IsOperator("owner@example.com", true) {
		t.Fatal("an empty OPERATOR_EMAILS must never grant the operator tier")
	}
	if r.OperatorCount() != 0 {
		t.Fatalf("OperatorCount = %d, want 0", r.OperatorCount())
	}
}

func TestResolveForOperator(t *testing.T) {
	r := operatorRegistry(t)

	// An operator is an operator regardless of the mm_users row — a stale free
	// row, a premium row, or no row at all.
	for _, user := range []*models.User{nil, {Tier: "free"}, {Tier: "premium"}} {
		if got := r.ResolveFor(true, "owner@example.com", true, user); got != TierOperator {
			t.Errorf("ResolveFor(operator, user=%+v) = %v, want operator", user, got)
		}
	}

	// Everyone else resolves exactly as Resolve does.
	if got := r.ResolveFor(true, "stranger@example.com", true, &models.User{Tier: "premium"}); got != TierPremium {
		t.Errorf("non-operator premium = %v, want premium", got)
	}
	if got := r.ResolveFor(true, "owner@example.com", false, &models.User{Tier: "free"}); got != TierFree {
		t.Errorf("unverified operator address = %v, want free", got)
	}
	// Not signed in is anonymous even if a spoofed email were somehow passed:
	// the identity must come from a verified token.
	if got := r.ResolveFor(false, "owner@example.com", true, nil); got != TierAnonymous {
		t.Errorf("signed-out = %v, want anonymous", got)
	}
}

// A database row that somehow says 'operator' must not be honoured. The tier
// is derived from the token on every request, never read from storage.
func TestResolveIgnoresStoredOperator(t *testing.T) {
	if got := Resolve(true, &models.User{Tier: string(TierOperator)}); got != TierFree {
		t.Fatalf("Resolve(stored operator) = %v, want free", got)
	}
	if got := operatorRegistry(t).ResolveFor(true, "stranger@example.com", true, &models.User{Tier: string(TierOperator)}); got != TierFree {
		t.Fatalf("ResolveFor(stored operator, not allowlisted) = %v, want free", got)
	}
}

// Operator is Premium with the allowances removed — never less than Premium
// on any axis, and uncounted on the ones the quota middleware counts.
func TestOperatorLimits(t *testing.T) {
	cfg := config.Load()
	cfg.OperatorEmails = []string{"owner@example.com"}
	r := New(cfg)
	op, premium := r.For(TierOperator), r.For(TierPremium)

	if op.OpsPerDay != Unlimited || op.AIOpsPerDay != Unlimited || op.StudioProjects != Unlimited {
		t.Errorf("operator allowances must be Unlimited, got ops=%d ai=%d projects=%d", op.OpsPerDay, op.AIOpsPerDay, op.StudioProjects)
	}
	if !op.RateLimitExempt {
		t.Error("operator must be exempt from the per-route hourly buckets")
	}
	if premium.RateLimitExempt {
		t.Error("premium must NOT be exempt from the per-route hourly buckets")
	}
	if op.MaxFileBytes != cfg.MaxFileSize {
		t.Errorf("operator MaxFileBytes = %d, want the deployment ceiling %d", op.MaxFileBytes, cfg.MaxFileSize)
	}
	if op.MaxFileBytes < premium.MaxFileBytes {
		t.Errorf("operator file cap %d is below premium's %d", op.MaxFileBytes, premium.MaxFileBytes)
	}
	if op.MaxVideoSeconds != cfg.MaxVideoDurationSeconds {
		t.Errorf("operator MaxVideoSeconds = %d, want the deployment ceiling %d", op.MaxVideoSeconds, cfg.MaxVideoDurationSeconds)
	}
	if op.MaxOutputHeight < premium.MaxOutputHeight || op.MaxLadderHeight < premium.MaxLadderHeight ||
		op.ResultRetentionHours < premium.ResultRetentionHours || !op.AdsRemoved || !op.PriorityGPU {
		t.Errorf("operator must inherit every premium capability, got %+v", op)
	}
}

// The public matrix never advertises the operator tier.
func TestAllExcludesOperator(t *testing.T) {
	for _, tier := range All() {
		if tier == TierOperator {
			t.Fatal("tiers.All() must not include operator — it is not a plan")
		}
	}
}

// Eligibility is the explanation behind IsOperator, for the account page.
func TestEligibility(t *testing.T) {
	r := operatorRegistry(t)
	cases := []struct {
		email    string
		verified bool
		want     OperatorEligibility
	}{
		{"owner@example.com", true, OperatorEligibilityGranted},
		{"OWNER@example.com", false, OperatorEligibilityUnverified},
		{"stranger@example.com", true, OperatorEligibilityNone},
		{"", true, OperatorEligibilityNone},
	}
	for _, tc := range cases {
		if got := r.Eligibility(tc.email, tc.verified); got != tc.want {
			t.Errorf("Eligibility(%q, %v) = %q, want %q", tc.email, tc.verified, got, tc.want)
		}
	}
	if got := New(config.Load()).Eligibility("owner@example.com", true); got != OperatorEligibilityNone {
		t.Errorf("empty allowlist: got %q, want none", got)
	}
}
