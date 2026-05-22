package services

import "testing"

func TestAudioWaveformOptions_Defaults(t *testing.T) {
	opts := &AudioWaveformOptions{}
	if err := opts.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if opts.OutputSelection != "video" {
		t.Fatalf("default OutputSelection = %q, want video", opts.OutputSelection)
	}
	if opts.VideoFormat != "mp4" || opts.ImageFormat != "png" {
		t.Fatalf("default formats wrong: video=%q image=%q", opts.VideoFormat, opts.ImageFormat)
	}
	if opts.Width != waveformDefaultWidth || opts.Height != waveformDefaultHeight {
		t.Fatalf("default dims = %dx%d, want %dx%d", opts.Width, opts.Height, waveformDefaultWidth, waveformDefaultHeight)
	}
	if opts.Mode != "point" {
		t.Fatalf("default mode = %q, want point", opts.Mode)
	}
	if opts.FrameRate != waveformDefaultRate {
		t.Fatalf("default rate = %v, want %v", opts.FrameRate, waveformDefaultRate)
	}
	if opts.Scale != "lin" || opts.Draw != "scale" {
		t.Fatalf("scale/draw defaults wrong: %q / %q", opts.Scale, opts.Draw)
	}
}

func TestAudioWaveformOptions_RejectsBadColor(t *testing.T) {
	opts := &AudioWaveformOptions{ColorPrimary: "not-a-hex"}
	if err := opts.applyDefaults(); err == nil {
		t.Fatalf("expected rejection of invalid color")
	}
	opts = &AudioWaveformOptions{ColorPrimary: "#ZZZZZZ"}
	if err := opts.applyDefaults(); err == nil {
		t.Fatalf("expected rejection of non-hex chars")
	}
	opts = &AudioWaveformOptions{ColorPrimary: "#abcdef"}
	if err := opts.applyDefaults(); err != nil {
		t.Fatalf("valid hex rejected: %v", err)
	}
}

func TestAudioWaveformOptions_RateAndNMutuallyExclusive(t *testing.T) {
	opts := &AudioWaveformOptions{FrameRate: 30, N: 100}
	if err := opts.applyDefaults(); err == nil {
		t.Fatalf("expected rejection when both rate and n are set")
	}
}

func TestAudioWaveformOptions_ForcesEvenDims(t *testing.T) {
	opts := &AudioWaveformOptions{Width: 1601, Height: 161}
	if err := opts.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if opts.Width%2 != 0 || opts.Height%2 != 0 {
		t.Fatalf("dimensions not even after defaults: %dx%d", opts.Width, opts.Height)
	}
}

func TestExtractFramesOptions_Defaults(t *testing.T) {
	o := ExtractFramesOptions{}
	if err := o.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if o.Mode != "every_n_seconds" {
		t.Fatalf("mode = %q, want every_n_seconds", o.Mode)
	}
	if o.Format != "jpg" {
		t.Fatalf("format = %q, want jpg", o.Format)
	}
	if o.IntervalSeconds <= 0 {
		t.Fatalf("intervalSeconds = %v, expected positive default", o.IntervalSeconds)
	}
	if o.MaxFrames <= 0 || o.MaxFrames > extractFramesAbsoluteMax {
		t.Fatalf("maxFrames out of range: %d", o.MaxFrames)
	}
}

func TestExtractFramesOptions_RejectsTimestampWithoutValues(t *testing.T) {
	o := ExtractFramesOptions{Mode: "timestamp"}
	if err := o.applyDefaults(); err == nil {
		t.Fatalf("expected rejection of timestamp mode with empty list")
	}
}

func TestHexToFFmpegColor(t *testing.T) {
	if got := hexToFFmpegColor("#FF0000"); got != "0xFF0000" {
		t.Fatalf("hex conversion = %q", got)
	}
}
