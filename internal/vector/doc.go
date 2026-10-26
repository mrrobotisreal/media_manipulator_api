// Package vector validates and sanitizes "Motion" documents (code name
// vector): the VectorDoc v1 JSON the browser editor produces.
//
// Contract with the UI: this package is the Go mirror of the Zod model in
// media-manipulator-ui/lib/vector/vectorTypes.ts (MM42). Both sides are pinned
// by the same fixture documents (testdata/, byte-identical copies of
// lib/vector/fixtures/, see testdata/README.md).
//
// Sanitize validates AND REBUILDS the document the way Zod v3 does: for every
// object schema the known keys are emitted in schema (shape) order, defaults
// are filled, absent optional keys are omitted, and unknown keys pass through
// after the known ones in input order (the additive-only rule). Canonical()
// therefore equals the UI's serializeVectorDoc output byte for byte.
//
// The server adds hardening beyond the client rules (see rawMarkupIssues):
// raw layer markup is held to an element allow-list and several extra bans
// after the MM42 security review found an XSS through foreignObject.
package vector

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Limits bound one document. Zero fields fall back to DefaultLimits values.
type Limits struct {
	MaxDocBytes          int // raw input length cap
	MaxDepth             int // layer-tree nesting depth (top-level tree is depth 1; mask/clip/symbol layers start at 2)
	MaxLayers            int // total layers in the doc, mask/clip/symbol trees and raw layers included
	MaxKeyframesPerTrack int
	MaxRawBytes          int // max length of one raw layer's markup
}

