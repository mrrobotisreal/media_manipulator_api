package billing

import (
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// Granted tiers. They equal tiers.TierPremium / tiers.TierPro / tiers.TierFree.
const (
	TierFree    = "free"
	TierPremium = "premium"
	TierPro     = "pro"
)

// PriceTiers maps a Stripe Price id to the tier it grants (MM39, design
// PRO_TIER.md §2.4). Built once from config; an id not in the table is
// "unknown" and grants premium, never pro, so a mis-set env var can never hand
// out the higher tier.
type PriceTiers map[string]string

// NewPriceTiers builds the table from config: the four Premium ids (monthly,
// yearly and the two monthly retention prices) -> premium, the two Pro ids ->
// pro. Unset (empty) ids are skipped. If one id were ever configured for both
// plans, premium wins (the never-elevate rule).
func NewPriceTiers(cfg *config.Config) PriceTiers {
	t := PriceTiers{}
	if cfg == nil {
		return t
	}
	for _, id := range []string{cfg.StripePriceProMonthly, cfg.StripePriceProYearly} {
		if id != "" {
			t[id] = TierPro
		}
	}
	for _, id := range []string{cfg.StripePricePremiumMonthly, cfg.StripePricePremiumYearly,
		cfg.StripePricePremiumMonthlyRetain7, cfg.StripePricePremiumMonthlyRetain6} {
		if id != "" {
			t[id] = TierPremium
		}
	}
	return t
}

// TierFor returns the tier a price grants and whether the id was known.
// Unknown (including empty) -> premium, false.
func (p PriceTiers) TierFor(priceID string) (tier string, known bool) {
	if t, ok := p[priceID]; ok && priceID != "" {
		return t, true
	}
	return TierPremium, false
}

// Decision is what the status -> tier matrix says to do to a user's tier.
type Decision struct {
	// Change is false when the subscription state must not alter the tier.
	Change    bool
	Tier      string // "free" | "premium" | "pro"
	Source    string // "subscription" | "default"
	ExpiresAt *time.Time
	// UnknownPrice is true when a granting decision fell back to premium
	// because the subscription's price id is not in the PriceTiers table. The
	// caller logs a warning and counts mm_billing_unknown_price_total.
	UnknownPrice bool
}

// Entitlement is the single pure status -> tier matrix (MM17 decision 9; the
// granted tier reads the price id since MM39).
//
//	trialing|active                                -> price tier, expires at items.data[0].current_period_end
//	past_due                                       -> price tier, expires at max(period end, now+grace)
//	canceled|unpaid|incomplete_expired|paused      -> free, source default, no expiry
//	incomplete (and anything unknown)              -> no change
//
// "price tier" is prices.TierFor(sub.PriceID): premium or pro, premium for an
// unknown id (UnknownPrice set). cancel_at_period_end is deliberately ignored
// (cancel_at only shortens the expiry): the tier stays until the period ends and
// tiers.Resolve downgrades it by expiry.
func Entitlement(sub *Subscription, now time.Time, grace time.Duration, prices PriceTiers) Decision {
	if sub == nil {
		return Decision{}
	}
	switch sub.Status {
	case StatusTrialing, StatusActive:
		if sub.CurrentPeriodEnd.IsZero() {
			// Without a period end there is nothing safe to expire on.
			return Decision{}
		}
		end := sub.CurrentPeriodEnd.UTC()
		// A scheduled cancel_at inside this period is when the plan really ends.
		if !sub.CancelAt.IsZero() && sub.CancelAt.After(now) && sub.CancelAt.Before(end) {
			end = sub.CancelAt.UTC()
		}
		tier, known := prices.TierFor(sub.PriceID)
		return Decision{Change: true, Tier: tier, Source: "subscription", ExpiresAt: &end, UnknownPrice: !known}
	case StatusPastDue:
		end := now.Add(grace).UTC()
		if pe := sub.CurrentPeriodEnd.UTC(); pe.After(end) {
			end = pe
		}
		tier, known := prices.TierFor(sub.PriceID)
		return Decision{Change: true, Tier: tier, Source: "subscription", ExpiresAt: &end, UnknownPrice: !known}
	case StatusCanceled, StatusUnpaid, StatusIncompleteExpired, StatusPaused:
		return Decision{Change: true, Tier: TierFree, Source: "default"}
	default:
		return Decision{}
	}
}
