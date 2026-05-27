package services

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestPNG(t *testing.T, dir string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 7 % 256), G: uint8(y * 5 % 256), B: 120, A: 255})
		}
	}
	path := filepath.Join(dir, "in.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return path
}

func writeTestJPEG(t *testing.T, dir string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: uint8(x % 256), B: uint8(y % 256), A: 255})
		}
	}
	path := filepath.Join(dir, "in.jpg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create jpg: %v", err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpg: %v", err)
	}
	return path
}

func assertValidPDF(t *testing.T, data []byte) {
	t.Helper()
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF- header")
	}
	if !bytes.Contains(data, []byte("/Type /Catalog")) {
		t.Fatalf("output missing catalog object")
	}
	if !bytes.Contains(data, []byte("/Subtype /Image")) {
		t.Fatalf("output missing image XObject")
	}
	if !bytes.Contains(data, []byte("startxref")) || !bytes.Contains(data, []byte("%%EOF")) {
		t.Fatalf("output missing xref/EOF trailer")
	}
}

func TestImageToPDFBytesPNGUsesFlate(t *testing.T) {
	dir := t.TempDir()
	in := writeTestPNG(t, dir, 24, 16)
	data, err := imageToPDFBytes(context.Background(), in, 90)
	if err != nil {
		t.Fatalf("imageToPDFBytes: %v", err)
	}
	assertValidPDF(t, data)
	if !bytes.Contains(data, []byte("/FlateDecode")) {
		t.Fatalf("PNG input should embed via /FlateDecode")
	}
}

func TestImageToPDFBytesJPEGUsesDCT(t *testing.T) {
	dir := t.TempDir()
	in := writeTestJPEG(t, dir, 24, 16)
	data, err := imageToPDFBytes(context.Background(), in, 90)
	if err != nil {
		t.Fatalf("imageToPDFBytes: %v", err)
	}
	assertValidPDF(t, data)
	if !bytes.Contains(data, []byte("/DCTDecode")) {
		t.Fatalf("JPEG input should embed via /DCTDecode")
	}
}

func TestBuildImagePDFRejectsBadDimensions(t *testing.T) {
	if _, err := buildImagePDF(0, 10, "/DeviceRGB", "/FlateDecode", []byte{1}, 96); err == nil {
		t.Fatalf("expected error for zero width")
	}
}

func TestImageToPDFEmptyInput(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.png")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := imageToPDFBytes(context.Background(), p, 90); err == nil {
		t.Fatalf("expected error for empty input")
	}
}

func TestParsePDFRenderOptionsDefaults(t *testing.T) {
	o := parsePDFRenderOptions(nil)
	if o.Format != "jpg" || o.PageSelection != "all" || o.DPI != 150 || o.Quality != 90 {
		t.Fatalf("unexpected defaults: %+v", o)
	}
}

func TestParsePDFRenderOptionsParsesAndClamps(t *testing.T) {
	o := parsePDFRenderOptions(map[string]interface{}{
		"format":        "png",
		"pageSelection": "first",
		"dpi":           float64(9000),
		"quality":       float64(200),
	})
	if o.Format != "png" {
		t.Errorf("format = %q, want png", o.Format)
	}
	if o.PageSelection != "first" {
		t.Errorf("pageSelection = %q, want first", o.PageSelection)
	}
	if o.DPI != 400 {
		t.Errorf("dpi = %d, want clamped 400", o.DPI)
	}
	if o.Quality != 100 {
		t.Errorf("quality = %d, want clamped 100", o.Quality)
	}
}

func TestParsePDFRenderOptionsLowDPIClamp(t *testing.T) {
	o := parsePDFRenderOptions(map[string]interface{}{"dpi": float64(10)})
	if o.DPI != 50 {
		t.Errorf("dpi = %d, want clamped 50", o.DPI)
	}
}

// Sanity check that flattenToRGB composites transparency over white.
func TestFlattenToRGBTransparentOverWhite(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0, G: 0, B: 0, A: 0}) // fully transparent
	rgb, w, h := flattenToRGB(img)
	if w != 1 || h != 1 || len(rgb) != 3 {
		t.Fatalf("unexpected dims w=%d h=%d len=%d", w, h, len(rgb))
	}
	if rgb[0] != 0xff || rgb[1] != 0xff || rgb[2] != 0xff {
		t.Fatalf("transparent pixel should flatten to white, got %v", rgb)
	}
}

func TestImageToPDFRoundTripsThroughFile(t *testing.T) {
	dir := t.TempDir()
	in := writeTestPNG(t, dir, 12, 12)
	data, err := imageToPDFBytes(context.Background(), in, 80)
	if err != nil {
		t.Fatalf("imageToPDFBytes: %v", err)
	}
	out := filepath.Join(dir, "out.pdf")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "%PDF-") {
		t.Fatalf("written file is not a PDF")
	}
}
