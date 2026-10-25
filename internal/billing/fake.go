package billing

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// FakeClient is an in-memory BillingClient with injectable state. It is exported
// so handler and processor tests in other packages never need Stripe. Fields are
// guarded by mu; set them before use or via the helper methods.
type FakeClient struct {
	mu sync.Mutex

	// WebhookSecret signs/verifies webhooks exactly like the real client does
	// (same SDK verification, 5 minute tolerance).
	WebhookSecret string

	Subscriptions map[string]*Subscription
	Sessions      map[string]*CheckoutSession

	// Injectable failures.
	EnsureCustomerErr error
	CheckoutErr       error
	PortalErr         error
	GetSubErr         error
	CancelErr         error // MM24
	ResumeErr         error
	UpdatePriceErr    error // MM39
	ScheduleErr       error // CreateSchedule
	ReleaseErr        error
	GetScheduleErr    error

	// Recorded calls.
	CustomersCreated int
	CheckoutCalls    []CheckoutRequest
	PortalCalls      []string
	GetSubCalls      []string
	CancelCalls      []string // MM24
	ResumeCalls      []ResumeCall
	UpdatePriceCalls []UpdatePriceCall // MM39
	ScheduleCalls    []ScheduleCall
	ReleaseCalls     []string

	// PriceCents maps a price id to its unit amount; ResumeSubscription applies
	// it to the stored subscription when the price is swapped.
	PriceCents map[string]int64

	customers map[string]string // user id -> customer id (idempotency key semantics)
}

// ResumeCall records one ResumeSubscription call.
type ResumeCall struct{ SubID, ItemID, NewPriceID, Offer string }

// UpdatePriceCall records one UpdateSubscriptionPrice call.
type UpdatePriceCall struct{ SubID, ItemID, NewPriceID, Proration string }

// ScheduleCall records one CreateSchedule call.
type ScheduleCall struct {
	SubID, CurrentPriceID, NextPriceID string
	SwitchAt                           time.Time
}

// NewFakeClient returns a FakeClient with a webhook secret.
func NewFakeClient(webhookSecret string) *FakeClient {
	return &FakeClient{
		WebhookSecret: webhookSecret,
		Subscriptions: map[string]*Subscription{},
		Sessions:      map[string]*CheckoutSession{},
		customers:     map[string]string{},
	}
}

// PutSubscription stores (or replaces) the latest state Stripe would return.
func (f *FakeClient) PutSubscription(s *Subscription) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Subscriptions[s.ID] = s
}

// PutSession stores a Checkout Session.
func (f *FakeClient) PutSession(s *CheckoutSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Sessions[s.ID] = s
}

func (f *FakeClient) EnsureCustomer(_ context.Context, userID, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.EnsureCustomerErr != nil {
		return "", f.EnsureCustomerErr
	}
	// Same Idempotency-Key => same Customer, like Stripe.
	if id, ok := f.customers[userID]; ok {
		return id, nil
	}
	f.CustomersCreated++
	id := fmt.Sprintf("cus_fake_%d", f.CustomersCreated)
	f.customers[userID] = id
	return id, nil
}

func (f *FakeClient) CreateCheckoutSession(_ context.Context, req CheckoutRequest) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CheckoutErr != nil {
		return "", "", f.CheckoutErr
	}
	f.CheckoutCalls = append(f.CheckoutCalls, req)
	id := fmt.Sprintf("cs_fake_%d", len(f.CheckoutCalls))
	return id, "https://checkout.stripe.test/" + id, nil
}

func (f *FakeClient) CreatePortalSession(_ context.Context, customerID, returnURL string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PortalErr != nil {
		return "", f.PortalErr
	}
	f.PortalCalls = append(f.PortalCalls, customerID)
	return "https://portal.stripe.test/" + customerID + "?return=" + returnURL, nil
}

