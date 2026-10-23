package billing

import (
	"context"
	"errors"

	"github.com/stripe/stripe-go/v87"
)

// MM24: ending a subscription when its owner deletes their account. Kept in its
// own file and behind its own interface so the BillingClient contract that the
// webhook processor and checkout handlers depend on is untouched.

// End results reported by SubscriptionEnder.EndSubscription.
const (
	EndCanceled         = "canceled"         // cancelled just now
	EndAlreadyCanceled  = "already_canceled" // terminal, or gone from Stripe
	EndSkippedNoBilling = "skipped"          // billing is not configured on this deploy
	EndNoSubscription   = "no_subscription"  // the user never had one
)

// SubscriptionEnder cancels a subscription immediately. Idempotent: ending one
// that is already over succeeds.
type SubscriptionEnder interface {
	EndSubscription(ctx context.Context, subscriptionID string) (string, error)
}

// NoopEnder is the real implementation used when billing is not configured
// (BILLING_ENABLED unset): there is nothing to cancel, and saying so is the
// honest answer.
type NoopEnder struct{}

func (NoopEnder) EndSubscription(context.Context, string) (string, error) {
	return EndSkippedNoBilling, nil
}

// canceller is the optional capability a BillingClient may have.
type canceller interface {
	CancelSubscriptionNow(ctx context.Context, subscriptionID string) error
}

type clientEnder struct {
	client BillingClient
	can    canceller
}

// NewSubscriptionEnder wraps a BillingClient. A nil client, or one that cannot
// cancel, yields the no-op ender.
func NewSubscriptionEnder(client BillingClient) SubscriptionEnder {
	if client == nil {
		return NoopEnder{}
	}
	can, ok := client.(canceller)
	if !ok {
		return NoopEnder{}
	}
	return &clientEnder{client: client, can: can}
}

func (e *clientEnder) EndSubscription(ctx context.Context, id string) (string, error) {
	if id == "" {
		return EndNoSubscription, nil
	}
	sub, err := e.client.GetSubscription(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return EndAlreadyCanceled, nil
		}
		return "", err
	}
	if sub.IsTerminal() {
		return EndAlreadyCanceled, nil
	}
	if err := e.can.CancelSubscriptionNow(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return EndAlreadyCanceled, nil
		}
		return "", err
	}
	return EndCanceled, nil
}

// CancelSubscriptionNow is DELETE /v1/subscriptions/:id: cancel immediately, no
// proration refund, no final invoice (the customer is leaving, not disputing).
func (c *stripeClient) CancelSubscriptionNow(ctx context.Context, id string) error {
	p := &stripe.SubscriptionCancelParams{
		InvoiceNow: stripe.Bool(false),
		Prorate:    stripe.Bool(false),
	}
	p.Params = c.params(ctx)
	_, err := c.sc.V1Subscriptions.Cancel(ctx, id, p)
	return wrapErr(err)
}

// CancelSubscriptionNow marks the fake subscription canceled and records the call.
func (f *FakeClient) CancelSubscriptionNow(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CancelCalls = append(f.CancelCalls, id)
	if f.CancelErr != nil {
		return f.CancelErr
	}
	s, ok := f.Subscriptions[id]
	if !ok {
		return ErrNotFound
	}
	s.Status = StatusCanceled
	return nil
}
