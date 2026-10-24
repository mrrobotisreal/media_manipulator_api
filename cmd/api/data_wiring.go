package main

import (
	"context"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/billing"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// wireDataControls mounts the MM24 deletion / export surface. It builds its OWN
// sweeper for user-initiated deletes: the ticker's sweeper only exists when
// CLEANUP_ENABLED is set, and a user's "delete" must work regardless. Both run the
// same per-kind deleters; ExpireNow ignores dry run because it acts only on rows
// the caller just asked to delete.
func wireDataControls(ctx context.Context, cfg *config.Config, store *telemetry.Store, jobManager *services.JobManager,
	s3Client *s3.Client, conversion *handlers.ConversionHandler, studio *handlers.StudioHandler,
	account *handlers.AccountHandler, limiter *limits.Limiter, m *metrics.Registry,
	emitter *analyticsemit.Emitter, mailer handlers.AccountDeletedMailer, log *slog.Logger) {

	var objStore retention.ObjectStore
	if s3Client != nil {
		objStore = retention.NewS3Store(s3Client)
	}
	sweeper := retention.NewSweeper(store, retention.OSDisk{}, objStore, jobManager.ActiveJobIDs,
		retention.NewMeter(m), log, retention.SweepConfig{
			UploadDir: cfg.UploadDir, OutputDir: cfg.OutputDir, Bucket: cfg.S3Bucket,
			AllowedPrefixes: []string{"videos", cfg.S3ResultPrefix, cfg.ContentStudioS3Prefix},
			Batch:           cfg.RetentionSweepBatch, Budget: cfg.RetentionSweepBudget,
			MaxAttempts: cfg.RetentionMaxAttempts,
		})

	// Per-item routes (jobs, keep longer) and projects.
	data := handlers.NewDataHandler(store, sweeper, jobManager, log)
	data.SetMetric(func(scope string) { m.DataDeleted(scope) })
	conversion.SetDataHandler(data)
	conversion.SetJobRecords(store)
	// MM32: result share links (owner create/revoke, public read-only token routes).
	conversion.SetShareHandler(handlers.NewShareHandler(conversion, store, jobManager, cfg, log))
	studio.SetDeletion(sweeper, store)
	studio.SetDeletionMetric(func(scope string) { m.DataDeleted(scope) })

	// Account-wide: delete-all, export, revoke, account deletion.
	acct := handlers.NewAccountDataHandler(store, sweeper, jobManager, log)
	acct.OnDelete = func(scope string) { m.DataDeleted(scope) }

	var exportLimiter handlers.ExportLimiter
	if limiter != nil && limiter.Enabled() {
		exportLimiter = limiter
	}
	acct.SetExport(handlers.ExportConfig{Store: store, Limiter: exportLimiter, OutputDir: cfg.OutputDir, Cfg: cfg})

	var fb handlers.FirebaseUserDeleter
	if admin, err := middleware.NewFirebaseAdmin(ctx, cfg.FirebaseProjectID); err != nil {
		log.Error("firebase admin init failed; sign-out-everywhere and account deletion are unavailable", "error", err.Error())
	} else {
		acct.SetRevoker(admin)
		fb = admin
	}

	// Stripe: the real canceller only when billing is fully configured; otherwise
	// the no-op ender reports "skipped".
	var ender billing.SubscriptionEnder = billing.NoopEnder{}
	if cfg.BillingConfigured() {
		ender = billing.NewSubscriptionEnder(billing.NewStripeClient(cfg.StripeSecretKey, cfg.StripeWebhookSecret, cfg.StripeAPIVersion, cfg.StripeAutomaticTax))
	}
	var dsr analyticsemit.DSRClient
	if c := analyticsemit.NewHTTPDSRClient(cfg.AnalyticsIngestURL, 0); c != nil {
		dsr = c
	} else {
		log.Warn("ANALYTICS_INGEST_URL is empty; account deletion will report analytics: not_configured")
	}
	acct.SetAccountDeletion(handlers.AccountDeleteConfig{
		Store: store, Expirer: sweeper, Stripe: ender, Analytics: dsr, Firebase: fb,
		Metrics: func(result string) { m.AccountDeleted(result) }, Emitter: emitter, Mailer: mailer,
	})
	account.SetAccountData(acct)
}
