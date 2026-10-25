package billing

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func priceCfg() *config.Config {
	return &config.Config{
		StripePricePremiumMonthly:        "price_pm",
		StripePricePremiumYearly:         "price_py",
		StripePricePremiumMonthlyRetain7: "price_r7",
		StripePricePremiumMonthlyRetain6: "price_r6",
		StripePriceProMonthly:            "price_prom",
		StripePriceProYearly:             "price_proy",
	}
}

// All six configured ids map to their plan; anything else is unknown and
// resolves to premium, never pro.
func TestPriceTiersTable(t *testing.T) {
	pt := NewPriceTiers(priceCfg())
	want := map[string]string{
		"price_pm": TierPremium, "price_py": TierPremium, "price_r7": TierPremium, "price_r6": TierPremium,
		"price_prom": TierPro, "price_proy": TierPro,
	}
	if len(pt) != len(want) {
		t.Fatalf("table = %v, want %d entries", pt, len(want))
	}
	for id, tier := range want {
		got, known := pt.TierFor(id)
		if got != tier || !known {
			t.Errorf("TierFor(%q) = %q,%v want %q,true", id, got, known, tier)
		}
	}
	for _, id := range []string{"price_unknown", "", "PRICE_PROM"} {
		if got, known := pt.TierFor(id); got != TierPremium || known {
			t.Errorf("TierFor(%q) = %q,%v want premium,false", id, got, known)
		}
	}
	// Unset ids are skipped, so "" never matches.
	empty := NewPriceTiers(&config.Config{StripePricePremiumMonthly: "price_pm"})
	if len(empty) != 1 {
		t.Fatalf("unset ids must be skipped: %v", empty)
	}
	if len(NewPriceTiers(nil)) != 0 {
		t.Fatal("nil config must give an empty table")
	}
	// The same id configured for both plans resolves to premium.
	clash := priceCfg()
	clash.StripePriceProMonthly = "price_pm"
	if got, _ := NewPriceTiers(clash).TierFor("price_pm"); got != TierPremium {
		t.Fatalf("clash = %q, want premium", got)
	}
	// A nil table: every id is unknown -> premium.
	var none PriceTiers
	if got, known := none.TierFor("price_prom"); got != TierPremium || known {
		t.Fatalf("nil table = %q,%v", got, known)
	}
}

// Entitlement matrix x {premium price, pro price, unknown price} for every
// Stripe status: the status rules are unchanged, only Decision.Tier follows
// the price, and only granting decisions flag an unknown price.
func TestEntitlementMatrixByPlan(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	grace := 7 * 24 * time.Hour
	end := now.Add(20 * 24 * time.Hour)
	pt := NewPriceTiers(priceCfg())
	type want struct {
		change bool
		tier   string // "" -> the plan's tier
		source string
	}
	statuses := map[string]want{
		StatusTrialing:          {true, "", "subscription"},
		StatusActive:            {true, "", "subscription"},
		StatusPastDue:           {true, "", "subscription"},
		StatusCanceled:          {true, TierFree, "default"},
		StatusUnpaid:            {true, TierFree, "default"},
		StatusIncompleteExpired: {true, TierFree, "default"},
		StatusPaused:            {true, TierFree, "default"},
		StatusIncomplete:        {false, "", ""},
	}
	plans := []struct {
		price, tier string
		unknown     bool
	}{
		{"price_pm", TierPremium, false}, {"price_py", TierPremium, false},
		{"price_r7", TierPremium, false}, {"price_r6", TierPremium, false},
		{"price_prom", TierPro, false}, {"price_proy", TierPro, false},
		{"price_mystery", TierPremium, true}, {"", TierPremium, true},
	}
	for status, w := range statuses {
		for _, p := range plans {
			d := Entitlement(&Subscription{Status: status, PriceID: p.price, CurrentPeriodEnd: end}, now, grace, pt)
			wantTier := w.tier
			if w.change && wantTier == "" {
				wantTier = p.tier
			}
			if d.Change != w.change || d.Tier != wantTier || d.Source != w.source {
				t.Errorf("%s/%q: got %+v, want change=%v tier=%q source=%q", status, p.price, d, w.change, wantTier, w.source)
			}
			granting := w.change && w.source == "subscription"
			if d.UnknownPrice != (granting && p.unknown) {
				t.Errorf("%s/%q: UnknownPrice=%v", status, p.price, d.UnknownPrice)
			}
			if granting && (d.ExpiresAt == nil || d.ExpiresAt.Before(end)) {
				t.Errorf("%s/%q: expiry %v, want >= period end", status, p.price, d.ExpiresAt)
			}
		}
	}
}

