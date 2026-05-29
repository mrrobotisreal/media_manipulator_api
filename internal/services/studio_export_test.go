package services

import (
	"strings"
	"testing"
)

// argIndex returns the index of want in args, or -1.
func argIndex(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func TestBuildSingleClipExportArgs_NVENC(t *testing.T) {
	args := buildSingleClipExportArgs("/tmp/in.mp4", 5.5, 12.25, "h264_nvenc", "high", "/tmp/out.mp4")

	// Fast input seek before -i, then duration after.
	ssIdx := argIndex(args, "-ss")
	iIdx := argIndex(args, "-i")
	tIdx := argIndex(args, "-t")
	if ssIdx == -1 || iIdx == -1 || tIdx == -1 {
		t.Fatalf("expected -ss, -i, -t in args: %v", args)
	}
	if !(ssIdx < iIdx && iIdx < tIdx) {
		t.Fatalf("expected order -ss < -i < -t, got ss=%d i=%d t=%d", ssIdx, iIdx, tIdx)
	}
	if args[ssIdx+1] != "5.500" {
		t.Errorf("sourceIn: got %q want 5.500", args[ssIdx+1])
	}
	// duration = sourceOut - sourceIn = 12.25 - 5.5 = 6.75
	if args[tIdx+1] != "6.750" {
		t.Errorf("duration: got %q want 6.750", args[tIdx+1])
	}
	if args[iIdx+1] != "/tmp/in.mp4" {
		t.Errorf("input: got %q", args[iIdx+1])
	}
	if cv := argIndex(args, "-c:v"); cv == -1 || args[cv+1] != "h264_nvenc" {
		t.Errorf("expected -c:v h264_nvenc, args=%v", args)
	}
	if argIndex(args, "-cq") == -1 {
		t.Errorf("nvenc should use -cq, args=%v", args)
	}
	if args[len(args)-1] != "/tmp/out.mp4" {
		t.Errorf("output must be last arg, got %q", args[len(args)-1])
	}
	if argIndex(args, "+faststart") == -1 {
		t.Errorf("expected +faststart for web playback, args=%v", args)
	}
}

func TestBuildSingleClipExportArgs_Libx264AndZeroIn(t *testing.T) {
	// sourceIn == 0 should omit -ss (no leading seek).
	args := buildSingleClipExportArgs("/tmp/in.mov", 0, 10, "libx264", "medium", "/tmp/out.mp4")
	if argIndex(args, "-ss") != -1 {
		t.Errorf("sourceIn=0 should omit -ss, args=%v", args)
	}
	if tIdx := argIndex(args, "-t"); tIdx == -1 || args[tIdx+1] != "10.000" {
		t.Errorf("expected -t 10.000, args=%v", args)
	}
	if cv := argIndex(args, "-c:v"); cv == -1 || args[cv+1] != "libx264" {
		t.Errorf("expected libx264, args=%v", args)
	}
	if argIndex(args, "-crf") == -1 {
		t.Errorf("libx264 should use -crf, args=%v", args)
	}
	if pix := argIndex(args, "-pix_fmt"); pix == -1 || args[pix+1] != "yuv420p" {
		t.Errorf("expected -pix_fmt yuv420p, args=%v", args)
	}
}

func TestBuildSingleClipExportArgs_NegativeDurationClamped(t *testing.T) {
	// Defensive: sourceOut < sourceIn must not emit a negative -t.
	args := buildSingleClipExportArgs("/tmp/in.mp4", 8, 3, "libx264", "low", "/tmp/out.mp4")
	if tIdx := argIndex(args, "-t"); tIdx != -1 {
		t.Errorf("non-positive duration should omit -t, args=%v", args)
	}
	if strings.Join(args, " ") == "" {
		t.Fatal("args should not be empty")
	}
}

func TestBuildConcatExportArgs(t *testing.T) {
	segs := []StudioExportSegment{
		{InputPath: "/tmp/a.mp4", SourceIn: 0, SourceOut: 4, HasAudio: true},
		{InputPath: "/tmp/b.mp4", SourceIn: 2, SourceOut: 5, HasAudio: false},
	}
	args := buildConcatExportArgs(segs, 1920, 1080, 30, "h264_nvenc", "high", "/tmp/out.mp4")

	// One -i per segment.
	inputs := 0
	for i, a := range args {
		if a == "-i" {
			inputs++
			_ = i
		}
	}
	if inputs != 2 {
		t.Fatalf("expected 2 inputs, got %d: %v", inputs, args)
	}

	fcIdx := argIndex(args, "-filter_complex")
	if fcIdx == -1 {
		t.Fatalf("expected -filter_complex, args=%v", args)
	}
	fc := args[fcIdx+1]

	for _, want := range []string{
		"[0:v]trim=start=0.000:end=4.000",
		"setpts=PTS-STARTPTS",
		"scale=1920:1080:force_original_aspect_ratio=decrease",
		"[0:a]atrim=start=0.000:end=4.000",        // segment 0 has audio
		"anullsrc=r=48000:cl=stereo,atrim=duration=3.000", // segment 1 is silent
		"[v0][a0][v1][a1]concat=n=2:v=1:a=1[outv][outa]",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("filter_complex missing %q\ngraph: %s", want, fc)
		}
	}

	if mapV := argIndex(args, "[outv]"); mapV == -1 {
		t.Errorf("expected -map [outv], args=%v", args)
	}
	if args[len(args)-1] != "/tmp/out.mp4" {
		t.Errorf("output must be last, got %q", args[len(args)-1])
	}
}
