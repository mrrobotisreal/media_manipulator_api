package vector

import (
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// ExportRequest is the body of POST /api/vector/exports (and the implied
// request of POST /api/vector/vectorize, format import_svg_vectorize).
type ExportRequest struct {
	ProjectID  string `json:"projectId,omitempty"`
	Format     string `json:"format"`
	DurationMs int    `json:"durationMs"`
	Height     int    `json:"height"`
	Fps        int    `json:"fps"`
}

// Decision outcomes. Each maps to exactly one HTTP response (see the handler).
const (
	DecisionAllow        = "allow"
	DecisionNotCountable = "not_countable" // 400 vector_format_not_countable (svg_static) / unknown format
	DecisionLocked       = "locked"        // 403 vector_format_locked
	DecisionCap          = "cap"           // 422 vector_cap_exceeded
)

// Cap names, the `cap` field of a 422.
const (
	CapDuration = "duration"
	CapHeight   = "height"
	CapFps      = "fps"
)

// Decision is CheckExport's answer. Max is the cap's limit for DecisionCap
// (ms for duration, px for height, frames/s for fps).
type Decision struct {
	Outcome string
	Cap     string
	Max     int
}

// Allowed reports whether the export may be counted.
func (d Decision) Allowed() bool { return d.Outcome == DecisionAllow }

// CheckExport applies the tier × format × caps matrix (design §3.4). Pure: the
// daily allowance itself is enforced by the counter, after this passes.
//
// Order: unknown/uncountable format, then format lock, then caps. A cap is
// checked only for a positive requested value (0 = not applicable, e.g. a
// Lottie export has no pixel height), and only when the tier's cap on that
// axis is positive (0 = no cap):
//
//   - duration: VectorMaxDurationMs for every format (anonymous: 10 s);
//   - raster formats (GIF, PNG sequence, MP4, WebM) also VectorVideo:
//     MaxSeconds (on durationMs), MaxHeight, MaxFps.
//
// Import conversions (import_lottie, import_svg_vectorize) carry no output
// dimensions and are only format-gated.
func CheckExport(l tiers.Limits, req ExportRequest) Decision {
	if !tiers.IsVectorFormat(req.Format) {
		return Decision{Outcome: DecisionNotCountable}
	}
	if !l.VectorFormats.Has(req.Format) {
		return Decision{Outcome: DecisionLocked}
	}
	if isImport(req.Format) {
		return Decision{Outcome: DecisionAllow}
	}
	if max := l.VectorMaxDurationMs; max > 0 && req.DurationMs > max {
		return Decision{Outcome: DecisionCap, Cap: CapDuration, Max: max}
	}
	if tiers.IsRasterVectorFormat(req.Format) {
		v := l.VectorVideo
		if v.MaxSeconds > 0 && req.DurationMs > v.MaxSeconds*1000 {
			return Decision{Outcome: DecisionCap, Cap: CapDuration, Max: v.MaxSeconds * 1000}
		}
		if v.MaxHeight > 0 && req.Height > v.MaxHeight {
			return Decision{Outcome: DecisionCap, Cap: CapHeight, Max: v.MaxHeight}
		}
		if v.MaxFps > 0 && req.Fps > v.MaxFps {
			return Decision{Outcome: DecisionCap, Cap: CapFps, Max: v.MaxFps}
		}
	}
	return Decision{Outcome: DecisionAllow}
}

func isImport(f string) bool {
	return f == tiers.VectorFormatImportLottie || f == tiers.VectorFormatImportSVGVectorize
}
