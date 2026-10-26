package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/billing"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// Plan-change timings in the change-plan response (MM39).
const (
	timingImmediate = "immediate"
	timingPeriodEnd = "period_end"
)

type changePlanRequest struct {
	Target string `json:"target"`
}

// planChangeSubscription loads and checks the caller's own live subscription
// for a plan change. It writes the refusal itself and returns ok=false:
//
//	409 not_subscription      free, manual grant, no live subscription, or not the caller's
//	409 change_not_available  cancel-at-period-end (resume in the Portal first)
//	502 billing_provider_error
func (h *BillingHandler) planChangeSubscription(c *gin.Context, user *models.User) (*billing.Subscription, bool) {
	notSub := func() (*billing.Subscription, bool) {
		c.JSON(http.StatusConflict, gin.H{"error": "not_subscription", "reason": "not_subscription"})
		return nil, false
	}
	if user.TierSource != "subscription" || !liveSubscription(user) {
		return notSub()
	}
	sub, err := h.client.GetSubscription(c.Request.Context(), user.StripeSubscriptionID)
	if errors.Is(err, billing.ErrNotFound) {
		return notSub()
	}
	if err != nil {
		h.logger.Warn("billing: change-plan: get subscription failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return nil, false
	}
	if user.StripeCustomerID != "" && sub.CustomerID != "" && sub.CustomerID != user.StripeCustomerID {
		return notSub()
	}
	if sub.Status != billing.StatusActive && sub.Status != billing.StatusTrialing {
		return notSub()
	}
	if sub.IsCancelling(h.now().UTC()) {
		c.JSON(http.StatusConflict, gin.H{"error": "change_not_available", "reason": "change_not_available"})
		return nil, false
	}
	return sub, true
}

func pendingResponse(plan string, p *pendingPlan) gin.H {
	return gin.H{"plan": plan, "pendingPlan": p, "timing": timingPeriodEnd}
}

// ChangePlan moves the caller's subscription between Premium and Pro
// (POST /api/billing/change-plan {target}). Upgrade = immediate, prorated
// price swap at the same interval with the renewal date unchanged; downgrade =
// a Subscription Schedule that switches to Premium at the end of the paid
// period (no refund; Pro stays until then). The server decides everything
// from Stripe's state and config; the client names only the target plan.
func (h *BillingHandler) ChangePlan(c *gin.Context) {
	var req changePlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	target := strings.TrimSpace(req.Target)
	if !validPlan(target) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_plan"})
		return
	}
	_, user, ok := h.caller(c)
	if !ok {
		return
	}
	sub, ok := h.planChangeSubscription(c, user)
	if !ok {
		return
	}
	now := h.now().UTC()
	current, _ := h.prices.TierFor(sub.PriceID)
	pending := h.pendingFor(sub, now)

	// A repeat downgrade while one is already scheduled is idempotent.
	if target == config.PlanPremium && current == config.PlanPro && pending != nil && pending.To == config.PlanPremium {
		c.JSON(http.StatusOK, pendingResponse(current, pending))
		return
	}
	if target == current {
		c.JSON(http.StatusConflict, gin.H{"error": "same_plan", "reason": "same_plan", "plan": current})
		return
	}
	newPrice, ok := h.planPrice(target, sub.Interval)
	if !ok {
		// Neither monthly nor yearly: no same-interval price to move to.
		c.JSON(http.StatusConflict, gin.H{"error": "change_not_available", "reason": "change_not_available"})
		return
	}
	if newPrice == "" {
		h.unavailable(c) // Pro prices not configured on this deployment
		return
	}
	if tiers.Rank(tiers.Tier(target)) > tiers.Rank(tiers.Tier(current)) {
		h.upgradePlan(c, user, sub, newPrice)
		return
	}
	h.downgradePlan(c, sub, current, newPrice, now)
}

