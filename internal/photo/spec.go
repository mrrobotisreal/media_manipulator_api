package photo

import (
	"encoding/base64"
	"encoding/binary"
	"regexp"
	"strconv"
	"strings"
)

var blendEnum = enumNode{vals: []string{"normal", "luminosity", "color"}, def: "normal"}
var modeEnum = enumNode{vals: []string{"add", "subtract", "intersect"}, def: "add"}

func pt(dx, dy float64) objNode {
	return obj(f("x", num(-1, 2, dx)), f("y", num(-1, 2, dy)))
}

func corner() objNode { return obj(f("x", num(-1, 1, 0)), f("y", num(-1, 1, 0))) }

var docSpec = obj(
	f("version", fixedNum(1)),
	f("source", sourceSpec),
	f("heals", arrNode{item: obj(f("id", idNode{}), f("maskRef", refNode{})), max: maxHeals, post: uniqueIDs}),
	f("geometry", geometrySpec),
	f("layers", layersSpec),
	f("output", obj(
		f("format", optNode{enumNode{vals: []string{"jpg", "png", "webp", "avif", "tiff"}}}),
		f("quality", optNode{intn(1, 100, 0)}),
		f("metadataMode", optNode{enumNode{vals: []string{"keep", "strip"}}}),
	)),
)

// fixedNum always emits v (version, orientation).
type fixedNum float64

func (n fixedNum) clean(_ any, _ bool, _ string, _ *ctx) (any, bool, *ValidationError) {
	return float64(n), false, nil
}

// sourceSpec: source is required with its width and height (pixel size of the
// EXIF-upright original). orientation is always 1.
var sourceSpec = obj(
	f("width", reqInt(1, 100000)),
	f("height", reqInt(1, 100000)),
	f("orientation", fixedNum(1)),
)

type requiredInt struct{ min, max float64 }

func reqInt(min, max float64) requiredInt { return requiredInt{min, max} }

func (n requiredInt) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	if !present {
		return nil, false, fail(CodeInvalidDoc, path, "required")
	}
	return intn(n.min, n.max, n.min).clean(v, true, path, c)
}

var geometrySpec = obj(
	f("rotate", num(-360, 360, 0)),
	f("flipH", boolNode{}),
	f("flipV", boolNode{}),
	f("perspective", nullNode{obj(f("tl", corner()), f("tr", corner()), f("br", corner()), f("bl", corner()))}),
	f("crop", nullNode{obj(
		f("x", num(0, 0.999, 0)),
		f("y", num(0, 0.999, 0)),
		f("width", num(0.001, 1, 1)),
		f("height", num(0.001, 1, 1)),
	).withPost(func(m map[string]any, _ string, _ *ctx) *ValidationError {
		x, y := m["x"].(float64), m["y"].(float64)
		if w := m["width"].(float64); x+w > 1 {
			m["width"] = 1 - x
		}
		if h := m["height"].(float64); y+h > 1 {
			m["height"] = 1 - y
		}
		return nil
	})}),
	f("fill", fillNode{}),
)

// fillNode: "transparent" | "edge" | #rrggbb (lowercased).
type fillNode struct{}

func (fillNode) clean(v any, present bool, path string, _ *ctx) (any, bool, *ValidationError) {
	if !present {
		return "transparent", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, false, typeErr(path, "string")
	}
	if s == "transparent" || s == "edge" {
		return s, false, nil
	}
	if colorRe.MatchString(s) {
		return strings.ToLower(s), false, nil
	}
	return nil, false, fail(CodeInvalidDoc, path, "fill must be transparent, edge or #rrggbb")
}

// ---- layers ----

var layersSpec = layerList{}

type layerList struct{}

func (layerList) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	if !present {
		return []any{}, false, nil
	}
	in, ok := v.([]any)
	if !ok {
		return nil, false, typeErr(path, "array")
	}
	if len(in) > c.lim.MaxLayers {
		return nil, false, fail(CodeTooManyLayers, path, "more than %d layers", c.lim.MaxLayers)
	}
	out := make([]any, 0, len(in))
	for i, e := range in {
		o, _, err := layerNode{}.clean(e, true, path+"/"+itoa(i), c)
		if err != nil {
			return nil, false, err
		}
		out = append(out, o)
	}
	if err := uniqueIDs(out, path, c); err != nil {
		return nil, false, err
	}
	return out, false, nil
}

