// Package photo validates and sanitizes "Aperture" documents (code name
// photo): the PhotoDoc v1 JSON the photo editor produces.
//
// Contract with the UI: this package is the Go mirror of the Zod model in
// media-manipulator-ui/lib/photo/photoTypes.ts (MM97). Both sides are pinned by
// the same fixture documents (testdata/docs, byte-identical copies of
// lib/photo/fixtures/docs, see testdata/README.md, which is also the field
// by field spec).
//
// Sanitize parses the document, validates it, CLAMPS out-of-range numbers
// (never refuses for those), fills every default, round-trips unknown fields
// (the additive-only rule) and emits canonical JSON: compact, keys sorted by
// code point at every depth.
//
// Refusals (all errors wrap ErrInvalidDoc and are *ValidationError): unknown
// version, unknown layer type, unknown mask kind, bad ids, duplicate ids,
// non-increasing curve points, oversize brush PNGs, too many layers or mask
// components, oversize LUTs, wrong JSON types, documents over MaxDocBytes.
package photo

import (
	"errors"
	"fmt"
)

// Limits bound one document. Zero fields fall back to DefaultLimits values.
type Limits struct {
	MaxDocBytes       int
	MaxLayers         int
	MaxMaskComponents int // per mask
	MaxBrushPNGBytes  int // decoded PNG bytes of one brush component
	MaxBrushPx        int // long side of a brush PNG
	MaxLutBytes       int // length of one lut layer's cube text
	MaxLutSize        int // LUT_3D_SIZE
	MaxCurvePoints    int // per curve channel
}

// DefaultLimits returns the production limits (== photoTypes.ts PHOTO_MAX_*).
func DefaultLimits() Limits {
	return Limits{
		MaxDocBytes:       8 << 20,
		MaxLayers:         64,
		MaxMaskComponents: 16,
		MaxBrushPNGBytes:  524288,
		MaxBrushPx:        1024,
		MaxLutBytes:       1 << 20,
		MaxLutSize:        65,
		MaxCurvePoints:    16,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxDocBytes <= 0 {
		l.MaxDocBytes = d.MaxDocBytes
	}
	if l.MaxLayers <= 0 {
		l.MaxLayers = d.MaxLayers
	}
	if l.MaxMaskComponents <= 0 {
		l.MaxMaskComponents = d.MaxMaskComponents
	}
	if l.MaxBrushPNGBytes <= 0 {
		l.MaxBrushPNGBytes = d.MaxBrushPNGBytes
	}
	if l.MaxBrushPx <= 0 {
		l.MaxBrushPx = d.MaxBrushPx
	}
	if l.MaxLutBytes <= 0 {
		l.MaxLutBytes = d.MaxLutBytes
	}
	if l.MaxLutSize <= 0 {
		l.MaxLutSize = d.MaxLutSize
	}
	if l.MaxCurvePoints <= 0 {
		l.MaxCurvePoints = d.MaxCurvePoints
	}
	return l
}

// Fixed structural caps that are not part of Limits.
const (
	maxHeals     = 256
	maxHSLRanges = 32
	maxColors    = 16
	maxNameLen   = 80
	maxIDLen     = 40
)

// ErrInvalidDoc is wrapped by every error Sanitize returns.
var ErrInvalidDoc = errors.New("invalid photo doc")

// Reason codes carried by ValidationError.Code.
const (
	CodeTooLarge           = "doc_too_large"
	CodeInvalidJSON        = "invalid_json"
	CodeUnsupportedVersion = "unsupported_version"
	CodeInvalidDoc         = "invalid_doc" // wrong type, missing required field, bad enum/pattern
	CodeUnknownLayerType   = "unknown_layer_type"
	CodeUnknownMaskKind    = "unknown_mask_kind"
	CodeBadID              = "bad_id"
	CodeDuplicateID        = "duplicate_id"
	CodeInvalidCurve       = "invalid_curve_points"
	CodeBrushTooLarge      = "brush_too_large"
	CodeTooManyLayers      = "too_many_layers"
	CodeTooManyComponents  = "too_many_mask_components"
	CodeLutTooLarge        = "lut_too_large"
)

// ValidationError is the only error type Sanitize returns. errors.Is(err,
// ErrInvalidDoc) is true for it.
type ValidationError struct {
	Code    string
	Path    string // JSON-pointer-ish path to the offending node ("" for whole-doc errors)
	Message string
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return e.Code + ": " + e.Message
	}
	return e.Code + " at " + e.Path + ": " + e.Message
}

// Unwrap makes errors.Is(err, ErrInvalidDoc) hold.
func (e *ValidationError) Unwrap() error { return ErrInvalidDoc }

func fail(code, path, format string, args ...any) *ValidationError {
	return &ValidationError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

// Sanitize validates raw against lim and returns the canonical document bytes
// (compact JSON, sorted keys, defaults filled, unknown fields preserved). The
// only error type returned is *ValidationError.
func Sanitize(raw []byte, lim Limits) ([]byte, error) {
	lim = lim.withDefaults()
	if len(raw) > lim.MaxDocBytes {
		return nil, fail(CodeTooLarge, "", "document exceeds %d bytes", lim.MaxDocBytes)
	}
	tree, verr := parseJSON(raw)
	if verr != nil {
		return nil, verr
	}
	root, ok := tree.(map[string]any)
	if !ok {
		return nil, fail(CodeInvalidJSON, "", "top-level value must be an object")
	}
	if v, ok := root["version"]; !ok {
		return nil, fail(CodeUnsupportedVersion, "/version", "version is required")
	} else if f, isNum := v.(float64); !isNum || f != 1 {
		return nil, fail(CodeUnsupportedVersion, "/version", "unsupported version %v (only 1)", v)
	}
	c := &ctx{lim: lim}
	out, _, verr := docSpec.clean(root, true, "", c)
	if verr != nil {
		return nil, verr
	}
	return emit(out), nil
}
