// Package tiers is the single source of truth for what each account tier is
// allowed to do.
//
// Every number the product promises — operations per day, file size, video
// length, output resolution, transcode rungs, retention — resolves through
// here. Nothing else may hard-code a limit: the UI reads them from
// GET /api/tiers, /pricing renders that response, and the validators and quota
// middleware call For(). One table, one place to change it.
package tiers

import (
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Tier identifies an account level. Anonymous is not stored anywhere; it is
// what a caller gets when there is no user row behind the request.
type Tier string

const (
	TierAnonymous Tier = "anonymous"
	TierFree      Tier = "free"
	TierPremium   Tier = "premium"
	// TierOperator is the site's own people — the owner and whoever they name
	// in OPERATOR_EMAILS. Nothing is counted, nothing is capped beyond the
	// deployment's physical ceilings, and the per-route abuse buckets are
	// skipped. It is NEVER stored: mm_users.tier cannot hold it, grant-tier
	// cannot grant it, and no client can ask for it. It exists only as the
	// result of Registry.ResolveFor on a request whose Firebase ID token
	// carries a VERIFIED email on the allowlist, re-derived on every request.
	TierOperator Tier = "operator"
)

// All returns the PUBLIC tiers in ascending order of capability. Used by the
// public /api/tiers endpoint so the UI never has to know the list. Operator is
// deliberately absent: it is not a plan anyone can get, so /pricing must not
// render it and the ladder's "unlocked by" copy must never name it.
func All() []Tier { return []Tier{TierAnonymous, TierFree, TierPremium} }

// Valid reports whether t is a tier this package knows.
func Valid(t Tier) bool {
	switch t {
	case TierAnonymous, TierFree, TierPremium, TierOperator:
		return true
	}
	return false
}

// Limits is the full capability set for one tier.
type Limits struct {
	OpsPerDay   int `json:"opsPerDay"`
	AIOpsPerDay int `json:"aiOpsPerDay"`
	// ExportsPerDay is the Content Studio export allowance: its own counter,
	// NOT drawn from OpsPerDay. Defaults equal OpsPerDay per tier.
	ExportsPerDay int `json:"exportsPerDay"`
	// MaxFileBytes caps a single upload.
	MaxFileBytes int64 `json:"maxFileBytes"`
	// MaxVideoSeconds caps source duration.
	MaxVideoSeconds int `json:"maxVideoSeconds"`
	// MaxOutputHeight caps the rendered output (1080 / 1440 / 2160).
	MaxOutputHeight int `json:"maxOutputHeight"`
	// MaxLadderHeight caps the tallest selectable transcode rung.
	MaxLadderHeight int `json:"maxLadderHeight"`
	// StudioProjects is the saved-project cap; -1 means unlimited.
	StudioProjects int `json:"studioProjects"`
	// StudioVersionAutoKeep is how many automatic version-history checkpoints
	// are retained per project (manual versions are never pruned). Uniform
	// across tiers today; a possible future monetization lever (ADR ws/0001).
	StudioVersionAutoKeep int  `json:"studioVersionAutoKeep"`
	PriorityGPU           bool `json:"priorityGpu"`
	ResultRetentionHours  int  `json:"resultRetentionHours"`
	AdsRemoved            bool `json:"adsRemoved"`
	// RateLimitExempt skips the per-route hourly abuse buckets
	// (internal/limits.Limiter.Route). Only the operator tier sets it; the
	// global per-IP RPS guard still applies to everyone because it is DoS
	// protection, not a usage allowance.
	RateLimitExempt bool `json:"rateLimitExempt"`
}

// Unlimited is the sentinel StudioProjects, OpsPerDay and AIOpsPerDay use for
// "no cap". The quota middleware treats a non-positive allowance as
// "nothing to count", and the UI renders it as ∞.
const Unlimited = -1

// Registry holds the resolved limits for every tier. Built once from config at
// startup so a request never re-reads the environment.
type Registry struct {
	limits map[Tier]Limits
	// operators is the lowercased OPERATOR_EMAILS allowlist. Membership is the
	// ONLY way to become TierOperator.
	operators map[string]struct{}
}

// New builds the registry from configuration. The defaults in config.go are the
// published matrix; see the "Tier limits" block there.
func New(cfg *config.Config) *Registry {
	operators := make(map[string]struct{}, len(cfg.OperatorEmails))
	for _, email := range cfg.OperatorEmails {
		if e := strings.ToLower(strings.TrimSpace(email)); e != "" {
			operators[e] = struct{}{}
		}
	}
	r := &Registry{operators: operators, limits: map[Tier]Limits{
		TierAnonymous: {
			OpsPerDay:             cfg.TierAnonOpsPerDay,
			AIOpsPerDay:           cfg.TierAnonAIOpsPerDay,
			ExportsPerDay:         cfg.TierAnonExportsPerDay,
			MaxFileBytes:          cfg.TierAnonMaxFileBytes,
			MaxVideoSeconds:       cfg.TierAnonMaxVideoSeconds,
			MaxOutputHeight:       cfg.TierAnonMaxOutputHeight,
			MaxLadderHeight:       cfg.TierAnonMaxLadderHeight,
			StudioProjects:        cfg.TierAnonStudioProjects,
			StudioVersionAutoKeep: cfg.TierAnonStudioVersionAutoKeep,
			PriorityGPU:           false,
			ResultRetentionHours:  cfg.TierAnonRetentionHours,
			AdsRemoved:            false,
		},
		TierFree: {
			OpsPerDay:             cfg.TierFreeOpsPerDay,
			AIOpsPerDay:           cfg.TierFreeAIOpsPerDay,
			ExportsPerDay:         cfg.TierFreeExportsPerDay,
			MaxFileBytes:          cfg.TierFreeMaxFileBytes,
			MaxVideoSeconds:       cfg.TierFreeMaxVideoSeconds,
			MaxOutputHeight:       cfg.TierFreeMaxOutputHeight,
			MaxLadderHeight:       cfg.TierFreeMaxLadderHeight,
			StudioProjects:        cfg.TierFreeStudioProjects,
			StudioVersionAutoKeep: cfg.TierFreeStudioVersionAutoKeep,
			PriorityGPU:           false,
			ResultRetentionHours:  cfg.TierFreeRetentionHours,
			// Free accounts still see ads. Only Premium removes them — that is
			// the deal, and weakening it would cost the AdSense inventory the
			// whole free product is funded by.
			AdsRemoved: false,
		},
		TierPremium: {
			OpsPerDay:             cfg.TierPremiumOpsPerDay,
			AIOpsPerDay:           cfg.TierPremiumAIOpsPerDay,
			ExportsPerDay:         cfg.TierPremiumExportsPerDay,
			MaxFileBytes:          cfg.TierPremiumMaxFileBytes,
			MaxVideoSeconds:       cfg.TierPremiumMaxVideoSeconds,
			MaxOutputHeight:       cfg.TierPremiumMaxOutputHeight,
			MaxLadderHeight:       cfg.TierPremiumMaxLadderHeight,
			StudioProjects:        cfg.TierPremiumStudioProjects,
			StudioVersionAutoKeep: cfg.TierPremiumStudioVersionAutoKeep,
			PriorityGPU:           true,
			ResultRetentionHours:  cfg.TierPremiumRetentionHours,
			AdsRemoved:            true,
		},
	}}
	// Operator = Premium with every allowance removed. Not configurable by
	// env on purpose: there is no economics to tune here, only "the owner can
	// test everything". The byte and duration caps are the deployment's own
	// physical ceilings, so the UI shows the real number rather than "—", and
	// the handlers' tighter-wins logic resolves to exactly that ceiling.
	operator := r.limits[TierPremium]
	operator.OpsPerDay = Unlimited
	operator.AIOpsPerDay = Unlimited
	operator.ExportsPerDay = Unlimited
	operator.MaxOutputHeight = DeploymentMaxOutputHeight
	operator.MaxFileBytes = cfg.MaxFileSize
	operator.MaxVideoSeconds = cfg.MaxVideoDurationSeconds
	operator.StudioProjects = Unlimited
	operator.RateLimitExempt = true
	r.limits[TierOperator] = operator
	return r
}

// IsOperator reports whether a verified identity is on the operator allowlist.
//
// Both conditions are load-bearing and neither is negotiable:
//
//   - The email must come from a Firebase ID token whose signature the Admin
//     SDK verified (the caller's job — middleware.OptionalFirebaseAuth). This
//     function never sees a request header.
//   - email_verified must be true. Firebase lets anyone create an
//     email/password account with any address they like; verification is what
//     proves they can read mail at it. Without this check, an attacker who
//     registers one of the allowlisted addresses before its owner does would
//     inherit the operator tier. Google sign-in always yields a verified email.
func (r *Registry) IsOperator(email string, emailVerified bool) bool {
	if r == nil || len(r.operators) == 0 || !emailVerified {
		return false
	}
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return false
	}
	_, ok := r.operators[e]
	return ok
}

