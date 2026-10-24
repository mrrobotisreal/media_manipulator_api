package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

const testSecret = "whsec_unit_test"

var t0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

type recEmitter struct {
	mu     sync.Mutex
	events []analyticsemit.Event
}

func (r *recEmitter) Emit(e analyticsemit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recEmitter) names() []string {
	var out []string
	for _, e := range r.events {
		out = append(out, e.EventName)
	}
	return out
}

type rig struct {
	store *FakeStore
	fc    *FakeClient
	em    *recEmitter
	p     *Processor
}

func newRig(u *models.User) *rig {
	st := NewFakeStore(u)
	fc := NewFakeClient(testSecret)
	em := &recEmitter{}
	return &rig{st, fc, em, &Processor{Store: st, Client: fc, Emitter: em, Grace: 7 * 24 * time.Hour, Now: func() time.Time { return t0 }}}
}

func freeUser() *models.User {
	return &models.User{ID: "u1", FirebaseUID: "fb1", Email: "a@example.com", Tier: "free", TierSource: "default"}
}

func activeSub(end time.Time) *Subscription {
	return &Subscription{ID: "sub_1", CustomerID: "cus_1", Status: StatusActive, CurrentPeriodEnd: end, Interval: IntervalMonthly,
		UserID: "u1", VisitorID: "11111111-1111-4111-8111-111111111111", SessionID: "22222222-2222-4222-8222-222222222222"}
}

