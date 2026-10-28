package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
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
	// DecrementUsage gives one unit back. Implementations floor at zero and
	// treat "no row for that day" as nothing to refund, not as an error.
	DecrementUsage(ctx context.Context, subjectKind models.UsageSubjectKind, subjectKey, metric string, day time.Time) (int64, error)
}

// CtxQuotaCharge is the gin context key under which RequireQuota leaves the
// unit it spent on this request. Read it through QuotaChargeOf, never by key.
const CtxQuotaCharge = "mm_quota_charge"

// QuotaCharge is one unit of allowance spent by RequireQuota, with everything
// needed to give it back. A charge is refunded when the request is refused
// after the middleware ran (a 4xx from the handler: oversized upload, over-limit
// framerate, unsupported file) or when the job it paid for later fails; in both
// cases nothing was delivered, so nothing should be counted.
type QuotaCharge struct {
	SubjectKind models.UsageSubjectKind
	SubjectKey  string
	Metric      string
	// Day is the UTC day the unit landed on, so a refund that crosses midnight
	// goes back to the day that was charged rather than to a fresh one.
	Day time.Time

	store  QuotaStore
	logger *slog.Logger
	// refunded makes Refund idempotent: the handler's 4xx path and a later
	// pipeline failure can both try, and only the first gives the unit back.
	refunded atomic.Bool
}

// Refund returns the unit to the ledger. Safe to call more than once, from any
// goroutine, and long after the request that charged it has finished (the
// pipeline failure path): it carries its own store reference and uses a fresh
// context. Returns true when this call performed the refund.
func (q *QuotaCharge) Refund(reason string) bool {
	if q == nil || q.store == nil || !q.refunded.CompareAndSwap(false, true) {
		return false
	}
	logger := q.logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := q.store.DecrementUsage(ctx, q.SubjectKind, q.SubjectKey, q.Metric, q.Day); err != nil {
		// Fail open in the user's favour was the charge's posture; a failed
		// refund is the one case where the ledger over-counts, so it is logged
		// at warning rather than silently dropped.
		logger.Warn("quota: refund failed",
			"metric", q.Metric, "subject_kind", string(q.SubjectKind), "reason", reason, "error", err.Error())
		return false
	}
	return true
}

// QuotaChargeOf returns the unit RequireQuota spent on this request, or nil
// when nothing was charged (uncounted tier, quota disabled, store down, or a
// route that is not gated). Handlers that create a job hand the charge to the
// job so a pipeline failure can refund it; handlers that refuse the request do
// not need to touch it, RefundOnRejection does that.
func QuotaChargeOf(c *gin.Context) *QuotaCharge {
	if c == nil {
		return nil
	}
	v, ok := c.Get(CtxQuotaCharge)
	if !ok {
		return nil
	}
	charge, _ := v.(*QuotaCharge)
	return charge
}

// QuotaRefunder is the slice of QuotaCharge a job needs: just the ability to
// give the unit back once. *QuotaCharge satisfies it; a nil interface is a
// no-op everywhere, which is what an uncharged job carries.
type QuotaRefunder interface {
	Refund(reason string) bool
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
	// Analytics receives a `server_quota_rejected` event on every 429. Optional;
	// nil (or an unconfigured emitter) makes it a no-op.
	Analytics *analyticsemit.Emitter
}

// resolution returns the caller's tier, preferring the shared per-request
// resolver TierContext installs so the mm_users row is read once per request
// rather than once per consumer. Falling back to a private resolution keeps
// RequireQuota usable on its own — which is how the tests drive it, and what
// makes it independent of middleware ordering elsewhere.
func (q QuotaConfig) resolution(c *gin.Context) TierResolution {
	if res, ok := ResolveTier(c); ok {
		return res
	}
	return TierConfig{Store: q.Store, Registry: q.Registry, Logger: q.Logger}.resolveNow(c)
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
	kind, key := QuotaSubjectFor(c, user, q.IPHashSecret)
	return quotaSubject{kind, key}
}

// QuotaSubjectFor is resolveSubject for counters kept outside RequireQuota (the
// Motion export counter, MM43), so every allowance is keyed the same way.
func QuotaSubjectFor(c *gin.Context, user *models.User, ipHashSecret []byte) (models.UsageSubjectKind, string) {
	if user != nil && user.ID != "" {
		return models.UsageSubjectUser, user.ID
	}
	if session := c.GetHeader("X-MM-Session-ID"); session != "" {
		return models.UsageSubjectSession, session
	}
	h := sha256.New()
	h.Write(ipHashSecret)
	h.Write([]byte(c.ClientIP()))
	return models.UsageSubjectIP, base64.RawURLEncoding.EncodeToString(h.Sum(nil))[:32]
}