func (f *FakeClient) GetSubscription(_ context.Context, id string) (*Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GetSubCalls = append(f.GetSubCalls, id)
	if f.GetSubErr != nil {
		return nil, f.GetSubErr
	}
	s, ok := f.Subscriptions[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	if s.Schedule != nil {
		sc := *s.Schedule
		cp.Schedule = &sc
	}
	return &cp, nil
}

func (f *FakeClient) GetCheckoutSession(_ context.Context, id string) (*CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.Sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *FakeClient) ConstructEvent(payload []byte, signatureHeader string) (Event, error) {
	return constructEvent(f.WebhookSecret, payload, signatureHeader)
}

func (f *FakeClient) ResumeSubscription(_ context.Context, subID, itemID, newPriceID, offer string) (*Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResumeCalls = append(f.ResumeCalls, ResumeCall{subID, itemID, newPriceID, offer})
	if f.ResumeErr != nil {
		return nil, f.ResumeErr
	}
	s, ok := f.Subscriptions[subID]
	if !ok {
		return nil, ErrNotFound
	}
	s.CancelAtPeriodEnd = false
	s.CancelAt = time.Time{}
	if newPriceID != "" {
		s.PriceID = newPriceID
		if c, ok := f.PriceCents[newPriceID]; ok {
			s.UnitAmountCents = &c
		}
	}
	cp := *s
	return &cp, nil
}

// UpdateSubscriptionPrice swaps the stored subscription's price (and its
// amount, from PriceCents) like Stripe would.
func (f *FakeClient) UpdateSubscriptionPrice(_ context.Context, subID, itemID, newPriceID, proration string) (*Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.UpdatePriceCalls = append(f.UpdatePriceCalls, UpdatePriceCall{subID, itemID, newPriceID, proration})
	if f.UpdatePriceErr != nil {
		return nil, f.UpdatePriceErr
	}
	s, ok := f.Subscriptions[subID]
	if !ok {
		return nil, ErrNotFound
	}
	s.PriceID = newPriceID
	if c, ok := f.PriceCents[newPriceID]; ok {
		s.UnitAmountCents = &c
	}
	cp := *s
	return &cp, nil
}

// CreateSchedule attaches a two-phase schedule. Like Stripe, a subscription
// that already has a live schedule refuses a second one.
func (f *FakeClient) CreateSchedule(_ context.Context, subID, currentPriceID, nextPriceID string, switchAt time.Time) (*Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ScheduleCalls = append(f.ScheduleCalls, ScheduleCall{subID, currentPriceID, nextPriceID, switchAt})
	if f.ScheduleErr != nil {
		return nil, f.ScheduleErr
	}
	s, ok := f.Subscriptions[subID]
	if !ok {
		return nil, ErrNotFound
	}
	if s.Schedule != nil {
		return nil, fmt.Errorf("fake: subscription %s already has schedule %s", subID, s.Schedule.ID)
	}
	sched := &Schedule{
		ID:         fmt.Sprintf("sub_sched_fake_%d", len(f.ScheduleCalls)),
		Status:     ScheduleStatusActive,
		PlanChange: true,
		Phases: []SchedulePhase{
			{PriceID: currentPriceID, End: switchAt.UTC()},
			{PriceID: nextPriceID, Start: switchAt.UTC()},
		},
	}
	s.Schedule = sched
	cp := *sched
	return &cp, nil
}

// ReleaseSchedule detaches the schedule from whichever subscription has it.
func (f *FakeClient) ReleaseSchedule(_ context.Context, scheduleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ReleaseCalls = append(f.ReleaseCalls, scheduleID)
	if f.ReleaseErr != nil {
		return f.ReleaseErr
	}
	for _, s := range f.Subscriptions {
		if s.Schedule != nil && s.Schedule.ID == scheduleID {
			s.Schedule = nil
			return nil
		}
	}
	return ErrNotFound
}

// GetSchedule returns the subscription's live schedule (nil when none).
func (f *FakeClient) GetSchedule(_ context.Context, subID string) (*Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GetScheduleErr != nil {
		return nil, f.GetScheduleErr
	}
	s, ok := f.Subscriptions[subID]
	if !ok {
		return nil, ErrNotFound
	}
	if s.Schedule == nil {
		return nil, nil
	}
	cp := *s.Schedule
	return &cp, nil
}

// SignPayload builds a valid Stripe-Signature header for payload with secret at
// time t, using the SDK's own signer (webhook.ComputeSignature).
func SignPayload(payload []byte, secret string, t time.Time) string {
	return signHeader(payload, secret, t)
}

var _ BillingClient = (*FakeClient)(nil)
