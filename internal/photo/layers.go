package photo

// MM103: user presets. A preset's `layers` is the layer subset of a PhotoDoc: an
// array of AdjustmentLayer objects. SanitizeLayers runs the very same layer
// spec Sanitize runs over doc.layers (layersSpec), so the two cannot drift.

// PresetMaxLayers and PresetMaxBytes bound one stored preset (ticket MM103).
const (
	PresetMaxLayers = 64
	PresetMaxBytes  = 1 << 20
)

// PresetLimits are the limits one preset is sanitized against: the document
// limits (brush masks, LUTs, curves) with the layer count and byte size cut to
// the preset caps. Zero fields of lim keep the DefaultLimits values.
func PresetLimits(lim Limits) Limits {
	lim = lim.withDefaults()
	if lim.MaxLayers > PresetMaxLayers {
		lim.MaxLayers = PresetMaxLayers
	}
	lim.MaxDocBytes = PresetMaxBytes
	return lim
}

// SanitizeLayers validates raw (a JSON array of layers) against lim and returns
// the canonical bytes: the same clamping, defaults and unknown-field round trip
// as Sanitize. Refusals are *ValidationError wrapping ErrInvalidDoc: a value that
// is not an array, an unknown layer type, a layer mask that is a {ref} (a preset
// carries no doc.masks to resolve it against), too many layers, duplicate ids,
// bytes over lim.MaxDocBytes. An empty array is valid here; the handler decides
// whether an empty preset is acceptable.
func SanitizeLayers(raw []byte, lim Limits) ([]byte, error) {
	lim = lim.withDefaults()
	if len(raw) > lim.MaxDocBytes {
		return nil, fail(CodeTooLarge, "", "layers exceed %d bytes", lim.MaxDocBytes)
	}
	tree, verr := parseJSON(raw)
	if verr != nil {
		return nil, verr
	}
	in, ok := tree.([]any)
	if !ok {
		return nil, fail(CodeInvalidJSON, "", "layers must be a JSON array")
	}
	for i, l := range in {
		lm, _ := l.(map[string]any)
		mk, _ := lm["mask"].(map[string]any)
		if _, isRef := mk["ref"]; isRef {
			if _, full := mk["components"]; !full {
				return nil, fail(CodeInvalidDoc, "/"+itoa(i)+"/mask/ref", "a preset layer cannot reference doc.masks")
			}
		}
	}
	out, _, verr := layersSpec.clean(in, true, "", &ctx{lim: lim})
	if verr != nil {
		return nil, verr
	}
	b := emit(out)
	if len(b) > lim.MaxDocBytes {
		return nil, fail(CodeTooLarge, "", "layers exceed %d bytes", lim.MaxDocBytes)
	}
	return b, nil
}
