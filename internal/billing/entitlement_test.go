package billing

import (
	"testing"
	"time"
)

func TestEntitlementMatrix(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	grace := 7 * 24 * time.Hour
	future := now.Add(20 * 24 * time.Hour)
	soon := now.Add(2 * 24 * time.Hour)
	past := now.Add(-24 * time.Hour)

	tests := []struct {
		name       string
		sub        Subscription
		wantChange bool
		wantTier   string
		wantSource string
		wantExpiry *time.Time
	}{
		{"trialing", Subscription{Status: StatusTrialing, CurrentPeriodEnd: future}, true, "premium", "subscription", &future},
		{"active", Subscription{Status: StatusActive, CurrentPeriodEnd: future}, true, "premium", "subscription", &future},
		{"active with cancel_at_period_end keeps premium", Subscription{Status: StatusActive, CurrentPeriodEnd: future, CancelAtPeriodEnd: true}, true, "premium", "subscription", &future},
		{"past_due short period gets 7 day grace", Subscription{Status: StatusPastDue, CurrentPeriodEnd: soon}, true, "premium", "subscription", ptr(now.Add(grace))},
		{"past_due expired period gets 7 day grace", Subscription{Status: StatusPastDue, CurrentPeriodEnd: past}, true, "premium", "subscription", ptr(now.Add(grace))},
		{"past_due long period keeps period end", Subscription{Status: StatusPastDue, CurrentPeriodEnd: future}, true, "premium", "subscription", &future},
		{"canceled", Subscription{Status: StatusCanceled, CurrentPeriodEnd: future}, true, "free", "default", nil},
		{"unpaid", Subscription{Status: StatusUnpaid, CurrentPeriodEnd: future}, true, "free", "default", nil},
		{"incomplete_expired", Subscription{Status: StatusIncompleteExpired}, true, "free", "default", nil},
		{"paused", Subscription{Status: StatusPaused, CurrentPeriodEnd: future}, true, "free", "default", nil},
		{"incomplete changes nothing", Subscription{Status: StatusIncomplete}, false, "", "", nil},
		{"unknown status changes nothing", Subscription{Status: "something_new"}, false, "", "", nil},
		{"active without a period end changes nothing", Subscription{Status: StatusActive}, false, "", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Entitlement(&tc.sub, now, grace)
			if got.Change != tc.wantChange || got.Tier != tc.wantTier || got.Source != tc.wantSource {
				t.Fatalf("got %+v", got)
			}
			switch {
			case tc.wantExpiry == nil && got.ExpiresAt != nil:
				t.Fatalf("expected no expiry, got %v", got.ExpiresAt)
			case tc.wantExpiry != nil && (got.ExpiresAt == nil || !got.ExpiresAt.Equal(*tc.wantExpiry)):
				t.Fatalf("expiry = %v, want %v", got.ExpiresAt, tc.wantExpiry)
			}
		})
	}
	if got := Entitlement(nil, now, grace); got.Change {
		t.Fatal("nil subscription must not change the tier")
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestEveryStripeStatusIsCovered(t *testing.T) {
	// Every status in the Stripe enum must be decided deliberately.
	for _, s := range []string{StatusTrialing, StatusActive, StatusPastDue, StatusCanceled, StatusUnpaid, StatusIncomplete, StatusIncompleteExpired, StatusPaused} {
		_ = Entitlement(&Subscription{Status: s, CurrentPeriodEnd: time.Now().Add(time.Hour)}, time.Now(), time.Hour)
	}
}

func TestLocaleMapping(t *testing.T) {
	cases := map[string][2]string{
		"ru-RU": {"ru", "ru"}, "en-US": {"en", "en"}, "de": {"de", "de"}, "es-ES": {"es", "es"},
		"uk-UA": {"uk", "auto"}, "he-IL": {"he", "auto"}, "fr": {"en", "en"}, "": {"en", "en"},
	}
	for in, want := range cases {
		if got := NormalizeUILocale(in); got != want[0] {
			t.Errorf("NormalizeUILocale(%q)=%q want %q", in, got, want[0])
		}
		if got := StripeLocale(in); got != want[1] {
			t.Errorf("StripeLocale(%q)=%q want %q", in, got, want[1])
		}
	}
}

func TestParseEventFallbacks(t *testing.T) {
	ev, err := parseEvent("evt_1", EventCheckoutCompleted, []byte(`{"id":"cs_1","customer":"cus_1","subscription":"sub_1","client_reference_id":"user-1"}`))
	if err != nil || ev.CustomerID != "cus_1" || ev.SubscriptionID != "sub_1" || ev.UserID != "user-1" {
		t.Fatalf("checkout: %+v %v", ev, err)
	}
	ev, _ = parseEvent("evt_2", EventSubscriptionUpdated, []byte(`{"id":"sub_2","customer":{"id":"cus_2"},"metadata":{"user_id":"user-2"}}`))
	if ev.SubscriptionID != "sub_2" || ev.CustomerID != "cus_2" || ev.UserID != "user-2" {
		t.Fatalf("subscription: %+v", ev)
	}
	ev, _ = parseEvent("evt_3", EventInvoicePaymentFailed, []byte(`{"id":"in_1","customer":"cus_3","parent":{"subscription_details":{"subscription":"sub_3","metadata":{"user_id":"user-3"}}}}`))
	if ev.SubscriptionID != "sub_3" || ev.UserID != "user-3" {
		t.Fatalf("invoice: %+v", ev)
	}
}

func TestSignedEventRoundTripAndVersionMismatch(t *testing.T) {
	const secret = "whsec_test_secret"
	f := NewFakeClient(secret)
	// api_version from an OLDER release train than the SDK's: must still verify.
	body := []byte(`{"id":"evt_9","object":"event","api_version":"2025-03-31.basil","type":"customer.subscription.updated","created":1,"livemode":false,"data":{"object":{"id":"sub_9","customer":"cus_9","metadata":{"user_id":"u9"}}}}`)
	ev, err := f.ConstructEvent(body, SignPayload(body, secret, time.Now()))
	if err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if ev.ID != "evt_9" || ev.SubscriptionID != "sub_9" || ev.UserID != "u9" {
		t.Fatalf("parsed %+v", ev)
	}
	if _, err := f.ConstructEvent(body, SignPayload(body, "wrong", time.Now())); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if _, err := f.ConstructEvent(body, SignPayload(body, secret, time.Now().Add(-10*time.Minute))); err == nil {
		t.Fatal("stale timestamp accepted")
	}
}

func TestPinnedAPIVersionRecorded(t *testing.T) {
	if PinnedAPIVersion() == "" {
		t.Fatal("empty stripe.APIVersion")
	}
	t.Logf("stripe.APIVersion = %s", PinnedAPIVersion())
}
