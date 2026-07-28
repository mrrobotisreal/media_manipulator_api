package handlers

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// AccountHandler serves the account and tier endpoints: who the caller is,
// what their tier allows, how much of today's allowance they have spent, and
// the public tier matrix the pricing page renders from.
type AccountHandler struct {
	store    *telemetry.Store
	registry *tiers.Registry
	cfg      *config.Config
	logger   *slog.Logger
}

func NewAccountHandler(store *telemetry.Store, registry *tiers.Registry, cfg *config.Config, logger *slog.Logger) *AccountHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &AccountHandler{store: store, registry: registry, cfg: cfg, logger: logger}
}

// tierDescriptor is one row of the public matrix.
type tierDescriptor struct {
	Tier   string       `json:"tier"`
	Limits tiers.Limits `json:"limits"`
}

// usageResponse is the shared shape for /me and /usage. `limits` is always
// present so the UI never has to hard-code a number to render a meter against.
type usageResponse struct {
	Tier      string           `json:"tier"`
	Limits    tiers.Limits     `json:"limits"`
	Usage     map[string]int64 `json:"usage"`
	ResetsAt  string           `json:"resetsAt"`
	User      *models.User     `json:"user,omitempty"`
	Anonymous bool             `json:"anonymous"`
}

func nextUTCMidnight(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
}

// subjectFor mirrors the quota middleware's preference order (user → session →
// nothing) so the meter the UI shows is counted the same way the enforcement
// counts it. The IP fallback is deliberately NOT reproduced here: this endpoint
// must not create a hashed-IP row as a side effect of rendering a meter.
func subjectFor(c *gin.Context, user *models.User) (models.UsageSubjectKind, string, bool) {
	if user != nil && user.ID != "" {
		return models.UsageSubjectUser, user.ID, true
	}
	if session := c.GetHeader("X-MM-Session-ID"); session != "" {
		return models.UsageSubjectSession, session, true
	}
	return "", "", false
}

// currentUser loads the account row for a verified caller. Returns (nil, false)
// for anonymous callers and, deliberately, also when the database is
// unavailable — a broken lookup must degrade the view, not 500 the page.
func (h *AccountHandler) currentUser(c *gin.Context) (*models.User, bool) {
	principal, ok := middleware.PrincipalFrom(c)
	if !ok {
		return nil, false
	}
	user, err := h.store.UserByFirebaseUID(c.Request.Context(), principal.UID)
	if err != nil {
		h.logger.Warn("account: user lookup failed", "error", err.Error())
		return nil, true
	}
	return user, true
}

func (h *AccountHandler) buildUsageResponse(c *gin.Context, user *models.User, signedIn bool) usageResponse {
	tier := tiers.Resolve(signedIn, user)
	now := time.Now().UTC()

	usage := map[string]int64{}
	if kind, key, ok := subjectFor(c, user); ok {
		if counted, err := h.store.UsageForDay(c.Request.Context(), kind, key, now); err == nil {
			usage = counted
		} else {
			h.logger.Warn("account: usage lookup failed", "error", err.Error())
		}
	}
	// Always report every countable metric, so the UI can render a meter
	// without special-casing "no row yet".
	for _, metric := range []string{models.MetricOps, models.MetricAIOps, models.MetricExport} {
		if _, ok := usage[metric]; !ok {
			usage[metric] = 0
		}
	}

	return usageResponse{
		Tier:      string(tier),
		Limits:    h.registry.For(tier),
		Usage:     usage,
		ResetsAt:  nextUTCMidnight(now).Format(time.RFC3339),
		User:      user,
		Anonymous: !signedIn,
	}
}

// Sync upserts mm_users from the verified token. Called by the UI immediately
// after sign-in, and idempotent — it is safe to call on every session restore.
//
// It never changes a tier. Sign-in is an identity event, not an entitlement
// event (see telemetry.UpsertUser).
func (h *AccountHandler) Sync(c *gin.Context) {
	principal, ok := middleware.PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if !h.store.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Accounts are temporarily unavailable"})
		return
	}

	var body struct {
		DisplayName  string `json:"displayName"`
		SignupSource string `json:"signupSource"`
	}
	_ = c.ShouldBindJSON(&body) // Optional; an empty body is a valid sync.

	user, err := h.store.UpsertUser(c.Request.Context(), telemetry.UserSyncInput{
		FirebaseUID:   principal.UID,
		Email:         principal.Email,
		EmailVerified: principal.EmailVerified,
		DisplayName:   body.DisplayName,
		SignupSource:  body.SignupSource,
	})
	if err != nil {
		h.logger.Error("account: sync failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not save your account"})
		return
	}

	c.JSON(http.StatusOK, h.buildUsageResponse(c, user, true))
}

// Me returns the signed-in caller's account, tier, limits and today's usage.
func (h *AccountHandler) Me(c *gin.Context) {
	user, signedIn := h.currentUser(c)
	if !signedIn {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	c.JSON(http.StatusOK, h.buildUsageResponse(c, user, true))
}

// Usage works anonymously. That is the point: a signed-out visitor's quota
// meter has to show "3 of 5 used today" before they have any reason to create
// an account, and making the value visible early is the whole conversion
// mechanism.
func (h *AccountHandler) Usage(c *gin.Context) {
	user, signedIn := h.currentUser(c)
	c.JSON(http.StatusOK, h.buildUsageResponse(c, user, signedIn))
}

// Tiers serves the public tier matrix. /pricing, the upgrade prompts and the
// inline capability hints all render from this, so the numbers exist in exactly
// one place (internal/tiers, fed by config).
func (h *AccountHandler) Tiers(c *gin.Context) {
	out := make([]tierDescriptor, 0, len(tiers.All()))
	for _, t := range tiers.All() {
		out = append(out, tierDescriptor{Tier: string(t), Limits: h.registry.For(t)})
	}
	// Cacheable: this is configuration, identical for every caller, and it
	// changes only on deploy.
	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, gin.H{
		"tiers": out,
		// Payments are not built yet. The UI shows "Coming soon" for Premium
		// rather than a checkout button, and this flag is how it knows.
		"premiumPurchasable": false,
		"premiumPriceUSD":    7,
	})
}

// RegisterAccountRoutes mounts the account surface.
//
// /sync and /me require a verified token and fail closed (503) if the verifier
// could not be built — an endpoint that returns account data must never guess.
// /usage and /tiers are public: /usage reads whatever identity the group-level
// OptionalFirebaseAuth already attached, and /tiers needs none.
func RegisterAccountRoutes(rg *gin.RouterGroup, h *AccountHandler, cfg *config.Config, verifier middleware.TokenVerifier) {
	rg.GET("/tiers", h.Tiers)

	account := rg.Group("/account")
	account.GET("/usage", h.Usage)

	authed := account.Group("")
	authed.Use(middleware.RequireFirebaseAuth(true, verifier))
	authed.POST("/sync", h.Sync)
	authed.GET("/me", h.Me)
}
