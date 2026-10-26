package billing

import "time"

// Retention offer kinds (the POST /api/billing/retain body and the status JSON).
const (
	OfferRetain7 = "retain7"
	OfferRetain6 = "retain6"
	OfferResume  = "resume"
)

// retainWindow: with this much time or less before Premium ends, the deeper
// $6 offer replaces $7.
const retainWindow = 3 * 24 * time.Hour

// RetentionPrices are the optional retention price ids and their amounts.
type RetentionPrices struct {
	Retain7ID string
	Retain6ID string
}

// Offer is a computed retention offer.
type Offer struct {
	Kind string
	// Price is the monthly price after accepting (resume keeps the current
	// one), as a numeral in Currency (MM69: retention offers are 7 / 6 in
	// either currency).
	Price float64
	// Currency is the subscription's currency ("usd" | "eur"); "" = unknown.
	Currency string
	// NextChargeAt is when the next payment happens (the end of the paid period).
	NextChargeAt time.Time
	// PriceID is the Stripe price to swap to; "" for resume.
	PriceID string
}

// CancelledEnd reports whether sub is cancelled-but-active with Premium still
// running, and when it ends.
func CancelledEnd(sub *Subscription, now time.Time) (time.Time, bool) {
	if sub == nil || sub.Status != StatusActive || !sub.IsCancelling(now) {
		return time.Time{}, false
	}
	end := sub.CurrentPeriodEnd
	if !sub.CancelAt.IsZero() && sub.CancelAt.After(now) && (end.IsZero() || sub.CancelAt.Before(end)) {
		end = sub.CancelAt
	}
	if end.IsZero() || !end.After(now) {
		return time.Time{}, false
	}
	return end.UTC(), true
}

// ComputeRetentionOffer is the pure offer rule. fallback is the display
// numeral used for "resume" when the subscription's real price is unknown.
// The offer is in the subscription's own currency: the retain prices carry
// the same numerals in USD and EUR, and swapping the price keeps the
// subscription's currency (MM69).
// nil = nothing to offer (not cancelled-but-active, or Premium already ended).
//
//   - yearly (or unknown interval): only resume;
//   - monthly: $7 with more than 3 days left, $6 with 3 days or fewer, but only
//     if that price is strictly below what the subscriber pays now, its price id
//     is configured, and the current price is known; otherwise only resume.
func ComputeRetentionOffer(sub *Subscription, now time.Time, prices RetentionPrices, fallback float64) *Offer {
	end, ok := CancelledEnd(sub, now)
	if !ok {
		return nil
	}
	cur := fallback
	var curCents int64 = -1
	currency, known := NormalizeCurrency(sub.Currency)
	if sub.UnitAmountCents != nil && known {
		curCents = *sub.UnitAmountCents
		cur = float64(curCents) / 100
	}
	resume := &Offer{Kind: OfferResume, Price: cur, Currency: currency, NextChargeAt: end}
	if sub.Interval != IntervalMonthly || curCents < 0 {
		return resume
	}
	kind, id, cents := OfferRetain7, prices.Retain7ID, int64(700)
	if end.Sub(now) <= retainWindow {
		kind, id, cents = OfferRetain6, prices.Retain6ID, 600
	}
	if id == "" || cents >= curCents {
		return resume
	}
	return &Offer{Kind: kind, Price: float64(cents) / 100, Currency: currency, NextChargeAt: end, PriceID: id}
}

// AnalyticsOffer maps an offer kind to the server_subscription_retained enum.
func AnalyticsOffer(kind string) string {
	if kind == OfferRetain7 || kind == OfferRetain6 {
		return kind
	}
	return "none"
}
