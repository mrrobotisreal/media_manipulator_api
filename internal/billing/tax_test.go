package billing

import (
	"testing"

	"github.com/stripe/stripe-go/v87"
)

func TestApplyAutomaticTax(t *testing.T) {
	on := &stripe.CheckoutSessionCreateParams{}
	applyAutomaticTax(on, true)
	if on.AutomaticTax == nil || on.AutomaticTax.Enabled == nil || !*on.AutomaticTax.Enabled {
		t.Fatalf("automatic tax must be enabled: %+v", on.AutomaticTax)
	}
	if on.BillingAddressCollection == nil || *on.BillingAddressCollection != "auto" {
		t.Fatalf("billing address collection must be auto")
	}
	if on.TaxIDCollection != nil {
		t.Fatal("tax id collection must stay unset")
	}
	off := &stripe.CheckoutSessionCreateParams{}
	applyAutomaticTax(off, false)
	if off.AutomaticTax != nil || off.BillingAddressCollection != nil {
		t.Fatalf("switch off must leave params untouched: %+v", off)
	}
}
