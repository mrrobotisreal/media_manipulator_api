package vector

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxJSONDepth bounds raw JSON nesting (objects/arrays of any kind) so the
// recursive parser and emitter cannot be driven into deep recursion.
const maxJSONDepth = 160

// object is a JSON object that remembers key insertion order. Go maps do not,
// and the canonical form must not reorder keys (the UI fixtures are byte
// compared).
type object struct {
	keys []string
	vals map[string]any
}

func newObject() *object { return &object{vals: map[string]any{}} }

func (o *object) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// parseJSON parses raw into an ordered tree: *object, []any, string,
// json.Number, bool, nil. It rejects invalid UTF-8, duplicate keys within an
// object, trailing data and JSON nesting deeper than maxJSONDepth.
func parseJSON(raw []byte) (any, *ValidationError) {
	if !utf8.Valid(raw) {
		return nil, &ValidationError{Code: CodeInvalidJSON, Message: "document is not valid UTF-8"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, &ValidationError{Code: CodeInvalidJSON, Message: "invalid JSON: " + err.Error()}
	}
	v, verr := parseValue(dec, tok, 1)
	if verr != nil {
		return nil, verr
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &ValidationError{Code: CodeInvalidJSON, Message: "unexpected data after the top-level value"}
	}
	return v, nil
}

func parseValue(dec *json.Decoder, tok json.Token, depth int) (any, *ValidationError) {
	bad := func(err error) *ValidationError {
		return &ValidationError{Code: CodeInvalidJSON, Message: "invalid JSON: " + err.Error()}
	}
	d, ok := tok.(json.Delim)
	if !ok {
		if n, isNum := tok.(json.Number); isNum {
			if f, err := strconv.ParseFloat(string(n), 64); err != nil || math.IsInf(f, 0) {
				return nil, &ValidationError{Code: CodeInvalidJSON, Message: "number out of range: " + string(n)}
			}
		}
		return tok, nil // string, json.Number, bool, nil
	}
	if depth > maxJSONDepth {
		return nil, &ValidationError{Code: CodeTooDeep, Message: "JSON is nested too deeply"}
	}
	if d == '{' {
		obj := newObject()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, bad(err)
			}
			key, _ := kt.(string)
			if _, dup := obj.vals[key]; dup {
				return nil, &ValidationError{Code: CodeInvalidJSON, Message: "duplicate key " + strconv.Quote(key)}
			}
			vt, err := dec.Token()
			if err != nil {
				return nil, bad(err)
			}
			v, verr := parseValue(dec, vt, depth+1)
			if verr != nil {
				return nil, verr
			}
			obj.set(key, v)
		}
		if _, err := dec.Token(); err != nil { // closing '}'
			return nil, bad(err)
		}
		return obj, nil
	}
	arr := []any{}
	for dec.More() {
		vt, err := dec.Token()
		if err != nil {
			return nil, bad(err)
		}
		v, verr := parseValue(dec, vt, depth+1)
		if verr != nil {
			return nil, verr
		}
		arr = append(arr, v)
	}
	if _, err := dec.Token(); err != nil { // closing ']'
		return nil, bad(err)
	}
	return arr, nil
}

// emit serialises a parsed tree the way JavaScript's JSON.stringify does:
// compact when indent is false, otherwise JSON.stringify(v, null, 2).
//
// Known, accepted limitation: JavaScript orders integer-like object keys
// ("1", "42") ahead of the others. VectorDoc never uses such keys, so we do
// not emulate it; insertion order is kept.
func emit(v any, indent bool) []byte {
	var b bytes.Buffer
	writeValue(&b, v, indent, 0)
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v any, indent bool, level int) {
	nl := func(l int) {
		if indent {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat("  ", l))
		}
	}
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, t)
	case json.Number:
		b.WriteString(formatNumber(t))
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			nl(level + 1)
			writeValue(b, e, indent, level+1)
		}
		nl(level)
		b.WriteByte(']')
	case *object:
		if len(t.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			nl(level + 1)
			writeString(b, k)
			b.WriteByte(':')
			if indent {
				b.WriteByte(' ')
			}
			writeValue(b, t.vals[k], indent, level+1)
		}
		nl(level)
		b.WriteByte('}')
	}
}

// formatNumber renders n like ES6 Number#toString. encoding/json already
// formats float64 with the ES6 rules; negative zero is mapped to 0.
func formatNumber(n json.Number) string {
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "0" // unreachable for validated docs (overflow is rejected earlier)
	}
	if f == 0 {
		return "0"
	}
	out, _ := json.Marshal(f)
	return string(out)
}

// writeString writes s as a JavaScript JSON string: no HTML escaping and no
// U+2028/2029 escaping (where Go's encoder differs from JS).
func writeString(b *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

// numberT is the parsed JSON number type.
type numberT = json.Number