// NextUTCMidnight is when every daily counter rolls over.
func NextUTCMidnight(now time.Time) time.Time { return nextUTCMidnight(now) }

// limitFor returns the daily allowance for a metric under a tier. ops, ai_ops
// and export are three independent counters. bytes_in is
// not a counted metric — it is enforced by MaxBytesReader at the handler — so
// it reports no limit here.
func limitFor(l tiers.Limits, metric string) int64 {
	switch metric {
	case models.MetricOps:
		return int64(l.OpsPerDay)
	case models.MetricExport:
		// Its own allowance (TIER_*_EXPORTS_PER_DAY), counted separately from ops.
		return int64(l.ExportsPerDay)
	case models.MetricAIOps:
		return int64(l.AIOpsPerDay)
	case models.MetricVectorExport:
		// Motion's own allowance (TIER_*_VECTOR_EXPORTS_PER_DAY). Counted by the
		// vector handler itself, transactionally with the vector_exports row,
		// not by RequireQuota; listed here so every reader agrees on the limit.
		return int64(l.VectorExportsPerDay)
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

// emitQuotaRejected sends one `server_quota_rejected` event. Fire-and-forget, and
// the properties mirror the JSON body the caller is about to receive so a report
// and a support ticket describe the same numbers.
func (q QuotaConfig) emitQuotaRejected(c *gin.Context, metric, tier string, limit, used int64) {
	if !q.Analytics.Enabled() || c == nil {
		return
	}
	q.Analytics.Emit(analyticsemit.Event{
		EventName: analyticsemit.ServerQuotaRejected,
		EventTS:   time.Now().UTC(),
		VisitorID: strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")),
		SessionID: strings.TrimSpace(c.GetHeader("X-MM-Session-ID")),
		Context:   analyticsemit.Context("", "", "", tier),
		Properties: map[string]any{
			"metric": metric,
			"tier":   tier,
			"limit":  limit,
			"used":   used,
		},
	})
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

		// Identity → account row → tier, shared with every other consumer on
		// this request so mm_users is read at most once.
		resolution := cfg.resolution(c)
		tier := resolution.Tier
		user := resolution.User
		limit := limitFor(resolution.Limits, metric)

		now := time.Now().UTC()
		reset := nextUTCMidnight(now)
		c.Header(HeaderTier, string(tier))
		c.Header(HeaderQuotaReset, reset.Format(time.RFC3339))

		if limit <= 0 {
			// No countable allowance for this metric under this tier — either
			// the metric is uncounted, or the tier is unlimited (operator,
			// whose OpsPerDay is tiers.Unlimited). Nothing is incremented, so
			// an operator's testing never lands in the usage ledger either.
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
		charge := &QuotaCharge{
			SubjectKind: subject.kind,
			SubjectKey:  subject.key,
			Metric:      metric,
			Day:         now,
			store:       cfg.Store,
			logger:      logger,
		}
		c.Set(CtxQuotaCharge, charge)

		remaining := limit - count
		if remaining < 0 {
			remaining = 0
		}
		c.Header(HeaderQuotaLimit, strconv.FormatInt(limit, 10))
		c.Header(HeaderQuotaRemaining, strconv.FormatInt(remaining, 10))

		if count > limit {
			// Server truth for the exact instant of monetization friction. The UI
			// emits its own `quota_exceeded_shown` when it renders the block, and
			// both are wanted: the client event says the visitor SAW it, this one
			// says it HAPPENED, and the gap between the two counts is how often the
			// 429 lands somewhere the UI never surfaced.
			cfg.emitQuotaRejected(c, metric, string(tier), limit, count)
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

		// The handler has run. A request it refused (any 4xx; a 5xx is our
		// failure, not theirs, and is refunded too) delivered nothing, so the
		// unit goes back. A 2xx that started a job keeps its charge: the job
		// carries it (handlers pass QuotaChargeOf(c) to the job manager) and
		// refunds it itself if the pipeline fails. The over-limit 429 above
		// never reaches this line because the request was aborted there, and
		// that charge is correct: the allowance was spent before it.
		//
		// The X-MM-Quota-Remaining header on such a response still shows the
		// charged value: gin writes headers with the body, so they cannot be
		// corrected here. The UI treats quota headers on a non-2xx response as
		// stale and re-reads /api/account/usage instead.
		if status := c.Writer.Status(); status >= 400 {
			charge.Refund("rejected_" + strconv.Itoa(status))
		}
	}
}
