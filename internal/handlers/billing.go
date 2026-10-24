package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/billing"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// webhookMaxBody caps the webhook body before any decode.
const webhookMaxBody = 1 << 20 // 1 MiB

// BillingHandler serves /api/billing/*: Checkout, Portal, status, session
// verification (all behind Firebase auth) and the Stripe webhook (signature
// authenticated, no Firebase). It never writes a tier itself; the webhook
// goes through billing.Processor, which goes through SetUserTier.
type BillingHandler struct {
	store     billing.Store
	client    billing.BillingClient
	processor *billing.Processor
	registry  *tiers.Registry
	cfg       *config.Config
	logger    *slog.Logger
	now       func() time.Time
}

// NewBillingHandler wires the handler. client and processor may be nil when
// billing is not configured; every route then answers 503 billing_unavailable.
func NewBillingHandler(store billing.Store, client billing.BillingClient, processor *billing.Processor, registry *tiers.Registry, cfg *config.Config, logger *slog.Logger) *BillingHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &BillingHandler{store: store, client: client, processor: processor, registry: registry, cfg: cfg, logger: logger, now: time.Now}
}

func (h *BillingHandler) available() bool {
	return h.cfg.BillingConfigured() && h.client != nil && h.store != nil
}

func (h *BillingHandler) unavailable(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "billing_unavailable"})
}

// requireBilling is the 503 guard for every authenticated billing route.
func (h *BillingHandler) requireBilling(c *gin.Context) {
	if !h.available() {
		h.unavailable(c)
		return
	}
	c.Next()
}

// caller loads the principal and the mm_users row. It writes the error
// response itself and returns ok=false when the request cannot proceed.
func (h *BillingHandler) caller(c *gin.Context) (*middleware.Principal, *models.User, bool) {
	principal, ok := middleware.PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return nil, nil, false
	}
	user, err := h.store.UserByFirebaseUID(c.Request.Context(), principal.UID)
	if err != nil {
		h.logger.Warn("billing: user lookup failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup_failed"})
		return nil, nil, false
	}
	if user == nil {
		// Signed in but POST /api/account/sync has not landed yet.
		c.JSON(http.StatusConflict, gin.H{"error": "account_not_synced", "reason": "account_not_synced"})
		return nil, nil, false
	}
	return principal, user, true
}

func (h *BillingHandler) intervalPrice(interval string) (string, bool) {
	switch interval {
	case billing.IntervalMonthly:
		return h.cfg.StripePricePremiumMonthly, true
	case billing.IntervalYearly:
		return h.cfg.StripePricePremiumYearly, true
	}
	return "", false
}

// liveManualGrant reports an unexpired (or non-expiring) premium manual grant.
func liveManualGrant(u *models.User, now time.Time) bool {
	if u.Tier != "premium" || u.TierSource != "manual_grant" {
		return false
	}
	return u.TierExpiresAt == nil || u.TierExpiresAt.After(now)
}

type checkoutRequest struct {
	Interval string `json:"interval"`
	Locale   string `json:"locale"`
}

