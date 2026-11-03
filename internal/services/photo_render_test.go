package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

type fakePhotoRunner struct {
	specs  []cmdaudit.Spec
	stdout string
	stderr string
	exit   int
	err    error
	write  string // path written on success (the "rendered.png")
}

func (f *fakePhotoRunner) Run(_ context.Context, spec cmdaudit.Spec) (cmdaudit.Result, error) {
	f.specs = append(f.specs, spec)
	if spec.Stdout != nil && f.stdout != "" {
		_, _ = spec.Stdout.Write([]byte(f.stdout))
	}
	if f.err == nil {
		for i, a := range spec.Args {
			if a == "--out" {
				_ = os.WriteFile(spec.Args[i+1], []byte("png"), 0o644)
			}
		}
	}
	return cmdaudit.Result{ExitCode: f.exit, Stderr: f.stderr, Success: f.err == nil, Err: f.err}, f.err
}

type photoRenderRig struct {
	r      *PhotoRenderer
	runner *fakePhotoRunner
	magick [][]string
	meta   []string
	dir    string
}

func newPhotoRenderRig(t *testing.T) *photoRenderRig {
	t.Helper()
	cfg := config.Load()
	cfg.PhotoPython = "/opt/ai/venvs/photo/bin/python"
	cfg.PhotoRenderScript = "/opt/ai/scripts/photo_render.py"
	cfg.TempDir = t.TempDir()
	rig := &photoRenderRig{runner: &fakePhotoRunner{}, dir: t.TempDir()}
	r := NewPhotoRenderer(cfg, nil, nil)
	r.runner = rig.runner
	r.magick = func(_ context.Context, args ...string) error {
		rig.magick = append(rig.magick, append([]string{}, args...))
		// the encode call writes its last argument
		_ = os.WriteFile(args[len(args)-1][strings.LastIndex(args[len(args)-1], ":")+1:], []byte("x"), 0o644)
		return nil
	}
	r.copyMeta = func(_ context.Context, original, out string) error {
		rig.meta = append(rig.meta, "copy:"+filepath.Base(original))
		return nil
	}
	r.strip = func(_ context.Context, out string) error { rig.meta = append(rig.meta, "strip"); return nil }
	rig.r = r
	return rig
}

func (rig *photoRenderRig) req(format, mode string, mp int) PhotoExportRequest {
	return PhotoExportRequest{
		JobID: "job-1", Input: "/up/original.jpg", OriginalPath: "/up/original.jpg", Doc: []byte(`{"version":1}`),
		MasksDir: "/proj/masks", OutPath: filepath.Join(rig.dir, "out", "Beach (retouched)."+format),
		Format: format, Quality: 88, MetadataMode: mode, MaxMegapixels: mp,
	}
}

// The renderer's argv is the contract with scripts/server/photo_render.py.
func TestPhotoExportArgv(t *testing.T) {
	rig := newPhotoRenderRig(t)
	if err := rig.r.RenderExport(context.Background(), rig.req("jpg", "keep", 12)); err != nil {
		t.Fatal(err)
	}
	if len(rig.runner.specs) != 1 {
		t.Fatalf("specs = %d", len(rig.runner.specs))
	}
	s := rig.runner.specs[0]
	if s.Executable != "/opt/ai/venvs/photo/bin/python" || s.Tool != "photo_render" || s.JobID != "job-1" {
		t.Fatalf("spec = %+v", s)
	}
	args := s.Args
	want := []string{"/opt/ai/scripts/photo_render.py", "--input", args[2], "--doc", args[4], "--masks", "/proj/masks", "--out", args[8], "--max-megapixels", "12", "--progress"}
	if !reflect.DeepEqual(args, want) || !strings.HasSuffix(args[2], "base.png") || !strings.HasSuffix(args[4], "doc.json") || !strings.HasSuffix(args[8], "rendered.png") {
		t.Fatalf("argv = %v", args)
	}
	// No cap -> no --max-megapixels flag.
	rig2 := newPhotoRenderRig(t)
	_ = rig2.r.RenderExport(context.Background(), rig2.req("png", "keep", 0))
	for _, a := range rig2.runner.specs[0].Args {
		if a == "--max-megapixels" {
			t.Fatalf("flag present without a cap: %v", rig2.runner.specs[0].Args)
		}
	}
}