type countMetrics struct{ unknown int }

func (c *countMetrics) BillingUnknownPrice() { c.unknown++ }

// A webhook carrying a Pro price grants pro (granted_by=stripe); one carrying
// an unknown price grants premium with a warning and the metric.
func TestWebhookGrantsTierByPrice(t *testing.T) {
	end := t0.Add(30 * 24 * time.Hour)

	r := newRig(freeUser())
	r.p.Prices = NewPriceTiers(priceCfg())
	m := &countMetrics{}
	r.p.Metrics = m
	sub := activeSub(end)
	sub.PriceID = "price_proy"
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_pro", EventSubscriptionCreated, subObj); err != nil {
		t.Fatal(err)
	}
	if len(r.store.TierWrites) != 1 {
		t.Fatalf("writes = %+v", r.store.TierWrites)
	}
	if w := r.store.TierWrites[0]; w.Tier != TierPro || w.GrantedBy != GrantedByStripe || w.Source != "subscription" {
		t.Fatalf("pro write = %+v", w)
	}
	if m.unknown != 0 {
		t.Fatal("a configured pro price is not unknown")
	}

	var logs bytes.Buffer
	r2 := newRig(freeUser())
	r2.p.Prices = NewPriceTiers(priceCfg())
	r2.p.Log = slog.New(slog.NewTextHandler(&logs, nil))
	m2 := &countMetrics{}
	r2.p.Metrics = m2
	sub2 := activeSub(end)
	sub2.PriceID = "price_typo"
	r2.fc.PutSubscription(sub2)
	if err := r2.deliver(t, "evt_unk", EventSubscriptionCreated, subObj); err != nil {
		t.Fatal(err)
	}
	if len(r2.store.TierWrites) != 1 || r2.store.TierWrites[0].Tier != TierPremium {
		t.Fatalf("unknown price writes = %+v", r2.store.TierWrites)
	}
	if m2.unknown != 1 {
		t.Fatalf("unknown-price metric = %d, want 1", m2.unknown)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "price_typo") {
		t.Fatalf("missing structured warning: %s", out)
	}
}

// A schedule that fired puts a Pro subscriber on a Premium price: the next
// webhook re-reads the price and grants premium.
func TestFiredDowngradeGrantsPremium(t *testing.T) {
	end := t0.Add(30 * 24 * time.Hour)
	u := freeUser()
	u.Tier, u.TierSource, u.TierExpiresAt = TierPro, "subscription", &end
	u.StripeCustomerID, u.StripeSubscriptionID, u.SubscriptionStatus = "cus_1", "sub_1", StatusActive
	r := newRig(u)
	r.p.Prices = NewPriceTiers(priceCfg())
	next := end.Add(30 * 24 * time.Hour)
	sub := activeSub(next)
	sub.PriceID = "price_pm"
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_fire", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	if got := r.store.User("u1"); got.Tier != TierPremium {
		t.Fatalf("tier after fired schedule = %+v", got)
	}
}

// A live manual pro grant is never replaced by a premium subscription, and a
// pro subscription replaces a live manual premium grant only if it outlasts it.
func TestManualProGrantPrecedence(t *testing.T) {
	later := t0.Add(90 * 24 * time.Hour)
	u := freeUser()
	u.Tier, u.TierSource, u.TierExpiresAt = TierPro, "manual_grant", ptr(t0.Add(2*24*time.Hour))
	r := newRig(u)
	r.p.Prices = NewPriceTiers(priceCfg())
	sub := activeSub(later)
	sub.PriceID = "price_pm"
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_mp", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	if len(r.store.TierWrites) != 0 || r.store.User("u1").Tier != TierPro {
		t.Fatalf("premium sub replaced a live pro grant: %+v", r.store.TierWrites)
	}

	u2 := freeUser()
	u2.Tier, u2.TierSource, u2.TierExpiresAt = TierPremium, "manual_grant", ptr(t0.Add(2*24*time.Hour))
	r2 := newRig(u2)
	r2.p.Prices = NewPriceTiers(priceCfg())
	sub2 := activeSub(later)
	sub2.PriceID = "price_prom"
	r2.fc.PutSubscription(sub2)
	if err := r2.deliver(t, "evt_mp2", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	if got := r2.store.User("u1"); got.Tier != TierPro || got.TierSource != "subscription" {
		t.Fatalf("pro sub outlasting a premium grant must win: %+v", got)
	}
}
