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