func TestPhotoExportEncodeArgsPerFormat(t *testing.T) {
	cases := []struct {
		format string
		has    []string
		not    []string
	}{
		{"jpg", []string{"-quality", "88", "-alpha", "remove", "-background", "white"}, nil},
		{"webp", []string{"-quality", "88", "-depth", "8"}, []string{"-alpha"}},
		{"avif", []string{"-quality", "88"}, nil},
		{"tiff", []string{"-depth", "8", "-compress", "zip"}, []string{"-quality"}},
		{"png", []string{"-depth", "8"}, []string{"-quality", "-alpha"}},
	}
	for _, tc := range cases {
		rig := newPhotoRenderRig(t)
		if err := rig.r.RenderExport(context.Background(), rig.req(tc.format, "keep", 0)); err != nil {
			t.Fatalf("%s: %v", tc.format, err)
		}
		if len(rig.magick) != 2 {
			t.Fatalf("%s: magick calls = %d", tc.format, len(rig.magick))
		}
		enc := strings.Join(rig.magick[1], " ")
		for _, h := range tc.has {
			if !strings.Contains(enc, h) {
				t.Errorf("%s: encode args %q lack %q", tc.format, enc, h)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(enc, n) {
				t.Errorf("%s: encode args %q must not contain %q", tc.format, enc, n)
			}
		}
		if !strings.HasSuffix(rig.magick[1][len(rig.magick[1])-1], "(retouched)."+tc.format) {
			t.Errorf("%s: output = %v", tc.format, rig.magick[1])
		}
	}
	// The decode keeps alpha only when the source has it.
	if a := strings.Join(photoDecodeArgs("a.png", "b.png", true), " "); !strings.Contains(a, "PNG64:b.png") || strings.Contains(a, "-alpha off") {
		t.Errorf("alpha decode = %s", a)
	}
	if a := strings.Join(photoDecodeArgs("a.jpg", "b.png", false), " "); !strings.Contains(a, "PNG48:b.png") || !strings.Contains(a, "-auto-orient") || !strings.Contains(a, "a.jpg[0]") {
		t.Errorf("opaque decode = %s", a)
	}
}

func TestPhotoExportMetadataKeepVsStrip(t *testing.T) {
	keep := newPhotoRenderRig(t)
	if err := keep.r.RenderExport(context.Background(), keep.req("jpg", "keep", 0)); err != nil || !reflect.DeepEqual(keep.meta, []string{"copy:original.jpg"}) {
		t.Fatalf("keep: %v %v", err, keep.meta)
	}
	strip := newPhotoRenderRig(t)
	if err := strip.r.RenderExport(context.Background(), strip.req("jpg", "strip", 0)); err != nil || !reflect.DeepEqual(strip.meta, []string{"strip"}) {
		t.Fatalf("strip: %v %v", strip.meta, err)
	}
}

// A non-zero exit becomes the job error (the handler's job failure refunds the
// unit); the stderr tail is bounded to 500 characters.
func TestPhotoExportFailureSurfacesStderrTail(t *testing.T) {
	rig := newPhotoRenderRig(t)
	rig.runner.err = errors.New("exit status 1")
	rig.runner.exit = 1
	rig.runner.stderr = strings.Repeat("x", 2000) + "ValueError: bad doc"
	err := rig.r.RenderExport(context.Background(), rig.req("jpg", "keep", 0))
	if err == nil || !strings.Contains(err.Error(), "photo_render.py exit 1") || !strings.Contains(err.Error(), "ValueError: bad doc") {
		t.Fatalf("err = %v", err)
	}
	if len(err.Error()) > 700 {
		t.Fatalf("error not bounded: %d chars", len(err.Error()))
	}
	if len(rig.magick) != 1 {
		t.Fatalf("nothing may be encoded after a failed render (magick calls %d)", len(rig.magick))
	}
}

func TestPhotoExportRejectsUnknownFormatAndProgressParses(t *testing.T) {
	rig := newPhotoRenderRig(t)
	if err := rig.r.RenderExport(context.Background(), rig.req("bmp", "keep", 0)); err == nil {
		t.Fatal("bmp must be refused")
	}
	var got []float64
	w := &photoProgressWriter{onFrac: func(f float64) { got = append(got, f) }}
	_, _ = w.Write([]byte("progress 0.250\nnoise\nprogress 1.0\nprogress 9\nprog"))
	_, _ = w.Write([]byte("ress 0.5\n"))
	if !reflect.DeepEqual(got, []float64{0.25, 1.0, 0.5}) {
		t.Fatalf("progress = %v", got)
	}
}

func TestPhotoAIArgs(t *testing.T) {
	a := strings.Join(photoDecodeRGB8Args("in.heic", "out.png"), " ")
	if !strings.Contains(a, "in.heic[0] -auto-orient -colorspace sRGB -alpha off -depth 8 PNG24:out.png") {
		t.Fatalf("decode = %s", a)
	}
	m := strings.Join(photoMaskGrayArgs("raw.png", "m.png"), " ")
	if !strings.Contains(m, "-colorspace Gray -depth 8 PNG8:m.png") {
		t.Fatalf("mask = %s", m)
	}
}
