package billing

import "time"

// Decision is what the status -> tier matrix says to do to a user's tier.
type Decision struct {
	// Change is false when the subscription state must not alter the tier.
	Change    bool
	Tier      string // "free" | "premium"
	Source    string // "subscription" | "default"
	ExpiresAt *time.Time
}

// Entitlement is the single pure status -> tier matrix (ticket decision 9).
//
//	trialing|active                                -> premium, expires at items.data[0].current_period_end
//	past_due                                       -> premium, expires at max(period end, now+grace)
//	canceled|unpaid|incomplete_expired|paused      -> free, source default, no expiry
//	incomplete (and anything unknown)              -> no change
//
// cancel_at_period_end is deliberately ignored (cancel_at only shortens the expiry): the tier stays premium until
// the period ends and tiers.Resolve downgrades it by expiry.
func Entitlement(sub *Subscription, now time.Time, grace time.Duration) Decision {
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
		// A scheduled cancel_at inside this period is when Premium really ends.
		if !sub.CancelAt.IsZero() && sub.CancelAt.After(now) && sub.CancelAt.Before(end) {
			end = sub.CancelAt.UTC()
		}
		return Decision{Change: true, Tier: "premium", Source: "subscription", ExpiresAt: &end}
	case StatusPastDue:
		end := now.Add(grace).UTC()
		if pe := sub.CurrentPeriodEnd.UTC(); pe.After(end) {
			end = pe
		}
		return Decision{Change: true, Tier: "premium", Source: "subscription", ExpiresAt: &end}
	case StatusCanceled, StatusUnpaid, StatusIncompleteExpired, StatusPaused:
		return Decision{Change: true, Tier: "free", Source: "default"}
	default:
		return Decision{}
	}
}