type layerNode struct{}

func (layerNode) clean(v any, _ bool, path string, c *ctx) (any, bool, *ValidationError) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false, typeErr(path, "object")
	}
	t, _ := m["type"].(string)
	params, known := layerParams[t]
	if !known {
		return nil, false, fail(CodeUnknownLayerType, path+"/type", "unknown layer type %q", t)
	}
	spec := obj(
		f("id", idNode{}),
		f("type", enumNode{vals: []string{t}, def: t}),
		f("name", optNode{strNode{max: maxNameLen}}),
		f("enabled", boolNode{def: true}),
		f("opacity", num(0, 1, 1)),
		f("blend", blendEnum),
		f("params", params),
		f("mask", nullNode{maskSpec}),
	)
	return spec.clean(m, true, path, c)
}

func chan5() objNode {
	return obj(
		f("inBlack", num(0, 1, 0)),
		f("inWhite", num(0, 1, 1)),
		f("gamma", num(0.1, 10, 1)),
		f("outBlack", num(0, 1, 0)),
		f("outWhite", num(0, 1, 1)),
	)
}

// curveChan: {points:[{x,y}]}, 2..MaxCurvePoints points, x strictly increasing.
type curveChan struct{}

func (curveChan) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	pointsNode := curvePoints{}
	return obj(f("points", pointsNode)).clean(v, present, path, c)
}

type curvePoints struct{}

func (curvePoints) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	if !present {
		return []any{map[string]any{"x": 0.0, "y": 0.0}, map[string]any{"x": 1.0, "y": 1.0}}, false, nil
	}
	in, ok := v.([]any)
	if !ok {
		return nil, false, typeErr(path, "array")
	}
	if len(in) < 2 || len(in) > c.lim.MaxCurvePoints {
		return nil, false, fail(CodeInvalidCurve, path, "a curve needs 2..%d points", c.lim.MaxCurvePoints)
	}
	pn := obj(f("x", num(0, 1, 0)), f("y", num(0, 1, 0)))
	out := make([]any, 0, len(in))
	prev := -1.0
	for i, e := range in {
		o, _, err := pn.clean(e, true, path+"/"+itoa(i), c)
		if err != nil {
			return nil, false, err
		}
		x := o.(map[string]any)["x"].(float64)
		if x <= prev {
			return nil, false, fail(CodeInvalidCurve, path+"/"+itoa(i)+"/x", "curve x values must strictly increase")
		}
		prev = x
		out = append(out, o)
	}
	return out, false, nil
}

func rgb100() objNode {
	return obj(f("r", num(-100, 100, 0)), f("g", num(-100, 100, 0)), f("b", num(-100, 100, 0)))
}

func mixRow(r, g, b float64) objNode {
	return obj(f("r", num(-200, 200, r)), f("g", num(-200, 200, g)), f("b", num(-200, 200, b)), f("constant", num(-200, 200, 0)))
}

func hueSat() objNode { return obj(f("hue", num(0, 360, 0)), f("saturation", num(0, 100, 0))) }

func hslRange() objNode {
	return obj(
		f("id", idNode{}),
		f("name", optNode{strNode{max: maxNameLen}}),
		f("center", num(0, 360, 0)),
		f("width", num(10, 120, 30)),
		f("feather", num(0, 90, 30)),
		f("hue", num(-180, 180, 0)),
		f("saturation", num(-100, 100, 0)),
		f("lightness", num(-100, 100, 0)),
	)
}

func defaultHSLRanges() []any {
	names := []string{"reds", "yellows", "greens", "cyans", "blues", "magentas"}
	out := make([]any, 0, 6)
	for i, n := range names {
		out = append(out, map[string]any{"id": n, "center": float64(i * 60)})
	}
	return out
}

func sliderParams(fields ...string) objNode {
	fs := make([]field, 0, len(fields))
	for _, n := range fields {
		fs = append(fs, f(n, num(-100, 100, 0)))
	}
	return obj(fs...)
}

type lutCube struct{}

var lutSizeRe = regexp.MustCompile(`(?m)^[ \t]*LUT_3D_SIZE[ \t]+([0-9]+)`)

