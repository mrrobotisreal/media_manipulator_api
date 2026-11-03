package photo

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

// maxJSONDepth bounds raw JSON nesting so parser and emitter recursion stays shallow.
const maxJSONDepth = 100

// parseJSON parses raw into a generic tree (map[string]any, []any, string,
// float64, bool, nil). It rejects invalid UTF-8, duplicate keys, trailing
// data, numbers outside float64 and nesting deeper than maxJSONDepth.
func parseJSON(raw []byte) (any, *ValidationError) {
	if !utf8.Valid(raw) {
		return nil, fail(CodeInvalidJSON, "", "document is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fail(CodeInvalidJSON, "", "invalid JSON: %v", err)
	}
	v, verr := parseValue(dec, tok, 1)
	if verr != nil {
		return nil, verr
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fail(CodeInvalidJSON, "", "unexpected data after the top-level value")
	}
	return v, nil
}

func parseValue(dec *json.Decoder, tok json.Token, depth int) (any, *ValidationError) {
	bad := func(err error) *ValidationError { return fail(CodeInvalidJSON, "", "invalid JSON: %v", err) }
	d, ok := tok.(json.Delim)
	if !ok {
		if n, isNum := tok.(json.Number); isNum {
			f, err := strconv.ParseFloat(string(n), 64)
			if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, fail(CodeInvalidJSON, "", "number out of range: %s", string(n))
			}
			return f, nil
		}
		return tok, nil // string, bool, nil
	}
	if depth > maxJSONDepth {
		return nil, fail(CodeInvalidJSON, "", "JSON is nested too deeply")
	}
	if d == '{' {
		obj := map[string]any{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, bad(err)
			}
			key, _ := kt.(string)
			if _, dup := obj[key]; dup {
				return nil, fail(CodeInvalidJSON, "", "duplicate key %s", strconv.Quote(key))
			}
			vt, err := dec.Token()
			if err != nil {
				return nil, bad(err)
			}
			v, verr := parseValue(dec, vt, depth+1)
			if verr != nil {
				return nil, verr
			}
			obj[key] = v
		}
		if _, err := dec.Token(); err != nil {
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
	if _, err := dec.Token(); err != nil {
		return nil, bad(err)
	}
	return arr, nil
}

// emit writes the canonical form: compact, keys sorted by code point at every
// depth, numbers as ECMAScript prints them, strings escaped as JSON.stringify
// does (no HTML escaping; U+2028/U+2029 written raw).
func emit(v any) []byte {
	var b bytes.Buffer
	writeValue(&b, v)
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v any) {
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
	case float64:
		b.WriteString(formatNumber(t))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeValue(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // byte order of UTF-8 == code point order
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			writeValue(b, t[k])
		}
		b.WriteByte('}')
	}
}

// formatNumber renders f like ES Number#toString; -0 becomes 0.
func formatNumber(f float64) string {
	if f == 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return "0"
	}
	out, _ := json.Marshal(f)
	return string(out)
}

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