// signed builds a signed webhook through the real verification path.
func (r *rig) deliver(t *testing.T, id, typ, object string) error {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"id":%q,"object":"event","api_version":"2026-09-30.endive","type":%q,"created":1,"data":{"object":%s}}`, id, typ, object))
	ev, err := r.fc.ConstructEvent(body, SignPayload(body, testSecret, time.Now()))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return r.p.Handle(context.Background(), ev)
}

const subObj = `{"id":"sub_1","customer":"cus_1","metadata":{"user_id":"u1"}}`

func TestWebhookActivatesPremiumFromCheckoutCompleted(t *testing.T) {
	r := newRig(freeUser())
	end := t0.Add(30 * 24 * time.Hour)
	r.fc.PutSubscription(activeSub(end))
	if err := r.deliver(t, "evt_1", EventCheckoutCompleted, `{"id":"cs_1","customer":"cus_1","subscription":"sub_1","client_reference_id":"u1"}`); err != nil {
		t.Fatal(err)
	}
	if len(r.store.TierWrites) != 1 {
		t.Fatalf("tier writes = %d", len(r.store.TierWrites))
	}
	w := r.store.TierWrites[0]
	if w.Tier != "premium" || w.Source != "subscription" || w.GrantedBy != "stripe" || w.Reason != EventCheckoutCompleted || !w.ExpiresAt.Equal(end) {
		t.Fatalf("write = %+v", w)
	}
	u := r.store.User("u1")
	if u.StripeCustomerID != "cus_1" || u.StripeSubscriptionID != "sub_1" || u.SubscriptionStatus != "active" || u.BillingInterval != "monthly" {
		t.Fatalf("mirror = %+v", u)
	}
	got := r.em.names()
	if len(got) != 2 || got[0] != analyticsemit.ServerTierChanged || got[1] != analyticsemit.ServerSubscriptionCreated {
		t.Fatalf("events = %v", got)
	}
	if r.em.events[0].VisitorID == "" || r.em.events[0].PersonHint.FirebaseUID != "fb1" {
		t.Fatalf("identity not carried: %+v", r.em.events[0])
	}
}

func TestWebhookReplayAppliesExactlyOnce(t *testing.T) {
	r := newRig(freeUser())
	r.fc.PutSubscription(activeSub(t0.Add(30 * 24 * time.Hour)))
	for i := 0; i < 3; i++ {
		if err := r.deliver(t, "evt_same", EventSubscriptionCreated, subObj); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.store.TierWrites) != 1 {
		t.Fatalf("SetUserTier called %d times for one event id", len(r.store.TierWrites))
	}
	if len(r.fc.GetSubCalls) != 1 {
		t.Fatalf("replays must do no work, GetSubscription calls = %d", len(r.fc.GetSubCalls))
	}
}

func TestWebhookFailedProcessingIsRetriedNotLost(t *testing.T) {
	r := newRig(freeUser())
	r.fc.PutSubscription(activeSub(t0.Add(time.Hour * 24)))
	r.store.FailSetTier = errors.New("db down")
	if err := r.deliver(t, "evt_r", EventSubscriptionUpdated, subObj); err == nil {
		t.Fatal("expected error so Stripe retries")
	}
	if r.store.Events["evt_r"].Processed || r.store.Events["evt_r"].Note == "" {
		t.Fatalf("failure must store error and leave processed unset: %+v", r.store.Events["evt_r"])
	}
	r.store.FailSetTier = nil
	if err := r.deliver(t, "evt_r", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	if len(r.store.TierWrites) != 1 || !r.store.Events["evt_r"].Processed {
		t.Fatalf("retry did not converge: %+v", r.store.TierWrites)
	}
}

func TestWebhookOutOfOrderConverges(t *testing.T) {
	// subscription.updated (carrying stale "past_due" in its body) arrives BEFORE
	// checkout.session.completed; Stripe's current state is active. The body is
	// ignored; the re-fetch decides, and the user is found by metadata because
	// the customer is not linked yet.
	r := newRig(freeUser())
	end := t0.Add(30 * 24 * time.Hour)
	r.fc.PutSubscription(activeSub(end))
	if err := r.deliver(t, "evt_a", EventSubscriptionUpdated, `{"id":"sub_1","customer":"cus_1","status":"past_due","metadata":{"user_id":"u1"}}`); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(t, "evt_b", EventCheckoutCompleted, `{"id":"cs_1","customer":"cus_1","subscription":"sub_1","client_reference_id":"u1"}`); err != nil {
		t.Fatal(err)
	}
	u := r.store.User("u1")
	if u.Tier != "premium" || u.SubscriptionStatus != "active" || u.StripeCustomerID != "cus_1" {
		t.Fatalf("did not converge: %+v", u)
	}
	if len(r.store.TierWrites) != 1 {
		t.Fatalf("second event must be a no-op tier-wise, writes = %d", len(r.store.TierWrites))
	}
}

func TestWebhookPastDueGrantsGrace(t *testing.T) {
	u := freeUser()
	r := newRig(u)
	sub := activeSub(t0.Add(-time.Hour))
	sub.Status = StatusPastDue
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_pd", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	w := r.store.TierWrites[0]
	if w.Tier != "premium" || !w.ExpiresAt.Equal(t0.Add(7*24*time.Hour)) {
		t.Fatalf("grace write = %+v", w)
	}
}

func TestInvoicePaymentFailedExtendsExpiryToGrace(t *testing.T) {
	r := newRig(freeUser())
	// Still "active" in Stripe (first retry pending) with only 2 days left.
	r.fc.PutSubscription(activeSub(t0.Add(48 * time.Hour)))
	err := r.deliver(t, "evt_pf", EventInvoicePaymentFailed, `{"id":"in_1","customer":"cus_1","parent":{"subscription_details":{"subscription":"sub_1","metadata":{"user_id":"u1"}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.store.TierWrites[0].ExpiresAt; !got.Equal(t0.Add(7 * 24 * time.Hour)) {
		t.Fatalf("expiry = %v, want now+7d", got)
	}
	names := r.em.names()
	if names[len(names)-1] != analyticsemit.ServerSubscriptionPaymentFailed {
		t.Fatalf("events = %v", names)
	}
}

func TestCancelAtPeriodEndKeepsPremiumAndSetsColumn(t *testing.T) {
	r := newRig(freeUser())
	end := t0.Add(10 * 24 * time.Hour)
	sub := activeSub(end)
	sub.CancelAtPeriodEnd = true
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_c", EventSubscriptionUpdated, subObj); err != nil {
		t.Fatal(err)
	}
	u := r.store.User("u1")
	if u.Tier != "premium" || !u.CancelAtPeriodEnd || !u.TierExpiresAt.Equal(end) {
		t.Fatalf("user = %+v", u)
	}
	if tiers.Resolve(true, u) != tiers.TierPremium {
		t.Fatal("must stay premium until period end")
	}
}

func TestSubscriptionDeletedDowngradesAndEmitsCancelled(t *testing.T) {
	u := freeUser()
	u.Tier, u.TierSource, u.StripeCustomerID, u.StripeSubscriptionID, u.SubscriptionStatus = "premium", "subscription", "cus_1", "sub_1", "active"
	exp := t0.Add(5 * 24 * time.Hour)
	u.TierExpiresAt = &exp
	r := newRig(u)
	sub := activeSub(exp)
	sub.Status = StatusCanceled
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_d", EventSubscriptionDeleted, subObj); err != nil {
		t.Fatal(err)
	}
	w := r.store.TierWrites[0]
	if w.Tier != "free" || w.Source != "default" || w.ExpiresAt != nil {
		t.Fatalf("write = %+v", w)
	}
	found := false
	for _, n := range r.em.names() {
		found = found || n == analyticsemit.ServerSubscriptionCancelled
	}
	if !found {
		t.Fatalf("events = %v", r.em.names())
	}
}

