package tiers

import (
	"encoding/json"
	"strings"
)

// Motion (code name `vector`, ADR-0012) allowance plumbing: the export format
// capability set and the raster video cap. The decision logic that reads them
// (vector.CheckExport) lives in internal/vector; this file only holds the
// vocabulary so tiers, config and the handlers share one list.

// Counted Motion export formats (design MOTION_SVG_ANIMATOR.md §3.4). The list
// is exhaustive: POST /api/vector/exports refuses anything else. Static SVG
// export (`svg_static`) is deliberately absent: it is free and never counted.
const (
	VectorFormatSVGAnimated        = "svg_animated"
	VectorFormatLottie             = "lottie"
	VectorFormatReactTSX           = "react_tsx"
	VectorFormatJSTS               = "js_ts"
	VectorFormatFlutter            = "flutter"
	VectorFormatGIF                = "gif"
	VectorFormatPNGSequence        = "png_sequence"
	VectorFormatMP4                = "mp4"
	VectorFormatWebM               = "webm"
	VectorFormatImportLottie       = "import_lottie"
	VectorFormatImportSVGVectorize = "import_svg_vectorize"
	// VectorFormatSVGStatic is named only so callers can recognise and refuse
	// it; it is never part of a FormatSet.
	VectorFormatSVGStatic = "svg_static"
)

// AllVectorFormats is every countable format in canonical order (also the
// order FormatSet serialises in, and the vector_exports.format CHECK list).
func AllVectorFormats() []string {
	return []string{
		VectorFormatSVGAnimated, VectorFormatLottie, VectorFormatReactTSX, VectorFormatJSTS,
		VectorFormatFlutter, VectorFormatGIF, VectorFormatPNGSequence, VectorFormatMP4,
		VectorFormatWebM, VectorFormatImportLottie, VectorFormatImportSVGVectorize,
	}
}

// IsVectorFormat reports whether f is a countable format.
func IsVectorFormat(f string) bool {
	for _, k := range AllVectorFormats() {
		if k == f {
			return true
		}
	}
	return false
}

// IsRasterVectorFormat reports the formats the VectorVideo cap applies to: the
// frame-by-frame raster exports (GIF, PNG sequence, MP4, WebM).
func IsRasterVectorFormat(f string) bool {
	switch f {
	case VectorFormatGIF, VectorFormatPNGSequence, VectorFormatMP4, VectorFormatWebM:
		return true
	}
	return false
}

// FormatSet is a tier's Motion format capability set.
//
// It is a canonical comma-joined string rather than a []string so that Limits
// stays a comparable struct (the tier tests and the handlers compare Limits
// values with ==). It serialises as a JSON array in AllVectorFormats order, so
// GET /api/tiers shows `"vectorFormats": ["svg_animated", ...]`.
type FormatSet string

// NewFormatSet builds a set from any list: unknown names (and svg_static) are
// dropped, duplicates collapse, order is canonical.
func NewFormatSet(formats ...string) FormatSet {
	want := make(map[string]bool, len(formats))
	for _, f := range formats {
		want[strings.ToLower(strings.TrimSpace(f))] = true
	}
	out := make([]string, 0, len(want))
	for _, f := range AllVectorFormats() {
		if want[f] {
			out = append(out, f)
		}
	}
	return FormatSet(strings.Join(out, ","))
}

// List returns the formats in canonical order (never nil).
func (s FormatSet) List() []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(string(s), ",")
}

// Has reports whether f is in the set.
func (s FormatSet) Has(f string) bool {
	for _, k := range s.List() {
		if k == f {
			return true
		}
	}
	return false
}

// Union is s ∪ o.
func (s FormatSet) Union(o FormatSet) FormatSet {
	return NewFormatSet(append(s.List(), o.List()...)...)
}

// SupersetOf reports whether every format of o is in s.
func (s FormatSet) SupersetOf(o FormatSet) bool {
	for _, f := range o.List() {
		if !s.Has(f) {
			return false
		}
	}
	return true
}

// MarshalJSON renders the set as a JSON array.
func (s FormatSet) MarshalJSON() ([]byte, error) { return json.Marshal(s.List()) }

// UnmarshalJSON accepts the array form.
func (s *FormatSet) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	*s = NewFormatSet(list...)
	return nil
}

// VectorVideo caps the raster exports (IsRasterVectorFormat). A zero field
// means "no cap on that axis" for a tier that has a raster format at all; a
// tier with no raster format in VectorFormats never reaches the cap check.
type VectorVideo struct {
	MaxHeight  int `json:"maxHeight"`
	MaxFps     int `json:"maxFps"`
	MaxSeconds int `json:"maxSeconds"`
}

// Deployment ceilings for the operator's Motion caps (it has no plan cap, only
// these): the Studio output clamp for height and the editor's fps clamp.
const (
	DeploymentMaxVectorFps = 240
)
