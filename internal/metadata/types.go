package metadata

import (
	"encoding/json"
	"strconv"
)

// SchemaVersion is the MetadataDocument schema version.
const SchemaVersion = 1

// MetadataDocument is the canonical metadata of one file.
type MetadataDocument struct {
	SchemaVersion int             `json:"schemaVersion"`
	File          FileSummary     `json:"file"`
	Tools         []string        `json:"tools"`
	Entries       []MetadataEntry `json:"entries"`
	Warnings      []string        `json:"warnings"`
}

// FileSummary describes the inspected file.
type FileSummary struct {
	Name             string `json:"name"`
	SizeBytes        int64  `json:"sizeBytes"`
	Mime             string `json:"mime"`
	Kind             string `json:"kind"`
	SHA256           string `json:"sha256,omitempty"`
	ModificationTime string `json:"modificationTime,omitempty"`
}

// MetadataEntry is one group-keyed tag.
type MetadataEntry struct {
	ID       string `json:"id"`
	Group    string `json:"group"`
	Tag      string `json:"tag"`
	Value    any    `json:"value"`
	RawValue any    `json:"rawValue,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	Category string `json:"category"`
	Privacy  string `json:"privacy,omitempty"`
}

// Privacy classes.
const (
	PrivacyLocation = "location"
	PrivacyIdentity = "identity"
	PrivacyDevice   = "device"
	PrivacyHistory  = "history"
)

// idAllocator hands out stable entry ids, appending #2, #3 ... to repeats of a
// (group, tag) pair in call order.
type idAllocator struct{ seen map[string]int }

func newIDAllocator() *idAllocator { return &idAllocator{seen: map[string]int{}} }

func (a *idAllocator) next(group, tag string) string {
	base := group + ":" + tag
	a.seen[base]++
	if n := a.seen[base]; n > 1 {
		return base + "#" + strconv.Itoa(n)
	}
	return base
}

// KV is one key/value of an OrderedMap.
type KV struct {
	Key string
	Val any
}

// OrderedMap is a JSON object that keeps key order, used for nested exiftool
// structs and ffprobe side data so output is untouched and deterministic.
type OrderedMap []KV

// MarshalJSON implements json.Marshaler.
func (m OrderedMap) MarshalJSON() ([]byte, error) {
	out := []byte{'{'}
	for i, kv := range m {
		if i > 0 {
			out = append(out, ',')
		}
		k, err := json.Marshal(kv.Key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(kv.Val)
		if err != nil {
			return nil, err
		}
		out = append(out, k...)
		out = append(out, ':')
		out = append(out, v...)
	}
	return append(out, '}'), nil
}

// BinaryValue is the Value of a binary entry.
type BinaryValue struct {
	Bytes int64 `json:"bytes"`
}
