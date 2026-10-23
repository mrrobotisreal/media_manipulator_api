package handlers

import (
	"errors"
	"net/http"
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

// rejectOverLimit checks a freshly-probed upload against the duration and
// resolution caps, reusing the ffprobe JSON MediaInspector already captured.
// Returns nil when the upload is acceptable — and also when the probe output
// cannot be read, because a malformed probe is not evidence of an oversized
// file and must not block a conversion.
func rejectOverLimit(c *gin.Context, cfg *config.Config, fileType models.FileType, metadata *services.MediaMetadata) *validation.Rejection {
	if metadata == nil || (fileType != models.FileTypeVideo && fileType != models.FileTypeAudio) {
		return nil
	}
	report, err := validation.ParseProbeJSON([]byte(metadata.Raw))
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

// outputHeightRefusal is the single handler-side entry to the output-height
// rule (tiers.EnforceOutputHeight). It returns nil when the caller may render
// requestedHeight, or when no tier has been resolved (tier system off, or a
// route outside /api): the same "no tier-derived limit" fallback as every other
// helper here.
func outputHeightRefusal(c *gin.Context, requestedHeight int) *tiers.Refusal {
	res, ok := middleware.ResolveTier(c)
	if !ok {
		return nil
	}
	return tiers.EnforceOutputHeight(res.Tier, res.Limits, requestedHeight)
}

// refuseOutputHeight writes the 403 output_height_limited body and reports
// whether it did. Callers return immediately when it is true.
func refuseOutputHeight(c *gin.Context, requestedHeight int) bool {
	refusal := outputHeightRefusal(c, requestedHeight)
	if refusal == nil {
		return false
	}
	c.AbortWithStatusJSON(http.StatusForbidden, refusal)
	return true
}

// requestedOutputHeight derives the rendered height a conversion request asks
// for, from its raw options map (JSON numbers arrive as float64), BEFORE any
// work starts. Rules, documented once here:
//
//   - An explicit height wins (video/image `height`, waveform `waveform.height`,
//     AI frame interpolation `ai.frameInterpolation.maxHeight`); the max of the
//     explicit values present is used.
//   - A width-only request has no knowable height before probing (a portrait
//     photo and a 16:9 video differ), so it is NOT enforced: guessing an aspect
//     ratio would wrongly refuse e.g. a Premium 3840-wide portrait image.
//     Video width is independently capped at 4096 by validateVideoOptions, so a
//     width-only video can exceed the Premium cap by at most 2304-2160 lines.
//   - Nothing requested (source passthrough) yields 0 and is never refused.
func requestedOutputHeight(options map[string]interface{}) int {
	if options == nil {
		return 0
	}
	best := 0
	consider := func(h int) {
		if h > best {
			best = h
		}
	}
	consider(optionInt(options, "height"))
	if nested, ok := options["waveform"].(map[string]interface{}); ok {
		consider(optionInt(nested, "height"))
	}
	if ai, ok := options["ai"].(map[string]interface{}); ok {
		if fi, ok := ai["frameInterpolation"].(map[string]interface{}); ok {
			consider(optionInt(fi, "maxHeight"))
		}
	}
	return best
}

func optionInt(options map[string]interface{}, key string) int {
	switch v := options[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// ExportHeightGate is the pre-quota half of the Studio export height rule. The
// quota dispatcher runs it BEFORE RequireQuota, so a refused export never
// spends the export allowance. lookup returns the persisted project height for
// the request (ok=false when it cannot be determined, in which case the gate
// stays out of the way and ExportProject itself decides).
func ExportHeightGate(lookup func(c *gin.Context) (int, bool)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if height, ok := lookup(c); ok {
			refuseOutputHeight(c, height)
		}
	}
}
