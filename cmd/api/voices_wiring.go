package main

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM77 voices wiring: the handler, its quota routes and its abuse bucket. Kept
// in one file so main.go only gains a constructor call, one Register line and
// one gate entry.

// newVoicesHandler builds the /api/voices handler. With no database the
// saved-voice routes answer 503; presets, shift and capabilities still work.
func newVoicesHandler(cfg *config.Config, registry *tiers.Registry, jobs *services.JobManager, gpuMgr *gpu.Manager,
	store *telemetry.Store, s3Client *s3.Client, limiter *limits.Limiter, sweeper *retention.Sweeper, log *slog.Logger) *handlers.VoicesHandler {
	client := services.NewVoicesClient(cfg.Voices)
	deps := handlers.VoicesDeps{
		Cfg: cfg, Registry: registry, Jobs: jobs, Backend: client,
		Service: services.NewVoicesService(cfg, jobs, gpuMgr, client),
		Speech:  services.WhisperSpeechChecker{Model: cfg.Voices.WhisperModel, Jobs: jobs},
		Logger:  log,
	}
	if store.Enabled() {
		deps.Store = store
	}
	if s3Client != nil && cfg.S3Bucket != "" {
		deps.Objects = handlers.S3VoiceObjects{Client: s3Client, Presign: s3.NewPresignClient(s3Client), Bucket: cfg.S3Bucket}
	}
	if sweeper != nil {
		deps.Expirer = sweeper
	}
	if limiter != nil && limiter.Enabled() {
		deps.Previews = limiter
	}
	return handlers.NewVoicesHandler(deps)
}

// voicesHandler is the /api/voices handler main builds; setupRouter mounts it.
// nil (tests) mounts nothing. A package variable rather than another
// setupRouter parameter, so the router signature every test spells out stays
// as it is.
var voicesHandler *handlers.VoicesHandler

// voicesSpeakPath matches POST /api/voices/:id/speak and
// /api/voices/presets/:preset/speak.
func voicesSpeakPath(path string) bool {
	return strings.HasPrefix(path, "/api/voices/") && strings.HasSuffix(path, "/speak")
}

// voicesPreCharge runs the voices sign-in and plan refusals BEFORE the quota
// dispatcher spends a unit (ticket decision 10). Installed on the /api group
// after TierContext and before quotaDispatcher; every other path passes.
func voicesPreCharge() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodPost && strings.HasPrefix(c.Request.URL.Path, "/api/voices") {
			handlers.VoicesPreChargeGate(c)
			if c.IsAborted() {
				return
			}
		}
		c.Next()
	}
}
