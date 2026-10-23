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

	// Recorded calls.
	CustomersCreated int
	CheckoutCalls    []CheckoutRequest
	PortalCalls      []string
	GetSubCalls      []string
	CancelCalls      []string // MM24

	customers map[string]string // user id -> customer id (idempotency key semantics)
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

// SignPayload builds a valid Stripe-Signature header for payload with secret at
// time t, using the SDK's own signer (webhook.ComputeSignature).
func SignPayload(payload []byte, secret string, t time.Time) string {
	return signHeader(payload, secret, t)
}

var _ BillingClient = (*FakeClient)(nil)
