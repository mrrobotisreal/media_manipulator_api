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

func filterComplex(t *testing.T, args []string) string {
	t.Helper()
	idx := argIndex(args, "-filter_complex")
	if idx == -1 || idx+1 >= len(args) {
		t.Fatalf("no -filter_complex in args: %v", args)
	}
	return args[idx+1]
}

func TestBuildMultiTrackExportArgs_OverlayAndMix(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4", "/b.mp4", "/music.mp3"},
		Video: []StudioExportVideoSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1, TrackIndex: 0},
			{InputIndex: 1, SourceIn: 1, SourceOut: 3, TimelineStart: 2, Opacity: 0.5, TrackIndex: 1},
		},
		Audio: []StudioExportAudioSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Volume: 1},
			{InputIndex: 2, SourceIn: 0, SourceOut: 10, TimelineStart: 0, Volume: 0.3},
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 10,
	}
	args := buildMultiTrackExportArgs(plan, "h264_nvenc", "high", "/out.mp4")

	// One -i per input.
	inputs := 0
	for _, a := range args {
		if a == "-i" {
			inputs++
		}
	}
	if inputs != 3 {
		t.Fatalf("expected 3 inputs, got %d: %v", inputs, args)
	}

	fc := filterComplex(t, args)
	for _, want := range []string{
		"color=c=black:s=1920x1080:r=30:d=10.000,format=yuv420p[vbase]",
		"[0:v]trim=start=0.000:end=5.000",
		"setpts=PTS-STARTPTS+0.000/TB",
		"setpts=PTS-STARTPTS+2.000/TB",       // second clip offset to its timeline start
		"colorchannelmixer=aa=0.500",          // opacity on the top clip
		"overlay=x=(W-w)/2:y=(H-h)/2:enable='between(t,2.000,4.000)':eof_action=pass[vout]",
		"volume=0.300",                        // music gain
		"amix=inputs=2:normalize=0:dropout_transition=0[aout]",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("filter_complex missing %q\ngraph: %s", want, fc)
		}
	}

	if argIndex(args, "[vout]") == -1 || argIndex(args, "[aout]") == -1 {
		t.Errorf("expected -map [vout] and -map [aout], args=%v", args)
	}
	if tIdx := argIndex(args, "-t"); tIdx == -1 || args[tIdx+1] != "10.000" {
		t.Errorf("expected -t 10.000, args=%v", args)
	}
	if args[len(args)-1] != "/out.mp4" {
		t.Errorf("output must be last, got %q", args[len(args)-1])
	}
}

func TestBuildMultiTrackExportArgs_SingleVideoClipNoExtraAudio(t *testing.T) {
	// One video clip with its embedded audio: single overlay, single audio
	// source (no amix), both mapped.
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4"},
		Video:  []StudioExportVideoSeg{{InputIndex: 0, SourceIn: 2, SourceOut: 8, TimelineStart: 0, Opacity: 1, TrackIndex: 0}},
		Audio:  []StudioExportAudioSeg{{InputIndex: 0, SourceIn: 2, SourceOut: 8, TimelineStart: 0, Volume: 1}},
		Width:  1280, Height: 720, FPS: 24, Duration: 6,
	}
	args := buildMultiTrackExportArgs(plan, "libx264", "medium", "/out.mp4")
	fc := filterComplex(t, args)

	if strings.Contains(fc, "amix") {
		t.Errorf("single audio source should not use amix: %s", fc)
	}
	if !strings.Contains(fc, "volume=1.000,adelay=0|0[aout]") {
		t.Errorf("single audio should be labeled [aout]: %s", fc)
	}
	if !strings.Contains(fc, "eof_action=pass[vout]") {
		t.Errorf("single video should produce [vout]: %s", fc)
	}
	if cv := argIndex(args, "-c:v"); cv == -1 || args[cv+1] != "libx264" {
		t.Errorf("expected libx264, args=%v", args)
	}
}

func TestBuildMultiTrackExportArgs_AudioOnly(t *testing.T) {
	// No video segments → audio-only output (no video map, no -c:v).
	plan := StudioExportPlan{
		Inputs: []string{"/voice.mp3"},
		Audio:  []StudioExportAudioSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 1, Volume: 0.8}},
		Width:  1920, Height: 1080, FPS: 30, Duration: 5,
	}
	args := buildMultiTrackExportArgs(plan, "h264_nvenc", "high", "/out.mp4")
	if argIndex(args, "-c:v") != -1 {
		t.Errorf("audio-only export should not set -c:v: %v", args)
	}
	if argIndex(args, "[aout]") == -1 {
		t.Errorf("expected audio map, args=%v", args)
	}
	fc := filterComplex(t, args)
	if !strings.Contains(fc, "adelay=1000|1000") {
		t.Errorf("expected 1000ms delay for timelineStart=1: %s", fc)
	}
}
