package billing

import (
	"context"
	"testing"
	"time"
)

func i64(v int64) *int64 { return &v }

func TestComputeRetentionOffer(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	both := RetentionPrices{Retain7ID: "price_r7", Retain6ID: "price_r6"}
	mk := func(interval string, cents int64, end time.Time) *Subscription {
		return &Subscription{ID: "sub_1", Status: StatusActive, CancelAtPeriodEnd: true, Interval: interval,
			CurrentPeriodEnd: end, UnitAmountCents: i64(cents), Currency: "usd"}
	}
	cases := []struct {
		name      string
		sub       *Subscription
		prices    RetentionPrices
		wantKind  string // "" = nil
		wantPrice float64
		wantID    string
	}{
		{"long left -> $7", mk("monthly", 800, now.Add(10*24*time.Hour)), both, OfferRetain7, 7, "price_r7"},
		{"3 days + 1s -> $7", mk("monthly", 800, now.Add(72*time.Hour+time.Second)), both, OfferRetain7, 7, "price_r7"},
		{"exactly 3 days -> $6", mk("monthly", 800, now.Add(72*time.Hour)), both, OfferRetain6, 6, "price_r6"},
		{"1 hour left -> $6", mk("monthly", 800, now.Add(time.Hour)), both, OfferRetain6, 6, "price_r6"},
		{"period already ended", mk("monthly", 800, now.Add(-time.Second)), both, "", 0, ""},
		{"period ends exactly now", mk("monthly", 800, now), both, "", 0, ""},
		{"yearly -> resume only", mk("yearly", 8000, now.Add(100*24*time.Hour)), both, OfferResume, 80, ""},
		{"already pays $6 -> resume", mk("monthly", 600, now.Add(10*24*time.Hour)), both, OfferResume, 6, ""},
		{"already pays $7, 3d+ -> resume", mk("monthly", 700, now.Add(10*24*time.Hour)), both, OfferResume, 7, ""},
		{"already pays $7, <=3d -> $6", mk("monthly", 700, now.Add(24*time.Hour)), both, OfferRetain6, 6, "price_r6"},
		{"RETAIN7 unset -> resume", mk("monthly", 800, now.Add(10*24*time.Hour)), RetentionPrices{Retain6ID: "price_r6"}, OfferResume, 8, ""},
		{"RETAIN6 unset -> resume", mk("monthly", 800, now.Add(time.Hour)), RetentionPrices{Retain7ID: "price_r7"}, OfferResume, 8, ""},
		{"both unset -> resume", mk("monthly", 800, now.Add(time.Hour)), RetentionPrices{}, OfferResume, 8, ""},
		{"not cancelled", &Subscription{Status: StatusActive, Interval: "monthly", CurrentPeriodEnd: now.Add(time.Hour * 100), UnitAmountCents: i64(800), Currency: "usd"}, both, "", 0, ""},
		{"past_due", func() *Subscription {
			s := mk("monthly", 800, now.Add(100*time.Hour))
			s.Status = StatusPastDue
			return s
		}(), both, "", 0, ""},
		{"unknown price -> resume at fallback", func() *Subscription {
			s := mk("monthly", 800, now.Add(100*time.Hour))
			s.UnitAmountCents, s.Currency = nil, ""
			return s
		}(), both, OfferResume, 8, ""},
		{"cancel_at only (portal)", func() *Subscription {
			s := mk("monthly", 800, now.Add(20*24*time.Hour))
			s.CancelAtPeriodEnd, s.CancelAt = false, now.Add(2*24*time.Hour)
			return s
		}(), both, OfferRetain6, 6, "price_r6"},
		{"nil", nil, both, "", 0, ""},
		// MM69: a EUR subscription gets the same numerals, in euros.
		{"eur long left -> 7", func() *Subscription {
			s := mk("monthly", 800, now.Add(10*24*time.Hour))
			s.Currency = "eur"
			return s
		}(), both, OfferRetain7, 7, "price_r7"},
		{"eur <=3d -> 6", func() *Subscription {
			s := mk("monthly", 800, now.Add(time.Hour))
			s.Currency = "eur"
			return s
		}(), both, OfferRetain6, 6, "price_r6"},
		{"unsupported currency -> resume at fallback", func() *Subscription {
			s := mk("monthly", 800, now.Add(10*24*time.Hour))
			s.Currency = "gbp"
			return s
		}(), both, OfferResume, 8, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := ComputeRetentionOffer(tc.sub, now, tc.prices, 8)
			if tc.wantKind == "" {
				if o != nil {
					t.Fatalf("want nil, got %+v", o)
				}
				return
			}
			if o == nil || o.Kind != tc.wantKind || o.Price != tc.wantPrice || o.PriceID != tc.wantID {
				t.Fatalf("got %+v", o)
			}
		})
	}
}

