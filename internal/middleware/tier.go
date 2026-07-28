package middleware

import (
	"log/slog"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// CtxTierResolver is the gin context key holding the per-request, lazily
// memoised tier resolver. Read it through ResolveTier, never by key.
const CtxTierResolver = "mm_tier_resolver"

// TierResolution is everything a downstream handler needs to know about who is
// calling and what they are allowed to do.
type TierResolution struct {
	Tier   tiers.Tier
	Limits tiers.Limits
	// User is the mm_users row behind the request, or nil for an anonymous
	// caller (or a signed-in one whose sync has not landed yet).
	User *models.User
}

// TierConfig is everything TierContext needs that is not per-request. Store may
// be nil — the resolution then degrades to "signed in ⇒ free", which is the
// correct fail-open answer when the ledger is unavailable.
type TierConfig struct {
	Store    QuotaStore
	Registry *tiers.Registry
	Logger   *slog.Logger
}

// tierResolver memoises one resolution for the lifetime of one request. The
// mm_users lookup is deferred until something actually asks, so a request that
// never consults its tier — most GETs — never touches the database, and a
// request that asks twice (quota middleware, then a handler) pays once.
type tierResolver struct {
	cfg  TierConfig
	once sync.Once
	res  TierResolution
}

// TierContext attaches a lazy tier resolver to every request on the group it is
// installed on.
//
// Like the quota dispatcher, it must be installed on the group BEFORE any route
// is registered — gin snapshots the middleware slice at registration time (the
// B3 lesson). It must also run AFTER OptionalFirebaseAuth, which is what puts
// the Principal on the context.
//
// When the tier system is switched off (TIER_QUOTA_ENABLED=false) it attaches
// nothing, so ResolveTier reports "unknown" and every caller keeps the
// pre-tier, global-config behaviour exactly. That makes the flag a single,
// complete kill switch for tier-differentiated limits, not just for counting.
func TierContext(cfg TierConfig, enabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !enabled || cfg.Registry == nil {
			c.Next()
			return
		}
		c.Set(CtxTierResolver, &tierResolver{cfg: cfg})
		c.Next()
	}
}

// ResolveTier returns the caller's tier and limits, resolving them on first
// use and reusing the answer for the rest of the request.
//
// ok is false when no resolver is installed — the tier system is disabled, or
// the route is outside the /api group. Callers MUST treat that as "no
// tier-derived limit applies" and fall back to the global configuration, never
// as "anonymous": a zero-value Limits would read as a 0-byte upload cap.
func ResolveTier(c *gin.Context) (TierResolution, bool) {
	if c == nil {
		return TierResolution{}, false
	}
	v, exists := c.Get(CtxTierResolver)
	if !exists {
		return TierResolution{}, false
	}
	r, ok := v.(*tierResolver)
	if !ok {
		return TierResolution{}, false
	}
	r.once.Do(func() { r.res = r.cfg.resolveNow(c) })
	return r.res, true
}

// resolveNow does the identity → account row → tier walk. A lookup failure
// degrades to the unsynced path rather than blocking the request.
func (cfg TierConfig) resolveNow(c *gin.Context) TierResolution {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var user *models.User
	principal, signedIn := PrincipalFrom(c)
	if signedIn && cfg.Store != nil && cfg.Store.Enabled() {
		u, err := cfg.Store.UserByFirebaseUID(c.Request.Context(), principal.UID)
		if err != nil {
			logger.Warn("tier: user lookup failed, treating as unsynced", "error", err.Error())
		} else {
			user = u
		}
	}

	tier := tiers.Resolve(signedIn, user)
	return TierResolution{Tier: tier, Limits: cfg.Registry.For(tier), User: user}
}