// OperatorEligibility explains, for ONE identity, where it stands with the
// allowlist. It exists so the account page can say "you are on the list but
// your email is not verified" instead of silently showing the free tier — the
// failure mode that is otherwise indistinguishable from "the variable was not
// set". It is safe to return to the caller: it only ever describes the
// caller's own verified-token email, never the list.
type OperatorEligibility string

const (
	// OperatorEligibilityNone: not on the allowlist (or no allowlist).
	OperatorEligibilityNone OperatorEligibility = "none"
	// OperatorEligibilityUnverified: on the allowlist, but the token's
	// email_verified claim is false. Verifying the address is all that is
	// missing.
	OperatorEligibilityUnverified OperatorEligibility = "unverified"
	// OperatorEligibilityGranted: on the allowlist and verified — the request
	// resolved to TierOperator.
	OperatorEligibilityGranted OperatorEligibility = "granted"
)

// Eligibility is IsOperator with the reason attached. Same inputs, same
// trust requirements (a verified token's claims, never a request header).
func (r *Registry) Eligibility(email string, emailVerified bool) OperatorEligibility {
	if r == nil || len(r.operators) == 0 {
		return OperatorEligibilityNone
	}
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return OperatorEligibilityNone
	}
	if _, listed := r.operators[e]; !listed {
		return OperatorEligibilityNone
	}
	if !emailVerified {
		return OperatorEligibilityUnverified
	}
	return OperatorEligibilityGranted
}