func TestManualGrantPrecedence(t *testing.T) {
	later := t0.Add(90 * 24 * time.Hour)
	cases := []struct {
		name   string
		expiry *time.Time
		subEnd time.Time
		status string
		wantWr int
	}{
		{"grant outlasts subscription", &later, t0.Add(30 * 24 * time.Hour), StatusActive, 0},
		{"grant with no expiry outlasts subscription", nil, t0.Add(30 * 24 * time.Hour), StatusActive, 0},
		{"cancellation never downgrades a live manual grant", &later, t0, StatusCanceled, 0},
		{"subscription outlasts grant", ptr(t0.Add(2 * 24 * time.Hour)), t0.Add(30 * 24 * time.Hour), StatusActive, 1},
		{"lapsed grant is replaceable", ptr(t0.Add(-time.Hour)), t0.Add(30 * 24 * time.Hour), StatusActive, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := freeUser()
			u.Tier, u.TierSource, u.TierExpiresAt = "premium", "manual_grant", tc.expiry
			r := newRig(u)
			sub := activeSub(tc.subEnd)
			sub.Status = tc.status
			r.fc.PutSubscription(sub)
			if err := r.deliver(t, "evt_m", EventSubscriptionUpdated, subObj); err != nil {
				t.Fatal(err)
			}
			if len(r.store.TierWrites) != tc.wantWr {
				t.Fatalf("writes = %d, want %d", len(r.store.TierWrites), tc.wantWr)
			}
			if tc.wantWr == 0 {
				if r.store.Events["evt_m"].Note != NoteManualGrant || !r.store.Events["evt_m"].Processed {
					t.Fatalf("event = %+v", r.store.Events["evt_m"])
				}
				if r.store.User("u1").TierSource != "manual_grant" {
					t.Fatal("manual grant was touched")
				}
			}
		})
	}
}

func TestOperatorIsNeverDowngraded(t *testing.T) {
	// Operators are derived per request from OPERATOR_EMAILS and cannot be stored
	// (mm_users CHECK tier IN ('free','premium')). Even when the webhook
	// downgrades the stored row to free, the resolved tier stays operator.
	cfg := &config.Config{OperatorEmails: []string{"owner@example.com"}}
	reg := tiers.New(cfg)
	u := freeUser()
	u.Email = "owner@example.com"
	u.Tier, u.TierSource, u.StripeCustomerID = "premium", "subscription", "cus_1"
	r := newRig(u)
	sub := activeSub(t0)
	sub.Status = StatusCanceled
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_o", EventSubscriptionDeleted, subObj); err != nil {
		t.Fatal(err)
	}
	stored := r.store.User("u1")
	if stored.Tier != "free" {
		t.Fatalf("stored = %+v", stored)
	}
	if got := reg.ResolveFor(true, "owner@example.com", true, stored); got != tiers.TierOperator {
		t.Fatalf("resolved = %v, want operator", got)
	}
	for _, w := range r.store.TierWrites {
		if w.Tier == string(tiers.TierOperator) {
			t.Fatal("operator must never be stored")
		}
	}
}

