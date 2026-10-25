package main

import (
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/billing"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
)

// MM39: the real registry satisfies billing.Metrics and exposes
// mm_billing_unknown_price_total.
func TestBillingUnknownPriceMetric(t *testing.T) {
	reg := metrics.New()
	var m billing.Metrics = reg
	m.BillingUnknownPrice()
	m.BillingUnknownPrice()
	families, err := reg.Reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "mm_billing_unknown_price_total" {
			found = true
			if v := f.GetMetric()[0].GetCounter().GetValue(); v != 2 {
				t.Fatalf("value = %v, want 2", v)
			}
		}
	}
	if !found {
		t.Fatal("mm_billing_unknown_price_total not registered")
	}
	var nilReg *metrics.Registry
	nilReg.BillingUnknownPrice() // must not panic
}
