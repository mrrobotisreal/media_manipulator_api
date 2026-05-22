// Package limits implements Redis-backed rate limiting.
//
// We use two strategies:
//
//  1. Token bucket per IP RPS (sliding 1-second window approximation, via
//     INCR + EXPIRE).
//  2. Fixed-window counters per (session|ip, route, hour) for the
//     session/IP-per-hour upload/transcode/analysis limits.
//
// Keys never contain the raw IP — we hash IPs first so a Redis dump doesn't
// leak end-user addresses. Raw IPs are still recorded in Postgres
// `mm_rate_limit_events.ip` for blocked requests (subject to standard audit
// retention), so abuse triage retains the data it needs.
package limits

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/geo"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

const keyPrefix = "media-manipulator:api:ratelimit:v1:"

// Limiter applies rate limits backed by Redis.
type Limiter struct {
	Redis        *redis.Client
	Cfg          *config.Config
	Store        *telemetry.Store
	Metrics      *metrics.Registry
	AuditAllowed bool
	hashSecret   []byte
}

// Decision describes the outcome of a single limiter check.
type Decision struct {
	Allowed           bool
	LimitCount        int
	Remaining         int
	RetryAfterSeconds int
	Scope             string
	KeyHash           string
}

// New constructs a Limiter. When rdb is nil all decisions Allow=true.
func New(rdb *redis.Client, cfg *config.Config, store *telemetry.Store, m *metrics.Registry) *Limiter {
	secret := sha256.Sum256([]byte("mm-api-limiter-key-salt"))
	return &Limiter{
		Redis:        rdb,
		Cfg:          cfg,
		Store:        store,
		Metrics:      m,
		AuditAllowed: cfg.RateLimitAuditAllowed,
		hashSecret:   secret[:],
	}
}

// Enabled reports whether the limiter has the Redis it needs to enforce.
func (l *Limiter) Enabled() bool {
	return l != nil && l.Redis != nil && l.Cfg != nil && l.Cfg.RateLimitEnabled
}

// GlobalIPRPS limits all requests by client IP using a 1-second sliding
// window. Burst is enforced by allowing up to `burst` events per window
// before tripping.
func (l *Limiter) GlobalIPRPS() gin.HandlerFunc {
	if !l.Enabled() {
		return func(c *gin.Context) { c.Next() }
	}
	rps := l.Cfg.RateLimitPerIPRPS
	burst := l.Cfg.RateLimitPerIPBurst
	if rps <= 0 || burst <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	// We approximate "X rps with burst N" by capping events per 1-second
	// window at burst (which gives a peak of burst per second and a long-run
	// average bounded by burst).
	limit := burst
	return func(c *gin.Context) {
		ip := geo.ExtractIP(c)
		if ip == "" {
			c.Next()
			return
		}
		ipHash := l.hash(ip)
		key := keyPrefix + "ip:" + ipHash + ":global"
		decision := l.fixedWindow(c.Request.Context(), key, limit, time.Second)
		decision.Scope = "ip"
		decision.KeyHash = ipHash
		if !decision.Allowed {
			l.deny(c, "ip", "global", "", ip, decision)
			return
		}
		c.Next()
	}
}

// Route applies session+ip per-hour bucket limits for a specific route or
// tool. routeKey appears in the Redis key and in Prometheus labels.
//
// When sessionLimit or ipLimit is zero, that bucket is skipped.
func (l *Limiter) Route(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
	if !l.Enabled() {
		return func(c *gin.Context) { c.Next() }
	}
	if sessionLimit <= 0 && ipLimit <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		// IP per hour first — cheaper to evaluate and a stronger guard
		// against anonymous flooding.
		ip := geo.ExtractIP(c)
		if ipLimit > 0 && ip != "" {
			ipHash := l.hash(ip)
			key := keyPrefix + "ip:" + ipHash + ":" + routeKey + ":hour"
			decision := l.fixedWindow(c.Request.Context(), key, ipLimit, time.Hour)
			decision.Scope = "ip"
			decision.KeyHash = ipHash
			if !decision.Allowed {
				l.deny(c, "ip", routeKey, tool, ip, decision)
				return
			}
			l.allow(c, "ip", routeKey, tool, ip, decision)
		}
		if sessionLimit > 0 {
			fields := requestFields(c)
			if fields != nil && fields.SessionID != "" {
				sHash := l.hash(fields.SessionID)
				key := keyPrefix + "session:" + sHash + ":" + routeKey + ":hour"
				decision := l.fixedWindow(c.Request.Context(), key, sessionLimit, time.Hour)
				decision.Scope = "session"
				decision.KeyHash = sHash
				if !decision.Allowed {
					l.deny(c, "session", routeKey, tool, ip, decision)
					return
				}
				l.allow(c, "session", routeKey, tool, ip, decision)
			}
		}
		c.Next()
	}
}

