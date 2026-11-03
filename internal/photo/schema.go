package photo

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf16"
)

// ctx carries limits through one Sanitize call.
type ctx struct{ lim Limits }

// node cleans one value. present is false when the key was absent. omit true
// means "leave the key out of the output" (optional field, unset).
type node interface {
	clean(v any, present bool, path string, c *ctx) (out any, omit bool, err *ValidationError)
}

var (
	idRe      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	colorRe   = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	maskRefRe = regexp.MustCompile(`^masks/[A-Za-z0-9_-]{1,40}\.png$`)
)

func typeErr(path, want string) *ValidationError {
	return fail(CodeInvalidDoc, path, "expected %s", want)
}

// ---- numbers ----

type numNode struct {
	min, max, def float64
	integer       bool
}

func num(min, max, def float64) numNode  { return numNode{min: min, max: max, def: def} }
func intn(min, max, def float64) numNode { return numNode{min: min, max: max, def: def, integer: true} }

func (n numNode) clean(v any, present bool, path string, _ *ctx) (any, bool, *ValidationError) {
	if !present {
		return n.def, false, nil
	}
	f, ok := v.(float64)
	if !ok {
		return nil, false, typeErr(path, "number")
	}
	if n.integer {
		f = math.Floor(f + 0.5)
	}
	if f < n.min {
		f = n.min
	}
	if f > n.max {
		f = n.max
	}
	if f == 0 {
		f = 0 // -0 -> 0
	}
	return f, false, nil
}

// ---- bool / enum / string / colour / id ----

type boolNode struct{ def bool }

func (n boolNode) clean(v any, present bool, path string, _ *ctx) (any, bool, *ValidationError) {
	if !present {
		return n.def, false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil, false, typeErr(path, "boolean")
	}
	return b, false, nil
}

type enumNode struct {
	vals []string
	def  string
}

func (n enumNode) clean(v any, present bool, path string, _ *ctx) (any, bool, *ValidationError) {
	if !present {
		return n.def, false, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, false, typeErr(path, "string")
	}
	for _, e := range n.vals {
		if e == s {
			return s, false, nil
		}
	}
	return nil, false, fail(CodeInvalidDoc, path, "%q is not one of %s", s, strings.Join(n.vals, "|"))
}

// utf16Len is the JavaScript string length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
		if r == 0xFFFD { // RuneLen(0xFFFD) is 1; invalid sequences cannot occur (UTF-8 validated)
			continue
		}
	}
	return n
}

type strNode struct {
	max int
	def string
}

func (n strNode) clean(v any, present bool, path string, _ *ctx) (any, bool, *ValidationError) {
	if !present {
		return n.def, false, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, false, typeErr(path, "string")
	}
	if utf16Len(s) > n.max {
		return nil, false, fail(CodeInvalidDoc, path, "string longer than %d", n.max)
	}
	return s, false, nil
}

type colorNode struct{ def string }

func (n colorNode) clean(v any, present bool, path string, _ *ctx) (any, bool, *ValidationError) {
	if !present {
		return n.def, false, nil
	}
	s, ok := v.(string)
	if !ok || !colorRe.MatchString(s) {
		return nil, false, fail(CodeInvalidDoc, path, "expected #rrggbb colour")
	}
	return strings.ToLower(s), false, nil
}

// idNode is a required id string.
type idNode struct{}

func (idNode) clean(v any, present bool, path string, _ *ctx) (any, bool, *ValidationError) {
	if !present {
		return nil, false, fail(CodeBadID, path, "id is required")
	}
	s, ok := v.(string)
	if !ok || !idRe.MatchString(s) {
		return nil, false, fail(CodeBadID, path, "id must match [A-Za-z0-9_-]{1,40}")
	}
	return s, false, nil
}

// refNode is a required masks/<id>.png reference.
type refNode struct{}

func (refNode) clean(v any, present bool, path string, _ *ctx) (any, bool, *ValidationError) {
	if !present {
		return nil, false, fail(CodeInvalidDoc, path, "maskRef is required")
	}
	s, ok := v.(string)
	if !ok || !maskRefRe.MatchString(s) {
		return nil, false, fail(CodeInvalidDoc, path, "maskRef must match masks/<id>.png")
	}
	return s, false, nil
}

// ---- wrappers ----

// optNode: absent -> omitted; present -> inner (null is refused).
type optNode struct{ in node }

func (n optNode) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	if !present {
		return nil, true, nil
	}
	return n.in.clean(v, true, path, c)
}

// nullNode: absent or null -> null; present -> inner.
type nullNode struct{ in node }

func (n nullNode) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	if !present || v == nil {
		return nil, false, nil
	}
	return n.in.clean(v, true, path, c)
}

// ---- objects ----

type field struct {
	name string
	in   node
}

// objNode cleans a JSON object: known fields via their nodes (defaults
// filled), unknown fields preserved verbatim. post, when set, runs on the
// cleaned map for cross-field rules.
type objNode struct {
	fields []field
	post   func(m map[string]any, path string, c *ctx) *ValidationError
}

func obj(fs ...field) objNode      { return objNode{fields: fs} }
func f(name string, in node) field { return field{name, in} }

func (n objNode) withPost(p func(map[string]any, string, *ctx) *ValidationError) objNode {
	n.post = p
	return n
}

func (n objNode) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	var in map[string]any
	if present {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false, typeErr(path, "object")
		}
		in = m
	}
	out := make(map[string]any, len(in)+len(n.fields))
	for k, val := range in {
		out[k] = val
	}
	for _, fd := range n.fields {
		raw, has := in[fd.name]
		o, omit, err := fd.in.clean(raw, has, path+"/"+fd.name, c)
		if err != nil {
			return nil, false, err
		}
		if omit {
			delete(out, fd.name)
			continue
		}
		out[fd.name] = o
	}
	if n.post != nil {
		if err := n.post(out, path, c); err != nil {
			return nil, false, err
		}
	}
	return out, false, nil
}

// ---- arrays ----

type arrNode struct {
	item node
	max  int
	code string // code for "too many" (defaults to invalid_doc)
	// def returns the value used when the key is absent (nil -> empty array).
	def func() []any
	// post sees the cleaned items.
	post func(items []any, path string, c *ctx) *ValidationError
}

func (n arrNode) clean(v any, present bool, path string, c *ctx) (any, bool, *ValidationError) {
	if !present {
		if n.def != nil {
			items, _, err := n.clean(n.def(), true, path, c)
			return items, false, err
		}
		return []any{}, false, nil
	}
	in, ok := v.([]any)
	if !ok {
		return nil, false, typeErr(path, "array")
	}
	if len(in) > n.max {
		code := n.code
		if code == "" {
			code = CodeInvalidDoc
		}
		return nil, false, fail(code, path, "more than %d items", n.max)
	}
	out := make([]any, 0, len(in))
	for i, e := range in {
		o, _, err := n.item.clean(e, true, path+"/"+itoa(i), c)
		if err != nil {
			return nil, false, err
		}
		out = append(out, o)
	}
	if n.post != nil {
		if err := n.post(out, path, c); err != nil {
			return nil, false, err
		}
	}
	return out, false, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// uniqueIDs is an arrNode.post checking that every item's "id" is distinct.
func uniqueIDs(items []any, path string, _ *ctx) *ValidationError {
	seen := map[string]bool{}
	for i, it := range items {
		id, _ := it.(map[string]any)["id"].(string)
		if seen[id] {
			return fail(CodeDuplicateID, path+"/"+itoa(i)+"/id", "duplicate id %q", id)
		}
		seen[id] = true
	}
	return nil
}
