package services

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func TestNormalizeICOSizesDefault(t *testing.T) {
	got := normalizeICOSizes(nil)
	want := []int{16, 32, 48, 64, 128, 256}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default sizes = %v, want %v", got, want)
	}
}

func TestNormalizeICOSizesFiltersAndDedupes(t *testing.T) {
	got := normalizeICOSizes([]int{32, 32, 0, 512, 16, 257, 48})
	want := []int{16, 32, 48}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered sizes = %v, want %v", got, want)
	}
}

func TestNormalizeICOSizesAllInvalidFallsBack(t *testing.T) {
	got := normalizeICOSizes([]int{0, 512, -3})
	if !reflect.DeepEqual(got, defaultICOSizes) {
		t.Fatalf("all-invalid should fall back to default, got %v", got)
	}
}

func TestNormalizeVectorizeParamsDefaults(t *testing.T) {
	th, turd := normalizeVectorizeParams(nil)
	if th != 50 || turd != 2 {
		t.Fatalf("defaults = (%d,%d), want (50,2)", th, turd)
	}
}

func TestNormalizeVectorizeParamsClamps(t *testing.T) {
	th, turd := normalizeVectorizeParams(&models.VectorizeOptions{Threshold: 0, TurdSize: 5000})
	if th != 50 {
		t.Errorf("threshold out of range should default to 50, got %d", th)
	}
	if turd != 2 {
		t.Errorf("turd out of range should default to 2, got %d", turd)
	}
	th2, turd2 := normalizeVectorizeParams(&models.VectorizeOptions{Threshold: 70, TurdSize: 10})
	if th2 != 70 || turd2 != 10 {
		t.Fatalf("valid params = (%d,%d), want (70,10)", th2, turd2)
	}
}

func TestIsSVGInputByExtension(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "logo.svg")
	if err := os.WriteFile(p, []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isSVGInput(p) {
		t.Fatalf("expected .svg file to be detected as SVG")
	}
}

func TestIsSVGInputBySniff(t *testing.T) {
	dir := t.TempDir()
	// No .svg extension, but content is clearly SVG.
	p := filepath.Join(dir, "graphic.txt")
	if err := os.WriteFile(p, []byte("<?xml version=\"1.0\"?>\n<svg width=\"10\" height=\"10\"></svg>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isSVGInput(p) {
		t.Fatalf("expected SVG content to be sniffed as SVG")
	}
}

func TestIsSVGInputNegative(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "photo.png")
	// PNG magic bytes.
	if err := os.WriteFile(p, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, 0o644); err != nil {
		t.Fatal(err)
	}
	if isSVGInput(p) {
		t.Fatalf("PNG should not be detected as SVG")
	}
}
