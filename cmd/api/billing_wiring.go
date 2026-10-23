package main

import (
	"log/slog"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/billing"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// newBillingHandler builds the Stripe client only when billing is fully
// configured; otherwise the handler is returned without one and every guarded
// route answers 503 billing_unavailable.
func newBillingHandler(cfg *config.Config, store *telemetry.Store, registry *tiers.Registry, emitter *analyticsemit.Emitter, log *slog.Logger) *handlers.BillingHandler {
	if !cfg.BillingConfigured() {
		return handlers.NewBillingHandler(nil, nil, nil, registry, cfg, log)
	}
	client := billing.NewStripeClient(cfg.StripeSecretKey, cfg.StripeWebhookSecret, cfg.StripeAPIVersion)
	log.Info("billing enabled", "stripeApiVersion", billing.PinnedAPIVersion(), "graceDays", cfg.BillingGraceDays)
	proc := &billing.Processor{
		Store:   store,
		Client:  client,
		Emitter: emitter,
		Grace:   time.Duration(cfg.BillingGraceDays) * 24 * time.Hour,
		Log:     log,
	}
	return handlers.NewBillingHandler(store, client, proc, registry, cfg, log)
}
