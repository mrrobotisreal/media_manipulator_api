package main

import (
	"context"
	"log/slog"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/push"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// senderFactory builds the FCM sender (a fake in tests).
type senderFactory func(ctx context.Context, projectID string) (push.Sender, error)

func newFCMSender(ctx context.Context, projectID string) (push.Sender, error) {
	return push.NewFCMSender(ctx, projectID)
}

// buildJobNotifier returns the finished-job notifier (MA23). PUSH_ENABLED and
// FIREBASE_PROJECT_ID set and the store enabled: the push notifier wrapping email
// (push first; email only when no push was accepted). Anything else, including an
// FCM init failure, is the email service alone, exactly as before MA23.
func buildJobNotifier(ctx context.Context, cfg *config.Config, store *telemetry.Store, emailSvc services.JobFinishedNotifier,
	emitter *analyticsemit.Emitter, m *metrics.Registry, log *slog.Logger, mk senderFactory) services.JobFinishedNotifier {
	if cfg == nil || !cfg.PushEnabled {
		return emailSvc
	}
	if cfg.FirebaseProjectID == "" || store == nil || !store.Enabled() {
		log.Warn("push: PUSH_ENABLED is set but FIREBASE_PROJECT_ID or the database is missing; email only")
		return emailSvc
	}
	sender, err := mk(ctx, cfg.FirebaseProjectID)
	if err != nil {
		log.Error("push: FCM init failed; email only", "error", err.Error())
		return emailSvc
	}
	log.Info("push notifications enabled")
	return push.NewNotifier(sender, store, emailSvc, emitter, m, log)
}
