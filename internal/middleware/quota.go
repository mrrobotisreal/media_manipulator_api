package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// Response headers the UI reads on every gated request so the quota meter can
// stay accurate without a separate poll. All four are in the CORS
// ExposeHeaders list in cmd/api/main.go — without that the browser hides them.
const (
	HeaderQuotaLimit     = "X-MM-Quota-Limit"
	HeaderQuotaRemaining = "X-MM-Quota-Remaining"
	HeaderQuotaReset     = "X-MM-Quota-Reset"
	HeaderTier           = "X-MM-Tier"
)

// QuotaStore is the slice of telemetry.Store the quota middleware needs. An
// interface so the middleware is testable without a database.
type QuotaStore interface {
	Enabled() bool
	UserByFirebaseUID(ctx context.Context, firebaseUID string) (*models.User, error)
	IncrementUsage(ctx context.Context, subjectKind models.UsageSubjectKind, subjectKey, metric string, day time.Time) (int64, error)
}

// QuotaConfig is everything RequireQuota needs that is not per-request.
type QuotaConfig struct {
	Store    QuotaStore
	Registry *tiers.Registry
	// Enabled turns the whole mechanism off (TIER_QUOTA_ENABLED=false).
	Enabled bool
	// IPHashSecret salts the hash of a caller's IP so the ledger never stores
	// an address. Mirrors internal/limits/limiter.go's approach.
	IPHashSecret []byte
	Logger       *slog.Logger
}

// quotaSubject is who a request is counted against.
type quotaSubject struct {
	kind models.UsageSubjectKind
	key  string
}

// resolveSubject picks the most stable identifier available.
//
// A signed-in user is counted per account, so the allowance follows them across
// devices. A signed-out visitor is counted per session id, which the client
// already sends on every request. Failing both, a salted hash of the IP — the
// last line, and the reason a fresh incognito window does not reset the
// anonymous allowance for free.
func (q QuotaConfig) resolveSubject(c *gin.Context, user *models.User) quotaSubject {
	if user != nil && user.ID != "" {
		return quotaSubject{models.UsageSubjectUser, user.ID}
	}
	if session := c.GetHeader("X-MM-Session-ID"); session != "" {
		return quotaSubject{models.UsageSubjectSession, session}
	}
	h := sha256.New()
	h.Write(q.IPHashSecret)
	h.Write([]byte(c.ClientIP()))
	return quotaSubject{models.UsageSubjectIP, base64.RawURLEncoding.EncodeToString(h.Sum(nil))[:32]}
}

// limitFor returns the daily allowance for a metric under a tier. bytes_in is
// not a counted metric — it is enforced by MaxBytesReader at the handler — so
// it reports no limit here.
func limitFor(l tiers.Limits, metric string) int64 {
	switch metric {
	case models.MetricOps, models.MetricExport:
		return int64(l.OpsPerDay)
	case models.MetricAIOps:
		return int64(l.AIOpsPerDay)
	default:
		return 0
	}
}

// nextUTCMidnight is when the counter rolls over. Returned to the client so the
// UI can say exactly when the allowance comes back instead of guessing.
func nextUTCMidnight(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
}

// upgradePathFor is what the UI's contextual prompt keys off: an anonymous
// visitor is asked to create a free account, a free account is offered Premium.
func upgradePathFor(t tiers.Tier) string {
	if t == tiers.TierAnonymous {
		return "signup"
	}
	return "premium"
}

// RequireQuota counts one unit of `metric` against the caller's daily
// allowance and rejects the request with 429 once the allowance is spent.
//
// It must run AFTER OptionalFirebaseAuth on the same chain, so a signed-in
// caller is counted against their account rather than their IP.
//
// Fails OPEN. If the database is unreachable, or the increment errors, the
// request proceeds and a warning is logged — the same posture as the existing
// fixed-window limiter. A dead database must never break the free product; the
// global per-IP rate limiter is still in front of this as an abuse backstop.
func RequireQuota(cfg QuotaConfig, metric string) gin.HandlerFunc {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(c *gin.Context) {
		if !cfg.Enabled || cfg.Store == nil || !cfg.Store.Enabled() || cfg.Registry == nil {
			c.Next()
			return
		}

		ctx := c.Request.Context()

		// Identity → account row → tier. A lookup failure degrades to the
		// anonymous/free path rather than blocking the request.
		var user *models.User
		principal, signedIn := PrincipalFrom(c)
		if signedIn {
			u, err := cfg.Store.UserByFirebaseUID(ctx, principal.UID)
			if err != nil {
				logger.Warn("quota: user lookup failed, treating as unsynced", "error", err.Error())
			} else {
				user = u
			}
		}
		tier := tiers.Resolve(signedIn, user)
		limits := cfg.Registry.For(tier)
		limit := limitFor(limits, metric)

		now := time.Now().UTC()
		reset := nextUTCMidnight(now)
		c.Header(HeaderTier, string(tier))
		c.Header(HeaderQuotaReset, reset.Format(time.RFC3339))

		if limit <= 0 {
			// No countable allowance for this metric under this tier.
			c.Next()
			return
		}

		subject := cfg.resolveSubject(c, user)
		count, err := cfg.Store.IncrementUsage(ctx, subject.kind, subject.key, metric, now)
		if err != nil {
			logger.Warn("quota: increment failed, failing open",
				"metric", metric, "subject_kind", string(subject.kind), "error", err.Error())
			c.Next()
			return
		}

		remaining := limit - count
		if remaining < 0 {
			remaining = 0
		}
		c.Header(HeaderQuotaLimit, strconv.FormatInt(limit, 10))
		c.Header(HeaderQuotaRemaining, strconv.FormatInt(remaining, 10))

		if count > limit {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "quota_exceeded",
				"tier":        string(tier),
				"metric":      metric,
				"limit":       limit,
				"used":        count,
				"resetsAt":    reset.Format(time.RFC3339),
				"upgradePath": upgradePathFor(tier),
			})
			return
		}

		c.Next()
	}
}
