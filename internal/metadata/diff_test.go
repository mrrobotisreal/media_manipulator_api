package metadata

import (
	"encoding/json"
	"testing"
)

func ent(id, cat string, v any) MetadataEntry {
	g, t, _ := splitKey(id)
	if i := indexByte(t, '#'); i >= 0 {
		t = t[:i]
	}
	return MetadataEntry{ID: id, Group: g, Tag: t, Value: v, Category: cat}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func docOf(es ...MetadataEntry) *MetadataDocument {
	return &MetadataDocument{SchemaVersion: 1, Entries: es}
}

func TestDiffTable(t *testing.T) {
	bin := func(n int64) any { return BinaryValue{Bytes: n} }
	cases := []struct {
		name                         string
		a, b                         []MetadataEntry
		removed, added, changed, uns int
	}{
		{"identical", []MetadataEntry{ent("IFD0:Make", "camera", "Apple")}, []MetadataEntry{ent("IFD0:Make", "camera", "Apple")}, 0, 0, 0, 1},
		{"removed", []MetadataEntry{ent("GPS:GPSLatitude", "gps", "1"), ent("IFD0:Make", "camera", "A")}, []MetadataEntry{ent("IFD0:Make", "camera", "A")}, 1, 0, 0, 1},
		{"added", nil, []MetadataEntry{ent("XMP-xmp:CreatorTool", "xmp", "x")}, 0, 1, 0, 0},
		{"changed", []MetadataEntry{ent("IFD0:Make", "camera", "A")}, []MetadataEntry{ent("IFD0:Make", "camera", "B")}, 0, 0, 1, 0},
		{"float sum", []MetadataEntry{ent("X:A", "other", 0.1+0.2)}, []MetadataEntry{ent("X:A", "other", 0.3)}, 0, 0, 0, 1},
		{"json.Number vs string", []MetadataEntry{ent("X:A", "other", json.Number("1.50"))}, []MetadataEntry{ent("X:A", "other", "1.5")}, 0, 0, 0, 1},
		{"numeric differs", []MetadataEntry{ent("X:A", "other", 1.0)}, []MetadataEntry{ent("X:A", "other", 1.001)}, 0, 0, 1, 0},
		{"trailing space", []MetadataEntry{ent("ExifIFD:ExposureTime", "camera", "1/250")}, []MetadataEntry{ent("ExifIFD:ExposureTime", "camera", "1/250 ")}, 0, 0, 0, 1},
		{"inner whitespace", []MetadataEntry{ent("X:A", "other", "a  b")}, []MetadataEntry{ent("X:A", "other", "a b")}, 0, 0, 0, 1},
		{"case sensitive", []MetadataEntry{ent("X:A", "other", "abc")}, []MetadataEntry{ent("X:A", "other", "ABC")}, 0, 0, 1, 0},
		{"duplicate ids", []MetadataEntry{ent("XMP:Subject", "xmp", "a"), ent("XMP:Subject#2", "xmp", "b")}, []MetadataEntry{ent("XMP:Subject", "xmp", "a")}, 1, 0, 0, 1},
		{"array leaves", []MetadataEntry{ent("X:A", "other", []any{"a ", json.Number("2")})}, []MetadataEntry{ent("X:A", "other", []any{"a", "2.0"})}, 0, 0, 0, 1},
		{"array len differs", []MetadataEntry{ent("X:A", "other", []any{"a"})}, []MetadataEntry{ent("X:A", "other", []any{"a", "b"})}, 0, 0, 1, 0},
		{"struct", []MetadataEntry{ent("X:A", "other", OrderedMap{{"k", "v "}, {"n", 1}})}, []MetadataEntry{ent("X:A", "other", OrderedMap{{"n", 1.0}, {"k", "v"}})}, 0, 0, 0, 1},
		{"binary same length", []MetadataEntry{{ID: "IFD1:ThumbnailImage", Value: bin(10), Binary: true, Category: "thumbnail"}}, []MetadataEntry{{ID: "IFD1:ThumbnailImage", Value: bin(10), Binary: true, Category: "thumbnail"}}, 0, 0, 0, 1},
		{"binary diff length", []MetadataEntry{{ID: "IFD1:ThumbnailImage", Value: bin(10), Binary: true, Category: "thumbnail"}}, []MetadataEntry{{ID: "IFD1:ThumbnailImage", Value: bin(11), Binary: true, Category: "thumbnail"}}, 0, 0, 1, 0},
		{"binary decoded from cache", []MetadataEntry{{ID: "B:T", Value: bin(5), Binary: true}}, []MetadataEntry{{ID: "B:T", Value: map[string]any{"bytes": float64(5)}, Binary: true}}, 0, 0, 0, 1},
		{"empty docs", nil, nil, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Diff(docOf(tc.a...), docOf(tc.b...))
			if len(d.Removed) != tc.removed || len(d.Added) != tc.added || len(d.Changed) != tc.changed || d.Unchanged != tc.uns {
				t.Fatalf("got r=%d a=%d c=%d u=%d, want %d %d %d %d", len(d.Removed), len(d.Added), len(d.Changed), d.Unchanged, tc.removed, tc.added, tc.changed, tc.uns)
			}
			var sr, sa, sc, su int
			for _, c := range d.ByCategory {
				sr, sa, sc, su = sr+c.Removed, sa+c.Added, sc+c.Changed, su+c.Unchanged
			}
			if sr != tc.removed || sa != tc.added || sc != tc.changed || su != tc.uns {
				t.Fatalf("byCategory sums %d %d %d %d != totals", sr, sa, sc, su)
			}
		})
	}
}