// OperatorCount is how many addresses are on the allowlist, for the startup
// log. The addresses themselves are never logged.
func (r *Registry) OperatorCount() int {
	if r == nil {
		return 0
	}
	return len(r.operators)
}

// ResolveFor is Resolve plus the operator allowlist: a signed-in caller whose
// verified email is on it is TierOperator regardless of what mm_users says,
// and everyone else resolves exactly as before. This is the only function
// that can return TierOperator.
func (r *Registry) ResolveFor(signedIn bool, email string, emailVerified bool, user *models.User) Tier {
	if signedIn && r.IsOperator(email, emailVerified) {
		return TierOperator
	}
	return Resolve(signedIn, user)
}

// For returns the limits for t, falling back to the anonymous tier for an
// unknown value. Never panics: an unrecognised tier string from the database
// must degrade to the most restrictive setting, not to an unbounded one.
func (r *Registry) For(t Tier) Limits {
	if r == nil {
		return Limits{}
	}
	if l, ok := r.limits[t]; ok {
		return l
	}
	return r.limits[TierAnonymous]
}

// Resolve determines the effective tier for a request.
//
//   - Not signed in → anonymous. The caller presented no valid token.
//   - Signed in but no user row → free. The sync call that creates the row may
//     simply not have landed yet, and a signed-in person should never be
//     treated as anonymous.
//   - User row → its tier, unless a premium grant has expired, in which case
//     free. Expiry is evaluated at read time; nothing rewrites the row, so a
//     grant can be extended without repairing state.
//
// `signedIn` is the second return of `middleware.PrincipalFrom(c)`. It is a
// bool rather than a `*middleware.Principal` on purpose: the middleware package
// imports this one (RequireQuota resolves tiers), so taking the Principal type
// here would be an import cycle.
func Resolve(signedIn bool, user *models.User) Tier {
	if !signedIn {
		return TierAnonymous
	}
	if user == nil {
		return TierFree
	}
	t := Tier(user.Tier)
	// Operator is not a storable tier (the mm_users CHECK forbids it), but a
	// row claiming it must still never be honoured: the allowlist is the only
	// path, and it is evaluated in ResolveFor from the verified token.
	if !Valid(t) || t == TierAnonymous || t == TierOperator {
		return TierFree
	}
	if t == TierPremium && user.TierExpiresAt != nil && !user.TierExpiresAt.After(time.Now().UTC()) {
		return TierFree
	}
	return t
}
