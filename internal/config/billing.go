package config

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
