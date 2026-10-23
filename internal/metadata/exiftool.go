package metadata

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// exiftool argument lists (RUNBOOK "metadata inspector").
func exiftoolPrintArgs(path string) []string {
	return []string{"-json", "-G1", "-s", "-a", "-struct", "-api", "largefilesupport=1", path}
}

// rationalTags are re-read with -n so RawValue carries the number behind a
// printed fraction such as "1/250".
var rationalTags = []string{"ExposureTime", "ShutterSpeedValue", "ApertureValue", "FNumber", "FocalLength", "ExposureCompensation", "BrightnessValue"}

func exiftoolNumericArgs(path string) []string {
	args := []string{"-json", "-G1", "-s", "-a", "-n", "-GPS:all", "-Composite:GPSPosition", "-Composite:GPSLatitude", "-Composite:GPSLongitude"}
	for _, t := range rationalTags {
		args = append(args, "-"+t)
	}
	return append(args, path)
}

// skippedKeys are exiftool keys that never become entries: server temp-path
// leakage, or data that goes elsewhere in the document.
var skippedKeys = map[string]bool{
	"SourceFile":               true,
	"ExifTool:ExifToolVersion": true,
	"File:Directory":           true,
	"File:FilePermissions":     true,
	"File:FileAccessDate":      true,
	"File:FileCreateDate":      true,
}

var binaryRE = regexp.MustCompile(`^\((?:Large )?[Bb]inary data (\d+) bytes[^)]*\)$`)

type rawPair struct {
	Key string
	Val any
}

// decodeExiftoolJSON walks the first record of exiftool's JSON array in key
// order. Numbers stay json.Number, objects become OrderedMap, arrays []any.
func decodeExiftoolJSON(stdout string) ([]rawPair, error) {
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("expected array")
	}
	if !dec.More() {
		return nil, nil
	}
	tok, err = dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected object")
	}
	om, err := decodeObjectBody(dec)
	if err != nil {
		return nil, err
	}
	pairs := make([]rawPair, len(om))
	for i, kv := range om {
		pairs[i] = rawPair{kv.Key, kv.Val}
	}
	return pairs, nil
}

// decodeObjectBody reads key/value pairs up to and including the closing brace.
func decodeObjectBody(dec *json.Decoder) (OrderedMap, error) {
	out := OrderedMap{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("non-string key")
		}
		v, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		out = append(out, KV{key, v})
	}
	if _, err := dec.Token(); err != nil { // closing }
		return nil, err
	}
	return out, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // string, json.Number, bool, nil
	}
	switch d {
	case '{':
		return decodeObjectBody(dec)
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil && err != io.EOF {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected delimiter %v", d)
}

// splitKey splits "Group:Tag" at the first colon.
func splitKey(key string) (group, tag string, ok bool) {
	i := strings.Index(key, ":")
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

type extraction struct {
	entries []MetadataEntry
	version string // exiftool version, "" when absent
	parsed  int    // keys read from the record
	skipped int    // keys not turned into entries
}

// buildExiftoolEntries converts decoded pairs into entries (unclassified order
// preserved). Keys without a group, and skipKeys, are skipped and counted.
func buildExiftoolEntries(pairs []rawPair) extraction {
	ex := extraction{parsed: len(pairs), entries: []MetadataEntry{}}
	alloc := newIDAllocator()
	for _, p := range pairs {
		if p.Key == "ExifTool:ExifToolVersion" {
			ex.version = fmt.Sprint(p.Val)
		}
		group, tag, ok := splitKey(p.Key)
		if !ok || skippedKeys[p.Key] {
			ex.skipped++
			continue
		}
		e := MetadataEntry{ID: alloc.next(group, tag), Group: group, Tag: tag, Value: p.Val}
		if s, isStr := p.Val.(string); isStr {
			if m := binaryRE.FindStringSubmatch(s); m != nil {
				n, _ := strconv.ParseInt(m[1], 10, 64)
				e.Value = BinaryValue{Bytes: n}
				e.Binary = true
			}
		}
		e.Category, e.Privacy = Classify(e.Group, e.Tag)
		ex.entries = append(ex.entries, e)
	}
	return ex
}

// mergeRaw sets RawValue on entries from the -n pass when the numeric value
// differs from the printed one. Unmatched pass-2 entries are ignored.
func mergeRaw(entries []MetadataEntry, numeric []MetadataEntry) {
	idx := make(map[string]int, len(entries))
	for i, e := range entries {
		idx[e.ID] = i
	}
	for _, n := range numeric {
		i, ok := idx[n.ID]
		if !ok || n.Binary || entries[i].Binary {
			continue
		}
		if !sameValue(entries[i].Value, n.Value) {
			entries[i].RawValue = n.Value
		}
	}
}

func sameValue(a, b any) bool {
	if isScalar(a) && isScalar(b) {
		return fmt.Sprint(a) == fmt.Sprint(b)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, json.Number, bool, nil:
		return true
	}
	return false
}
