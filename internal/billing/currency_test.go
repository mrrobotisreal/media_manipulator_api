package billing

import (
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func TestDefaultCurrencyFor(t *testing.T) {
	d := NewCurrencyDefaults(&config.Config{BillingEURCountries: config.ParseCountryList(config.DefaultBillingEURCountries)})
	for _, tc := range []struct{ country, want string }{
		{"DE", "eur"}, {"fr", "eur"}, {"PL", "eur"}, // EU members (euro and non-euro)
		{"NO", "eur"}, {"IS", "eur"}, {"LI", "eur"}, // EEA
		{"CH", "eur"}, {"GB", "eur"}, {"MC", "eur"}, {"RE", "eur"},
		{"US", "usd"}, {"CA", "usd"}, {"UA", "usd"}, {"IL", "usd"}, {"RU", "usd"},
		{"", "usd"}, {"XX", "usd"}, {"T1", "usd"}, {" de ", "eur"},
	} {
		if got := d.DefaultCurrencyFor(tc.country); got != tc.want {
			t.Errorf("DefaultCurrencyFor(%q) = %q, want %q", tc.country, got, tc.want)
		}
	}
	// Env override: only the listed countries see euros.
	t.Setenv("BILLING_EUR_COUNTRIES", "us, ca")
	o := NewCurrencyDefaults(config.Load())
	if o.DefaultCurrencyFor("US") != "eur" || o.DefaultCurrencyFor("CA") != "eur" || o.DefaultCurrencyFor("DE") != "usd" {
		t.Errorf("env override not applied: %+v", o)
	}
	if (CurrencyDefaults{}).DefaultCurrencyFor("DE") != "usd" {
		t.Error("the zero value must default everyone to usd")
	}
}

func TestDefaultEURListShape(t *testing.T) {
	list := config.ParseCountryList(config.DefaultBillingEURCountries)
	if len(list) != 47 {
		t.Errorf("default EUR list has %d countries, want 47 (EU-27 + IS LI NO CH GB + AD MC SM VA ME XK + 9 territories)", len(list))
	}
	seen := map[string]bool{}
	for _, c := range list {
		if seen[c] {
			t.Errorf("duplicate %s", c)
		}
		seen[c] = true
	}
	if seen["US"] {
		t.Error("US must not be in the EUR list")
	}
}

func TestNormalizeCurrencyAllowlist(t *testing.T) {
	for in, want := range map[string]string{"usd": "usd", "EUR": "eur", " eur ": "eur"} {
		if got, ok := NormalizeCurrency(in); !ok || got != want {
			t.Errorf("NormalizeCurrency(%q) = %q,%v", in, got, ok)
		}
	}
	for _, bad := range []string{"gbp", "", "euro", "us", "jpy"} {
		if _, ok := NormalizeCurrency(bad); ok {
			t.Errorf("NormalizeCurrency(%q) must be refused", bad)
		}
	}
}

func TestResolveCheckoutCurrencyPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, locked, requested, want string
		conflict                      bool
	}{
		{"nothing known = usd (a pre-MM69 UI showed dollars)", "", "", "usd", false},
		{"body eur", "", "eur", "eur", false},
		{"body usd", "", "usd", "usd", false},
		{"lock without a body", "eur", "", "eur", false},
		{"lock + same request", "eur", "eur", "eur", false},
		{"lock eur + ask usd = conflict", "eur", "usd", "eur", true},
		{"lock usd + ask eur = conflict", "usd", "eur", "usd", true},
	} {
		got, conflict := ResolveCheckoutCurrency(tc.locked, tc.requested)
		if got != tc.want || conflict != tc.conflict {
			t.Errorf("%s: got %q conflict=%v, want %q conflict=%v", tc.name, got, conflict, tc.want, tc.conflict)
		}
	}
}