func TestOldSubscriptionTerminalEventDoesNotClobberNewOne(t *testing.T) {
	u := freeUser()
	exp := t0.Add(30 * 24 * time.Hour)
	u.Tier, u.TierSource, u.TierExpiresAt = "premium", "subscription", &exp
	u.StripeCustomerID, u.StripeSubscriptionID, u.SubscriptionStatus = "cus_1", "sub_new", "active"
	r := newRig(u)
	r.fc.PutSubscription(&Subscription{ID: "sub_old", CustomerID: "cus_1", Status: StatusCanceled})
	if err := r.deliver(t, "evt_old", EventSubscriptionDeleted, `{"id":"sub_old","customer":"cus_1"}`); err != nil {
		t.Fatal(err)
	}
	if len(r.store.TierWrites) != 0 || r.store.User("u1").StripeSubscriptionID != "sub_new" {
		t.Fatalf("new subscription clobbered: %+v", r.store.User("u1"))
	}
}

func TestUnknownEventTypeRecordedAndAcknowledged(t *testing.T) {
	r := newRig(freeUser())
	if err := r.deliver(t, "evt_u", "charge.refunded", `{"id":"ch_1"}`); err != nil {
		t.Fatal(err)
	}
	e := r.store.Events["evt_u"]
	if e == nil || !e.Processed || e.Note != NoteUnhandledType || len(r.fc.GetSubCalls) != 0 {
		t.Fatalf("event = %+v", e)
	}
}

func TestUnknownUserIsAcknowledgedNotRetriedForever(t *testing.T) {
	r := newRig(freeUser())
	r.fc.PutSubscription(&Subscription{ID: "sub_x", CustomerID: "cus_x", Status: StatusActive, CurrentPeriodEnd: t0.Add(time.Hour)})
	if err := r.deliver(t, "evt_x", EventSubscriptionUpdated, `{"id":"sub_x","customer":"cus_x"}`); err != nil {
		t.Fatal(err)
	}
	if r.store.Events["evt_x"].Note != NoteUnknownUser || len(r.store.TierWrites) != 0 {
		t.Fatalf("event = %+v", r.store.Events["evt_x"])
	}
}

func TestInvalidAndStaleSignaturesRejected(t *testing.T) {
	r := newRig(freeUser())
	body := []byte(`{"id":"evt_s","object":"event","api_version":"2026-09-30.endive","type":"invoice.paid","created":1,"data":{"object":{}}}`)
	if _, err := r.fc.ConstructEvent(body, SignPayload(body, "other_secret", time.Now())); err == nil {
		t.Fatal("invalid signature accepted")
	}
	if _, err := r.fc.ConstructEvent(body, SignPayload(body, testSecret, time.Now().Add(-6*time.Minute))); err == nil {
		t.Fatal("stale timestamp accepted")
	}
	if _, err := r.fc.ConstructEvent(body, ""); err == nil {
		t.Fatal("missing header accepted")
	}
	if _, err := r.fc.ConstructEvent(body, SignPayload(body, testSecret, time.Now())); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if _, err := r.fc.ConstructEvent(append(body, ' '), SignPayload(body, testSecret, time.Now())); err == nil {
		t.Fatal("tampered body accepted")
	}
}

func (r *rig) props(name string) map[string]any {
	for _, e := range r.em.events {
		if e.EventName == name {
			return e.Properties
		}
	}
	return nil
}

func cents(n int64) *int64 { return &n }

