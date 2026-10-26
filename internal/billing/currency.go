package billing

import (
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// Billing currencies (MM69, PRO_TIER.md §7). Lowercase ISO 4217, as Stripe
// spells them. Both are sold at the SAME numerals (Premium 8 / 80, Pro
// 15 / 150, retention 7 / 6): there is no FX conversion anywhere, and USD
// and EUR amounts are never added together.
const (
	CurrencyUSD = "usd"
	CurrencyEUR = "eur"
)

// Currencies is the allowlist, in display order.
var Currencies = []string{CurrencyUSD, CurrencyEUR}

// NormalizeCurrency lower-cases and trims s and reports whether it is on the
// allowlist. Anything else (gbp, "", "EURO") is ok=false.
func NormalizeCurrency(s string) (string, bool) {
	c := strings.ToLower(strings.TrimSpace(s))
	if c == CurrencyUSD || c == CurrencyEUR {
		return c, true
	}
	return "", false
}

// CurrencyDefaults decides the geographic default currency. Build it once
// from config (NewCurrencyDefaults); the zero value defaults everyone to USD.
type CurrencyDefaults struct {
	eur map[string]bool
}

// NewCurrencyDefaults reads BILLING_EUR_COUNTRIES (config.BillingEURCountries).
func NewCurrencyDefaults(cfg *config.Config) CurrencyDefaults {
	d := CurrencyDefaults{eur: map[string]bool{}}
	if cfg == nil {
		return d
	}
	for _, c := range cfg.BillingEURCountries {
		d.eur[strings.ToUpper(strings.TrimSpace(c))] = true
	}
	return d
}

// DefaultCurrencyFor is EUR for a country in the EUR region list, USD for
// every other country and for an unknown one ("", "XX", "T1").
func (d CurrencyDefaults) DefaultCurrencyFor(countryISO2 string) string {
	if d.eur[strings.ToUpper(strings.TrimSpace(countryISO2))] {
		return CurrencyEUR
	}
	return CurrencyUSD
}

// ResolveCheckoutCurrency picks the currency a Checkout Session is opened
// in (MM69 decision 4, as implemented): the locked currency (the user's
// Stripe Customer/subscription currency) first, then the request body.
//
// There is deliberately no settings or geographic step: Checkout must charge
// what the client displayed. A new UI always sends the currency it rendered
// (its own lock > setting > cookie > geo resolution); a body without one
// comes from a pre-MM69 UI, which rendered US dollars, so it charges USD.
//
// A request for a currency other than the locked one is a conflict
// (conflict=true, currency = the locked one): Stripe would refuse the
// session, and the visitor must never be charged in a currency they did not
// see. Inputs must already be normalized ("" = not given).
func ResolveCheckoutCurrency(locked, requested string) (currency string, conflict bool) {
	switch {
	case locked != "":
		return locked, requested != "" && requested != locked
	case requested != "":
		return requested, false
	}
	return CurrencyUSD, false
}
