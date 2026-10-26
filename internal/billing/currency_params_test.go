package billing

import (
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"
)

// MM69: every Checkout Session carries an explicit currency. Without one,
// Stripe picks a multi-currency Price's currency from the buyer's IP, so the
// visitor could be charged in a currency the page never showed.
func TestApplyCurrencyAlwaysSetsCheckoutCurrency(t *testing.T) {
	for in, want := range map[string]string{"eur": "eur", "EUR": "eur", "usd": "usd", "": "usd", "gbp": "usd"} {
		p := &stripe.CheckoutSessionCreateParams{}
		applyCurrency(p, in)
		if p.Currency == nil || *p.Currency != want {
			t.Errorf("applyCurrency(%q) = %v, want %q", in, p.Currency, want)
		}
	}
}

// MM69: the downgrade schedule's phases are pinned to the subscription's
// currency, so a EUR subscriber is never switched to the Price default (usd).
func TestPinPhaseCurrency(t *testing.T) {
	a, b := &stripe.SubscriptionScheduleUpdatePhaseParams{}, &stripe.SubscriptionScheduleUpdatePhaseParams{}
	pinPhaseCurrency("eur", a, b)
	if a.Currency == nil || *a.Currency != "eur" || b.Currency == nil || *b.Currency != "eur" {
		t.Fatalf("phases not pinned: %v %v", a.Currency, b.Currency)
	}
	c := &stripe.SubscriptionScheduleUpdatePhaseParams{}
	pinPhaseCurrency("", c)
	if c.Currency != nil {
		t.Errorf("an unknown currency must leave the phase to Stripe, got %v", *c.Currency)
	}
}

// MM69: the Subscription's own currency wins over the item price's default
// currency (a multi-currency Price reports its default, usd, there).
func TestToSubscriptionCurrency(t *testing.T) {
	item := &stripe.SubscriptionItem{ID: "si_1", Price: &stripe.Price{ID: "price_1", Currency: "usd", UnitAmount: 800,
		Recurring: &stripe.PriceRecurring{Interval: stripe.PriceRecurringIntervalMonth}}, CurrentPeriodEnd: time.Now().Unix()}
	s := &stripe.Subscription{ID: "sub_1", Status: "active", Currency: "eur", Items: &stripe.SubscriptionItemList{Data: []*stripe.SubscriptionItem{item}}}
	if got := toSubscription(s); got.Currency != "eur" || got.UnitAmountCents == nil || *got.UnitAmountCents != 800 {
		t.Fatalf("currency = %q amount = %v", got.Currency, got.UnitAmountCents)
	}
	s.Currency = ""
	if got := toSubscription(s); got.Currency != "usd" {
		t.Fatalf("fallback to the price currency, got %q", got.Currency)
	}
}
