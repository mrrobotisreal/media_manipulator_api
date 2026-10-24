package config

import "testing"

func TestBillingConfigured(t *testing.T) {
	full := func() *Config {
		return &Config{BillingEnabled: true, StripeSecretKey: "k", StripeWebhookSecret: "w",
			StripePricePremiumMonthly: "price_m", StripePricePremiumYearly: "price_y"}
	}
	if !full().BillingConfigured() {
		t.Fatal("fully configured must be true")
	}
	var nilCfg *Config
	if nilCfg.BillingConfigured() {
		t.Fatal("nil config must be false")
	}
	mutators := map[string]func(*Config){
		"disabled":      func(c *Config) { c.BillingEnabled = false },
		"no secret key": func(c *Config) { c.StripeSecretKey = "" },
		"no webhook":    func(c *Config) { c.StripeWebhookSecret = "" },
		"no monthly":    func(c *Config) { c.StripePricePremiumMonthly = "" },
		"no yearly":     func(c *Config) { c.StripePricePremiumYearly = "" },
	}
	for name, m := range mutators {
		c := full()
		m(c)
		if c.BillingConfigured() {
			t.Errorf("%s: must not be configured", name)
		}
	}
}

func TestBillingDefaultsAreDark(t *testing.T) {
	t.Setenv("BILLING_ENABLED", "")
	t.Setenv("BILLING_PUBLIC_BASE_URL", "")
	c := Load()
	if c.BillingEnabled || c.BillingConfigured() {
		t.Fatal("billing must default off")
	}
	if c.BillingGraceDays != 7 || c.BillingPriceMonthlyUSD != 8 || c.BillingPriceYearlyUSD != 80 {
		t.Fatalf("defaults wrong: %+v", c)
	}
	if c.BillingPublicBaseURL != "https://media-manipulator.com" {
		t.Fatalf("base url = %q", c.BillingPublicBaseURL)
	}
}

func TestStripeAutomaticTaxDefaultsOffAndCanBeEnabled(t *testing.T) {
	t.Setenv("STRIPE_AUTOMATIC_TAX", "")
	if Load().StripeAutomaticTax {
		t.Fatal("must default to false (no Stripe Tax)")
	}
	t.Setenv("STRIPE_AUTOMATIC_TAX", "true")
	if !Load().StripeAutomaticTax {
		t.Fatal("true must enable")
	}
}

func TestRetentionPriceEnvVars(t *testing.T) {
	t.Setenv("STRIPE_PRICE_PREMIUM_MONTHLY_RETAIN7", " price_r7 ")
	t.Setenv("STRIPE_PRICE_PREMIUM_MONTHLY_RETAIN6", "")
	c := Load()
	if c.StripePricePremiumMonthlyRetain7 != "price_r7" || c.StripePricePremiumMonthlyRetain6 != "" {
		t.Fatalf("%q %q", c.StripePricePremiumMonthlyRetain7, c.StripePricePremiumMonthlyRetain6)
	}
}
