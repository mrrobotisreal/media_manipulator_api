package metadata

import (
	"encoding/json"
	"os"
	"testing"
)

func categorySet() map[string]bool {
	m := map[string]bool{}
	for _, c := range Categories {
		m[c] = true
	}
	return m
}

func TestCatalogRowsMapToKnownCategories(t *testing.T) {
	if len(catalogRows) < 1000 {
		t.Fatalf("catalog suspiciously small: %d rows", len(catalogRows))
	}
	cats := categorySet()
	privs := map[string]bool{"": true, PrivacyLocation: true, PrivacyIdentity: true, PrivacyDevice: true, PrivacyHistory: true}
	for _, r := range catalogRows {
		if !cats[r.Category] {
			t.Errorf("%s:%s has unknown category %q", r.Group, r.Tag, r.Category)
		}
		if !privs[r.Privacy] {
			t.Errorf("%s:%s has unknown privacy %q", r.Group, r.Tag, r.Privacy)
		}
	}
}

func TestClassifyTable(t *testing.T) {
	cases := []struct{ group, tag, cat, priv string }{
		{"IFD0", "ModifyDate", "image", ""},
		{"XMP-xmp", "ModifyDate", "datetime", ""},
		{"IFD0", "Make", "image", PrivacyDevice},
		{"ExifIFD", "BodySerialNumber", "camera", PrivacyDevice},
		{"ExifIFD", "ISO", "camera", ""},
		{"ExifIFD", "LensSerialNumber", "lens", PrivacyDevice},
		{"GPS", "GPSLatitude", "gps", PrivacyLocation},
		{"GPS", "SomethingNew", "gps", PrivacyLocation},
		{"xmp-dc", "Creator", "xmp", PrivacyIdentity},
		{"XMP-dc", "Title", "xmp", ""},
		{"XMP-xmpMM", "DocumentID", "history", PrivacyIdentity},
		{"XMP-xmpMM", "NeverHeardOfIt", "history", ""},
		{"XMP-GPano", "ProjectionType", "pano", ""},
		{"Canon", "SerialNumber", "makernote", PrivacyDevice},
		{"Nikon", "TotallyUnknown", "makernote", ""},
		{"Keys", "com.apple.quicktime.make", "apple", PrivacyDevice},
		{"Keys", "com.apple.quicktime.location.ISO6709", "apple", PrivacyLocation},
		{"QuickTime", "com.apple.quicktime.something.new", "apple", ""},
		{"Track1", "HandlerDescription", "container", PrivacyIdentity},
		{"PDF", "Author", "pdf", PrivacyIdentity},
		{"PDF", "Producer", "pdf", PrivacyIdentity},
		{"ICC-header", "ProfileCMMType", "color", ""},
		{"ICC_Profile", "ProfileDescription", "color", ""},
		{"File", "FileType", "file", ""},
		{"Composite", "ImageSize", "composite", ""},
		{"Format", "duration", "container", ""},
		{"Stream:0", "StreamType", "container", ""},
		{"Stream:3", "width", "container", ""},
		{"SideData", "0:Display Matrix", "video", ""},
		{"Chapter:2", "title", "container", ""},
		{"C2PA", "Whatever", "c2pa", ""},
		{"IFD1", "ThumbnailImage", "thumbnail", PrivacyHistory},
	}
	for _, c := range cases {
		cat, priv := Classify(c.group, c.tag)
		if cat != c.cat || priv != c.priv {
			t.Errorf("Classify(%q,%q) = (%q,%q), want (%q,%q)", c.group, c.tag, cat, priv, c.cat, c.priv)
		}
	}
}

func TestClassifyUnknownGroupIsOther(t *testing.T) {
	for _, g := range []string{"", "NoSuchGroup", "Zzz9"} {
		if cat, priv := Classify(g, "Anything"); cat != "other" || priv != "" {
			t.Errorf("Classify(%q) = %q,%q", g, cat, priv)
		}
	}
}

func TestClassifyGroupCaseInsensitive(t *testing.T) {
	a, ap := Classify("exififd", "BodySerialNumber")
	b, bp := Classify("ExifIFD", "BodySerialNumber")
	if a != b || ap != bp {
		t.Fatalf("case differs: %s/%s vs %s/%s", a, ap, b, bp)
	}
}

// Every classified result is in the fixed category list, over all fixtures.
func TestFixtureEntriesUseKnownCategories(t *testing.T) {
	cats := categorySet()
	for _, f := range fixtures {
		doc := inspectDoc(t, f.name, f.file, f.kind)
		for _, e := range doc.Entries {
			if !cats[e.Category] {
				t.Errorf("%s: %s has category %q", f.name, e.ID, e.Category)
			}
		}
	}
}

func TestCatalogJSONRoundTrip(t *testing.T) {
	b, err := os.ReadFile("catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Categories    []struct {
			ID       string `json:"id"`
			TitleKey string `json:"titleKey"`
		} `json:"categories"`
		Tags []CatalogRow `json:"tags"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != 1 || len(doc.Categories) != len(Categories) || len(doc.Tags) != len(catalogRows) {
		t.Fatalf("schema %d cats %d tags %d (want rows %d)", doc.SchemaVersion, len(doc.Categories), len(doc.Tags), len(catalogRows))
	}
	for i, r := range doc.Tags {
		g := catalogRows[i]
		if r.Group != g.Group || r.Tag != g.Tag || r.Category != g.Category || r.Privacy != g.Privacy || r.Unverified != g.Unverified || r.Description != g.Description {
			t.Fatalf("row %d differs between catalog.json and catalog_gen.go: %+v vs %+v", i, r, g)
		}
	}
}

func TestUnverifiedSectionsFlagged(t *testing.T) {
	n := 0
	for _, r := range catalogRows {
		if r.Unverified {
			n++
		}
		if (r.Section == "2.1" || r.Section == "2.12") && !r.Unverified {
			t.Errorf("%s should be unverified", r.Tag)
		}
	}
	if n == 0 {
		t.Fatal("no unverified rows flagged")
	}
}
