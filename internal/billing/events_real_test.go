package billing

import (
	"testing"
	"time"
)

// Real Stripe deliveries: data.object carries its own "object" type string.
// Before the fix every one of these failed to decode and the webhook answered
// 400, so no payment ever upgraded an account.
func TestConstructEventRealStripeShapes(t *testing.T) {
	const secret = "whsec_real_shape_test"
	cases := []struct {
		name, typ, object      string
		wantSub, wantCus, user string
	}{
		{"checkout", EventCheckoutCompleted,
			`{"id":"cs_live_1","object":"checkout.session","customer":"cus_1","subscription":"sub_1","client_reference_id":"user-1","payment_status":"paid"}`,
			"sub_1", "cus_1", "user-1"},
		{"subscription", EventSubscriptionCreated,
			`{"id":"sub_2","object":"subscription","customer":"cus_2","status":"active","metadata":{"user_id":"user-2"}}`,
			"sub_2", "cus_2", "user-2"},
		{"invoice", EventInvoicePaid,
			`{"id":"in_3","object":"invoice","customer":"cus_3","amount_paid":700,"currency":"usd","billing_reason":"subscription_create","parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_3","metadata":{"user_id":"user-3"}}}}`,
			"sub_3", "cus_3", "user-3"},
	}
	for _, tc := range cases {
		payload := []byte(`{"id":"evt_` + tc.name + `","object":"event","api_version":"2025-09-30.clover","type":"` + tc.typ + `","data":{"object":` + tc.object + `}}`)
		ev, err := constructEvent(secret, payload, signHeader(payload, secret, time.Now()))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if ev.SubscriptionID != tc.wantSub || ev.CustomerID != tc.wantCus || ev.UserID != tc.user {
			t.Fatalf("%s: got %+v", tc.name, ev)
		}
	}
}
