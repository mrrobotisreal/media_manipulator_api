// Package vector validates and sanitizes "Motion" documents (code name
// vector): the VectorDoc v1 JSON the browser editor produces.
//
// Contract with the UI: the browser validates with a Zod schema
// (media-manipulator-ui/lib/vector) and this package is its Go mirror; the
// server runs Sanitize before storing the document in a Postgres jsonb column.
// Both sides are pinned by the same fixture documents (testdata/*.json, copied
// byte-identical between repos, see testdata/README.md).
//
// The design is additive-only: new optional fields may appear later and must
// round-trip, so unknown keys below the top level are kept verbatim. Only
// unknown top-level keys outside the allow-list are stripped. An unknown
// version is rejected. Validation is all-or-nothing and returns the first
// error in document order (layers, defs, animations).
package vector

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Limits bound one document. Zero fields fall back to DefaultLimits values.
type Limits struct {
	MaxDocBytes          int // raw input length cap
	MaxDepth             int // layer-tree nesting depth, across layers and def/symbol children
	MaxLayers            int // total layers in the doc, def/symbol child trees and raw layers included
	MaxKeyframesPerTrack int
	MaxRawBytes          int // max length of one raw layer's markup
}

// DefaultLimits returns the production limits.
func DefaultLimits() Limits {
	return Limits{
		MaxDocBytes:          4 << 20,
		MaxDepth:             32,
		MaxLayers:            5000,
		MaxKeyframesPerTrack: 2000,
		MaxRawBytes:          256 << 10,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxDocBytes <= 0 {
		l.MaxDocBytes = d.MaxDocBytes
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxLayers <= 0 {
		l.MaxLayers = d.MaxLayers
	}
	if l.MaxKeyframesPerTrack <= 0 {
		l.MaxKeyframesPerTrack = d.MaxKeyframesPerTrack
	}
	if l.MaxRawBytes <= 0 {
		l.MaxRawBytes = d.MaxRawBytes
	}
	return l
}

// Error codes carried by ValidationError.Code.
const (
	CodeTooLarge           = "doc_too_large"
	CodeInvalidJSON        = "invalid_json"
	CodeUnsupportedVersion = "unsupported_version"
	CodeInvalidDoc         = "invalid_doc" // wrong type / missing required field / bad enum
	CodeDuplicateID        = "duplicate_id"
	CodeDanglingRef        = "dangling_ref"
	CodeUseCycle           = "use_cycle"
	CodeTooDeep            = "too_deep"
	CodeTooManyLayers      = "too_many_layers"
	CodeTooManyKeyframes   = "too_many_keyframes"
	CodeUnsafeContent      = "unsafe_content" // raw markup / href security failures
)

// ValidationError is the only error type Sanitize returns.
type ValidationError struct {
	Code    string // one of the Code* constants
	Path    string // JSON-pointer-ish path to the offending node ("" for whole-doc errors)
	Message string // short human-readable English
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return e.Code + ": " + e.Message
	}
	return e.Code + " at " + e.Path + ": " + e.Message
}

// AdditiveTopLevelKeys are top-level keys added to v1 after release that must round-trip.
var AdditiveTopLevelKeys = []string{"loop", "playbackRange", "meta"}

// knownTopLevelKeys are the v1 top-level keys; anything else (that is not in
// AdditiveTopLevelKeys) is stripped.
var knownTopLevelKeys = []string{"version", "canvas", "duration", "fps", "layers", "defs", "animations"}

// layerTypes are the legal Layer.type values.
var layerTypes = map[string]bool{
	"group": true, "path": true, "rect": true, "ellipse": true, "line": true,
	"polyline": true, "polygon": true, "text": true, "image": true, "use": true, "raw": true,
}

// trackProperties are the animatable property names. The UI Zod enum
// (media-manipulator-ui/lib/vector) MUST change in the same commit as this
// list. Properties of the form gradientStop[N].color|offset are matched by
// gradientStopProp instead.
var trackProperties = map[string]bool{
	"transform.x": true, "transform.y": true, "transform.scaleX": true, "transform.scaleY": true,
	"transform.rotation": true, "transform.skewX": true, "transform.skewY": true,
	"transform.anchorX": true, "transform.anchorY": true,
	"opacity": true, "fill": true, "stroke": true, "strokeWidth": true, "strokeDashoffset": true,
	"d": true, "mask": true, "clipPath": true, "filter.blur": true, "motionPath": true,
}

var easingNames = map[string]bool{"linear": true, "easeIn": true, "easeOut": true, "easeBoth": true, "hold": true}

var (
	gradientStopProp = regexp.MustCompile(`^gradientStop\[\d+\]\.(color|offset)$`)
	urlRef           = regexp.MustCompile(`^\s*url\(\s*['"]?\s*#([^'")\s]+)\s*['"]?\s*\)\s*$`)
	uriScheme        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*:`)

	rawEventAttr = regexp.MustCompile(`(?i)[\s"'/]on[a-z]+\s*=`)
	rawHrefAttr  = regexp.MustCompile(`(?i)href\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	rawJSURL     = regexp.MustCompile(`(?i)javascript\s*:`)
	rawCSSURL    = regexp.MustCompile(`(?i)url\(\s*['"]?\s*(?:https?:|//)`)
	rawImport    = regexp.MustCompile(`(?i)@import`)
	// Active or embedding elements, namespaced or not (<svg:script>, <iframe>,
	// <foreignObject> which can host HTML), and DTD constructs (<!DOCTYPE,
	// <!ENTITY) that can smuggle entities past the attribute checks.
	rawActiveTag = regexp.MustCompile(`(?i)<\s*(?:[a-z0-9_.\-]+:)?(?:script|iframe|embed|object|foreignobject|meta|base|link|frame|frameset|applet|audio|video|handler|listener)\b`)
	rawDTD       = regexp.MustCompile(`<!(?i:doctype|entity|element|attlist)`)
	rawSrcAttr   = regexp.MustCompile(`(?i)[\s"'/]src\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// Doc is a validated, sanitized VectorDoc.
type Doc struct {
	root   *object
	layers int
	tracks int
}

// Version returns the document version (always 1 for a valid Doc).
func (d Doc) Version() int { return 1 }

// LayerCount is the total layers, including def/symbol child trees.
func (d Doc) LayerCount() int { return d.layers }

// TrackCount is the number of animation tracks.
func (d Doc) TrackCount() int { return d.tracks }

// Canonical is the compact canonical JSON (== JS JSON.stringify(doc)), for storage.
func (d Doc) Canonical() []byte { return emit(d.root, false) }

// CanonicalIndent is JSON.stringify(doc, null, 2) + "\n", the fixture byte form.
func (d Doc) CanonicalIndent() []byte { return append(emit(d.root, true), '\n') }

// Sanitize validates raw with DefaultLimits.
func Sanitize(raw []byte) (Doc, error) { return SanitizeWith(raw, DefaultLimits()) }

// SanitizeWith validates raw against l. A top-level value that is not an
// object is reported as CodeInvalidJSON. The only error type returned is
// *ValidationError.
func SanitizeWith(raw []byte, l Limits) (Doc, error) {
	l = l.withDefaults()
	if len(raw) > l.MaxDocBytes {
		return Doc{}, &ValidationError{Code: CodeTooLarge, Message: fmt.Sprintf("document exceeds %d bytes", l.MaxDocBytes)}
	}
	tree, verr := parseJSON(raw)
	if verr != nil {
		return Doc{}, verr
	}
	root, ok := tree.(*object)
	if !ok {
		return Doc{}, &ValidationError{Code: CodeInvalidJSON, Message: "top-level value must be an object"}
	}
	v := &validator{
		l:       l,
		ids:     map[string]idInfo{},
		useEdge: map[string][]string{},
	}
	if verr := v.run(root); verr != nil {
		return Doc{}, verr
	}
	// Strip unknown top-level keys, preserving the order of the rest.
	out := newObject()
	for _, k := range root.keys {
		if topLevelAllowed(k) {
			out.set(k, root.vals[k])
		}
	}
	return Doc{root: out, layers: v.layerCount, tracks: v.trackCount}, nil
}

func topLevelAllowed(k string) bool {
	for _, a := range knownTopLevelKeys {
		if a == k {
			return true
		}
	}
	for _, a := range AdditiveTopLevelKeys {
		if a == k {
			return true
		}
	}
	return false
}

// idInfo records what an id names: "layer" (with layerType), or a def kind
// ("gradient", "mask", "clipPath", "filter", "symbol").
type idInfo struct {
	kind      string
	layerType string
}

// pendingRef is a reference resolved after the whole doc has been walked, so
// forward references work.
type pendingRef struct {
	path string
	kind string // gradient, mask, clipPath, filter, use, any, path
	id   string
}

type validator struct {
	l          Limits
	ids        map[string]idInfo
	refs       []pendingRef
	useEdge    map[string][]string // referable id -> use targets in its subtree
	layerCount int
	trackCount int
}

func fail(code, path, format string, args ...any) *ValidationError {
	return &ValidationError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

func (v *validator) run(root *object) *ValidationError {
	// version
	ver, ok := root.get("version")
	if !ok {
		return fail(CodeUnsupportedVersion, "version", "version is required")
	}
	if n, isNum := ver.(numberT); !isNum || !isOne(n) {
		return fail(CodeUnsupportedVersion, "version", "only version 1 is supported")
	}

	// canvas
	cv, ok := root.get("canvas")
	if !ok {
		return fail(CodeInvalidDoc, "canvas", "canvas is required")
	}
	canvas, ok := cv.(*object)
	if !ok {
		return fail(CodeInvalidDoc, "canvas", "canvas must be an object")
	}
	for _, k := range []string{"width", "height"} {
		f, present, err := numField(canvas, k, "canvas."+k)
		if err != nil {
			return err
		}
		if !present {
			return fail(CodeInvalidDoc, "canvas."+k, "%s is required", k)
		}
		if f <= 0 || f > 16384 {
			return fail(CodeInvalidDoc, "canvas."+k, "%s must be > 0 and <= 16384", k)
		}
	}
	if vb, ok := canvas.get("viewBox"); ok {
		switch t := vb.(type) {
		case string:
		case []any:
			if len(t) != 4 {
				return fail(CodeInvalidDoc, "canvas.viewBox", "viewBox array must have 4 numbers")
			}
			for i, e := range t {
				if _, isNum := e.(numberT); !isNum {
					return fail(CodeInvalidDoc, fmt.Sprintf("canvas.viewBox[%d]", i), "must be a number")
				}
			}
		default:
			return fail(CodeInvalidDoc, "canvas.viewBox", "viewBox must be a string or 4 numbers")
		}
	}
	if err := strField(canvas, "background", "canvas.background"); err != nil {
		return err
	}

	// duration / fps
	if f, present, err := numField(root, "duration", "duration"); err != nil {
		return err
	} else if present && (f < 0 || f > 3600000) {
		return fail(CodeInvalidDoc, "duration", "duration must be within 0..3600000 ms")
	}
	if f, present, err := numField(root, "fps", "fps"); err != nil {
		return err
	} else if present && (f <= 0 || f > 240) {
		return fail(CodeInvalidDoc, "fps", "fps must be > 0 and <= 240")
	}

	// layers
	lv, ok := root.get("layers")
	if !ok {
		return fail(CodeInvalidDoc, "layers", "layers is required")
	}
	layers, ok := lv.([]any)
	if !ok {
		return fail(CodeInvalidDoc, "layers", "layers must be an array")
	}
	if err := v.walkLayers(layers, "layers", 1, nil); err != nil {
		return err
	}

	// defs
	if dv, ok := root.get("defs"); ok {
		if err := v.walkDefs(dv); err != nil {
			return err
		}
	}

	// animations
	var tracks []any
	if av, ok := root.get("animations"); ok {
		var isArr bool
		if tracks, isArr = av.([]any); !isArr {
			return fail(CodeInvalidDoc, "animations", "animations must be an array")
		}
	}
	// Tracks resolve against the full id namespace, which is complete now.
	for i, tv := range tracks {
		if err := v.walkTrack(tv, fmt.Sprintf("animations[%d]", i)); err != nil {
			return err
		}
	}
	v.trackCount = len(tracks)

	// Resolve references in the order they were found.
	for _, r := range v.refs {
		if err := v.resolve(r); err != nil {
			return err
		}
	}
	return v.checkCycles()
}

// isOne reports whether n is the integer 1.
func isOne(n numberT) bool {
	f, err := n.Float64()
	return err == nil && f == 1
}

func (v *validator) resolve(r pendingRef) *ValidationError {
	info, ok := v.ids[r.id]
	good := false
	if ok {
		switch r.kind {
		case "use":
			good = info.kind == "layer" || info.kind == "symbol"
		case "any":
			good = true
		case "path":
			good = info.kind == "layer" && info.layerType == "path"
		default:
			good = info.kind == r.kind
		}
	}
	if !good {
		return fail(CodeDanglingRef, r.path, "%q does not resolve to a %s", r.id, refDescription(r.kind))
	}
	return nil
}

func refDescription(kind string) string {
	switch kind {
	case "use":
		return "layer or symbol"
	case "any":
		return "layer or def"
	case "path":
		return "path layer"
	}
	return kind
}

func (v *validator) declare(id, path string, info idInfo) *ValidationError {
	if _, dup := v.ids[id]; dup {
		return fail(CodeDuplicateID, path, "duplicate id %q", id)
	}
	v.ids[id] = info
	return nil
}

// checkID validates a required id field and returns it.
func checkID(o *object, path string) (string, *ValidationError) {
	iv, ok := o.get("id")
	if !ok {
		return "", fail(CodeInvalidDoc, path+".id", "id is required")
	}
	s, ok := iv.(string)
	if !ok || s == "" {
		return "", fail(CodeInvalidDoc, path+".id", "id must be a non-empty string")
	}
	if len(s) > 256 {
		return "", fail(CodeInvalidDoc, path+".id", "id must be at most 256 bytes")
	}
	return s, nil
}

// walkLayers validates a layer array. depth is the depth of these layers (a
// top-level layer is 1); owners are the referable ids enclosing the array,
// which gain a use edge for every use found inside.
func (v *validator) walkLayers(arr []any, path string, depth int, owners []string) *ValidationError {
	for i, lv := range arr {
		p := fmt.Sprintf("%s[%d]", path, i)
		if depth > v.l.MaxDepth {
			return fail(CodeTooDeep, p, "layer tree deeper than %d", v.l.MaxDepth)
		}
		v.layerCount++
		if v.layerCount > v.l.MaxLayers {
			return fail(CodeTooManyLayers, p, "more than %d layers", v.l.MaxLayers)
		}
		if err := v.walkLayer(lv, p, depth, owners); err != nil {
			return err
		}
	}
	return nil
}

func (v *validator) walkLayer(lv any, p string, depth int, owners []string) *ValidationError {
	o, ok := lv.(*object)
	if !ok {
		return fail(CodeInvalidDoc, p, "layer must be an object")
	}
	id, err := checkID(o, p)
	if err != nil {
		return err
	}
	tv, ok := o.get("type")
	typ, isStr := tv.(string)
	if !ok || !isStr || !layerTypes[typ] {
		return fail(CodeInvalidDoc, p+".type", "type must be one of the known layer types")
	}
	if err := v.declare(id, p+".id", idInfo{kind: "layer", layerType: typ}); err != nil {
		return err
	}

	// common fields
	if err := strField(o, "name", p+".name"); err != nil {
		return err
	}
	for _, k := range []string{"visible", "locked"} {
		if x, ok := o.get(k); ok {
			if _, isB := x.(bool); !isB {
				return fail(CodeInvalidDoc, p+"."+k, "%s must be a boolean", k)
			}
		}
	}
	for _, k := range []string{"opacity", "strokeWidth"} {
		if _, _, err := numField(o, k, p+"."+k); err != nil {
			return err
		}
	}
	for _, k := range []string{"fill", "stroke"} {
		if err := strField(o, k, p+"."+k); err != nil {
			return err
		}
		if s, _ := o.vals[k].(string); s != "" {
			if m := urlRef.FindStringSubmatch(s); m != nil {
				v.refs = append(v.refs, pendingRef{p + "." + k, "gradient", m[1]})
			}
		}
	}
	if tr, ok := o.get("transform"); ok {
		to, isObj := tr.(*object)
		if !isObj {
			return fail(CodeInvalidDoc, p+".transform", "transform must be an object")
		}
		for _, k := range []string{"x", "y", "scaleX", "scaleY", "rotation", "skewX", "skewY", "anchorX", "anchorY"} {
			if _, _, err := numField(to, k, p+".transform."+k); err != nil {
				return err
			}
		}
	}
	for _, rk := range [][2]string{{"mask", "mask"}, {"clipPath", "clipPath"}, {"filter", "filter"}} {
		if err := strField(o, rk[0], p+"."+rk[0]); err != nil {
			return err
		}
		if s, _ := o.vals[rk[0]].(string); strings.TrimSpace(s) != "" {
			v.refs = append(v.refs, pendingRef{p + "." + rk[0], rk[1], refTarget(s)})
		}
	}

	// type-specific fields
	nums := func(keys ...string) *ValidationError {
		for _, k := range keys {
			if _, _, err := numField(o, k, p+"."+k); err != nil {
				return err
			}
		}
		return nil
	}
	switch typ {
	case "group":
		if cv, ok := o.get("children"); ok {
			children, isArr := cv.([]any)
			if !isArr {
				return fail(CodeInvalidDoc, p+".children", "children must be an array")
			}
			return v.walkLayers(children, p+".children", depth+1, append(owners[:len(owners):len(owners)], id))
		}
	case "path":
		return reqStr(o, "d", p+".d")
	case "rect":
		return nums("x", "y", "width", "height", "rx", "ry")
	case "ellipse":
		return nums("cx", "cy", "rx", "ry")
	case "line":
		return nums("x1", "y1", "x2", "y2")
	case "polyline", "polygon":
		if pv, ok := o.get("points"); ok {
			switch t := pv.(type) {
			case string:
			case []any:
				for i, e := range t {
					if _, isNum := e.(numberT); !isNum {
						return fail(CodeInvalidDoc, fmt.Sprintf("%s.points[%d]", p, i), "must be a number")
					}
				}
			default:
				return fail(CodeInvalidDoc, p+".points", "points must be a string or an array of numbers")
			}
		}
	case "text":
		if err := reqStr(o, "text", p+".text"); err != nil {
			return err
		}
		if err := nums("x", "y", "fontSize"); err != nil {
			return err
		}
		for _, k := range []string{"fontFamily", "fontWeight", "textAnchor"} {
			if err := strField(o, k, p+"."+k); err != nil {
				return err
			}
		}
	case "image":
		if err := reqStr(o, "href", p+".href"); err != nil {
			return err
		}
		if !safeImageHref(o.vals["href"].(string)) {
			return fail(CodeUnsafeContent, p+".href", "image href is not allowed")
		}
	case "use":
		if err := reqStr(o, "href", p+".href"); err != nil {
			return err
		}
		href := o.vals["href"].(string)
		if !strings.HasPrefix(href, "#") {
			return fail(CodeUnsafeContent, p+".href", "use href must be #<id>")
		}
		target := href[1:]
		v.refs = append(v.refs, pendingRef{p + ".href", "use", target})
		for _, ow := range append(owners[:len(owners):len(owners)], id) {
			v.useEdge[ow] = append(v.useEdge[ow], target)
		}
	case "raw":
		if err := reqStr(o, "markup", p+".markup"); err != nil {
			return err
		}
		return v.checkRaw(o.vals["markup"].(string), p+".markup")
	}
	return nil
}

// refTarget extracts the id from "X", "#X" or "url(#X)".
func refTarget(s string) string {
	s = strings.TrimSpace(s)
	if m := urlRef.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return strings.TrimPrefix(s, "#")
}

// safeImageHref allows data:image/*, asset:, and scheme-less non-protocol-relative URLs.
func safeImageHref(h string) bool {
	for _, r := range h {
		if r < 0x20 || r == 0x7f {
			return false // control chars are stripped by browsers; refuse the trick
		}
	}
	h = strings.TrimSpace(h)
	lower := strings.ToLower(h)
	switch {
	case strings.HasPrefix(lower, "data:image/"), strings.HasPrefix(lower, "asset:"):
		return true
	case strings.HasPrefix(h, "//"), strings.HasPrefix(h, `\`):
		return false
	}
	return !uriScheme.MatchString(h)
}

// checkRaw rejects active or external content in raw layer markup.
func (v *validator) checkRaw(m, path string) *ValidationError {
	if len(m) > v.l.MaxRawBytes {
		return fail(CodeTooLarge, path, "raw markup exceeds %d bytes", v.l.MaxRawBytes)
	}
	unsafe := func(msg string) *ValidationError { return fail(CodeUnsafeContent, path, "%s", msg) }
	if strings.Contains(strings.ToLower(m), "<script") || rawActiveTag.MatchString(m) {
		return unsafe("script, embedding and foreignObject elements are not allowed")
	}
	if rawDTD.MatchString(m) {
		return unsafe("DOCTYPE and ENTITY declarations are not allowed")
	}
	for _, mm := range rawSrcAttr.FindAllStringSubmatch(m, -1) {
		if lv := strings.ToLower(strings.TrimSpace(mm[1] + mm[2] + mm[3])); lv != "" && !strings.HasPrefix(lv, "data:image/") {
			return unsafe("external src is not allowed")
		}
	}
	if rawEventAttr.MatchString(m) {
		return unsafe("event-handler attributes are not allowed")
	}
	for _, mm := range rawHrefAttr.FindAllStringSubmatch(m, -1) {
		val := strings.TrimSpace(mm[1] + mm[2] + mm[3])
		lv := strings.ToLower(val)
		if val == "" || strings.HasPrefix(val, "#") || strings.HasPrefix(lv, "data:image/") {
			continue
		}
		return unsafe("external or unsafe href is not allowed")
	}
	if rawJSURL.MatchString(m) {
		return unsafe("javascript: URLs are not allowed")
	}
	if rawCSSURL.MatchString(m) {
		return unsafe("external CSS url() is not allowed")
	}
	if rawImport.MatchString(m) {
		return unsafe("@import is not allowed")
	}
	return nil
}

func (v *validator) walkDefs(dv any) *ValidationError {
	defs, ok := dv.(*object)
	if !ok {
		return fail(CodeInvalidDoc, "defs", "defs must be an object")
	}
	// Fixed order so error reporting is deterministic.
	for _, kind := range []string{"gradients", "masks", "clipPaths", "filters", "symbols"} {
		av, ok := defs.get(kind)
		if !ok {
			continue
		}
		arr, ok := av.([]any)
		if !ok {
			return fail(CodeInvalidDoc, "defs."+kind, "%s must be an array", kind)
		}
		for i, ev := range arr {
			p := fmt.Sprintf("defs.%s[%d]", kind, i)
			e, ok := ev.(*object)
			if !ok {
				return fail(CodeInvalidDoc, p, "entry must be an object")
			}
			id, err := checkID(e, p)
			if err != nil {
				return err
			}
			// kind without the plural "s": gradients -> gradient, clipPaths -> clipPath
			single := strings.TrimSuffix(kind, "s")
			if err := v.declare(id, p+".id", idInfo{kind: single}); err != nil {
				return err
			}
			switch kind {
			case "gradients":
				if err := v.checkGradient(e, p); err != nil {
					return err
				}
			case "filters":
				if _, _, err := numField(e, "blur", p+".blur"); err != nil {
					return err
				}
			default: // masks, clipPaths, symbols
				if cv, ok := e.get("children"); ok {
					children, isArr := cv.([]any)
					if !isArr {
						return fail(CodeInvalidDoc, p+".children", "children must be an array")
					}
					if err := v.walkLayers(children, p+".children", 1, []string{id}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (v *validator) checkGradient(e *object, p string) *ValidationError {
	if kv, ok := e.get("kind"); ok {
		if s, isStr := kv.(string); !isStr || (s != "linear" && s != "radial") {
			return fail(CodeInvalidDoc, p+".kind", "kind must be linear or radial")
		}
	}
	if sv, ok := e.get("stops"); ok {
		stops, isArr := sv.([]any)
		if !isArr {
			return fail(CodeInvalidDoc, p+".stops", "stops must be an array")
		}
		for i, st := range stops {
			sp := fmt.Sprintf("%s.stops[%d]", p, i)
			so, isObj := st.(*object)
			if !isObj {
				return fail(CodeInvalidDoc, sp, "stop must be an object")
			}
			if _, _, err := numField(so, "offset", sp+".offset"); err != nil {
				return err
			}
			if err := strField(so, "color", sp+".color"); err != nil {
				return err
			}
		}
	}
	if hv, ok := e.get("href"); ok {
		s, isStr := hv.(string)
		if !isStr || !strings.HasPrefix(s, "#") {
			return fail(CodeInvalidDoc, p+".href", "href must be #<gradient id>")
		}
		v.refs = append(v.refs, pendingRef{p + ".href", "gradient", s[1:]})
	}
	return nil
}

func (v *validator) walkTrack(tv any, p string) *ValidationError {
	t, ok := tv.(*object)
	if !ok {
		return fail(CodeInvalidDoc, p, "track must be an object")
	}
	if err := reqStr(t, "layerId", p+".layerId"); err != nil {
		return err
	}
	v.refs = append(v.refs, pendingRef{p + ".layerId", "any", t.vals["layerId"].(string)})
	if err := reqStr(t, "property", p+".property"); err != nil {
		return err
	}
	prop := t.vals["property"].(string)
	if !trackProperties[prop] && !gradientStopProp.MatchString(prop) {
		return fail(CodeInvalidDoc, p+".property", "unknown animatable property %q", prop)
	}
	kv, ok := t.get("keyframes")
	if !ok {
		return fail(CodeInvalidDoc, p+".keyframes", "keyframes is required")
	}
	kfs, ok := kv.([]any)
	if !ok {
		return fail(CodeInvalidDoc, p+".keyframes", "keyframes must be an array")
	}
	if len(kfs) > v.l.MaxKeyframesPerTrack {
		return fail(CodeTooManyKeyframes, p+".keyframes", "more than %d keyframes", v.l.MaxKeyframesPerTrack)
	}
	for i, kf := range kfs {
		kp := fmt.Sprintf("%s.keyframes[%d]", p, i)
		ko, ok := kf.(*object)
		if !ok {
			return fail(CodeInvalidDoc, kp, "keyframe must be an object")
		}
		f, present, err := numField(ko, "t", kp+".t")
		if err != nil {
			return err
		}
		if !present || f < 0 {
			return fail(CodeInvalidDoc, kp+".t", "t is required and must be >= 0")
		}
		if _, ok := ko.get("value"); !ok {
			return fail(CodeInvalidDoc, kp+".value", "value is required")
		}
		if ev, ok := ko.get("easing"); ok {
			if !validEasing(ev) {
				return fail(CodeInvalidDoc, kp+".easing", "invalid easing")
			}
		}
		if xv, ok := ko.get("extras"); ok {
			xo, isObj := xv.(*object)
			if !isObj {
				return fail(CodeInvalidDoc, kp+".extras", "extras must be an object")
			}
			if pv, ok := xo.get("pathId"); ok {
				ps, isStr := pv.(string)
				if !isStr {
					return fail(CodeInvalidDoc, kp+".extras.pathId", "pathId must be a string")
				}
				v.refs = append(v.refs, pendingRef{kp + ".extras.pathId", "path", ps})
			}
		}
	}
	return nil
}

func validEasing(e any) bool {
	switch t := e.(type) {
	case string:
		return easingNames[t]
	case *object:
		cb, ok := t.get("cubicBezier")
		if !ok {
			return false
		}
		arr, ok := cb.([]any)
		if !ok || len(arr) != 4 {
			return false
		}
		for i, x := range arr {
			n, isNum := x.(numberT)
			if !isNum {
				return false
			}
			f, _ := n.Float64()
			if (i == 0 || i == 2) && (f < 0 || f > 1) {
				return false
			}
		}
		return true
	}
	return false
}

// checkCycles finds any cycle in the use graph (layer/symbol id -> use targets
// inside its subtree). Iterates in sorted-free, deterministic order via the
// first-seen id order of the edge list keys.
func (v *validator) checkCycles() *ValidationError {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = grey
		for _, t := range v.useEdge[id] {
			switch color[t] {
			case grey:
				return true
			case white:
				if visit(t) {
					return true
				}
			}
		}
		color[id] = black
		return false
	}
	for _, id := range v.edgeOrder() {
		if color[id] == white && visit(id) {
			return fail(CodeUseCycle, "", "use references form a cycle through %q", id)
		}
	}
	return nil
}

// edgeOrder returns edge-source ids in a stable order (sorted) so the reported
// id in a cycle message is deterministic.
func (v *validator) edgeOrder() []string {
	keys := make([]string, 0, len(v.useEdge))
	for k := range v.useEdge {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

// --- small typed-field helpers ---

func numField(o *object, k, path string) (float64, bool, *ValidationError) {
	x, ok := o.get(k)
	if !ok {
		return 0, false, nil
	}
	n, isNum := x.(numberT)
	if !isNum {
		return 0, true, fail(CodeInvalidDoc, path, "%s must be a number", k)
	}
	f, _ := n.Float64()
	return f, true, nil
}

func strField(o *object, k, path string) *ValidationError {
	if x, ok := o.get(k); ok {
		if _, isStr := x.(string); !isStr {
			return fail(CodeInvalidDoc, path, "%s must be a string", k)
		}
	}
	return nil
}

func reqStr(o *object, k, path string) *ValidationError {
	x, ok := o.get(k)
	if !ok {
		return fail(CodeInvalidDoc, path, "%s is required", k)
	}
	if _, isStr := x.(string); !isStr {
		return fail(CodeInvalidDoc, path, "%s must be a string", k)
	}
	return nil
}

func sortStrings(s []string) { sort.Strings(s) }
