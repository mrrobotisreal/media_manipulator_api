package handlers

import (
	"fmt"
	"net/http"
	"syscall"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// MM120 ProRes size guard (design DARKROOM_OUTPUTS_AND_CAPTIONS.md §5). A
// predictable-size output (ProRes 422, ~147 Mb/s at 1080p30) is estimated from
// the output table before any job exists, and refused when it would exceed
// the caller's per-file cap (the tier's MaxFileBytes, the same number the plan
// advertises for one file) or the free space on the output disk minus
// headroom. Constant-quality codecs have no estimate and are never refused.

// ErrCodeExportTooLarge is the 413's `error` (and the failure class the
// analytics vocabulary already has).
const ErrCodeExportTooLarge = "too_large"

// StudioExportTooLarge is the 413 body.
type StudioExportTooLarge struct {
	Error          string `json:"error"`
	Message        string `json:"message"`
	Reason         string `json:"reason"` // file_cap | disk
	EstimatedBytes int64  `json:"estimatedBytes"`
	MaxBytes       int64  `json:"maxBytes,omitempty"`
}

// studioOutputFreeBytes reports the free space for the export output dir (-1
// when unknown). A variable so tests can pin it.
var studioOutputFreeBytes = func(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// studioOutputSizeRefusal is the guard's decision for one export (nil =
// allowed). The cap is the resolved tier's MaxFileBytes; with no resolved
// tier (tier system off) only the operator MaxFileSize applies.
func studioOutputSizeRefusal(c *gin.Context, out services.StudioOutput, width, height int, fps, seconds float64, operatorMax int64, outputDir string) *StudioExportTooLarge {
	estimate, ok := services.StudioOutputEstimateBytes(out, width, height, fps, seconds)
	if !ok {
		return nil
	}
	maxBytes := operatorMax
	if res, resolved := middleware.ResolveTier(c); resolved && res.Limits.MaxFileBytes > 0 {
		maxBytes = res.Limits.MaxFileBytes
	}
	refuse, reason := services.StudioOutputTooLarge(estimate, true, maxBytes, studioOutputFreeBytes(outputDir))
	if !refuse {
		return nil
	}
	body := &StudioExportTooLarge{Error: ErrCodeExportTooLarge, Reason: reason, EstimatedBytes: estimate}
	gb := func(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) }
	if reason == "file_cap" {
		body.MaxBytes = maxBytes
		body.Message = fmt.Sprintf("This export would be about %s, over your plan's %s per-file limit. Shorten the timeline, pick a lower resolution, or use MOV H.265.", gb(estimate), gb(maxBytes))
	} else {
		body.Message = fmt.Sprintf("This export would be about %s and the render server is short of space for it right now. Try a shorter range, a lower resolution, or MOV H.265.", gb(estimate))
	}
	return body
}

// refuseStudioOutputSize writes the 413 and reports whether it did.
func refuseStudioOutputSize(c *gin.Context, out services.StudioOutput, width, height int, fps, seconds float64, operatorMax int64, outputDir string) bool {
	body := studioOutputSizeRefusal(c, out, width, height, fps, seconds, operatorMax, outputDir)
	if body == nil {
		return false
	}
	c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, body)
	return true
}
