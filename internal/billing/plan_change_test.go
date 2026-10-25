package billing

import (
	"context"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
)

func TestPlanChangeSource(t *testing.T) {
	cases := map[string]Event{
		"change_plan": {RequestID: "req_1", IdempotencyKey: "plan-sub_1-price_pro"},
		"webhook":     {},
		"portal":      {RequestID: "req_2", IdempotencyKey: "4d2c-portal-key"},
	}
	for want, ev := range cases {
		if got := PlanChangeSource(ev); got != want {
			t.Errorf("PlanChangeSource(%+v) = %q, want %q", ev, got, want)
		}
	}
}

// The event envelope's request id and idempotency key are carried through
// signature verification.
func TestConstructEventCarriesRequest(t *testing.T) {
	f := NewFakeClient(testSecret)
	body := []byte(`{"id":"evt_r","object":"event","api_version":"2026-09-30.endive","type":"customer.subscription.updated","created":1,"request":{"id":"req_9","idempotency_key":"plan-sub_1-price_pro"},"data":{"object":{"id":"sub_1","customer":"cus_1"}}}`)
	ev, err := f.ConstructEvent(body, SignPayload(body, testSecret, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if ev.RequestID != "req_9" || ev.IdempotencyKey != "plan-sub_1-price_pro" || PlanChangeSource(ev) != "change_plan" {
		t.Fatalf("ev = %+v", ev)
	}
	auto := []byte(`{"id":"evt_a","object":"event","api_version":"2026-09-30.endive","type":"customer.subscription.updated","created":1,"request":{"id":null,"idempotency_key":null},"data":{"object":{"id":"sub_1"}}}`)
	ev, err = f.ConstructEvent(auto, SignPayload(auto, testSecret, time.Now()))
	if err != nil || PlanChangeSource(ev) != "webhook" {
		t.Fatalf("automatic = %+v %v", ev, err)
	}
}

func eventsNamed(r *recEmitter, name string) []analyticsemit.Event {
	var out []analyticsemit.Event
	for _, e := range r.events {
		if e.EventName == name {
			out = append(out, e)
		}
	}
	return out
}

// server_plan_changed fires once when the price tier differs from the stored
// subscription tier, with the source from the event envelope; the
// server_subscription_* events carry plan.
func TestServerPlanChangedAndPlanProps(t *testing.T) {
	end := t0.Add(30 * 24 * time.Hour)
	u := freeUser()
	u.Tier, u.TierSource, u.TierExpiresAt = TierPremium, "subscription", &end
	u.StripeCustomerID, u.StripeSubscriptionID, u.SubscriptionStatus = "cus_1", "sub_1", StatusActive
	r := newRig(u)
	r.p.Prices = NewPriceTiers(priceCfg())
	sub := activeSub(end)
	sub.PriceID = "price_prom"
	r.fc.PutSubscription(sub)

	// A Portal switch (request id, no plan- key).
	body := []byte(`{"id":"evt_p","object":"event","api_version":"2026-09-30.endive","type":"customer.subscription.updated","created":1,"request":{"id":"req_portal","idempotency_key":"abc"},"data":{"object":` + subObj + `}}`)
	ev, err := r.fc.ConstructEvent(body, SignPayload(body, testSecret, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.p.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	got := eventsNamed(r.em, analyticsemit.ServerPlanChanged)
	if len(got) != 1 || got[0].Properties["from"] != TierPremium || got[0].Properties["to"] != TierPro || got[0].Properties["source"] != "portal" {
		t.Fatalf("plan changed = %+v", got)
	}
	// Redelivery / a second event in the same state: nothing new.
	if err := r.deliver(t, "evt_p2", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	if n := len(eventsNamed(r.em, analyticsemit.ServerPlanChanged)); n != 1 {
		t.Fatalf("plan changed fired %d times", n)
	}

	// The schedule firing (automatic, no request) back to premium: source webhook.
	next := end.Add(30 * 24 * time.Hour)
	sub2 := activeSub(next)
	sub2.PriceID = "price_pm"
	r.fc.PutSubscription(sub2)
	if err := r.deliver(t, "evt_fire", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	got = eventsNamed(r.em, analyticsemit.ServerPlanChanged)
	if len(got) != 2 || got[1].Properties["to"] != TierPremium || got[1].Properties["source"] != "webhook" {
		t.Fatalf("fired = %+v", got)
	}

	// A first purchase is not a plan change, and created carries plan.
	f := newRig(freeUser())
	f.p.Prices = NewPriceTiers(priceCfg())
	s3 := activeSub(end)
	s3.PriceID = "price_proy"
	f.fc.PutSubscription(s3)
	if err := f.deliver(t, "evt_new", EventCheckoutCompleted, `{"id":"cs_1","customer":"cus_1","subscription":"sub_1","client_reference_id":"u1"}`); err != nil {
		t.Fatal(err)
	}
	if n := len(eventsNamed(f.em, analyticsemit.ServerPlanChanged)); n != 0 {
		t.Fatal("first purchase must not emit server_plan_changed")
	}
	created := eventsNamed(f.em, analyticsemit.ServerSubscriptionCreated)
	if len(created) != 1 || created[0].Properties["plan"] != TierPro {
		t.Fatalf("created = %+v", created)
	}
}

// ApplyPlanChange applies the re-priced subscription now, labelled change_plan.
func TestApplyPlanChange(t *testing.T) {
	end := t0.Add(30 * 24 * time.Hour)
	u := freeUser()
	u.Tier, u.TierSource, u.TierExpiresAt = TierPremium, "subscription", &end
	u.StripeCustomerID, u.StripeSubscriptionID, u.SubscriptionStatus = "cus_1", "sub_1", StatusActive
	r := newRig(u)
	r.p.Prices = NewPriceTiers(priceCfg())
	sub := activeSub(end)
	sub.PriceID = "price_prom"
	r.fc.PutSubscription(sub)
	if err := r.p.ApplyPlanChange(context.Background(), sub, "u1"); err != nil {
		t.Fatal(err)
	}
	if got := r.store.User("u1"); got.Tier != TierPro {
		t.Fatalf("user = %+v", got)
	}
	got := eventsNamed(r.em, analyticsemit.ServerPlanChanged)
	if len(got) != 1 || got[0].Properties["source"] != "change_plan" {
		t.Fatalf("events = %+v", got)
	}
	if err := r.p.ApplyPlanChange(context.Background(), nil, "u1"); err != nil {
		t.Fatal("nil subscription is a no-op")
	}
}
