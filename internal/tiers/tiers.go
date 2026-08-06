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
)

// All returns the tiers in ascending order of capability. Used by the public
// /api/tiers endpoint so the UI never has to know the list.
func All() []Tier { return []Tier{TierAnonymous, TierFree, TierPremium} }

// Valid reports whether t is a tier this package knows.
func Valid(t Tier) bool {
	switch t {
	case TierAnonymous, TierFree, TierPremium:
		return true
	}
	return false
}

// Limits is the full capability set for one tier.
type Limits struct {
	OpsPerDay   int `json:"opsPerDay"`
	AIOpsPerDay int `json:"aiOpsPerDay"`
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
	BatchEnabled          bool `json:"batchEnabled"`
	PriorityGPU          bool `json:"priorityGpu"`
	ResultRetentionHours int  `json:"resultRetentionHours"`
	AdsRemoved           bool `json:"adsRemoved"`
}

// Unlimited is the sentinel StudioProjects uses.
const Unlimited = -1

// Registry holds the resolved limits for every tier. Built once from config at
// startup so a request never re-reads the environment.
type Registry struct {
	limits map[Tier]Limits
}

// New builds the registry from configuration. The defaults in config.go are the
// published matrix; see the "Tier limits" block there.
func New(cfg *config.Config) *Registry {
	return &Registry{limits: map[Tier]Limits{
		TierAnonymous: {
			OpsPerDay:            cfg.TierAnonOpsPerDay,
			AIOpsPerDay:          cfg.TierAnonAIOpsPerDay,
			MaxFileBytes:         cfg.TierAnonMaxFileBytes,
			MaxVideoSeconds:      cfg.TierAnonMaxVideoSeconds,
			MaxOutputHeight:      cfg.TierAnonMaxOutputHeight,
			MaxLadderHeight:      cfg.TierAnonMaxLadderHeight,
			StudioProjects:        cfg.TierAnonStudioProjects,
			StudioVersionAutoKeep: cfg.TierAnonStudioVersionAutoKeep,
			BatchEnabled:          false,
			PriorityGPU:          false,
			ResultRetentionHours: cfg.TierAnonRetentionHours,
			AdsRemoved:           false,
		},
		TierFree: {
			OpsPerDay:            cfg.TierFreeOpsPerDay,
			AIOpsPerDay:          cfg.TierFreeAIOpsPerDay,
			MaxFileBytes:         cfg.TierFreeMaxFileBytes,
			MaxVideoSeconds:      cfg.TierFreeMaxVideoSeconds,
			MaxOutputHeight:      cfg.TierFreeMaxOutputHeight,
			MaxLadderHeight:      cfg.TierFreeMaxLadderHeight,
			StudioProjects:        cfg.TierFreeStudioProjects,
			StudioVersionAutoKeep: cfg.TierFreeStudioVersionAutoKeep,
			BatchEnabled:          false,
			PriorityGPU:          false,
			ResultRetentionHours: cfg.TierFreeRetentionHours,
			// Free accounts still see ads. Only Premium removes them — that is
			// the deal, and weakening it would cost the AdSense inventory the
			// whole free product is funded by.
			AdsRemoved: false,
		},
		TierPremium: {
			OpsPerDay:            cfg.TierPremiumOpsPerDay,
			AIOpsPerDay:          cfg.TierPremiumAIOpsPerDay,
			MaxFileBytes:         cfg.TierPremiumMaxFileBytes,
			MaxVideoSeconds:      cfg.TierPremiumMaxVideoSeconds,
			MaxOutputHeight:      cfg.TierPremiumMaxOutputHeight,
			MaxLadderHeight:      cfg.TierPremiumMaxLadderHeight,
			StudioProjects:        cfg.TierPremiumStudioProjects,
			StudioVersionAutoKeep: cfg.TierPremiumStudioVersionAutoKeep,
			BatchEnabled:          true,
			PriorityGPU:          true,
			ResultRetentionHours: cfg.TierPremiumRetentionHours,
			AdsRemoved:           true,
		},
	}}
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
	if !Valid(t) || t == TierAnonymous {
		return TierFree
	}
	if t == TierPremium && user.TierExpiresAt != nil && !user.TierExpiresAt.After(time.Now().UTC()) {
		return TierFree
	}
	return t
}