// Checkout creates a hosted Checkout Session. The price id comes from config
// only; the client chooses just the interval.
func (h *BillingHandler) Checkout(c *gin.Context) {
	var req checkoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	priceID, ok := h.intervalPrice(req.Interval)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_interval"})
		return
	}
	principal, user, ok := h.caller(c)
	if !ok {
		return
	}
	if h.registry.IsOperator(principal.Email, principal.EmailVerified) {
		c.JSON(http.StatusForbidden, gin.H{"error": "operator_account", "reason": "operator_account"})
		return
	}
	now := h.now().UTC()
	if liveManualGrant(user, now) {
		var exp any
		if user.TierExpiresAt != nil {
			exp = user.TierExpiresAt.UTC().Format(time.RFC3339)
		}
		c.JSON(http.StatusConflict, gin.H{"error": "manual_grant_active", "reason": "manual_grant_active", "expiresAt": exp})
		return
	}
	if user.StripeSubscriptionID != "" && user.SubscriptionStatus != "" && !billing.IsTerminalStatus(user.SubscriptionStatus) {
		c.JSON(http.StatusConflict, gin.H{"error": "already_subscribed", "reason": "already_subscribed", "portalAvailable": user.StripeCustomerID != ""})
		return
	}

	ctx := c.Request.Context()
	customerID := user.StripeCustomerID
	if customerID == "" {
		created, err := h.client.EnsureCustomer(ctx, user.ID, principal.Email)
		if err != nil {
			h.logger.Warn("billing: ensure customer failed", "error", err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
			return
		}
		// Stored BEFORE the session exists. SetStripeCustomer keeps the first id
		// if a concurrent request won; the idempotency key makes both the same.
		customerID, err = h.store.SetStripeCustomer(ctx, user.ID, created)
		if err != nil {
			h.logger.Warn("billing: store customer failed", "error", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "billing_store_error"})
			return
		}
	}

	_, url, err := h.client.CreateCheckoutSession(ctx, billing.CheckoutRequest{
		UserID:     user.ID,
		CustomerID: customerID,
		PriceID:    priceID,
		Locale:     req.Locale,
		BaseURL:    h.cfg.BillingPublicBaseURL,
		VisitorID:  strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")),
		SessionID:  strings.TrimSpace(c.GetHeader("X-MM-Session-ID")),
	})
	if err != nil {
		h.logger.Warn("billing: create checkout session failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url})
}

type portalRequest struct {
	Locale string `json:"locale"`
}

// Portal opens the Stripe-hosted customer portal.
func (h *BillingHandler) Portal(c *gin.Context) {
	var req portalRequest
	// Body is optional.
	if c.Request.ContentLength != 0 {
		_ = c.ShouldBindJSON(&req)
	}
	_, user, ok := h.caller(c)
	if !ok {
		return
	}
	if user.StripeCustomerID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "no_billing_account", "reason": "no_billing_account"})
		return
	}
	returnURL := h.cfg.BillingPublicBaseURL + "/" + billing.NormalizeUILocale(req.Locale) + "/account"
	url, err := h.client.CreatePortalSession(c.Request.Context(), user.StripeCustomerID, returnURL)
	if err != nil {
		h.logger.Warn("billing: create portal session failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url})
}

type manualGrantInfo struct {
	ExpiresAt *string `json:"expiresAt"`
}

type retentionOffer struct {
	Kind         string  `json:"kind"`
	PriceUSD     float64 `json:"priceUSD"`
	NextChargeAt string  `json:"nextChargeAt"`
}

type billingStatusResponse struct {
	Purchasable       bool             `json:"purchasable"`
	Interval          *string          `json:"interval"`
	Status            *string          `json:"status"`
	RenewsAt          *string          `json:"renewsAt"`
	CancelAtPeriodEnd bool             `json:"cancelAtPeriodEnd"`
	PortalAvailable   bool             `json:"portalAvailable"`
	ManualGrant       *manualGrantInfo `json:"manualGrant"`
	// EndsAt is when Premium ends for a cancelled-but-active subscription
	// (equal to renewsAt then); null otherwise.
	EndsAt *string `json:"endsAt"`
	// PriceMonthlyUSD is what the subscriber pays per interval, read from the
	// Stripe subscription item. Only resolved while the plan is cancelled (the
	// only time the UI needs it); null otherwise or when unknown.
	PriceMonthlyUSD *float64 `json:"priceMonthlyUSD"`
	// Offer is the retention offer, or null.
	Offer *retentionOffer `json:"offer"`
	// PeriodsPaid is the number of paid billing periods on the current MONTHLY
	// subscription (processed invoice.paid rows in mm_billing_events, no Stripe
	// call). Omitted for yearly or no subscription, and when the count is
	// unavailable.
	PeriodsPaid *int `json:"periodsPaid,omitempty"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (h *BillingHandler) retentionPrices() billing.RetentionPrices {
	return billing.RetentionPrices{Retain7ID: h.cfg.StripePricePremiumMonthlyRetain7, Retain6ID: h.cfg.StripePricePremiumMonthlyRetain6}
}

func toRetentionOffer(o *billing.Offer) *retentionOffer {
	if o == nil {
		return nil
	}
	return &retentionOffer{Kind: o.Kind, PriceUSD: o.PriceUSD, NextChargeAt: o.NextChargeAt.UTC().Format(time.RFC3339)}
}

// cancelledActive: the mirror says the subscription is active and ending.
// liveSubscription: the user has a Stripe subscription that still grants
// access (active or trialing), cancelled or not.
func liveSubscription(u *models.User) bool {
	return u.StripeSubscriptionID != "" &&
		(u.SubscriptionStatus == billing.StatusActive || u.SubscriptionStatus == billing.StatusTrialing)
}

func cancelledActive(u *models.User) bool {
	return u.CancelAtPeriodEnd && u.StripeSubscriptionID != "" &&
		(u.SubscriptionStatus == billing.StatusActive || u.SubscriptionStatus == billing.StatusTrialing)
}

// buildStatus renders the status JSON. The only Stripe call is a Subscription
// retrieve for a cancelled-but-active plan (price + offer); a failure there
// just leaves priceMonthlyUSD and offer null.
func (h *BillingHandler) buildStatus(ctx context.Context, user *models.User) (billingStatusResponse, *billing.Subscription) {
	now := h.now().UTC()
	out := billingStatusResponse{
		Purchasable:       h.available(),
		Interval:          strPtr(user.BillingInterval),
		Status:            strPtr(user.SubscriptionStatus),
		CancelAtPeriodEnd: user.CancelAtPeriodEnd,
		PortalAvailable:   h.available() && user.StripeCustomerID != "",
	}
	if user.TierSource == "subscription" && user.TierExpiresAt != nil {
		s := user.TierExpiresAt.UTC().Format(time.RFC3339)
		out.RenewsAt = &s
		if cancelledActive(user) && user.TierExpiresAt.After(now) {
			out.EndsAt = &s
		}
	}
	var fetched *billing.Subscription
	// Read Stripe for every live subscription, not only rows already flagged as
	// cancelled: a cancellation made before this code shipped (Stripe's
	// cancel_at) is not in the stored mirror, so trusting the mirror alone hides
	// it until the next webhook. When Stripe disagrees with the mirror, repair
	// the mirror through the processor (same path as a webhook).
	if h.available() && liveSubscription(user) {
		if sub, err := h.client.GetSubscription(ctx, user.StripeSubscriptionID); err != nil {
			h.logger.Warn("billing: status subscription fetch failed", "error", err.Error())
		} else {
			fetched = sub
			if user.CancelAtPeriodEnd != sub.IsCancelling(now) && h.processor != nil {
				if err := h.processor.Resync(ctx, sub, user.ID); err != nil {
					h.logger.Warn("billing: status mirror resync failed", "error", err.Error())
				}
			}
			out.CancelAtPeriodEnd = sub.IsCancelling(now)
			if !out.CancelAtPeriodEnd {
				out.EndsAt = nil // Stripe says it renews; the mirror's end date is stale
			}
		}
	}
	if sub := fetched; sub != nil && sub.IsCancelling(now) {
		if sub.UnitAmountCents != nil && sub.Currency == "usd" {
			v := float64(*sub.UnitAmountCents) / 100
			out.PriceMonthlyUSD = &v
		}
		if end, ok := billing.CancelledEnd(sub, now); ok {
			e := end.Format(time.RFC3339)
			out.EndsAt, out.RenewsAt, out.CancelAtPeriodEnd = &e, &e, true
		}
		fallback := float64(h.cfg.BillingPriceMonthlyUSD)
		if sub.Interval == billing.IntervalYearly {
			fallback = float64(h.cfg.BillingPriceYearlyUSD)
		}
		out.Offer = toRetentionOffer(billing.ComputeRetentionOffer(sub, now, h.retentionPrices(), fallback))
	}
	if h.available() && user.BillingInterval == "monthly" && user.StripeSubscriptionID != "" {
		if n, err := h.store.CountPaidPeriods(ctx, user.ID, user.StripeSubscriptionID); err != nil {
			h.logger.Warn("billing: periodsPaid unavailable", "error", err.Error())
		} else {
			out.PeriodsPaid = &n
		}
	}
	if liveManualGrant(user, now) {
		info := &manualGrantInfo{}
		if user.TierExpiresAt != nil {
			s := user.TierExpiresAt.UTC().Format(time.RFC3339)
			info.ExpiresAt = &s
		}
		out.ManualGrant = info
	}
	return out, fetched
}

// Status reports the caller's billing state from the stored mirror. It answers
// even when billing is off (purchasable:false) so the UI can render the account
// page unconditionally. The one Stripe read is described on buildStatus.
func (h *BillingHandler) Status(c *gin.Context) {
	_, user, ok := h.caller(c)
	if !ok {
		return
	}
	out, _ := h.buildStatus(c.Request.Context(), user)
	c.JSON(http.StatusOK, out)
}

type retainRequest struct {
	Offer string `json:"offer"`
}

// Retain accepts the "stay" offer (or a plain resume) for the caller's own
// cancelled-but-active subscription. The server recomputes the offer from its
// own clock and Stripe's state; the client's choice is only accepted if it
// matches. One Stripe update, then the result is applied through the processor
// so the UI flips before the webhook arrives.
func (h *BillingHandler) Retain(c *gin.Context) {
	var req retainRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	switch req.Offer {
	case billing.OfferRetain7, billing.OfferRetain6, billing.OfferResume:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_offer"})
		return
	}
	_, user, ok := h.caller(c)
	if !ok {
		return
	}
	if user.StripeSubscriptionID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "no_subscription"})
		return
	}
	ctx := c.Request.Context()
	sub, err := h.client.GetSubscription(ctx, user.StripeSubscriptionID)
	if errors.Is(err, billing.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no_subscription"})
		return
	}
	if err != nil {
		h.logger.Warn("billing: retain: get subscription failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	if user.StripeCustomerID != "" && sub.CustomerID != "" && sub.CustomerID != user.StripeCustomerID {
		c.JSON(http.StatusNotFound, gin.H{"error": "no_subscription"})
		return
	}
	now := h.now().UTC()
	fallback := float64(h.cfg.BillingPriceMonthlyUSD)
	if sub.Interval == billing.IntervalYearly {
		fallback = float64(h.cfg.BillingPriceYearlyUSD)
	}
	offer := billing.ComputeRetentionOffer(sub, now, h.retentionPrices(), fallback)
	if offer == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "not_cancelled", "reason": "not_cancelled"})
		return
	}
	// "resume" is always acceptable: it keeps the current price, so a client that
	// asked for it never gets a different price than it saw. A discount must match.
	if req.Offer != billing.OfferResume && req.Offer != offer.Kind {
		c.JSON(http.StatusConflict, gin.H{"error": "offer_changed", "reason": "offer_changed", "offer": toRetentionOffer(offer)})
		return
	}
	applied := offer
	if req.Offer == billing.OfferResume {
		applied = &billing.Offer{Kind: billing.OfferResume}
	}
	if applied.PriceID != "" && sub.ItemID == "" {
		h.logger.Warn("billing: retain: subscription has no item id")
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	updated, err := h.client.ResumeSubscription(ctx, sub.ID, sub.ItemID, applied.PriceID, applied.Kind)
	if err != nil {
		h.logger.Warn("billing: retain: resume failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	if h.processor != nil {
		if err := h.processor.ApplyRetained(ctx, updated, user.ID, billing.AnalyticsOffer(applied.Kind)); err != nil {
			// Stripe already changed; the webhook converges the mirror.
			h.logger.Warn("billing: retain: applying resumed subscription failed", "error", err.Error())
		}
	}
	fresh, err := h.store.UserByID(ctx, user.ID)
	if err != nil || fresh == nil {
		fresh = user
	}
	out, _ := h.buildStatus(ctx, fresh)
	c.JSON(http.StatusOK, gin.H{"status": out})
}

// Session verifies a Checkout return: does this session belong to the caller,
// was it paid, and has the webhook already applied Premium. Another user's
// session reveals nothing beyond belongsToUser:false.
func (h *BillingHandler) Session(c *gin.Context) {
	_, user, ok := h.caller(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if !strings.HasPrefix(id, "cs_") || len(id) > 255 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_session_id"})
		return
	}
	sess, err := h.client.GetCheckoutSession(c.Request.Context(), id)
	if errors.Is(err, billing.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	if err != nil {
		h.logger.Warn("billing: get checkout session failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	belongs := sess.UserID == user.ID ||
		(user.StripeCustomerID != "" && sess.CustomerID == user.StripeCustomerID)
	if !belongs {
		c.JSON(http.StatusOK, gin.H{"belongsToUser": false, "paid": false, "tierApplied": false})
		return
	}
	applied := sess.Paid && tiers.Resolve(true, user) == tiers.TierPremium && user.TierSource == "subscription"
	if sess.Paid && !applied && h.processor != nil {
		// Don't make a buyer wait on the webhook: apply the paid subscription now.
		if err := h.processor.ApplyCheckout(c.Request.Context(), sess); err != nil {
			h.logger.Warn("billing: applying paid checkout on return failed", "error", err.Error())
		} else if refreshed, err := h.store.UserByID(c.Request.Context(), user.ID); err == nil && refreshed != nil {
			applied = tiers.Resolve(true, refreshed) == tiers.TierPremium && refreshed.TierSource == "subscription"
		}
	}
	c.JSON(http.StatusOK, gin.H{"belongsToUser": true, "paid": sess.Paid, "tierApplied": applied})
}

// Webhook receives Stripe events. Authenticated by signature only.
func (h *BillingHandler) Webhook(c *gin.Context) {
	if !h.available() || h.processor == nil {
		h.unavailable(c)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, webhookMaxBody)
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "payload_too_large"})
		return
	}
	ev, err := h.client.ConstructEvent(payload, c.GetHeader("Stripe-Signature"))
	if err != nil {
		// Never log the body or the header; the error text names no secrets.
		h.logger.Warn("billing: webhook signature/parse rejected", "error", err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_signature"})
		return
	}
	if err := h.processor.Handle(c.Request.Context(), ev); err != nil {
		h.logger.Error("billing: webhook processing failed", "eventId", ev.ID, "type", ev.Type, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "processing_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"received": true})
}

// RegisterBillingRoutes mounts /billing/*. Everything except the webhook sits
// behind RequireFirebaseAuth (401 without a token, 503 if the verifier could
// not be built) and then the billing_unavailable guard. The webhook is
// registered outside the auth group: Stripe sends no Firebase token.
func RegisterBillingRoutes(rg *gin.RouterGroup, h *BillingHandler, verifier middleware.TokenVerifier) {
	rg.POST("/billing/webhook", h.Webhook)

	authed := rg.Group("/billing")
	authed.Use(middleware.RequireFirebaseAuth(true, verifier), h.requireBilling)
	authed.POST("/checkout", h.Checkout)
	authed.POST("/portal", h.Portal)
	authed.POST("/retain", h.Retain)
	authed.GET("/session/:id", h.Session)

	// /status stays reachable when billing is off (purchasable:false) so the
	// account page renders unconditionally; it is outside the 503 guard.
	status := rg.Group("/billing")
	status.Use(middleware.RequireFirebaseAuth(true, verifier))
	status.GET("/status", h.Status)
}
