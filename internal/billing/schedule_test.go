package billing

import (
	"context"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"
)

func TestSchedulePending(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	end := now.Add(10 * 24 * time.Hour)
	s := &Schedule{ID: "sched", Phases: []SchedulePhase{
		{PriceID: "price_pro", Start: now.Add(-20 * 24 * time.Hour), End: end},
		{PriceID: "price_prem", Start: end},
	}}
	price, at, ok := s.Pending(now)
	if !ok || price != "price_prem" || !at.Equal(end) {
		t.Fatalf("pending = %q %v %v", price, at, ok)
	}
	// After the switch nothing is pending any more.
	if _, _, ok := s.Pending(end.Add(time.Hour)); ok {
		t.Fatal("a fired schedule has nothing pending")
	}
	var none *Schedule
	if _, _, ok := none.Pending(now); ok {
		t.Fatal("nil schedule has nothing pending")
	}
}

// toSchedule keeps only live, expanded schedules and reads each phase's price.
func TestToSchedule(t *testing.T) {
	if toSchedule(nil) != nil || toSchedule(&stripe.SubscriptionSchedule{ID: "sched_1"}) != nil {
		t.Fatal("nil / unexpanded (no status) schedules are not live")
	}
	for _, st := range []stripe.SubscriptionScheduleStatus{stripe.SubscriptionScheduleStatusReleased, stripe.SubscriptionScheduleStatusCanceled, stripe.SubscriptionScheduleStatusCompleted} {
		if toSchedule(&stripe.SubscriptionSchedule{ID: "s", Status: st}) != nil {
			t.Errorf("%s schedule must be ignored", st)
		}
	}
	in := &stripe.SubscriptionSchedule{
		ID: "sub_sched_1", Status: stripe.SubscriptionScheduleStatusActive,
		Metadata: map[string]string{planChangeMetadataKey: "1"},
		Phases: []*stripe.SubscriptionSchedulePhase{
			{StartDate: 100, EndDate: 200, Items: []*stripe.SubscriptionSchedulePhaseItem{{Price: &stripe.Price{ID: "price_pro"}}}},
			{StartDate: 200, Items: []*stripe.SubscriptionSchedulePhaseItem{{Price: &stripe.Price{ID: "price_prem"}}}},
		},
	}
	got := toSchedule(in)
	if got == nil || got.ID != "sub_sched_1" || !got.PlanChange || len(got.Phases) != 2 ||
		got.Phases[0].PriceID != "price_pro" || !got.Phases[0].End.Equal(time.Unix(200, 0)) ||
		got.Phases[1].PriceID != "price_prem" || !got.Phases[1].Start.Equal(time.Unix(200, 0)) || !got.Phases[1].End.IsZero() {
		t.Fatalf("toSchedule = %+v", got)
	}
}

// The subscription conversion surfaces the expanded schedule.
func TestToSubscriptionCarriesSchedule(t *testing.T) {
	s := &stripe.Subscription{ID: "sub_1", Status: stripe.SubscriptionStatusActive,
		Schedule: &stripe.SubscriptionSchedule{ID: "sched", Status: stripe.SubscriptionScheduleStatusActive},
		Items:    &stripe.SubscriptionItemList{Data: []*stripe.SubscriptionItem{{ID: "si_1", Price: &stripe.Price{ID: "price_pro"}, CurrentPeriodEnd: 300}}}}
	out := toSubscription(s)
	if out.Schedule == nil || out.Schedule.ID != "sched" || out.PriceID != "price_pro" || out.ItemID != "si_1" {
		t.Fatalf("toSubscription = %+v", out)
	}
}

func TestFakeClientPlanChange(t *testing.T) {
	ctx := context.Background()
	f := NewFakeClient(testSecret)
	f.PriceCents = map[string]int64{"price_pro": 1500}
	f.PutSubscription(&Subscription{ID: "sub_1", ItemID: "si_1", PriceID: "price_prem"})
	sub, err := f.UpdateSubscriptionPrice(ctx, "sub_1", "si_1", "price_pro", ProrationCreate)
	if err != nil || sub.PriceID != "price_pro" || sub.UnitAmountCents == nil || *sub.UnitAmountCents != 1500 {
		t.Fatalf("update = %+v %v", sub, err)
	}
	at := time.Unix(1_800_000_000, 0).UTC()
	sched, err := f.CreateSchedule(ctx, "sub_1", "price_pro", "price_prem", "eur", at)
	if err != nil || sched == nil || len(sched.Phases) != 2 {
		t.Fatalf("schedule = %+v %v", sched, err)
	}
	if _, err := f.CreateSchedule(ctx, "sub_1", "price_pro", "price_prem", "eur", at); err == nil {
		t.Fatal("a second schedule must be refused, like Stripe")
	}
	got, _ := f.GetSchedule(ctx, "sub_1")
	if got == nil || got.ID != sched.ID {
		t.Fatalf("get = %+v", got)
	}
	if err := f.ReleaseSchedule(ctx, sched.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.GetSchedule(ctx, "sub_1"); got != nil {
		t.Fatal("released schedule still attached")
	}
	if err := f.ReleaseSchedule(ctx, sched.ID); err != ErrNotFound {
		t.Fatalf("second release = %v, want ErrNotFound", err)
	}
}

func TestSubscriptionMetadataCarriesPlan(t *testing.T) {
	m := subscriptionMetadata(CheckoutRequest{UserID: "u1", Plan: "pro"})
	if m["plan"] != "pro" || m["user_id"] != "u1" {
		t.Fatalf("metadata = %v", m)
	}
	if _, has := subscriptionMetadata(CheckoutRequest{UserID: "u1"})["plan"]; has {
		t.Fatal("empty plan must be omitted")
	}
}
