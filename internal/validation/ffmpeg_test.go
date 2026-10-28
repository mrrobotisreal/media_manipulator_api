package validation

import (
	"errors"
	"strings"
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
	if err := ValidateVideo(cfg(), nil, report); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestValidateVideo_Duration(t *testing.T) {
	c := cfg()
	c.MaxVideoDurationSeconds = 60
	report := &ProbeReport{HasVideo: true, Width: 1280, Height: 720, FPS: 30, DurationSeconds: 600}
	err := ValidateVideo(c, nil, report)
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
	err := ValidateVideo(c, nil, report)
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
	err := ValidateVideo(c, nil, report)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Reason != "video_fps" {
		t.Fatalf("expected video_fps Rejection, got %v", err)
	}
}

func TestValidateVideo_AudioDuration(t *testing.T) {
	c := cfg()
	c.MaxAudioDurationSeconds = 60
	report := &ProbeReport{HasAudio: true, DurationSeconds: 120}
	err := ValidateVideo(c, nil, report)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Reason != "audio_duration" {
		t.Fatalf("expected audio_duration Rejection, got %v", err)
	}
}

// The tier allowance and the deployment ceiling are two independent caps and
// the tighter one has to win in both directions — otherwise either a Premium
// user can exhaust the box, or the box's own headroom silently raises a plan.
func TestValidateVideo_TierDurationIsTighterThanConfig(t *testing.T) {
	c := cfg() // 7200s ceiling
	report := &ProbeReport{HasVideo: true, Width: 1280, Height: 720, FPS: 30, DurationSeconds: 600}

	// Anonymous: 5 minutes. A 10-minute source is rejected even though the
	// deployment would happily take 2 hours.
	err := ValidateVideo(c, &Limits{MaxVideoSeconds: 300}, report)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Reason != "video_duration" {
		t.Fatalf("expected video_duration Rejection for anonymous, got %v", err)
	}
	if want := "Your plan allows up to 5 minutes."; !strings.Contains(rej.UserMessage, want) {
		t.Errorf("message %q should contain %q", rej.UserMessage, want)
	}

	// Free: 20 minutes. Same source passes.
	if err := ValidateVideo(c, &Limits{MaxVideoSeconds: 1200}, report); err != nil {
		t.Errorf("expected free tier to accept a 10-minute video, got %v", err)
	}
}

func TestValidateVideo_ConfigCeilingBeatsGenerousTier(t *testing.T) {
	c := cfg()
	c.MaxVideoDurationSeconds = 60 // a small box
	report := &ProbeReport{HasVideo: true, Width: 1280, Height: 720, FPS: 30, DurationSeconds: 600}
	// Premium's 2 hours cannot raise a 60-second deployment ceiling.
	err := ValidateVideo(c, &Limits{MaxVideoSeconds: 7200}, report)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Reason != "video_duration" {
		t.Fatalf("expected the config ceiling to still reject, got %v", err)
	}
}

// Resolution is deliberately NOT tier-differentiated on the source side: the
// matrix caps output resolution, and converting a 4K source down to 720p is an
// ordinary thing for a free user to do.
func TestValidateVideo_TierDoesNotCapSourceResolution(t *testing.T) {
	report := &ProbeReport{HasVideo: true, Width: 3840, Height: 2160, FPS: 30, DurationSeconds: 5}
	if err := ValidateVideo(cfg(), &Limits{MaxVideoSeconds: 300}, report); err != nil {
		t.Errorf("anonymous tier should still accept a 4K source, got %v", err)
	}
}

func TestDescribeDuration(t *testing.T) {
	cases := map[int]string{300: "5 minutes", 1200: "20 minutes", 3600: "1 hour", 7200: "2 hours", 60: "1 minute", 45: "45 seconds"}
	for in, want := range cases {
		if got := describeDuration(in); got != want {
			t.Errorf("describeDuration(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseProbeJSON(t *testing.T) {
	raw := []byte(`{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"avg_frame_rate":"30/1"},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"123.4"}}`)
	report, err := ParseProbeJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !report.HasVideo || !report.HasAudio {
		t.Errorf("expected both streams detected, got %+v", report)
	}
	if report.Width != 1920 || report.Height != 1080 {
		t.Errorf("expected 1920x1080, got %dx%d", report.Width, report.Height)
	}
	if report.DurationSeconds < 123.3 || report.DurationSeconds > 123.5 {
		t.Errorf("expected ~123.4s, got %v", report.DurationSeconds)
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

// An MP3 (or M4A) with embedded album art shows up in ffprobe with a second,
// "video" stream: one PNG/MJPEG frame flagged attached_pic, avg_frame_rate 0/0
// and r_frame_rate equal to the 90 kHz container timebase. That is not a video
// and its timebase is not a frame rate. Before this test the parser reported
// HasVideo=true and FPS=90000, and a user's "remove vocals" job was refused with
// "Video framerate 90000.0fps exceeds the limit of 120 fps."
func TestParseProbeJSON_SkipsEmbeddedCoverArt(t *testing.T) {
	raw := []byte(`{
	  "streams": [
	    {"index":0,"codec_type":"audio","codec_name":"mp3","avg_frame_rate":"0/0","r_frame_rate":"0/0","duration":"2.0"},
	    {"index":1,"codec_type":"video","codec_name":"png","width":320,"height":240,
	     "avg_frame_rate":"0/0","r_frame_rate":"90000/1","disposition":{"attached_pic":1}}
	  ],
	  "format": {"duration":"2.0"}
	}`)
	report, err := ParseProbeJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if report.HasVideo {
		t.Errorf("cover art must not make an audio file a video: %+v", report)
	}
	if !report.HasAudio || report.AudioCodec != "mp3" {
		t.Errorf("audio stream lost: %+v", report)
	}
	if report.FPS != 0 || report.Width != 0 {
		t.Errorf("cover art dimensions/fps leaked into the report: %+v", report)
	}
	if err := ValidateVideo(cfg(), nil, report); err != nil {
		t.Errorf("an mp3 with cover art must validate, got %v", err)
	}
}

// Older ffprobe builds do not always flag attached_pic; the still-image codec
// alone is enough to tell a cover from a picture track.
func TestParseProbeJSON_SkipsStillImageCodecWithoutDisposition(t *testing.T) {
	raw := []byte(`{
	  "streams": [
	    {"index":0,"codec_type":"audio","codec_name":"aac","duration":"3.0"},
	    {"index":1,"codec_type":"video","codec_name":"mjpeg","width":600,"height":600,"avg_frame_rate":"0/0","r_frame_rate":"90000/1"}
	  ],
	  "format": {"duration":"3.0"}
	}`)
	report, err := ParseProbeJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if report.HasVideo || report.FPS != 0 {
		t.Errorf("mjpeg cover art treated as video: %+v", report)
	}
}

// avg_frame_rate is the real rate; r_frame_rate is only the finest timestamp
// grid and is wildly high for variable-frame-rate screen recordings. The real
// rate must win whenever it is known.
func TestParseProbeJSON_PrefersAverageOverNominalFrameRate(t *testing.T) {
	raw := []byte(`{
	  "streams": [
	    {"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080,
	     "avg_frame_rate":"30000/1001","r_frame_rate":"1000/1","duration":"10.0"}
	  ],
	  "format": {"duration":"10.0"}
	}`)
	report, err := ParseProbeJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if report.FPS < 29.9 || report.FPS > 30.0 {
		t.Errorf("FPS = %v, want ~29.97 (avg_frame_rate), not 1000 (r_frame_rate)", report.FPS)
	}
	// Only when the average is unknown does the nominal rate stand in.
	raw2 := []byte(`{"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,"avg_frame_rate":"0/0","r_frame_rate":"60/1"}],"format":{}}`)
	report2, err := ParseProbeJSON(raw2)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if report2.FPS != 60 {
		t.Errorf("FPS fallback = %v, want 60", report2.FPS)
	}
}
