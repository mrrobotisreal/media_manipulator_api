package vector

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// MM110: the Motion Templates catalog (UI lib/vector/templates.ts) produces
// documents the sanitizer keeps byte-identical. testdata/templates/*.json are
// byte copies of the UI's lib/vector/fixtures/templates/ goldens.
func TestTemplateGoldens(t *testing.T) {
	files, _ := filepath.Glob("testdata/templates/*.json")
	if len(files) != 6 {
		t.Fatalf("found %d template goldens, want 6", len(files))
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			d, err := Sanitize(b)
			if err != nil {
				t.Fatalf("sanitize: %v", err)
			}
			if !bytes.Equal(d.CanonicalIndent(), b) {
				t.Fatal("the sanitizer changed a template document")
			}
		})
	}
}

// The goldens match the UI's copy (skipped when the sibling checkout is absent).
func TestTemplateGoldensMatchUI(t *testing.T) {
	ui := os.Getenv("MEDI_UI_TEMPLATE_GOLDENS")
	if ui == "" {
		ui = filepath.Join("..", "..", "..", "media-manipulator-ui", "lib", "vector", "fixtures", "templates")
	}
	files, _ := filepath.Glob(filepath.Join(ui, "*.json"))
	if len(files) == 0 {
		t.Skipf("UI goldens not found (%s)", ui)
	}
	for _, f := range files {
		want, _ := os.ReadFile(f)
		got, err := os.ReadFile(filepath.Join("testdata", "templates", filepath.Base(f)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("testdata/templates/%s differs from the UI golden: copy lib/vector/fixtures/templates/*.json", filepath.Base(f))
		}
	}
}