// upgradePlan: Premium -> Pro now, prorated, same renewal date.
func (h *BillingHandler) upgradePlan(c *gin.Context, user *models.User, sub *billing.Subscription, newPrice string) {
	ctx := c.Request.Context()
	if sub.ItemID == "" {
		h.logger.Warn("billing: change-plan: subscription has no item id")
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	if sched := sub.Schedule; sched != nil {
		// Our own downgrade schedule running out its Premium phase is released
		// first, so it cannot re-impose Premium; anyone else's is left alone.
		if !sched.PlanChange {
			c.JSON(http.StatusConflict, gin.H{"error": "change_not_available", "reason": "change_not_available"})
			return
		}
		if err := h.client.ReleaseSchedule(ctx, sched.ID); err != nil && !errors.Is(err, billing.ErrNotFound) {
			h.logger.Warn("billing: change-plan: releasing old schedule failed", "error", err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
			return
		}
	}
	updated, err := h.client.UpdateSubscriptionPrice(ctx, sub.ID, sub.ItemID, newPrice, billing.ProrationCreate)
	if err != nil {
		h.logger.Warn("billing: change-plan: price update failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	if h.processor != nil {
		if err := h.processor.ApplyPlanChange(ctx, updated, user.ID); err != nil {
			// Stripe already changed; the webhook converges the mirror.
			h.logger.Warn("billing: change-plan: applying upgraded subscription failed", "error", err.Error())
		}
	}
	plan, _ := h.prices.TierFor(updated.PriceID)
	c.JSON(http.StatusOK, gin.H{"plan": plan, "pendingPlan": nil, "timing": timingImmediate})
}

// downgradePlan: Pro -> Premium at the end of the paid period, via a
// two-phase schedule. The tier stays Pro until the switch.
func (h *BillingHandler) downgradePlan(c *gin.Context, sub *billing.Subscription, current, newPrice string, now time.Time) {
	ctx := c.Request.Context()
	if sub.Schedule != nil || sub.CurrentPeriodEnd.IsZero() || !sub.CurrentPeriodEnd.After(now) {
		// Some other schedule already drives this subscription, or there is no
		// period end to switch at.
		c.JSON(http.StatusConflict, gin.H{"error": "change_not_available", "reason": "change_not_available"})
		return
	}
	sched, err := h.client.CreateSchedule(ctx, sub.ID, sub.PriceID, newPrice, sub.Currency, sub.CurrentPeriodEnd)
	if err != nil {
		// A concurrent request may have won the race; if so, answer like a
		// repeat downgrade.
		if p := h.rereadPending(ctx, sub, now); p != nil && p.To == config.PlanPremium {
			c.JSON(http.StatusOK, pendingResponse(current, p))
			return
		}
		h.logger.Warn("billing: change-plan: create schedule failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	withSched := *sub
	withSched.Schedule = sched
	p := h.pendingFor(&withSched, now)
	if p == nil {
		// Stripe accepted it, so the switch is at the period end we asked for.
		next, _ := h.prices.TierFor(newPrice)
		p = &pendingPlan{To: next, At: sub.CurrentPeriodEnd.UTC().Format(time.RFC3339)}
	}
	c.JSON(http.StatusOK, pendingResponse(current, p))
}

func (h *BillingHandler) rereadPending(ctx context.Context, sub *billing.Subscription, now time.Time) *pendingPlan {
	sched, err := h.client.GetSchedule(ctx, sub.ID)
	if err != nil || sched == nil {
		return nil
	}
	withSched := *sub
	withSched.Schedule = sched
	return h.pendingFor(&withSched, now)
}

// KeepPlan releases a scheduled downgrade ("Keep Pro";
// DELETE /api/billing/change-plan). Same guards as ChangePlan. Nothing
// scheduled is a no-op 200.
func (h *BillingHandler) KeepPlan(c *gin.Context) {
	_, user, ok := h.caller(c)
	if !ok {
		return
	}
	sub, ok := h.planChangeSubscription(c, user)
	if !ok {
		return
	}
	now := h.now().UTC()
	current, _ := h.prices.TierFor(sub.PriceID)
	if h.pendingFor(sub, now) == nil {
		c.JSON(http.StatusOK, gin.H{"plan": current, "pendingPlan": nil})
		return
	}
	if !sub.Schedule.PlanChange {
		// A schedule this API did not create (e.g. made in the Dashboard).
		c.JSON(http.StatusConflict, gin.H{"error": "change_not_available", "reason": "change_not_available"})
		return
	}
	if err := h.client.ReleaseSchedule(c.Request.Context(), sub.Schedule.ID); err != nil && !errors.Is(err, billing.ErrNotFound) {
		h.logger.Warn("billing: keep-plan: release schedule failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "billing_provider_error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"plan": current, "pendingPlan": nil})
}