func TestDiffNilDocsAndEmptyJSONArrays(t *testing.T) {
	d := Diff(nil, nil)
	b, _ := json.Marshal(d)
	if string(b) != `{"removed":[],"added":[],"changed":[],"unchanged":0,"byCategory":{}}` {
		t.Fatalf("got %s", b)
	}
}

func TestDiffOrderingAndCategories(t *testing.T) {
	orig := docOf(
		ent("A:1", "gps", "x"), ent("A:2", "gps", "y"), ent("B:1", "camera", "1"), ent("B:2", "camera", "2"), ent("C:1", "other", "same"),
	)
	res := docOf(
		ent("Z:9", "xmp", "n1"), ent("B:2", "color", "two"), ent("B:1", "camera", "uno"), ent("C:1", "other", "same"), ent("Y:8", "xmp", "n2"),
	)
	d1, _ := json.Marshal(Diff(orig, res))
	d2, _ := json.Marshal(Diff(orig, res))
	if string(d1) != string(d2) {
		t.Fatal("not deterministic")
	}
	d := Diff(orig, res)
	if d.Removed[0].ID != "A:1" || d.Removed[1].ID != "A:2" {
		t.Fatalf("removed order %v", d.Removed)
	}
	if d.Changed[0].ID != "B:1" || d.Changed[1].ID != "B:2" {
		t.Fatalf("changed follows original order: %v", d.Changed)
	}
	if d.Added[0].ID != "Z:9" || d.Added[1].ID != "Y:8" {
		t.Fatalf("added follows result order: %v", d.Added)
	}
	if d.ByCategory["gps"].Removed != 2 {
		t.Fatalf("removed uses original category: %+v", d.ByCategory)
	}
	if d.ByCategory["color"].Changed != 1 || d.ByCategory["camera"].Changed != 1 {
		t.Fatalf("changed uses result category: %+v", d.ByCategory)
	}
	if d.ByCategory["xmp"].Added != 2 || d.ByCategory["other"].Unchanged != 1 {
		t.Fatalf("%+v", d.ByCategory)
	}
}

func TestDiffExpectedChangesStillReported(t *testing.T) {
	orig := docOf(ent("System:FileName", "file", "a.jpg"), ent("Composite:ImageSize", "composite", "4000x3000"), ent("System:FileSize", "file", "5 MB"))
	res := docOf(ent("System:FileName", "file", "converted.jpg"), ent("Composite:ImageSize", "composite", "2000x1500"), ent("System:FileSize", "file", "1 MB"))
	d := Diff(orig, res)
	if len(d.Changed) != 3 {
		t.Fatalf("expected 3 changes: %+v", d.Changed)
	}
	for _, c := range d.Changed {
		if !IsExpectedChange(c.ID) {
			t.Fatalf("%s should be flagged expected", c.ID)
		}
	}
	for _, id := range []string{"File:FileType", "System:FileTypeExtension", "System:MIMEType", "File:FileModifyDate", "System:FileName#2"} {
		if !IsExpectedChange(id) {
			t.Fatalf("%s should be expected", id)
		}
	}
	for _, id := range []string{"IFD0:Make", "GPS:GPSLatitude", "Composite:GPSPosition", "bad"} {
		if IsExpectedChange(id) {
			t.Fatalf("%s must not be expected", id)
		}
	}
}

func TestDiffSampleJSON(t *testing.T) {
	orig := docOf(ent("IFD0:Make", "camera", "Apple"), ent("GPS:GPSLatitude", "gps", "40.7 N"), ent("System:FileSize", "file", "5 MB"), ent("Composite:ImageSize", "composite", "10x10"))
	res := docOf(ent("IFD0:Make", "camera", "Apple"), ent("System:FileSize", "file", "1 MB"), ent("PNG:Software", "png", "mm"))
	b, _ := json.MarshalIndent(Diff(orig, res), "", "  ")
	t.Logf("DiffResult sample:\n%s", b)
}
