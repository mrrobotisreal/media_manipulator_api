package validation

import (
	"errors"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func cfg() *config.Config {
	return &config.Config{
		MaxVideoDurationSeconds: 7200,
		MaxVideoWidth:           3840,
		MaxVideoHeight:          2160,
		MaxVideoPixels:          8294400,
		MaxVideoFPS:             120,
		MaxAudioDurationSeconds: 14400,
	}
}

func TestValidateVideo_OK(t *testing.T) {
	report := &ProbeReport{HasVideo: true, Width: 1920, Height: 1080, FPS: 60, DurationSeconds: 60}
	if err := ValidateVideo(cfg(), report); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestValidateVideo_Duration(t *testing.T) {
	c := cfg()
	c.MaxVideoDurationSeconds = 60
	report := &ProbeReport{HasVideo: true, Width: 1280, Height: 720, FPS: 30, DurationSeconds: 600}
	err := ValidateVideo(c, report)
	var rej *Rejection
	if !errors.As(err, &rej) {
		t.Fatalf("expected Rejection, got %v", err)
	}
	if rej.Reason != "video_duration" {
		t.Errorf("expected video_duration reason, got %q", rej.Reason)
	}
}

func TestValidateVideo_Pixels(t *testing.T) {
	c := cfg()
	c.MaxVideoPixels = 2073600 // 1080p
	report := &ProbeReport{HasVideo: true, Width: 3840, Height: 2160, FPS: 30, DurationSeconds: 5}
	err := ValidateVideo(c, report)
	var rej *Rejection
	if !errors.As(err, &rej) {
		t.Fatalf("expected Rejection, got %v", err)
	}
	if rej.Reason != "video_width" && rej.Reason != "video_pixels" {
		t.Errorf("expected width or pixels reason, got %q", rej.Reason)
	}
}

func TestValidateVideo_FPS(t *testing.T) {
	c := cfg()
	c.MaxVideoFPS = 60
	report := &ProbeReport{HasVideo: true, Width: 1280, Height: 720, FPS: 240, DurationSeconds: 5}
	err := ValidateVideo(c, report)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Reason != "video_fps" {
		t.Fatalf("expected video_fps Rejection, got %v", err)
	}
}

func TestValidateVideo_AudioDuration(t *testing.T) {
	c := cfg()
	c.MaxAudioDurationSeconds = 60
	report := &ProbeReport{HasAudio: true, DurationSeconds: 120}
	err := ValidateVideo(c, report)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Reason != "audio_duration" {
		t.Fatalf("expected audio_duration Rejection, got %v", err)
	}
}

func TestParseFrameRate(t *testing.T) {
	cases := map[string]float64{
		"30/1":       30,
		"30000/1001": 29.97002997002997,
		"60":         60,
		"":           0,
		"bad":        0,
	}
	for in, want := range cases {
		got := parseFrameRate(in)
		if (got-want) > 0.001 || (got-want) < -0.001 {
			t.Errorf("parseFrameRate(%q) = %v, want %v", in, got, want)
		}
	}
}
