package telemetry

import (
	"strings"
	"testing"
)

func TestUserColumnsIncludeBillingMirror(t *testing.T) {
	for _, col := range []string{"stripe_customer_id", "stripe_subscription_id", "subscription_status", "billing_interval", "cancel_at_period_end"} {
		if !strings.Contains(userColumns, col) {
			t.Errorf("userColumns missing %s; scanUser and the SELECT would drift", col)
		}
	}
}

func TestBillingQueriesShape(t *testing.T) {
	if !strings.Contains(insertBillingEventQuery, "ON CONFLICT (event_id) DO NOTHING") {
		t.Error("billing event insert must be idempotent")
	}
	if !strings.Contains(setStripeCustomerQuery, "COALESCE(stripe_customer_id") {
		t.Error("SetStripeCustomer must keep the first customer id")
	}
	for name, q := range map[string]string{"update": updateSubscriptionStateQuery, "set customer": setStripeCustomerQuery} {
		for _, forbidden := range []string{"tier =", "tier_source", "tier_expires_at"} {
			if strings.Contains(q, forbidden) {
				t.Errorf("%s query writes %q; only SetUserTier may change tiers", name, forbidden)
			}
		}
	}
}
