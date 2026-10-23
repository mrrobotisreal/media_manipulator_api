package metadata

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ChangedEntry is one entry present in both documents with different values.
type ChangedEntry struct {
	ID     string `json:"id"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// CategoryCounts tallies a diff per entry category.
type CategoryCounts struct {
	Removed   int `json:"removed"`
	Added     int `json:"added"`
	Changed   int `json:"changed"`
	Unchanged int `json:"unchanged"`
}

// DiffResult is the server-computed difference between an original and a
// result document (design §3.4). Slices are never nil so the JSON has [].
type DiffResult struct {
	Removed    []MetadataEntry           `json:"removed"`
	Added      []MetadataEntry           `json:"added"`
	Changed    []ChangedEntry            `json:"changed"`
	Unchanged  int                       `json:"unchanged"`
	ByCategory map[string]CategoryCounts `json:"byCategory"`
}

// numericTolerance is the relative-or-absolute tolerance for numbers.
const numericTolerance = 1e-6

// Diff compares two documents on entry id. It is pure and deterministic:
// removed and changed follow the original's order, added follows the result's.
// A nil document counts as empty.
func Diff(original, result *MetadataDocument) DiffResult {
	out := DiffResult{
		Removed:    []MetadataEntry{},
		Added:      []MetadataEntry{},
		Changed:    []ChangedEntry{},
		ByCategory: map[string]CategoryCounts{},
	}
	var oe, re []MetadataEntry
	if original != nil {
		oe = original.Entries
	}
	if result != nil {
		re = result.Entries
	}
	resByID := make(map[string]int, len(re))
	for i, e := range re {
		if _, dup := resByID[e.ID]; !dup {
			resByID[e.ID] = i
		}
	}
	origIDs := make(map[string]bool, len(oe))
	bump := func(cat string, f func(*CategoryCounts)) {
		c := out.ByCategory[cat]
		f(&c)
		out.ByCategory[cat] = c
	}
	for _, o := range oe {
		if origIDs[o.ID] {
			continue // duplicate id inside one document: first wins
		}
		origIDs[o.ID] = true
		i, ok := resByID[o.ID]
		if !ok {
			out.Removed = append(out.Removed, o)
			bump(o.Category, func(c *CategoryCounts) { c.Removed++ })
			continue
		}
		r := re[i]
		if entriesEqual(o, r) {
			out.Unchanged++
			bump(o.Category, func(c *CategoryCounts) { c.Unchanged++ })
			continue
		}
		out.Changed = append(out.Changed, ChangedEntry{ID: o.ID, Before: o.Value, After: r.Value})
		bump(r.Category, func(c *CategoryCounts) { c.Changed++ })
	}
	seenAdded := map[string]bool{}
	for _, r := range re {
		if origIDs[r.ID] || seenAdded[r.ID] {
			continue
		}
		seenAdded[r.ID] = true
		out.Added = append(out.Added, r)
		bump(r.Category, func(c *CategoryCounts) { c.Added++ })
	}
	return out
}

// IsExpectedChange reports whether an entry id names a tag that always differs
// between an input and its converted output (name, size, dates, type), so
// callers counting "what was removed" can ignore it. Diff still reports them.
func IsExpectedChange(id string) bool {
	if i := strings.Index(id, "#"); i >= 0 {
		id = id[:i]
	}
	group, tag, ok := splitKey(id)
	if !ok {
		return false
	}
	switch {
	case group == "Composite" && tag == "ImageSize":
		return true
	case fileSystemGroups[group]:
		switch tag {
		case "FileName", "FileSize", "FileModifyDate", "MIMEType":
			return true
		}
		return strings.HasPrefix(tag, "FileType")
	}
	return false
}

func entriesEqual(a, b MetadataEntry) bool {
	if a.Binary || b.Binary {
		an, aok := binaryLen(a)
		bn, bok := binaryLen(b)
		if aok && bok {
			return an == bn
		}
	}
	return valuesEqual(canon(a.Value), canon(b.Value))
}

func binaryLen(e MetadataEntry) (int64, bool) {
	switch v := e.Value.(type) {
	case BinaryValue:
		return v.Bytes, true
	case *BinaryValue:
		if v != nil {
			return v.Bytes, true
		}
	}
	n := canon(e.Value)
	if m, ok := n.(map[string]any); ok {
		if f, ok := m["bytes"].(float64); ok {
			return int64(f), true
		}
	}
	return 0, false
}

var (
	spaceRun   = regexp.MustCompile(`\s+`)
	decimalRE  = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	errNotJSON = struct{}{}
)

// canon normalizes any value into a comparable tree of: nil, bool, float64,
// string (trimmed, whitespace runs collapsed), []any and map[string]any.
// Numeric strings that parse cleanly become float64. Marshalling first makes
// in-memory values (json.Number, OrderedMap, BinaryValue) and values decoded
// from a cached file (float64, map) compare the same way.
func canon(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return errNotJSON
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return errNotJSON
	}
	return canonTree(tree)
}

func canonTree(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return string(t)
		}
		return f
	case string:
		s := spaceRun.ReplaceAllString(strings.TrimSpace(t), " ")
		if decimalRE.MatchString(s) {
			if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) {
				return f
			}
		}
		return s
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = canonTree(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = canonTree(x)
		}
		return out
	}
	return v
}

func valuesEqual(a, b any) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && numbersEqual(x, y)
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !valuesEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			yv, ok := y[k]
			if !ok || !valuesEqual(x[k], yv) {
				return false
			}
		}
		return true
	}
	return a == b
}

func numbersEqual(a, b float64) bool {
	d := math.Abs(a - b)
	if d <= numericTolerance {
		return true
	}
	return d <= numericTolerance*math.Max(math.Abs(a), math.Abs(b))
}