// MM17 follow-up: amount_cents + currency on the four subscription events,
// from data already in hand (re-fetched subscription price, or the invoice in
// the event), omitted when unknown.
func TestSubscriptionEventsCarryAmountAndCurrency(t *testing.T) {
	inv := `{"id":"in_1","customer":"cus_1","billing_reason":"subscription_cycle","amount_paid":700,"amount_due":700,"currency":"USD","parent":{"subscription_details":{"subscription":"sub_1","metadata":{"user_id":"u1"}}}}`

	// created: from the subscription price.
	r := newRig(freeUser())
	sub := activeSub(t0.Add(30 * 24 * time.Hour))
	sub.UnitAmountCents, sub.Currency = cents(700), "usd"
	r.fc.PutSubscription(sub)
	if err := r.deliver(t, "evt_c", EventSubscriptionCreated, subObj); err != nil {
		t.Fatal(err)
	}
	if p := r.props(analyticsemit.ServerSubscriptionCreated); p["amount_cents"] != int64(700) || p["currency"] != "usd" || p["interval"] != "monthly" {
		t.Fatalf("created props = %v", p)
	}

	// renewed: invoice amount_paid wins, currency lowercased.
	if err := r.deliver(t, "evt_r", EventInvoicePaid, inv); err != nil {
		t.Fatal(err)
	}
	if p := r.props(analyticsemit.ServerSubscriptionRenewed); p["amount_cents"] != int64(700) || p["currency"] != "usd" {
		t.Fatalf("renewed props = %v", p)
	}

	// payment_failed: invoice amount_due.
	if err := r.deliver(t, "evt_f", EventInvoicePaymentFailed, `{"id":"in_2","customer":"cus_1","amount_due":7000,"currency":"usd","parent":{"subscription_details":{"subscription":"sub_1","metadata":{"user_id":"u1"}}}}`); err != nil {
		t.Fatal(err)
	}
	if p := r.props(analyticsemit.ServerSubscriptionPaymentFailed); p["amount_cents"] != int64(7000) || p["currency"] != "usd" {
		t.Fatalf("payment_failed props = %v", p)
	}

	// cancelled: from the subscription price.
	sub2 := *sub
	sub2.Status = StatusCanceled
	r.fc.PutSubscription(&sub2)
	if err := r.deliver(t, "evt_d", EventSubscriptionDeleted, subObj); err != nil {
		t.Fatal(err)
	}
	if p := r.props(analyticsemit.ServerSubscriptionCancelled); p["amount_cents"] != int64(700) || p["currency"] != "usd" {
		t.Fatalf("cancelled props = %v", p)
	}
}

func TestSubscriptionEventAmountOmittedWhenUnknown(t *testing.T) {
	r := newRig(freeUser())
	r.fc.PutSubscription(activeSub(t0.Add(30 * 24 * time.Hour))) // no price amount
	if err := r.deliver(t, "evt_u", EventSubscriptionCreated, subObj); err != nil {
		t.Fatal(err)
	}
	p := r.props(analyticsemit.ServerSubscriptionCreated)
	if _, has := p["amount_cents"]; has {
		t.Fatalf("amount_cents must be omitted when unknown: %v", p)
	}
	if _, has := p["currency"]; has {
		t.Fatalf("currency must be omitted when unknown: %v", p)
	}
	if p["interval"] != "monthly" {
		t.Fatalf("interval lost: %v", p)
	}
}

type recMailer struct {
	mu    sync.Mutex
	users []string
}

func (m *recMailer) PaymentFailed(u *models.User) {
	m.mu.Lock()
	m.users = append(m.users, u.ID)
	m.mu.Unlock()
}

// MM31: only invoice.payment_failed mails, once per delivered event, and a nil
// Mailer changes nothing.
func TestPaymentFailedSendsEmailOnlyForThatEvent(t *testing.T) {
	r := newRig(freeUser())
	m := &recMailer{}
	r.p.Mailer = m
	r.fc.PutSubscription(activeSub(t0.Add(48 * time.Hour)))
	if err := r.deliver(t, "evt_paid", EventInvoicePaid, `{"id":"in_0","customer":"cus_1","parent":{"subscription_details":{"subscription":"sub_1","metadata":{"user_id":"u1"}}}}`); err != nil {
		t.Fatal(err)
	}
	if len(m.users) != 0 {
		t.Fatalf("invoice.paid mailed: %v", m.users)
	}
	body := `{"id":"in_1","customer":"cus_1","parent":{"subscription_details":{"subscription":"sub_1","metadata":{"user_id":"u1"}}}}`
	if err := r.deliver(t, "evt_pf", EventInvoicePaymentFailed, body); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(t, "evt_pf", EventInvoicePaymentFailed, body); err != nil { // Stripe replay
		t.Fatal(err)
	}
	if len(m.users) != 1 || m.users[0] != "u1" {
		t.Fatalf("mailer calls = %v, want one for u1", m.users)
	}
}

func TestPaymentFailedWithoutMailerStillProcesses(t *testing.T) {
	r := newRig(freeUser())
	r.fc.PutSubscription(activeSub(t0.Add(48 * time.Hour)))
	if err := r.deliver(t, "evt_pf", EventInvoicePaymentFailed, `{"id":"in_1","customer":"cus_1","parent":{"subscription_details":{"subscription":"sub_1","metadata":{"user_id":"u1"}}}}`); err != nil {
		t.Fatal(err)
	}
}
