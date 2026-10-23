package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/validation"
)

// maxUploadBytes is the byte cap to apply to one caller's request body.
//
// Two ceilings apply and the tighter one wins: MAX_FILE_SIZE_BYTES is the
// deployment's absolute ceiling — what the disk, the memory budget and the
// command timeouts can actually survive — and the caller's tier allowance is
// the product limit sold on the pricing page. Premium cannot exceed the
// deployment ceiling, and no tier can raise it.
//
// When no tier has been resolved (the tier system is off, or the route is not
// under /api) this returns the configured ceiling unchanged, which is exactly
// the pre-tier behaviour.
func maxUploadBytes(c *gin.Context, cfg *config.Config) int64 {
	if cfg == nil {
		return 0
	}
	ceiling := cfg.MaxFileSize
	res, ok := middleware.ResolveTier(c)
	if !ok || res.Limits.MaxFileBytes <= 0 {
		return ceiling
	}
	if ceiling <= 0 || res.Limits.MaxFileBytes < ceiling {
		return res.Limits.MaxFileBytes
	}
	return ceiling
}

// tierValidationLimits is the caller's tier allowance in the shape the
// validation package wants, or nil when no tier has been resolved. nil means
// "deployment ceiling only", which is how the service behaved before tiers.
func tierValidationLimits(c *gin.Context) *validation.Limits {
	res, ok := middleware.ResolveTier(c)
	if !ok {
		return nil
	}
	return &validation.Limits{MaxVideoSeconds: res.Limits.MaxVideoSeconds}
}

// rejectOverLimit checks a freshly-stored upload against the duration and
// resolution caps with one ffprobe run. Returns nil when the upload is
// acceptable, and also when the probe fails or cannot be read, because a failed
// probe is not evidence of an oversized file and must not block a conversion.
func rejectOverLimit(ctx context.Context, c *gin.Context, cfg *config.Config, fileType models.FileType, path string) *validation.Rejection {
	if fileType != models.FileTypeVideo && fileType != models.FileTypeAudio {
		return nil
	}
	report, err := validation.Probe(ctx, "ffprobe", path, cfg.CommandTimeout)
	if err != nil || report == nil {
		return nil
	}
	var rejection *validation.Rejection
	if errors.As(validation.ValidateVideo(cfg, tierValidationLimits(c), report), &rejection) {
		return rejection
	}
	return nil
}

// gpuPriorityFor is the scheduler priority this caller's jobs run at. Premium
// buys a place at the front of the GPU queue; everyone else shares one queue.
func gpuPriorityFor(c *gin.Context) int {
	if res, ok := middleware.ResolveTier(c); ok && res.Limits.PriorityGPU {
		return services.GPUPriorityPremium
	}
	return services.GPUPriorityStandard
}

// markRetention records the caller's result-retention window alongside the job
// artifacts, so the cleanup sweeper keeps a Premium result for 30 days and an
// anonymous one for 24 hours. Silent on failure: a missing marker falls back to
// the global retention, which never deletes anything early.
func markRetention(c *gin.Context, dirs ...string) {
	res, ok := middleware.ResolveTier(c)
	if !ok || res.Limits.ResultRetentionHours <= 0 {
		return
	}
	window := time.Duration(res.Limits.ResultRetentionHours) * time.Hour
	for _, dir := range dirs {
		retention.WriteQuietly(dir, window)
	}
}

// ladderAccessFor is the transcode-ladder slice this caller may select from.
// Unresolved tier or missing registry both degrade to the anonymous ladder —
// the same rungs every caller got before tiers existed.
func ladderAccessFor(c *gin.Context, registry *tiers.Registry) services.LadderAccess {
	res, ok := middleware.ResolveTier(c)
	if !ok || registry == nil {
		return services.AnonymousLadderAccess()
	}
	return services.LadderAccessFor(res.Tier, registry)
}