func (lutCube) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	if !present {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, false, typeErr(path, "string")
	}
	if len(s) > c.lim.MaxLutBytes {
		return nil, false, fail(CodeLutTooLarge, path, "cube text exceeds %d bytes", c.lim.MaxLutBytes)
	}
	if s != "" {
		m := lutSizeRe.FindStringSubmatch(s)
		if m == nil {
			return nil, false, fail(CodeInvalidDoc, path, "cube must declare LUT_3D_SIZE")
		}
		n, _ := strconv.Atoi(m[1])
		if n < 2 || n > c.lim.MaxLutSize {
			return nil, false, fail(CodeLutTooLarge, path, "LUT_3D_SIZE must be 2..%d", c.lim.MaxLutSize)
		}
	}
	return s, false, nil
}

var layerParams = map[string]node{
	"exposure":           obj(f("ev", num(-5, 5, 0)), f("offset", num(-0.5, 0.5, 0)), f("gamma", num(0.1, 10, 1))),
	"brightnessContrast": sliderParams("brightness", "contrast"),
	"tone":               sliderParams("highlights", "shadows", "whites", "blacks"),
	"levels":             obj(f("master", chan5()), f("r", chan5()), f("g", chan5()), f("b", chan5())),
	"curves":             obj(f("master", curveChan{}), f("r", curveChan{}), f("g", curveChan{}), f("b", curveChan{})),
	"whiteBalance":       sliderParams("temperature", "tint"),
	"vibranceSaturation": sliderParams("vibrance", "saturation"),
	"hsl": obj(
		f("master", obj(f("hue", num(-180, 180, 0)), f("saturation", num(-100, 100, 0)), f("lightness", num(-100, 100, 0)))),
		f("ranges", arrNode{item: hslRange(), max: maxHSLRanges, def: defaultHSLRanges, post: uniqueIDs}),
	),
	"colorBalance": obj(f("shadows", rgb100()), f("midtones", rgb100()), f("highlights", rgb100()), f("preserveLuminosity", boolNode{def: true})),
	"blackWhite": obj(
		f("reds", num(-200, 300, 40)), f("yellows", num(-200, 300, 60)), f("greens", num(-200, 300, 40)),
		f("cyans", num(-200, 300, 60)), f("blues", num(-200, 300, 20)), f("magentas", num(-200, 300, 80)),
		f("tint", nullNode{hueSat()}),
	),
	"channelMixer": obj(f("r", mixRow(100, 0, 0)), f("g", mixRow(0, 100, 0)), f("b", mixRow(0, 0, 100)), f("monochrome", boolNode{})),
	"photoFilter":  obj(f("color", colorNode{def: "#ec8a00"}), f("density", num(0, 100, 25)), f("preserveLuminosity", boolNode{def: true})),
	"splitTone":    obj(f("shadows", hueSat()), f("highlights", hueSat()), f("balance", num(-100, 100, 0))),
	"lut":          obj(f("cube", lutCube{}), f("intensity", num(0, 1, 1))),
	"invert":       obj(),
	"posterize":    obj(f("levels", intn(2, 255, 4))),
	"threshold":    obj(f("level", num(0, 1, 0.5))),
	"gamma":        obj(f("gamma", num(0.1, 10, 1))),
	"redEye":       obj(),

	"sharpen":        obj(f("amount", num(0, 500, 100)), f("radius", num(0.1, 50, 1)), f("threshold", num(0, 255, 0))),
	"blur":           obj(f("radius", num(0, 100, 0))),
	"noiseReduction": obj(f("luminance", num(0, 100, 0)), f("color", num(0, 100, 0))),
	"clarity":        sliderParams("amount"),
	"texture":        sliderParams("amount"),
	"dehaze":         sliderParams("amount"),
	"vignette": obj(
		f("amount", num(-100, 100, 0)), f("midpoint", num(0, 100, 50)), f("roundness", num(-100, 100, 0)),
		f("feather", num(0, 100, 50)), f("highlights", num(0, 100, 0)),
	),
	"grain": obj(f("amount", num(0, 100, 0)), f("size", num(1, 3, 1)), f("roughness", num(0, 100, 50)), f("seed", intn(0, 4294967295, 0))),
}

// ---- masks ----

var maskSpec = obj(
	f("components", maskComponents{}),
	f("feather", num(0, 100, 0)),
	f("invert", boolNode{}),
	f("opacity", num(0, 1, 1)),
)

type maskComponents struct{}