// fixedWindow implements a fixed-window counter. INCR returns the new value;
// on the first hit we set the TTL with EXPIRE.
func (l *Limiter) fixedWindow(ctx context.Context, key string, limit int, window time.Duration) Decision {
	if l.Redis == nil {
		return Decision{Allowed: true, LimitCount: limit, Remaining: limit, RetryAfterSeconds: 0}
	}
	count, err := l.Redis.Incr(ctx, key).Result()
	if err != nil {
		// Fail-open on Redis errors — never block real users because Redis
		// is down. Operator should set RATE_LIMIT_ENABLED=false and
		// investigate. The error is logged.
		logger.FromContext(ctx).Warn("rate limit redis error", "key", key, "error", err.Error())
		return Decision{Allowed: true, LimitCount: limit, Remaining: limit, RetryAfterSeconds: 0}
	}
	if count == 1 {
		_ = l.Redis.Expire(ctx, key, window).Err()
	}
	remaining := limit - int(count)
	if remaining < 0 {
		remaining = 0
	}
	allowed := int(count) <= limit
	ttl := time.Duration(0)
	if !allowed {
		ttlResult, terr := l.Redis.TTL(ctx, key).Result()
		if terr == nil && ttlResult > 0 {
			ttl = ttlResult
		} else {
			ttl = window
		}
	}
	return Decision{
		Allowed:           allowed,
		LimitCount:        limit,
		Remaining:         remaining,
		RetryAfterSeconds: int(ttl / time.Second),
	}
}

func (l *Limiter) deny(c *gin.Context, scope, route, tool, ip string, d Decision) {
	retryAfter := d.RetryAfterSeconds
	if retryAfter <= 0 {
		retryAfter = 1
	}
	c.Writer.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	l.recordEvent(c, scope, route, tool, ip, d, false)
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error":             "Too many requests. Please slow down and try again shortly.",
		"retryAfterSeconds": retryAfter,
		"scope":             scope,
	})
}

func (l *Limiter) allow(c *gin.Context, scope, route, tool, ip string, d Decision) {
	if !l.AuditAllowed {
		if l.Metrics != nil {
			l.Metrics.RateLimitAllowed(scope, route)
		}
		return
	}
	l.recordEvent(c, scope, route, tool, ip, d, true)
}

func (l *Limiter) recordEvent(c *gin.Context, scope, route, tool, ip string, d Decision, allowed bool) {
	if l.Metrics != nil {
		if allowed {
			l.Metrics.RateLimitAllowed(scope, route)
		} else {
			l.Metrics.RateLimitBlocked(scope, route)
		}
	}
	if l.Store == nil {
		return
	}
	fields := requestFields(c)
	rl := telemetry.RateLimitEvent{
		LimiterKeyHash:    d.KeyHash,
		LimiterScope:      scope,
		Route:             route,
		Tool:              tool,
		Allowed:           allowed,
		LimitCount:        d.LimitCount,
		Remaining:         d.Remaining,
		RetryAfterSeconds: d.RetryAfterSeconds,
		IP: func() string {
			if allowed && !l.AuditAllowed {
				return ""
			}
			return ip
		}(),
	}
	if fields != nil {
		rl.VisitorID = fields.VisitorID
		rl.SessionID = fields.SessionID
		rl.RequestID = fields.RequestID
	}
	go l.Store.InsertRateLimitEvent(context.Background(), rl)
}

// hash returns a stable opaque key suitable for Redis. The salt is a fixed
// constant — the goal is non-reversibility within a snapshot, not cryptographic
// secrecy.
func (l *Limiter) hash(input string) string {
	h := sha256.New()
	h.Write(l.hashSecret)
	h.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))[:24]
}

func requestFields(c *gin.Context) *logger.Fields {
	if c == nil {
		return nil
	}
	v, ok := c.Get(logger.GinKey)
	if !ok {
		return nil
	}
	f, _ := v.(*logger.Fields)
	return f
}

// ErrDisabled is returned by helpers that assume the limiter is on.
var ErrDisabled = errors.New("rate limiter disabled")

// HashIP is exposed for tests.
func (l *Limiter) HashIP(ip string) string {
	if l == nil {
		return ""
	}
	return l.hash(strings.TrimSpace(ip))
}
