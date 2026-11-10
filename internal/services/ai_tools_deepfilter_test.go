package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deepFilter must write into a directory of its own, and a build that exits 0
// without writing there is an error that names the fix. Found 2026-11-10: the
// host's hand-rewritten df/io.py ignored --output-dir and overwrote the input,
// so every clean_voice / remove_background_noise job failed.
func TestRunDeepFilter_OwnOutputDirAndInPlaceOverwriteIsAnError(t *testing.T) {
	for _, mode := range []string{"writes-output-dir", "overwrites-input"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := newAITestService(t, 1, 1, 1)
			var dfInput, dfOutDir, encodeInput string
			a.run = func(_ context.Context, label string, _ []string, _ string, args ...string) error {
				switch label {
				case "deepFilter":
					dfOutDir = argAfter(args, "--output-dir")
					dfInput = args[len(args)-1]
					if mode == "writes-output-dir" {
						stem := strings.TrimSuffix(filepath.Base(dfInput), ".wav")
						return os.WriteFile(filepath.Join(dfOutDir, stem+"_DeepFilterNet3.wav"), []byte("enhanced"), 0o644)
					}
					return os.WriteFile(dfInput, []byte("enhanced-in-place"), 0o644)
				case "ffmpeg encode":
					encodeInput = argAfter(args, "-i")
				}
				// ffmpeg normalize / encode: create the file they were asked for.
				return os.WriteFile(args[len(args)-1], []byte("wav"), 0o644)
			}
			out := filepath.Join(t.TempDir(), "out.wav")
			err := a.RemoveBackgroundNoise(context.Background(), "job", "/in.wav", out)

			if dfOutDir == "" || dfOutDir == filepath.Dir(dfInput) {
				t.Fatalf("deepFilter must get an output dir of its own: --output-dir %q, input %q", dfOutDir, dfInput)
			}
			if mode == "writes-output-dir" {
				if err != nil {
					t.Fatalf("a normal run must succeed: %v", err)
				}
				if filepath.Dir(encodeInput) != dfOutDir {
					t.Fatalf("the encode must read the enhanced file from %q, got %q", dfOutDir, encodeInput)
				}
				return
			}
			if err == nil {
				t.Fatal("a deepFilter that only overwrote its input must be an error, not a silent pass-through")
			}
			if !strings.Contains(err.Error(), "patch_deepfilter_io.py") {
				t.Fatalf("the error must name the fix: %v", err)
			}
			if encodeInput != "" {
				t.Fatalf("nothing may be encoded after a failed denoise; encode read %q", encodeInput)
			}
		})
	}
}