func TestCancelAtCountsAsCancelled(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	end := now.Add(10 * 24 * time.Hour)
	s := &Subscription{CurrentPeriodEnd: end, CancelAt: end}
	if !s.IsCancelling(now) {
		t.Fatal("cancel_at in the future must count")
	}
	s.CancelAt = now.Add(-time.Hour)
	if s.IsCancelling(now) {
		t.Fatal("past cancel_at must not count")
	}
	s.CancelAt = end.Add(24 * time.Hour)
	if s.IsCancelling(now) {
		t.Fatal("cancel_at after the next renewal is not ending yet")
	}
}

func TestPortalCancelStaysPremiumUntilPeriodEndThenDeletedDowngrades(t *testing.T) {
	r := newRig(freeUser())
	end := t0.Add(30 * 24 * time.Hour)
	sub := activeSub(end)
	sub.CancelAtPeriodEnd, sub.CancelAt = false, end // newer-API portal shape: cancel_at only
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_u", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	u := r.store.User("u1")
	if u.Tier != "premium" || !u.CancelAtPeriodEnd || u.TierExpiresAt == nil || !u.TierExpiresAt.Equal(end) {
		t.Fatalf("cancelled-but-active: %+v", u)
	}
	// Period ends: Stripe deletes the subscription.
	del := *sub
	del.Status = StatusCanceled
	r.fc.PutSubscription(&del)
	if err := r.deliver(t, "evt_del", EventSubscriptionDeleted, subObj); err != nil {
		t.Fatal(err)
	}
	u = r.store.User("u1")
	if u.Tier != "free" || u.TierSource != "default" || u.TierExpiresAt != nil {
		t.Fatalf("after deleted: %+v", u)
	}
}

func TestApplyRetainedFlipsMirrorAndWebhookIsNoOp(t *testing.T) {
	r := newRig(freeUser())
	end := t0.Add(10 * 24 * time.Hour)
	sub := activeSub(end)
	sub.CancelAtPeriodEnd = true
	sub.UnitAmountCents, sub.Currency, sub.ItemID = i64(800), "usd", "si_1"
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_1", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	r.fc.PriceCents = map[string]int64{"price_r7": 700}
	updated, err := r.fc.ResumeSubscription(context.Background(), "sub_1", "si_1", "price_r7", OfferRetain7)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.p.ApplyRetained(context.Background(), updated, "u1", "retain7"); err != nil {
		t.Fatal(err)
	}
	if u := r.store.User("u1"); u.CancelAtPeriodEnd || u.Tier != "premium" {
		t.Fatalf("not flipped: %+v", u)
	}
	writes := len(r.store.TierWrites)
	if err := r.deliver(t, "evt_2", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	if len(r.store.TierWrites) != writes || r.store.Events["evt_2"].Note != NoteTierUnchanged {
		t.Fatalf("webhook not a no-op: %d writes, note %q", len(r.store.TierWrites)-writes, r.store.Events["evt_2"].Note)
	}
	if p := r.props(analyticsemit_retained); p["offer"] != "retain7" || p["interval"] != IntervalMonthly {
		t.Fatalf("retained props = %v", p)
	}
}

const analyticsemit_retained = "server_subscription_retained"

// MM69: the offer names the subscription's currency, and the retain price id
// is the same for both currencies (Stripe keeps the subscription's currency
// on the swap, so no currency is ever passed).
func TestRetentionOfferCurrency(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, cur := range []string{"usd", "eur"} {
		sub := &Subscription{ID: "sub_1", Status: StatusActive, CancelAtPeriodEnd: true, Interval: "monthly",
			CurrentPeriodEnd: now.Add(10 * 24 * time.Hour), UnitAmountCents: i64(800), Currency: cur}
		o := ComputeRetentionOffer(sub, now, RetentionPrices{Retain7ID: "price_r7", Retain6ID: "price_r6"}, 8)
		if o == nil || o.Currency != cur || o.PriceID != "price_r7" || o.Price != 7 {
			t.Errorf("%s: offer = %+v", cur, o)
		}
	}
}
