package photo

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The UI (MM97) authors golden-docs/, images/ and goldens/ and this repo copies
// them byte for byte. Every golden doc must already be canonical for the Go
// sanitizer, and every doc must have a golden for each fixture image, so
// photo_render.py --selftest (MM96) has a complete set to check against.
func TestPhotoGoldenDocsCanonical(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "golden-docs", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no golden docs: %v", err)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			want, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Sanitize(want, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(append(got, '\n'), want) {
				t.Fatalf("golden doc not canonical:\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestPhotoGoldensComplete(t *testing.T) {
	images, _ := filepath.Glob(filepath.Join("testdata", "images", "*.png"))
	goldens, _ := filepath.Glob(filepath.Join("testdata", "goldens", "*.png"))
	if len(images) == 0 || len(goldens) == 0 {
		t.Fatal("missing fixture images or goldens")
	}
	have := map[string]bool{}
	docs := map[string]bool{}
	for _, g := range goldens {
		name := strings.TrimSuffix(filepath.Base(g), ".png")
		have[name] = true
		docs[name[:strings.LastIndex(name, ".")]] = true
	}
	for d := range docs {
		inDocs := fileExists(filepath.Join("testdata", "docs", d+".json"))
		inGolden := fileExists(filepath.Join("testdata", "golden-docs", d+".json"))
		if !inDocs && !inGolden {
			t.Errorf("golden for unknown doc %q", d)
		}
		for _, img := range images {
			key := d + "." + strings.TrimSuffix(filepath.Base(img), ".png")
			if !have[key] {
				t.Errorf("missing golden %s.png", key)
			}
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
