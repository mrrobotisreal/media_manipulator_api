package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeIngest(w, h int, alpha bool, calls *[][]string) *PhotoIngest {
	return &PhotoIngest{
		probe:    func(context.Context, string) (int, int, error) { return w, h, nil },
		hasAlpha: func(context.Context, string) bool { return alpha },
		magick:   func(_ context.Context, args ...string) error { *calls = append(*calls, args); return nil },
	}
}

func TestPhotoIngestMakesProxyThumbAndFolders(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "original.heic")
	var calls [][]string
	res, err := fakeIngest(4000, 3000, false, &calls).Ingest(context.Background(), dir, orig, 120)
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 4000 || res.Height != 3000 || res.HasAlpha || res.Ext != "jpg" {
		t.Fatalf("result %+v", res)
	}
	if len(calls) != 2 {
		t.Fatalf("magick calls = %d", len(calls))
	}
	// Byte-identical to Darkroom's argument lists.
	wantProxy := buildStudioImageProxyArgs(orig, filepath.Join(dir, "proxy.jpg"), false)
	wantThumb := buildStudioImageThumbArgs(orig, filepath.Join(dir, "thumb.jpg"), false)
	if strings.Join(calls[0], "\x00") != strings.Join(wantProxy, "\x00") || strings.Join(calls[1], "\x00") != strings.Join(wantThumb, "\x00") {
		t.Fatalf("args differ from the studio helpers:\n%v\n%v", calls[0], calls[1])
	}
	for _, sub := range []string{"masks", "bases"} {
		if st, err := os.Stat(filepath.Join(dir, sub)); err != nil || !st.IsDir() {
			t.Fatalf("%s/ missing", sub)
		}
	}
}

func TestPhotoIngestAlphaMakesPNG(t *testing.T) {
	dir := t.TempDir()
	var calls [][]string
	res, err := fakeIngest(100, 100, true, &calls).Ingest(context.Background(), dir, filepath.Join(dir, "original.png"), 120)
	if err != nil || res.Ext != "png" || !res.HasAlpha {
		t.Fatalf("%+v %v", res, err)
	}
	if last := calls[0][len(calls[0])-1]; !strings.HasSuffix(last, "proxy.png") {
		t.Fatalf("proxy out = %s", last)
	}
}

func TestPhotoIngestRefusesOverMegapixelCeiling(t *testing.T) {
	var calls [][]string
	_, err := fakeIngest(12000, 10001, false, &calls).Ingest(context.Background(), t.TempDir(), "x", 120)
	var tm *PhotoTooManyPixelsError
	if !errors.As(err, &tm) || tm.MaxMegapixels != 120 {
		t.Fatalf("err = %v", err)
	}
	if len(calls) != 0 {
		t.Fatal("no proxy may be made before the ceiling check")
	}
	// Exactly at the ceiling is fine.
	if _, err := fakeIngest(12000, 10000, false, &calls).Ingest(context.Background(), t.TempDir(), "x", 120); err != nil {
		t.Fatal(err)
	}
}

func TestPhotoIngestPropagatesFailures(t *testing.T) {
	p := &PhotoIngest{
		probe:    func(context.Context, string) (int, int, error) { return 10, 10, nil },
		hasAlpha: func(context.Context, string) bool { return false },
		magick:   func(context.Context, ...string) error { return errors.New("boom") },
	}
	if _, err := p.Ingest(context.Background(), t.TempDir(), "x", 120); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	p.probe = func(context.Context, string) (int, int, error) { return 0, 0, errors.New("bad header") }
	if _, err := p.Ingest(context.Background(), t.TempDir(), "x", 120); err == nil {
		t.Fatal("probe failure must fail the ingest")
	}
}

func TestParsePhotoGeometry(t *testing.T) {
	for _, c := range []struct {
		in   string
		w, h int
		bad  bool
	}{
		{"4000 3000 1", 4000, 3000, false},
		{"4000 3000", 4000, 3000, false},
		{"4000 3000 6", 3000, 4000, false},
		{"4000 3000 8\n", 3000, 4000, false},
		{"4000 3000 3", 4000, 3000, false},
		{"4000 3000 Undefined", 4000, 3000, false},
		{"", 0, 0, true},
		{"x y", 0, 0, true},
	} {
		w, h, err := parsePhotoGeometry(c.in)
		if (err != nil) != c.bad || w != c.w || h != c.h {
			t.Errorf("%q -> %d %d %v", c.in, w, h, err)
		}
	}
}