// DefaultLimits returns the production limits (== vectorTypes.ts VECTOR_MAX_*).
func DefaultLimits() Limits {
	return Limits{
		MaxDocBytes:          4 << 20,
		MaxDepth:             32,
		MaxLayers:            5000,
		MaxKeyframesPerTrack: 2000,
		MaxRawBytes:          1 << 20,
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
	CodeInvalidDoc         = "invalid_doc" // wrong type / missing required field / bad enum / pattern
	CodeDuplicateID        = "duplicate_id"
	CodeDanglingRef        = "dangling_ref"
	CodeUseCycle           = "use_cycle"
	CodeTooDeep            = "too_deep"
	CodeTooManyLayers      = "too_many_layers"
	CodeTooManyKeyframes   = "too_many_keyframes"
	CodeUnsafeContent      = "unsafe_content" // raw markup / paint / attrs / href security failures
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

func fail(code, path, format string, args ...any) *ValidationError {
	return &ValidationError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

// Doc is a validated, sanitized VectorDoc.
type Doc struct {
	root   *object
	layers int
	tracks int
}

// Version returns the document version (always 1 for a valid Doc).
func (d Doc) Version() int { return 1 }

// LayerCount is the total layers, including mask/clip/symbol child trees.
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
		return Doc{}, fail(CodeTooLarge, "", "document exceeds %d bytes", l.MaxDocBytes)
	}
	tree, verr := parseJSON(raw)
	if verr != nil {
		return Doc{}, verr
	}
	root, ok := tree.(*object)
	if !ok {
		return Doc{}, fail(CodeInvalidJSON, "", "top-level value must be an object")
	}
	// z.literal(1) comes first.
	ver, ok := root.get("version")
	if !ok {
		return Doc{}, fail(CodeUnsupportedVersion, "version", "version is required")
	}
	if n, isNum := ver.(numberT); !isNum || !isOne(n) {
		return Doc{}, fail(CodeUnsupportedVersion, "version", "only version 1 is supported")
	}
	c := &ctx{l: l}
	out, verr2 := docSpec.build(c, "", root)
	if verr2 != nil {
		return Doc{}, verr2
	}
	layers, verr3 := docIntegrity(out, l)
	if verr3 != nil {
		return Doc{}, verr3
	}
	tracks := 0
	if a, ok := out.get("animations"); ok {
		tracks = len(a.([]any))
	}
	return Doc{root: out, layers: layers, tracks: tracks}, nil
}

// ---------------------------------------------------------------------------
// Declarative schema engine (mirrors Zod v3 object parsing)
// ---------------------------------------------------------------------------

// Limits mirrored from vectorTypes.ts that are not configurable through Limits.
const (
	maxImageHrefBytes = 4 << 20 // VECTOR_MAX_DOC_BYTES, the image href cap
	maxRawIDs         = 5000    // VECTOR_MAX_LAYERS, the raw layer `ids` cap
	maxPoints         = 200000
	maxPathBytes      = 2000000
)

// ctx carries the per-run limits through the schema walk.
type ctx struct{ l Limits }

// vfn validates one value and returns the value to emit (rebuilt for objects
// and arrays, so output key order follows the schema).
type vfn func(c *ctx, path string, v any) (any, *ValidationError)

// field is one entry of an object shape, in schema order.
type field struct {
	name string
	opt  bool                // optional: omitted from the output when absent
	def  func(o *object) any // .default(): value (pre-validation) used when absent; o is the output built so far
	v    vfn
}

// spec is a Zod `.passthrough()` object: known fields in shape order, then
// unknown input keys in input order.
type spec struct{ fields []field }

func req(name string, v vfn) field { return field{name: name, v: v} }
func opt(name string, v vfn) field { return field{name: name, opt: true, v: v} }
func dflt(name string, d func(o *object) any, v vfn) field {
	return field{name: name, def: d, v: v}
}
func constDef(x any) func(*object) any { return func(*object) any { return x } }

func sub(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
func idx(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

func (s *spec) build(c *ctx, path string, v any) (*object, *ValidationError) {
	in, ok := v.(*object)
	if !ok {
		return nil, fail(CodeInvalidDoc, path, "expected an object")
	}
	out := newObject()
	known := make(map[string]bool, len(s.fields))
	for _, f := range s.fields {
		known[f.name] = true
		val, present := in.get(f.name)
		if !present {
			if f.def != nil {
				val = f.def(out)
			} else if f.opt {
				continue
			} else {
				return nil, fail(CodeInvalidDoc, sub(path, f.name), "%s is required", f.name)
			}
		}
		r, err := f.v(c, sub(path, f.name), val)
		if err != nil {
			return nil, err
		}
		out.set(f.name, r)
	}
	for _, k := range in.keys {
		if !known[k] {
			out.set(k, in.vals[k])
		}
	}
	return out, nil
}

func (s *spec) vfn() vfn {
	return func(c *ctx, path string, v any) (any, *ValidationError) {
		o, err := s.build(c, path, v)
		if err != nil {
			return nil, err
		}
		return o, nil
	}
}

func isOne(n numberT) bool {
	f, err := strconv.ParseFloat(string(n), 64)
	return err == nil && f == 1
}

func asFloat(v any) (float64, bool) {
	n, ok := v.(numberT)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// numV is z.number().finite() with optional bounds. exLo makes lo exclusive
// (.positive()); integer adds .int().
func numV(lo, hi float64, exLo, integer bool) vfn {
	return func(_ *ctx, path string, v any) (any, *ValidationError) {
		f, ok := asFloat(v)
		if !ok {
			return nil, fail(CodeInvalidDoc, path, "expected a finite number")
		}
		if integer && f != math.Trunc(f) {
			return nil, fail(CodeInvalidDoc, path, "expected an integer")
		}
		if f < lo || (exLo && f == lo) || f > hi {
			return nil, fail(CodeInvalidDoc, path, "number out of range")
		}
		return v, nil
	}
}

var (
	inf     = math.Inf(1)
	vNum    = numV(-inf, inf, false, false)
	vNonNeg = numV(0, inf, false, false)
	vPos    = numV(0, inf, true, false)
	vUnit   = numV(0, 1, false, false)
)

func vBool(_ *ctx, path string, v any) (any, *ValidationError) {
	if _, ok := v.(bool); !ok {
		return nil, fail(CodeInvalidDoc, path, "expected a boolean")
	}
	return v, nil
}

// utf16Len is the JavaScript string length, which Zod's .max() measures.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// strV is z.string().max(n) with an optional extra check.
func strV(max int, check func(path, s string) *ValidationError) vfn {
	return func(_ *ctx, path string, v any) (any, *ValidationError) {
		s, ok := v.(string)
		if !ok {
			return nil, fail(CodeInvalidDoc, path, "expected a string")
		}
		if max >= 0 && utf16Len(s) > max {
			return nil, fail(CodeInvalidDoc, path, "string longer than %d", max)
		}
		if check != nil {
			if err := check(path, s); err != nil {
				return nil, err
			}
		}
		return v, nil
	}
}

func enumV(vals ...string) vfn {
	return func(_ *ctx, path string, v any) (any, *ValidationError) {
		s, ok := v.(string)
		if ok {
			for _, e := range vals {
				if s == e {
					return v, nil
				}
			}
		}
		return nil, fail(CodeInvalidDoc, path, "expected one of %s", strings.Join(vals, ", "))
	}
}

func litV(lit string) vfn { return enumV(lit) }

// arrV is z.array(elem).max(max); max < 0 means unbounded.
func arrV(elem vfn, max int) vfn {
	return func(c *ctx, path string, v any) (any, *ValidationError) {
		a, ok := v.([]any)
		if !ok {
			return nil, fail(CodeInvalidDoc, path, "expected an array")
		}
		if max >= 0 && len(a) > max {
			return nil, fail(CodeInvalidDoc, path, "array longer than %d", max)
		}
		out := make([]any, len(a))
		for i, e := range a {
			r, err := elem(c, idx(path, i), e)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
}

// tupleV is z.tuple([...]).
func tupleV(elems ...vfn) vfn {
	return func(c *ctx, path string, v any) (any, *ValidationError) {
		a, ok := v.([]any)
		if !ok || len(a) != len(elems) {
			return nil, fail(CodeInvalidDoc, path, "expected an array of %d items", len(elems))
		}
		out := make([]any, len(a))
		for i, e := range a {
			r, err := elems[i](c, idx(path, i), e)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
}

// anyV is z.unknown().
func anyV(_ *ctx, _ string, v any) (any, *ValidationError) { return v, nil }

// ---- ids, colours, paints ---------------------------------------------------

// idRE is VECTOR_ID_PATTERN /^[A-Za-z_][\w-]*$/ (JS \w is ASCII, as in RE2;
// Go's $ without (?m) matches only at the end of text, like JS).
var idRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

var vID = strV(128, func(path, s string) *ValidationError {
	if !idRE.MatchString(s) {
		return fail(CodeInvalidDoc, path, "invalid id")
	}
	return nil
})

// isJSSpace is the JavaScript `\s` class (RE2's \s is ASCII-only).
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// jsS is the same class for use inside RE2 character classes.
const jsS = `\s\x0B\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

func hasURLParen(s string) bool { return strings.Contains(strings.ToLower(s), "url(") }

// colorBody reports whether s is non-empty and entirely made of the
// COLOR_PATTERN character class [#A-Za-z0-9(),.%\s/+-] and contains no "url(".
//
// JS translation: the pattern is /^(?!.*url\()[...]+$/i. RE2 has no lookahead,
// so the lookahead is the explicit hasURLParen check. In JS `.` stops at line
// terminators, so the lookahead only scans the first line; we scan the whole
// string, which is strictly stricter (a later-line "url(" is rejected here).
func colorBody(s string) bool {
	if s == "" || hasURLParen(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("#(),.%/+-", r):
		case isJSSpace(r):
		default:
			return false
		}
	}
	return true
}

// unsafeValue mirrors UNSAFE_VALUE / UNSAFE_ATTR_VALUE (case-insensitive),
// plus data:text/html. JS translation of `url\(\s*['"]?(?!#)`: because \s* and
// ['"]? can backtrack to zero width, the lookahead is satisfied unless the
// character IMMEDIATELY after "url(" is '#', so we test exactly that.
func unsafeValue(s string) bool {
	l := strings.ToLower(s)
	for _, bad := range []string{"javascript:", "vbscript:", "data:text/html", "expression(", "@import", "image-set("} {
		if strings.Contains(l, bad) {
			return true
		}
	}
	if strings.Contains(s, `\`) {
		return true
	}
	for from := 0; ; {
		i := strings.Index(l[from:], "url(")
		if i < 0 {
			return false
		}
		next := from + i + 4
		if next >= len(l) || l[next] != '#' {
			return true
		}
		from = next
	}
}

// isSafeColor is COLOR_PATTERN applied to the trimmed value (length <= 128).
func isSafeColor(v string) bool {
	return utf16Len(v) <= 128 && colorBody(jsTrim(v))
}

// isSafePaint is PAINT_PATTERN applied to the trimmed value (length <= 256):
//
//	^( url\(\s*#id\s*\) (\s+ BODY)?  |  BODY )$     (i flag)
//
// where BODY is colorBody. The per-alternative lookahead (?!.*url\() is the
// hasURLParen check inside colorBody.
func isSafePaint(v string) bool {
	if utf16Len(v) > 256 {
		return false
	}
	t := jsTrim(v)
	if colorBody(t) {
		return true
	}
	if len(t) < 4 || !strings.EqualFold(t[:4], "url(") {
		return false
	}
	rest := strings.TrimLeftFunc(t[4:], isJSSpace)
	if !strings.HasPrefix(rest, "#") {
		return false
	}
	rest = rest[1:]
	n := 0
	for n < len(rest) && (rest[n] == '_' || rest[n] == '-' || (rest[n] >= 'a' && rest[n] <= 'z') || (rest[n] >= 'A' && rest[n] <= 'Z') || (n > 0 && rest[n] >= '0' && rest[n] <= '9')) {
		n++
	}
	if n == 0 || (rest[0] >= '0' && rest[0] <= '9') || rest[0] == '-' {
		return false
	}
	rest = strings.TrimLeftFunc(rest[n:], isJSSpace)
	if !strings.HasPrefix(rest, ")") {
		return false
	}
	rest = rest[1:]
	if rest == "" {
		return true
	}
	r0, size := firstRune(rest)
	if !isJSSpace(r0) {
		return false
	}
	return colorBody(rest[size:])
}

func firstRune(s string) (rune, int) {
	for i, r := range s {
		_ = i
		return r, len(string(r))
	}
	return 0, 0
}

func paintLike(max int, ok func(string) bool) vfn {
	return func(_ *ctx, path string, v any) (any, *ValidationError) {
		s, isStr := v.(string)
		if !isStr || s == "" || utf16Len(s) > max {
			return nil, fail(CodeInvalidDoc, path, "expected a non-empty string of at most %d characters", max)
		}
		// Coordinator hardening on top of the pattern: the unsafe-value rules
		// (backslash, javascript:, url( not followed by '#', ...) are security failures.
		if unsafeValue(s) {
			return nil, fail(CodeUnsafeContent, path, "unsafe value")
		}
		if !ok(s) {
			return nil, fail(CodeInvalidDoc, path, "invalid value")
		}
		return v, nil
	}
}

var (
	vPaint = paintLike(256, isSafePaint)
	vColor = paintLike(128, isSafeColor)
)

// paintRef mirrors PAINT_URL /^url\(\s*#([^)\s]+)\s*\)(?:\s+[^\s()]+)?$/ on the
// trimmed paint (case-sensitive). ok is false for a plain colour.
func paintRef(paint string) (string, bool) {
	t := jsTrim(paint)
	if !strings.HasPrefix(t, "url(") {
		return "", false
	}
	rest := strings.TrimLeftFunc(t[4:], isJSSpace)
	if !strings.HasPrefix(rest, "#") {
		return "", false
	}
	rest = rest[1:]
	end := strings.IndexFunc(rest, func(r rune) bool { return r == ')' || isJSSpace(r) })
	if end <= 0 {
		return "", false
	}
	id := rest[:end]
	rest = strings.TrimLeftFunc(rest[end:], isJSSpace)
	if !strings.HasPrefix(rest, ")") {
		return "", false
	}
	rest = rest[1:]
	if rest == "" {
		return id, true
	}
	r0, size := firstRune(rest)
	if !isJSSpace(r0) {
		return "", false
	}
	tok := strings.TrimLeftFunc(rest[size:], isJSSpace)
	if tok == "" || strings.IndexFunc(tok, func(r rune) bool { return isJSSpace(r) || r == '(' || r == ')' }) >= 0 {
		return "", false
	}
	return id, true
}

// ---- attrs, image href ------------------------------------------------------

// safeAttrs is SAFE_ATTRS copied verbatim from media-manipulator-ui/lib/vector/svgMap.ts.
var safeAttrs = map[string]bool{
	"class": true, "color": true, "display": true, "visibility": true, "paint-order": true,
	"vector-effect": true, "shape-rendering": true, "text-rendering": true, "image-rendering": true,
	"color-interpolation": true, "color-interpolation-filters": true, "color-rendering": true,
	"dominant-baseline": true, "alignment-baseline": true, "baseline-shift": true,
	"font-family": true, "font-size": true, "font-weight": true, "font-style": true,
	"font-variant": true, "font-stretch": true, "font-size-adjust": true, "text-anchor": true,
	"text-decoration": true, "letter-spacing": true, "word-spacing": true, "writing-mode": true,
	"direction": true, "unicode-bidi": true, "mix-blend-mode": true, "isolation": true,
	"pathLength": true, "marker-start": true, "marker-mid": true, "marker-end": true,
	"lengthAdjust": true, "textLength": true, "stroke-dasharray": true, "stroke-width": true,
	"stroke-dashoffset": true, "stroke-miterlimit": true, "opacity": true, "fill-opacity": true,
	"stroke-opacity": true,
}

var (
	attrKeyRE  = regexp.MustCompile(`^[a-z][a-zA-Z0-9-]*$`)
	dataAttrRE = regexp.MustCompile(`^data-[a-z0-9-]+$`)
)

// vAttrs is attrsSchema: z.record(key regex, string<=4096) + the allow-list refine.
func vAttrs(_ *ctx, path string, v any) (any, *ValidationError) {
	in, ok := v.(*object)
	if !ok {
		return nil, fail(CodeInvalidDoc, path, "expected an object")
	}
	for _, k := range in.keys {
		p := sub(path, k)
		if !attrKeyRE.MatchString(k) {
			return nil, fail(CodeInvalidDoc, p, "invalid attribute name")
		}
		s, isStr := in.vals[k].(string)
		if !isStr || utf16Len(s) > 4096 {
			return nil, fail(CodeInvalidDoc, p, "attribute value must be a string of at most 4096 characters")
		}
	}
	for _, k := range in.keys {
		p := sub(path, k)
		if !safeAttrs[k] && !dataAttrRE.MatchString(k) {
			return nil, fail(CodeUnsafeContent, p, "attribute %q is not allow-listed", k)
		}
		if unsafeValue(in.vals[k].(string)) {
			return nil, fail(CodeUnsafeContent, p, "unsafe attribute value")
		}
	}
	return in, nil
}

// vImageHref is IMAGE_HREF_PATTERN (case-sensitive):
// data:image/(png|jpeg|gif|webp|avif);base64,[A-Za-z0-9+/=\s]* | asset:[A-Za-z0-9_-]{1,128}.
func vImageHref(_ *ctx, path string, v any) (any, *ValidationError) {
	s, ok := v.(string)
	if !ok {
		return nil, fail(CodeInvalidDoc, path, "expected a string")
	}
	if len(s) > maxImageHrefBytes {
		return nil, fail(CodeInvalidDoc, path, "href too long")
	}
	good := false
	if strings.HasPrefix(s, "asset:") {
		id := s[6:]
		good = len(id) >= 1 && len(id) <= 128
		for i := 0; good && i < len(id); i++ {
			c := id[i]
			good = c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		}
	} else if strings.HasPrefix(s, "data:image/") {
		rest := s[len("data:image/"):]
		for _, mime := range []string{"png", "jpeg", "gif", "webp", "avif"} {
			if strings.HasPrefix(rest, mime+";base64,") {
				good = true
				for _, r := range rest[len(mime)+8:] {
					if !(r == '+' || r == '/' || r == '=' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || isJSSpace(r)) {
						good = false
						break
					}
				}
				break
			}
		}
	}
	if !good {
		return nil, fail(CodeUnsafeContent, path, "image href must be a base64 raster data URI or asset:<id>")
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Schemas (vectorTypes.ts, field for field, in shape order)
// ---------------------------------------------------------------------------

func optNum(name string) field { return opt(name, vNum) }

var (
	viewBoxSpec = &spec{[]field{req("x", vNum), req("y", vNum), req("width", vPos), req("height", vPos)}}

	transformSpec = &spec{[]field{
		req("x", vNum), req("y", vNum), req("scaleX", vNum), req("scaleY", vNum), req("rotation", vNum),
		req("skewX", vNum), req("skewY", vNum), req("anchorX", vNum), req("anchorY", vNum),
	}}

	vMatrix = tupleV(vNum, vNum, vNum, vNum, vNum, vNum)

	gradientUnits = enumV("objectBoundingBox", "userSpaceOnUse")
	spreadMethod  = enumV("pad", "reflect", "repeat")

	gradientStopSpec = &spec{[]field{req("offset", vUnit), req("color", vColor), opt("opacity", vUnit)}}

	filterRegionRE = regexp.MustCompile(`^-?(\d+\.?\d*|\.\d+)%?$`)
	vFilterRegion  = strV(-1, func(path, s string) *ValidationError {
		if !filterRegionRE.MatchString(s) {
			return fail(CodeInvalidDoc, path, "invalid filter region")
		}
		return nil
	})

	// TRACK_PROPERTY_PATTERN, ported verbatim (no lookaheads in it).
	trackPropertyRE = regexp.MustCompile(`^(transform\.(x|y|scaleX|scaleY|rotation|skewX|skewY|anchorX|anchorY)|opacity|fill|stroke|strokeWidth|strokeDashoffset|d|gradientStop\[(0|[1-9]\d{0,2})\]\.(color|offset)|mask|clipPath|filter\.blur|motionPath)$`)

	cubicBezierSpec = &spec{[]field{req("cubicBezier", tupleV(vUnit, vNum, vUnit, vNum))}}
	vPresetEasing   = enumV("linear", "easeIn", "easeOut", "easeBoth", "hold")

	keyframeSpec  *spec
	trackSpec     *spec
	maskSpec      *spec
	clipPathSpec  *spec
	symbolSpec    *spec
	defsSpec      *spec
	canvasSpec    *spec
	docSpec       *spec
	layerSpecs    map[string]*spec
	gradientSpecs map[string]*spec
	filterSpecs   map[string]*spec
)

// vEasing is z.union([enum, passthrough({cubicBezier})]).
func vEasing(c *ctx, path string, v any) (any, *ValidationError) {
	if _, isStr := v.(string); isStr {
		return vPresetEasing(c, path, v)
	}
	if _, isObj := v.(*object); isObj {
		return cubicBezierSpec.vfn()(c, path, v)
	}
	return nil, fail(CodeInvalidDoc, path, "expected an easing preset or {cubicBezier}")
}

// vKeyValue is z.union([num, string<=2_000_000]).
func vKeyValue(c *ctx, path string, v any) (any, *ValidationError) {
	if _, isStr := v.(string); isStr {
		return strV(maxPathBytes, nil)(c, path, v)
	}
	return vNum(c, path, v)
}

// vRecordAny is z.record(z.string(), z.unknown()).
func vRecordAny(_ *ctx, path string, v any) (any, *ValidationError) {
	if _, ok := v.(*object); !ok {
		return nil, fail(CodeInvalidDoc, path, "expected an object")
	}
	return v, nil
}

func vPoints(_ *ctx, path string, v any) (any, *ValidationError) {
	a, ok := v.([]any)
	if !ok {
		return nil, fail(CodeInvalidDoc, path, "expected an array")
	}
	if len(a) > maxPoints {
		return nil, fail(CodeInvalidDoc, path, "too many points")
	}
	for i, e := range a {
		if _, err := vNum(nil, idx(path, i), e); err != nil {
			return nil, err
		}
	}
	if len(a)%2 != 0 {
		return nil, fail(CodeInvalidDoc, path, "points must hold x,y pairs")
	}
	return v, nil
}

// discV is z.discriminatedUnion(key, ...).
func discV(key string, specs map[string]*spec) vfn {
	return func(c *ctx, path string, v any) (any, *ValidationError) {
		o, ok := v.(*object)
		if !ok {
			return nil, fail(CodeInvalidDoc, path, "expected an object")
		}
		t, _ := o.get(key)
		ts, isStr := t.(string)
		s := specs[ts]
		if !isStr || s == nil {
			return nil, fail(CodeInvalidDoc, sub(path, key), "unknown or missing %s", key)
		}
		return s.vfn()(c, path, v)
	}
}

var vLayers = arrV(layerV, -1)

func layerV(c *ctx, path string, v any) (any, *ValidationError) {
	return discV("type", layerSpecs)(c, path, v)
}

func head() []field {
	return []field{req("id", vID), opt("name", strV(200, nil)), opt("hidden", vBool), opt("locked", vBool)}
}

func tail() []field {
	return []field{
		opt("opacity", vUnit),
		opt("fill", vPaint),
		opt("fillOpacity", vUnit),
		opt("fillRule", enumV("nonzero", "evenodd")),
		opt("stroke", vPaint),
		opt("strokeWidth", vNonNeg),
		opt("strokeOpacity", vUnit),
		opt("strokeLinecap", enumV("butt", "round", "square")),
		opt("strokeLinejoin", enumV("miter", "round", "bevel", "arcs", "miter-clip")),
		opt("strokeMiterlimit", numV(1, inf, false, false)),
		opt("strokeDasharray", arrV(vNonNeg, 64)),
		optNum("strokeDashoffset"),
		opt("clipRule", enumV("nonzero", "evenodd")),
		opt("mask", vID),
		opt("clipPath", vID),
		opt("filter", vID),
		opt("transform", transformSpec.vfn()),
		opt("attrs", vAttrs),
	}
}

func layerSpec(typ string, mid ...field) *spec {
	f := []field{req("type", litV(typ))}
	f = append(f, head()...)
	f = append(f, mid...)
	f = append(f, tail()...)
	return &spec{f}
}

// rawMarkupV checks the length cap (CodeTooLarge, as before) and the shared
// raw-markup safety rules.
func rawMarkupV(c *ctx, path string, v any) (any, *ValidationError) {
	s, ok := v.(string)
	if !ok {
		return nil, fail(CodeInvalidDoc, path, "expected a string")
	}
	if len(s) > c.l.MaxRawBytes {
		return nil, fail(CodeTooLarge, path, "raw markup exceeds %d bytes", c.l.MaxRawBytes)
	}
	if issue := rawMarkupIssue(s); issue != "" {
		return nil, fail(CodeUnsafeContent, path, "unsafe raw markup: %s", issue)
	}
	return v, nil
}

func init() {
	layerSpecs = map[string]*spec{
		"rect": layerSpec("rect", req("x", vNum), req("y", vNum), req("width", vNonNeg), req("height", vNonNeg),
			opt("rx", vNonNeg), opt("ry", vNonNeg)),
		"ellipse":  layerSpec("ellipse", req("cx", vNum), req("cy", vNum), req("rx", vNonNeg), req("ry", vNonNeg)),
		"line":     layerSpec("line", req("x1", vNum), req("y1", vNum), req("x2", vNum), req("y2", vNum)),
		"polyline": layerSpec("polyline", req("points", vPoints)),
		"polygon":  layerSpec("polygon", req("points", vPoints)),
		"path":     layerSpec("path", req("d", strV(maxPathBytes, nil))),
		"text": layerSpec("text", req("x", vNum), req("y", vNum), req("text", strV(10000, nil)),
			opt("fontFamily", strV(256, nil)), opt("fontSize", vPos), opt("fontWeight", strV(32, nil)),
			opt("fontStyle", enumV("normal", "italic", "oblique")), opt("textAnchor", enumV("start", "middle", "end")),
			optNum("letterSpacing")),
		"image": layerSpec("image", req("x", vNum), req("y", vNum), req("width", vNonNeg), req("height", vNonNeg),
			req("href", vImageHref), opt("preserveAspectRatio", strV(64, nil))),
		"use": layerSpec("use", req("ref", vID), optNum("x"), optNum("y"), opt("width", vNonNeg), opt("height", vNonNeg)),
		"raw": {[]field{
			req("type", litV("raw")), req("id", vID), opt("name", strV(200, nil)),
			req("markup", rawMarkupV), opt("ids", arrV(vID, maxRawIDs)),
		}},
	}
	// groupLayerBase.extend({children}): children is LAST.
	g := layerSpec("group")
	g.fields = append(g.fields, req("children", vLayers))
	layerSpecs["group"] = g

	gradientSpecs = map[string]*spec{
		"linear": {[]field{
			req("type", litV("linear")), req("id", vID), opt("units", gradientUnits),
			req("x1", vNum), req("y1", vNum), req("x2", vNum), req("y2", vNum),
			req("stops", arrV(gradientStopSpec.vfn(), 256)), opt("spreadMethod", spreadMethod), opt("gradientTransform", vMatrix),
		}},
		"radial": {[]field{
			req("type", litV("radial")), req("id", vID), opt("units", gradientUnits),
			req("cx", vNum), req("cy", vNum), req("r", vNonNeg), optNum("fx"), optNum("fy"), opt("fr", vNonNeg),
			req("stops", arrV(gradientStopSpec.vfn(), 256)), opt("spreadMethod", spreadMethod), opt("gradientTransform", vMatrix),
		}},
	}
	filterSpecs = map[string]*spec{
		"blur": {[]field{
			req("type", litV("blur")), req("id", vID), req("stdDeviation", vNonNeg),
			opt("x", vFilterRegion), opt("y", vFilterRegion), opt("width", vFilterRegion), opt("height", vFilterRegion),
		}},
	}
	maskSpec = &spec{[]field{
		req("id", vID), opt("units", gradientUnits), opt("contentUnits", gradientUnits),
		optNum("x"), optNum("y"), optNum("width"), optNum("height"), req("layers", vLayers),
	}}
	clipPathSpec = &spec{[]field{
		req("id", vID), opt("units", gradientUnits), opt("transform", vMatrix), req("layers", vLayers),
	}}
	symbolSpec = &spec{[]field{
		req("id", vID), opt("viewBox", viewBoxSpec.vfn()), opt("preserveAspectRatio", strV(64, nil)), req("layers", vLayers),
	}}
	emptyArr := func(*object) any { return []any{} }
	defsSpec = &spec{[]field{
		dflt("gradients", emptyArr, arrV(discV("type", gradientSpecs), -1)),
		dflt("masks", emptyArr, arrV(maskSpec.vfn(), -1)),
		dflt("clipPaths", emptyArr, arrV(clipPathSpec.vfn(), -1)),
		dflt("filters", emptyArr, arrV(discV("type", filterSpecs), -1)),
		dflt("symbols", emptyArr, arrV(symbolSpec.vfn(), -1)),
	}}
	keyframeSpec = &spec{[]field{req("t", vNonNeg), req("value", vKeyValue), req("easing", vEasing)}}
	trackSpec = &spec{[]field{
		req("layerId", vID),
		req("property", strV(-1, func(path, s string) *ValidationError {
			if !trackPropertyRE.MatchString(s) {
				return fail(CodeInvalidDoc, path, "unknown animation property")
			}
			return nil
		})),
		req("keyframes", func(c *ctx, path string, v any) (any, *ValidationError) {
			if a, ok := v.([]any); ok && len(a) > c.l.MaxKeyframesPerTrack {
				return nil, fail(CodeTooManyKeyframes, path, "more than %d keyframes", c.l.MaxKeyframesPerTrack)
			}
			return arrV(keyframeSpec.vfn(), -1)(c, path, v)
		}),
		opt("extras", vRecordAny),
	}}
	num := func(s string) func(*object) any { return func(*object) any { return numberT(s) } }
	canvasSpec = &spec{fields: []field{
		dflt("width", num("512"), vPos),
		dflt("height", num("512"), vPos),
		opt("viewBox", viewBoxSpec.vfn()),
		dflt("background", constDef("transparent"), vColor),
	}}
	docSpec = &spec{[]field{
		req("version", anyV),
		dflt("canvas", func(*object) any { return newObject() }, canvasV),
		dflt("duration", num("3000"), numV(0, 3600000, false, true)),
		dflt("fps", num("24"), numV(0, 120, true, false)),
		dflt("layers", emptyArr, vLayers),
		dflt("defs", func(*object) any { return newObject() }, defsSpec.vfn()),
		dflt("animations", emptyArr, arrV(vTrack, -1)),
		opt("loop", vBool),
	}}
}

// vTrack validates a track, then the per-property rules (MM49): extras of a
// motionPath track and the keyframe value kind per property. Every rule is
// the weakest check that still holds for everything the UI editor, animators
// and importers write.
func vTrack(c *ctx, path string, v any) (any, *ValidationError) {
	r, err := trackSpec.vfn()(c, path, v)
	if err != nil {
		return nil, err
	}
	o := r.(*object)
	prop := str(o, "property")
	if prop == "motionPath" {
		if ex, ok := o.vals["extras"].(*object); ok {
			if e := checkMotionExtras(sub(path, "extras"), ex); e != nil {
				return nil, e
			}
		}
	}
	for i, k := range arr(o, "keyframes") {
		kf := k.(*object)
		if e := checkKeyValue(prop, sub(idx(sub(path, "keyframes"), i), "value"), kf.vals["value"]); e != nil {
			return nil, e
		}
	}
	return r, nil
}

// pathDataClass is [MmLlHhVvCcSsQqTtAaZz0-9eE.,+\-\s]; JS \s is mapped to a
// plain space first because RE2's \s is ASCII-only.
var pathDataClass = regexp.MustCompile(`^[MmLlHhVvCcSsQqTtAaZz0-9eE.,+ -]*$`)

func isPathData(s string) bool {
	return pathDataClass.MatchString(strings.Map(func(r rune) rune {
		if isJSSpace(r) {
			return ' '
		}
		return r
	}, s))
}

// pathId deliberately checks the id pattern only, not that the layer exists:
// the editor does not prune extras when a guide layer is deleted (layerTree
// removeLayers / vectorStore deleteSelection) and motionPathData treats a
// dangling pathId as "no path".
func checkMotionExtras(path string, ex *object) *ValidationError {
	if v, ok := ex.get("pathId"); ok {
		s, isStr := v.(string)
		if !isStr || len(s) > 128 || !idRE.MatchString(s) {
			return fail(CodeInvalidDoc, sub(path, "pathId"), "invalid id")
		}
	}
	if v, ok := ex.get("d"); ok {
		s, isStr := v.(string)
		if !isStr || utf16Len(s) > maxPathBytes || !isPathData(s) {
			return fail(CodeInvalidDoc, sub(path, "d"), "invalid path data")
		}
	}
	if v, ok := ex.get("autoOrient"); ok {
		if _, isBool := v.(bool); !isBool {
			return fail(CodeInvalidDoc, sub(path, "autoOrient"), "expected a boolean")
		}
	}
	return nil
}

var stopColorRE = regexp.MustCompile(`^gradientStop\[(0|[1-9]\d{0,2})\]\.color$`)
var stopOffsetRE = regexp.MustCompile(`^gradientStop\[(0|[1-9]\d{0,2})\]\.offset$`)

func checkKeyValue(prop, path string, v any) *ValidationError {
	_, isStr := v.(string)
	_, isNum := v.(numberT)
	switch {
	case prop == "d":
		if !isStr || !isPathData(v.(string)) {
			return fail(CodeInvalidDoc, path, "expected path data")
		}
	case prop == "motionPath", prop == "strokeDashoffset", prop == "filter.blur", stopOffsetRE.MatchString(prop):
		if !isNum {
			return fail(CodeInvalidDoc, path, "expected a number")
		}
	case stopColorRE.MatchString(prop):
		if !isStr || !isSafeColor(v.(string)) {
			return fail(CodeInvalidDoc, path, "invalid color")
		}
	}
	return nil
}

// canvasV applies canvasSchema.transform: emit width, height, viewBox,
// background, then the remaining (unknown) keys; viewBox is derived from
// width/height when absent.
func canvasV(c *ctx, path string, v any) (any, *ValidationError) {
	o, err := canvasSpec.build(c, path, v)
	if err != nil {
		return nil, err
	}
	out := newObject()
	out.set("width", o.vals["width"])
	out.set("height", o.vals["height"])
	if vb, ok := o.get("viewBox"); ok {
		out.set("viewBox", vb)
	} else {
		d := newObject()
		d.set("x", numberT("0"))
		d.set("y", numberT("0"))
		d.set("width", o.vals["width"])
		d.set("height", o.vals["height"])
		out.set("viewBox", d)
	}
	out.set("background", o.vals["background"])
	for _, k := range o.keys {
		if k != "width" && k != "height" && k != "viewBox" && k != "background" {
			out.set(k, o.vals[k])
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Raw markup rules: rawMarkupIssues (vectorTypes.ts) + server hardening
// ---------------------------------------------------------------------------

// rawElements is the ELEMENTS allow-list of lib/vector/rawRender.ts plus
// AS_GROUP's `a`. Raw markup is canonical XMLSerializer output, so element
// names are matched case-sensitively and never carry a namespace prefix.
var rawElements = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(`g path rect circle ellipse line polyline polygon text tspan textPath
		image use defs symbol linearGradient radialGradient stop pattern mask
		clipPath marker filter switch svg title desc mpath
		animate animateTransform animateMotion
		feBlend feColorMatrix feComponentTransfer feComposite feConvolveMatrix feDiffuseLighting
		feDisplacementMap feDistantLight feDropShadow feFlood feFuncA feFuncB feFuncG feFuncR
		feGaussianBlur feMerge feMergeNode feMorphology feOffset fePointLight feSpecularLighting
		feSpotLight feTile feTurbulence a`) {
		m[e] = true
	}
	return m
}()

// All JS `\s` occurrences are widened to the full Unicode class (jsS) because
// RE2's \s is ASCII-only. JS `\w` and `\b` are ASCII, like RE2.
var (
	reScript    = regexp.MustCompile(`(?i)<[` + jsS + `]*script`)
	reForbidden = regexp.MustCompile(`(?i)<[` + jsS + `]*/?[` + jsS + `]*(iframe|object|embed|frame|frameset|applet|meta|link|base|form|input|button|textarea|select|template|noscript|portal|style)\b`)
	reDecl      = regexp.MustCompile(`(?i)<!\[CDATA\[|<!--|<\?|<!DOCTYPE|<!ENTITY`)
	reEvent     = regexp.MustCompile(`(?i)[` + jsS + `"'/]on[a-z]+[` + jsS + `]*=`)
	reDanger    = regexp.MustCompile(`(?i)\b(srcdoc|formaction|srcset|ping|background)[` + jsS + `]*=`)
	reScriptURL = regexp.MustCompile(`(?i)javascript:|vbscript:|data:text/html`)
	reAttrStart = regexp.MustCompile(`[` + jsS + `][\w:-]+[` + jsS + `]*=`)
	reRefAttr   = regexp.MustCompile(`(?i)(?:^|[` + jsS + `"'])(?:xlink:)?(?:href|src)[` + jsS + `]*=[` + jsS + `]*["']`)
	reAnimHref  = regexp.MustCompile(`(?i)attributeName[` + jsS + `]*=[` + jsS + `]*["'][` + jsS + `]*(xlink:)?href`)
	reBackslash = regexp.MustCompile(`=[` + jsS + `]*"[^"]*\\|=[` + jsS + `]*'[^']*\\`)
	reXmlns     = regexp.MustCompile(`(?i)[` + jsS + `"']xmlns(?::[^` + jsS + `=]*)?[` + jsS + `]*=`)
	reHrefOK    = regexp.MustCompile(`(?i)^(?:#|data:image/(?:png|jpeg|gif|webp|avif);)`)
	reUseHref   = regexp.MustCompile(`(?i)(?:^|[` + jsS + `"'])(?:xlink:)?href[` + jsS + `]*=[` + jsS + `]*(?:"([^"]*)"|'([^']*)')`)
)

// charRefOK lists the references rawMarkupIssues accepts after '&'.
var charRefOK = []string{"amp;", "lt;", "gt;", "quot;", "apos;", "#9;", "#10;", "#13;", "#xA;", "#xD;", "#x9;"}

// rawMarkupIssue returns the first problem with a raw layer's markup, or "".
func rawMarkupIssue(m string) string {
	if reScript.MatchString(m) {
		return "script element"
	}
	if reForbidden.MatchString(m) {
		return "forbidden element"
	}
	if reDecl.MatchString(m) {
		return "CDATA, comment or declaration"
	}
	// JS: /&(?!(amp|lt|gt|quot|apos|#9|#10|#13|#xA|#xD|#x9);)/i. RE2 has no
	// lookahead: at every '&' test the text that follows against the allowed
	// references (case-insensitively) by hand.
	for i := 0; i < len(m); i++ {
		if m[i] != '&' {
			continue
		}
		ok := false
		for _, ref := range charRefOK {
			if len(m)-i-1 >= len(ref) && strings.EqualFold(m[i+1:i+1+len(ref)], ref) {
				ok = true
				break
			}
		}
		if !ok {
			return "character reference"
		}
	}
	if reEvent.MatchString(m) {
		return "event-handler attribute"
	}
	if reDanger.MatchString(m) {
		return "dangerous attribute"
	}
	if reScriptURL.MatchString(m) {
		return "script URL"
	}
	// JS: /\s[\w:-]+\s*=\s*(?!["'])/. After "=" the \s* may backtrack to zero
	// width, so the lookahead only fails when the character IMMEDIATELY after
	// "=" is a quote. Scan candidates with a regex, then test that character.
	for pos := 0; pos < len(m); {
		loc := reAttrStart.FindStringIndex(m[pos:])
		if loc == nil {
			break
		}
		end := pos + loc[1]
		if end >= len(m) || (m[end] != '"' && m[end] != '\'') {
			return "unquoted attribute"
		}
		pos += loc[0] + 1
	}
	// JS: /(?:^|[\s"'])(?:xlink:)?(?:href|src)\s*=\s*["'](?!#|data:image\/(?:png|jpeg|gif|webp|avif);)/i.
	// The lookahead is tested by hand on the text after the opening quote.
	for pos := 0; pos < len(m); {
		loc := reRefAttr.FindStringIndex(m[pos:])
		if loc == nil {
			break
		}
		end := pos + loc[1]
		if !reHrefOK.MatchString(m[end:]) {
			return "external reference"
		}
		pos += loc[0] + 1
	}
	if reAnimHref.MatchString(m) {
		return "animated href"
	}
	if hasNonLocalURL(m) || strings.Contains(strings.ToLower(m), "image-set(") {
		return "external url()"
	}
	if strings.Contains(strings.ToLower(m), "@import") {
		return "@import"
	}
	if reBackslash.MatchString(m) {
		return "backslash in attribute"
	}

	// ---- server-side hardening beyond the client rules ----
	if strings.Contains(strings.ToLower(m), "expression(") {
		return "expression("
	}
	if reXmlns.MatchString(m) {
		return "xmlns attribute"
	}
	return scanTags(m)
}

// hasNonLocalURL is /url\(\s*["']?(?!#)/i: with \s* and ["']? able to match
// nothing, it is true unless the character right after "url(" is '#'.
func hasNonLocalURL(s string) bool {
	l := strings.ToLower(s)
	for from := 0; ; {
		i := strings.Index(l[from:], "url(")
		if i < 0 {
			return false
		}
		next := from + i + 4
		if next >= len(l) || l[next] != '#' {
			return true
		}
		from = next
	}
}

// scanTags walks every '<' in the markup (canonical XMLSerializer output, so a
// raw '<' always starts a tag), checks the element name against the
// allow-list and checks `<use>` hrefs.
func scanTags(m string) string {
	for i := 0; i < len(m); i++ {
		if m[i] != '<' {
			continue
		}
		j := i + 1
		closing := false
		if j < len(m) && m[j] == '/' {
			closing = true
			j++
		}
		start := j
		for j < len(m) && !isJSSpace(rune(m[j])) && m[j] != '/' && m[j] != '>' {
			j++
		}
		name := m[start:j]
		lower := strings.ToLower(name)
		if k := strings.LastIndex(lower, ":"); k >= 0 {
			lower = lower[k+1:]
		}
		if lower == "foreignobject" {
			return "foreignObject element"
		}
		if lower == "script" {
			return "script element"
		}
		if name == "" {
			return "malformed tag"
		}
		if !rawElements[name] {
			return "element " + name + " is not allowed"
		}
		// Find the end of the tag, honouring quotes.
		end := j
		var q byte
		for end < len(m) {
			c := m[end]
			if q != 0 {
				if c == q {
					q = 0
				}
			} else if c == '"' || c == '\'' {
				q = c
			} else if c == '>' {
				break
			}
			end++
		}
		if !closing && name == "use" {
			for _, mm := range reUseHref.FindAllStringSubmatch(m[j:end], -1) {
				val := mm[1] + mm[2]
				if !strings.HasPrefix(strings.TrimSpace(val), "#") {
					return "use href must be a local #id"
				}
			}
		}
		i = end
	}
	return ""
}

// ---------------------------------------------------------------------------
// Document integrity (docIntegrityIssues, forEachLayer, hasUseCycle)
// ---------------------------------------------------------------------------

type layerVisit func(l *object, depth int, path string) *ValidationError

func walkLayers(layers []any, depth int, path string, visit layerVisit) *ValidationError {
	for i, e := range layers {
		l := e.(*object)
		p := idx(path, i)
		if err := visit(l, depth, p); err != nil {
			return err
		}
		if str(l, "type") == "group" {
			ch, _ := l.get("children")
			if err := walkLayers(ch.([]any), depth+1, p+".children", visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func str(o *object, k string) string {
	s, _ := o.vals[k].(string)
	return s
}

func arr(o *object, k string) []any {
	a, _ := o.vals[k].([]any)
	return a
}

// forEachLayer visits the tree (depth 1), then mask, clipPath and symbol
// layers (depth 2), in forEachLayer's order.
func forEachLayer(root *object, visit layerVisit) *ValidationError {
	if err := walkLayers(arr(root, "layers"), 1, "layers", visit); err != nil {
		return err
	}
	defs := root.vals["defs"].(*object)
	for _, kind := range []string{"masks", "clipPaths", "symbols"} {
		for i, d := range arr(defs, kind) {
			p := idx("defs."+kind, i) + ".layers"
			if err := walkLayers(arr(d.(*object), "layers"), 2, p, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// docIntegrity applies docIntegrityIssues and returns the total layer count.
func docIntegrity(root *object, l Limits) (int, *ValidationError) {
	seen := map[string]bool{}
	claim := func(id, what, path string) *ValidationError {
		if seen[id] {
			return fail(CodeDuplicateID, path, "duplicate id %q (%s)", id, what)
		}
		seen[id] = true
		return nil
	}
	layerIDs := map[string]bool{}
	rawIDs := map[string]bool{}
	count := 0
	err := forEachLayer(root, func(ly *object, depth int, path string) *ValidationError {
		count++
		id := str(ly, "id")
		if depth > l.MaxDepth {
			return fail(CodeTooDeep, path, "layer %q nests deeper than %d", id, l.MaxDepth)
		}
		if err := claim(id, "layer", path+".id"); err != nil {
			return err
		}
		layerIDs[id] = true
		if str(ly, "type") == "raw" {
			for i, rid := range arr(ly, "ids") {
				s := rid.(string)
				if err := claim(s, "raw markup", idx(path+".ids", i)); err != nil {
					return err
				}
				rawIDs[s] = true
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if count > l.MaxLayers {
		return 0, fail(CodeTooManyLayers, "", "more than %d layers", l.MaxLayers)
	}

	defs := root.vals["defs"].(*object)
	bucket := func(kind string) map[string]bool {
		m := map[string]bool{}
		for i, d := range arr(defs, kind) {
			id := str(d.(*object), "id")
			_ = i
			m[id] = true
		}
		return m
	}
	// Claim order: gradients, masks, clipPaths, filters, symbols.
	for _, kind := range []string{"gradients", "masks", "clipPaths", "filters", "symbols"} {
		for i, d := range arr(defs, kind) {
			if e := claim(str(d.(*object), "id"), strings.TrimSuffix(kind, "s"), idx("defs."+kind, i)+".id"); e != nil {
				return 0, e
			}
		}
	}
	gradientIDs, maskIDs, clipIDs, filterIDs, symbolIDs := bucket("gradients"), bucket("masks"), bucket("clipPaths"), bucket("filters"), bucket("symbols")
	resolves := func(id string, b map[string]bool) bool { return b[id] || rawIDs[id] }

	err = forEachLayer(root, func(ly *object, _ int, path string) *ValidationError {
		if str(ly, "type") == "raw" {
			return nil
		}
		id := str(ly, "id")
		for _, key := range []string{"fill", "stroke"} {
			paint, ok := ly.vals[key].(string)
			if !ok || !strings.Contains(strings.ToLower(paint), "url(") {
				continue
			}
			ref, isRef := paintRef(paint)
			if !isRef {
				return fail(CodeUnsafeContent, sub(path, key), "layer %q %s has a non-local url()", id, key)
			}
			if !resolves(ref, gradientIDs) {
				return fail(CodeDanglingRef, sub(path, key), "layer %q %s references missing %q", id, key, ref)
			}
		}
		for _, c := range []struct {
			key string
			set map[string]bool
		}{{"mask", maskIDs}, {"clipPath", clipIDs}, {"filter", filterIDs}} {
			if ref, ok := ly.vals[c.key].(string); ok && ref != "" && !resolves(ref, c.set) {
				return fail(CodeDanglingRef, sub(path, c.key), "layer %q %s references missing %q", id, c.key, ref)
			}
		}
		if str(ly, "type") == "use" {
			ref := str(ly, "ref")
			if !symbolIDs[ref] && !layerIDs[ref] && !rawIDs[ref] {
				return fail(CodeDanglingRef, sub(path, "ref"), "use %q references missing %q", id, ref)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	if hasUseCycle(root) {
		return 0, fail(CodeUseCycle, "", "a use element references itself or an ancestor")
	}
	for i, t := range arr(root, "animations") {
		lid := str(t.(*object), "layerId")
		if !layerIDs[lid] {
			return 0, fail(CodeDanglingRef, idx("animations", i)+".layerId", "track references missing layer %q", lid)
		}
	}
	return count, nil
}

// hasUseCycle builds the same graph as vectorTypes.ts: container -> child
// (structure) and use -> ref, rooted at symbols, masks, clipPaths and the
// document root, then looks for a cycle by DFS.
func hasUseCycle(root *object) bool {
	edges := map[string][]string{}
	var link func(parent string, layers []any)
	link = func(parent string, layers []any) {
		for _, e := range layers {
			l := e.(*object)
			id := str(l, "id")
			edges[parent] = append(edges[parent], id)
			switch str(l, "type") {
			case "group":
				link(id, arr(l, "children"))
			case "use":
				edges[id] = append(edges[id], str(l, "ref"))
			}
		}
	}
	defs := root.vals["defs"].(*object)
	for _, kind := range []string{"symbols", "masks", "clipPaths"} {
		for _, d := range arr(defs, kind) {
			link(str(d.(*object), "id"), arr(d.(*object), "layers"))
		}
	}
	link("\x00root", arr(root, "layers"))

	state := map[string]int{} // 1 = on stack, 2 = done
	var visit func(string) bool
	visit = func(n string) bool {
		switch state[n] {
		case 1:
			return true
		case 2:
			return false
		}
		state[n] = 1
		for _, next := range edges[n] {
			if visit(next) {
				return true
			}
		}
		state[n] = 2
		return false
	}
	for n := range edges {
		if visit(n) {
			return true
		}
	}
	return false
}
