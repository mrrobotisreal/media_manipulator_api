package config

import (
	"strings"
	"unicode"
)

// BillingConfigured is true only when billing is switched on AND every value
// Stripe needs is present. Billing routes answer 503 billing_unavailable and
// /api/tiers reports premiumPurchasable:false otherwise.
func (c *Config) BillingConfigured() bool {
	if c == nil || !c.BillingEnabled {
		return false
	}
	return c.StripeSecretKey != "" && c.StripeWebhookSecret != "" &&
		c.StripePricePremiumMonthly != "" && c.StripePricePremiumYearly != ""
}

// ProPurchasable reports whether Pro can be sold (MM39): billing is configured
// and both Pro Price ids are set. Separate from BillingConfigured on purpose, so
// a deployment without Pro prices still sells Premium.
func (c *Config) ProPurchasable() bool {
	return c.BillingConfigured() && c.StripePriceProMonthly != "" && c.StripePriceProYearly != ""
}

// Plan names as they appear in API bodies (checkout `plan`, change-plan
// `target`, status `plan`). They equal the tier strings on purpose.
const (
	PlanPremium = "premium"
	PlanPro     = "pro"
)

// DisplayPrices are the display-only price numerals per plan and interval.
// The amount charged is always the Stripe Price; these feed /api/tiers and
// the status fallbacks. They carry NO currency: MM69 serves the same numerals
// in USD and EUR, and wraps this one helper rather than every call site.
type DisplayPrices struct {
	PremiumMonthly int
	PremiumYearly  int
	ProMonthly     int
	ProYearly      int
}

// DisplayPrices is the one source of price numerals (BILLING_PRICE_*_USD).
func (c *Config) DisplayPrices() DisplayPrices {
	if c == nil {
		return DisplayPrices{}
	}
	return DisplayPrices{
		PremiumMonthly: c.BillingPriceMonthlyUSD,
		PremiumYearly:  c.BillingPriceYearlyUSD,
		ProMonthly:     c.BillingPriceProMonthlyUSD,
		ProYearly:      c.BillingPriceProYearlyUSD,
	}
}

// For returns the numeral for (plan, interval), interval "monthly"|"yearly".
// Unknown plans resolve to Premium (the same never-elevate rule as the
// entitlement table); an unknown interval is treated as monthly.
func (p DisplayPrices) For(plan, interval string) int {
	if plan == PlanPro {
		if interval == "yearly" {
			return p.ProYearly
		}
		return p.ProMonthly
	}
	if interval == "yearly" {
		return p.PremiumYearly
	}
	return p.PremiumMonthly
}

// DefaultBillingEURCountries is the proposed EUR region (MM69, PRO_TIER.md
// §7.3): the EU-27, the EEA (IS, LI, NO), CH, GB, the euro micro-states (AD,
// MC, SM, VA) plus ME and XK, and the euro-using territories with their own
// ISO code (AX, GF, GP, MQ, YT, RE, BL, MF, PM). Override with
// BILLING_EUR_COUNTRIES.
const DefaultBillingEURCountries = "AT BE BG HR CY CZ DK EE FI FR DE GR HU IE IT LV LT LU MT NL PL PT RO SK SI ES SE " +
	"IS LI NO CH GB AD MC SM VA ME XK AX GF GP MQ YT RE BL MF PM"

// ParseCountryList parses a comma- and/or whitespace-separated country list
// into upper-case two-letter codes; anything else is dropped.
func ParseCountryList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ToUpper(strings.TrimSpace(f))
		if len(f) == 2 && f[0] >= 'A' && f[0] <= 'Z' && f[1] >= 'A' && f[1] <= 'Z' {
			out = append(out, f)
		}
	}
	return out
}