func (maskComponents) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	if !present {
		return []any{}, false, nil
	}
	in, ok := v.([]any)
	if !ok {
		return nil, false, typeErr(path, "array")
	}
	if len(in) > c.lim.MaxMaskComponents {
		return nil, false, fail(CodeTooManyComponents, path, "more than %d mask components", c.lim.MaxMaskComponents)
	}
	out := make([]any, 0, len(in))
	for i, e := range in {
		o, err := cleanComponent(e, path+"/"+itoa(i), c)
		if err != nil {
			return nil, false, err
		}
		out = append(out, o)
	}
	return out, false, nil
}

func cleanComponent(v any, path string, c *ctx) (any, *ValidationError) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, typeErr(path, "object")
	}
	kind, _ := m["kind"].(string)
	var kf []field
	switch kind {
	case "gradient":
		kf = []field{f("from", pt(0.5, 0.3)), f("to", pt(0.5, 0.7)), f("feather", num(0, 1, 0))}
	case "radial":
		kf = []field{f("center", pt(0.5, 0.5)), f("rx", num(0, 2, 0.3)), f("ry", num(0, 2, 0.3)), f("rotate", num(-180, 180, 0)), f("feather", num(0, 1, 0.5))}
	case "brush":
		kf = []field{f("png", brushPNG{}), f("width", reqInt(1, float64(c.lim.MaxBrushPx))), f("height", reqInt(1, float64(c.lim.MaxBrushPx)))}
	case "luminosity":
		kf = []field{f("lo", num(0, 1, 0)), f("hi", num(0, 1, 1)), f("feather", num(0, 1, 0.1))}
	case "colorRange":
		kf = []field{f("colors", arrNode{item: obj(f("r", intn(0, 255, 0)), f("g", intn(0, 255, 0)), f("b", intn(0, 255, 0))), max: maxColors}), f("fuzziness", num(0, 100, 40))}
	case "subject":
		kf = []field{f("maskRef", refNode{}), f("invert", boolNode{})}
	default:
		return nil, fail(CodeUnknownMaskKind, path+"/kind", "unknown mask kind %q", kind)
	}
	if kind == "brush" {
		// Out-of-limit sizes are refused, not clamped.
		for _, k := range []string{"width", "height"} {
			if n, isNum := m[k].(float64); isNum && n > float64(c.lim.MaxBrushPx) {
				return nil, fail(CodeBrushTooLarge, path+"/"+k, "brush is larger than %d px", c.lim.MaxBrushPx)
			}
		}
	}
	fs := append([]field{f("kind", enumNode{vals: []string{kind}, def: kind}), f("mode", modeEnum)}, kf...)
	o, _, err := obj(fs...).clean(m, true, path, c)
	if err != nil {
		return nil, err
	}
	return o, nil
}

const pngPrefix = "data:image/png;base64,"

type brushPNG struct{}

func (brushPNG) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	if !present {
		return nil, false, fail(CodeInvalidDoc, path, "png is required")
	}
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, pngPrefix) {
		return nil, false, fail(CodeInvalidDoc, path, "png must be a data:image/png;base64, URI")
	}
	b64 := s[len(pngPrefix):]
	if base64.StdEncoding.DecodedLen(len(b64)) > c.lim.MaxBrushPNGBytes+2 {
		return nil, false, fail(CodeBrushTooLarge, path, "brush PNG exceeds %d bytes", c.lim.MaxBrushPNGBytes)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, false, fail(CodeInvalidDoc, path, "png is not valid base64")
	}
	if len(raw) > c.lim.MaxBrushPNGBytes {
		return nil, false, fail(CodeBrushTooLarge, path, "brush PNG exceeds %d bytes", c.lim.MaxBrushPNGBytes)
	}
	if len(raw) < 24 || string(raw[:8]) != "\x89PNG\r\n\x1a\n" || string(raw[12:16]) != "IHDR" {
		return nil, false, fail(CodeInvalidDoc, path, "png is not a PNG image")
	}
	w, h := binary.BigEndian.Uint32(raw[16:20]), binary.BigEndian.Uint32(raw[20:24])
	if w == 0 || h == 0 || int(w) > c.lim.MaxBrushPx || int(h) > c.lim.MaxBrushPx {
		return nil, false, fail(CodeBrushTooLarge, path, "brush PNG is %dx%d, max %d px per side", w, h, c.lim.MaxBrushPx)
	}
	return s, false, nil
}
