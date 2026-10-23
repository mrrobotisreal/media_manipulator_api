package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func ffprobeArgs(path string) []string {
	return []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "-show_chapters", "-show_program_version", path}
}

// foldFFprobe appends ffprobe output as synthetic groups. Failure adds a
// warning and leaves the document as is.
func (i *Inspector) foldFFprobe(ctx context.Context, path string, doc *MetadataDocument) {
	stdout, stderr, err := i.run(ctx, "ffprobe", ffprobeArgs(path)...)
	if w := toolWarning("ffprobe", stderr, err); w != "" {
		doc.Warnings = append(doc.Warnings, w)
		return
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.UseNumber()
	root, perr := decodeTopObject(dec)
	if perr != nil {
		doc.Warnings = append(doc.Warnings, fmt.Sprintf(warnUnparseable, "ffprobe"))
		return
	}
	entries, version := foldFFprobeDoc(root)
	doc.Entries = append(doc.Entries, entries...)
	if version != "" {
		doc.Tools = append(doc.Tools, "ffprobe "+version)
	} else {
		doc.Tools = append(doc.Tools, "ffprobe")
	}
}

func decodeTopObject(dec *json.Decoder) (OrderedMap, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected object")
	}
	return decodeObjectBody(dec)
}

func (m OrderedMap) get(key string) (any, bool) {
	for _, kv := range m {
		if kv.Key == key {
			return kv.Val, true
		}
	}
	return nil, false
}

func asMap(v any) OrderedMap { m, _ := v.(OrderedMap); return m }
func asList(v any) []any     { l, _ := v.([]any); return l }

// foldFFprobeDoc turns the decoded ffprobe JSON into entries: Format, then each
// Stream:<n> (StreamType first), Chapter:<n>, and SideData last. Pure.
func foldFFprobeDoc(root OrderedMap) (entries []MetadataEntry, version string) {
	add := func(group, tag string, val any, category string) {
		e := MetadataEntry{ID: group + ":" + tag, Group: group, Tag: tag, Value: val}
		cat, priv := Classify(group, tag)
		e.Category, e.Privacy = cat, priv
		if category != "" {
			e.Category = category
		}
		entries = append(entries, e)
	}
	// addFields emits scalar/nested fields; "tags" become <group>:<tag> entries.
	addFields := func(group string, fields OrderedMap, category string, skip map[string]bool) {
		for _, kv := range fields {
			if skip[kv.Key] || kv.Key == "tags" {
				continue
			}
			add(group, kv.Key, kv.Val, category)
		}
		for _, kv := range asMap(mustGet(fields, "tags")) {
			add(group, kv.Key, kv.Val, category)
		}
	}

	if pv, ok := root.get("program_version"); ok {
		if v, ok := asMap(pv).get("version"); ok {
			version = fmt.Sprint(v)
		}
	}
	if f, ok := root.get("format"); ok {
		addFields("Format", asMap(f), "container", map[string]bool{"filename": true})
	}

	type sideItem struct {
		stream string
		item   OrderedMap
	}
	var sides []sideItem
	streams, _ := root.get("streams")
	for pos, s := range asList(streams) {
		sm := asMap(s)
		idx := strconv.Itoa(pos)
		if v, ok := sm.get("index"); ok {
			idx = fmt.Sprint(v)
		}
		group := "Stream:" + idx
		ctype := ""
		if v, ok := sm.get("codec_type"); ok {
			ctype = fmt.Sprint(v)
		}
		cat := streamCategory(ctype)
		add(group, "StreamType", ctype, cat)
		addFields(group, sm, cat, map[string]bool{"side_data_list": true})
		for _, sd := range asList(mustGet(sm, "side_data_list")) {
			sides = append(sides, sideItem{idx, asMap(sd)})
		}
	}

	chapters, _ := root.get("chapters")
	for pos, c := range asList(chapters) {
		addFields("Chapter:"+strconv.Itoa(pos), asMap(c), "container", nil)
	}

	seen := newIDAllocator()
	for _, s := range sides {
		typ := "unknown"
		rest := OrderedMap{}
		for _, kv := range s.item {
			if kv.Key == "side_data_type" {
				typ = fmt.Sprint(kv.Val)
				continue
			}
			rest = append(rest, kv)
		}
		tag := s.stream + ":" + typ
		entries = append(entries, MetadataEntry{
			ID: seen.next("SideData", tag), Group: "SideData", Tag: tag, Value: rest, Category: "video",
		})
	}
	return entries, version
}

func mustGet(m OrderedMap, key string) any { v, _ := m.get(key); return v }

func streamCategory(codecType string) string {
	switch codecType {
	case "video":
		return "video"
	case "audio":
		return "audio"
	}
	return "container"
}
