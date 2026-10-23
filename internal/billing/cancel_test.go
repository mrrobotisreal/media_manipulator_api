package billing

import (
	"context"
	"errors"
	"testing"
)

func TestSubscriptionEnder(t *testing.T) {
	ctx := context.Background()
	t.Run("no client is the no-op", func(t *testing.T) {
		got, err := NewSubscriptionEnder(nil).EndSubscription(ctx, "sub_1")
		if err != nil || got != EndSkippedNoBilling {
			t.Errorf("%q %v", got, err)
		}
	})
	t.Run("active subscription is cancelled once", func(t *testing.T) {
		f := NewFakeClient("s")
		f.PutSubscription(&Subscription{ID: "sub_1", Status: StatusActive})
		e := NewSubscriptionEnder(f)
		got, err := e.EndSubscription(ctx, "sub_1")
		if err != nil || got != EndCanceled || len(f.CancelCalls) != 1 {
			t.Fatalf("%q %v %v", got, err, f.CancelCalls)
		}
		// A retry finds it terminal and does not call Stripe again.
		got, err = e.EndSubscription(ctx, "sub_1")
		if err != nil || got != EndAlreadyCanceled || len(f.CancelCalls) != 1 {
			t.Errorf("retry: %q %v %v", got, err, f.CancelCalls)
		}
	})
	t.Run("missing at Stripe counts as done", func(t *testing.T) {
		got, err := NewSubscriptionEnder(NewFakeClient("s")).EndSubscription(ctx, "sub_gone")
		if err != nil || got != EndAlreadyCanceled {
			t.Errorf("%q %v", got, err)
		}
	})
	t.Run("no subscription id", func(t *testing.T) {
		got, err := NewSubscriptionEnder(NewFakeClient("s")).EndSubscription(ctx, "")
		if err != nil || got != EndNoSubscription {
			t.Errorf("%q %v", got, err)
		}
	})
	t.Run("a Stripe error is surfaced so deletion halts", func(t *testing.T) {
		f := NewFakeClient("s")
		f.PutSubscription(&Subscription{ID: "sub_1", Status: StatusActive})
		f.CancelErr = errors.New("stripe 500")
		if _, err := NewSubscriptionEnder(f).EndSubscription(ctx, "sub_1"); err == nil {
			t.Error("must fail")
		}
	})
}
