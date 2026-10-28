// Package validation enforces user-facing limits on uploaded media — duration,
// resolution, framerate — before we kick off expensive ffmpeg work.
package validation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// ProbeReport is what ffprobe gives us, reduced to the fields we validate.
type ProbeReport struct {
	DurationSeconds float64
	Width           int
	Height          int
	FPS             float64
	VideoCodec      string
	AudioCodec      string
	HasVideo        bool
	HasAudio        bool
}

// Rejection is the structured error returned when validation fails.
type Rejection struct {
	Reason          string // canonical code, used as Prometheus label
	UserMessage     string // friendly user-facing message
	HTTPStatus      int
	DurationSeconds float64
	Width           int
	Height          int
	FPS             float64
}

// Error satisfies the error interface.
func (r *Rejection) Error() string {
	return r.UserMessage
}

// Limits is the tier-derived slice of the caps this package enforces. It is a
// local struct rather than tiers.Limits so the validation package stays
// dependency-free and testable with a literal.
//
// Only the caps the published tier matrix actually differentiates appear here.
// Source width/height/pixels/framerate stay deployment-wide: the matrix's
// "max output resolution" row governs what we *render*, which is the transcode
// ladder's job (see services.LadderAccess), not what a caller may upload. A
// free user converting a 4K source down to 720p is a completely ordinary thing
// to do and must keep working.
type Limits struct {
	// MaxVideoSeconds caps source duration. 0 means "no tier cap".
	MaxVideoSeconds int
}

// ValidateVideo enforces the caps against a probed video file.
//
// Two ceilings apply and the tighter wins: cfg is the deployment's absolute
// ceiling and limits is the caller's tier allowance. limits may be nil, which
// means no tier has been resolved and only the deployment ceiling applies —
// the behaviour before tiers existed.
func ValidateVideo(cfg *config.Config, limits *Limits, p *ProbeReport) error {
	if cfg == nil || p == nil {
		return nil
	}
	maxVideoSeconds := tighter(cfg.MaxVideoDurationSeconds, limits.videoSeconds())
	maxAudioSeconds := tighter(cfg.MaxAudioDurationSeconds, limits.videoSeconds())
	if p.HasVideo {
		if maxVideoSeconds > 0 && p.DurationSeconds > float64(maxVideoSeconds) {
			return &Rejection{
				Reason:          "video_duration",
				UserMessage:     fmt.Sprintf("Video is too long. Your plan allows up to %s.", describeDuration(maxVideoSeconds)),
				HTTPStatus:      413,
				DurationSeconds: p.DurationSeconds,
			}
		}
		if cfg.MaxVideoWidth > 0 && p.Width > cfg.MaxVideoWidth {
			return &Rejection{
				Reason:      "video_width",
				UserMessage: fmt.Sprintf("Video width %dpx exceeds limit of %dpx.", p.Width, cfg.MaxVideoWidth),
				HTTPStatus:  413,
				Width:       p.Width,
			}
		}
		if cfg.MaxVideoHeight > 0 && p.Height > cfg.MaxVideoHeight {
			return &Rejection{
				Reason:      "video_height",
				UserMessage: fmt.Sprintf("Video height %dpx exceeds limit of %dpx.", p.Height, cfg.MaxVideoHeight),
				HTTPStatus:  413,
				Height:      p.Height,
			}
		}
		pixels := int64(p.Width) * int64(p.Height)
		if cfg.MaxVideoPixels > 0 && pixels > cfg.MaxVideoPixels {
			return &Rejection{
				Reason:      "video_pixels",
				UserMessage: fmt.Sprintf("Video resolution %dx%d exceeds the maximum this service accepts (max %d pixels).", p.Width, p.Height, cfg.MaxVideoPixels),
				HTTPStatus:  413,
				Width:       p.Width,
				Height:      p.Height,
			}
		}
		if cfg.MaxVideoFPS > 0 && p.FPS > float64(cfg.MaxVideoFPS) {
			return &Rejection{
				Reason:      "video_fps",
				UserMessage: fmt.Sprintf("Video framerate %.1ffps exceeds the limit of %d fps.", p.FPS, cfg.MaxVideoFPS),
				HTTPStatus:  413,
				FPS:         p.FPS,
			}
		}
	}
	if p.HasAudio && !p.HasVideo && maxAudioSeconds > 0 && p.DurationSeconds > float64(maxAudioSeconds) {
		return &Rejection{
			Reason:          "audio_duration",
			UserMessage:     fmt.Sprintf("Audio is too long. Your plan allows up to %s.", describeDuration(maxAudioSeconds)),
			HTTPStatus:      413,
			DurationSeconds: p.DurationSeconds,
		}
	}
	return nil
}

func (l *Limits) videoSeconds() int {
	if l == nil {
		return 0
	}
	return l.MaxVideoSeconds
}

// tighter returns the smaller of two caps, treating 0 or less as "uncapped".
func tighter(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

// describeDuration renders a second count the way the pricing page does:
// "5 minutes", "20 minutes", "2 hours".
func describeDuration(seconds int) string {
	switch {
	case seconds%3600 == 0 && seconds >= 3600:
		if seconds == 3600 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", seconds/3600)
	case seconds >= 60:
		if seconds/60 == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", seconds/60)
	default:
		return fmt.Sprintf("%d seconds", seconds)
	}
}

// Probe runs ffprobe and returns a ProbeReport. Used by handlers that don't
// already have a probe report from the transcode pipeline.
func Probe(ctx context.Context, ffprobeBin, path string, timeout time.Duration) (*ProbeReport, error) {
	if ffprobeBin == "" {
		ffprobeBin = "ffprobe"
	}
	if _, err := exec.LookPath(ffprobeBin); err != nil {
		return nil, fmt.Errorf("ffprobe not found: %w", err)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, ffprobeBin, "-v", "error", "-print_format", "json", "-show_streams", "-show_format", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	return ParseProbeJSON(out)
}

// ParseProbeJSON reduces raw `ffprobe -print_format json -show_streams
// -show_format` output to a ProbeReport.
//
// Exported so callers that have already run ffprobe for another reason can
// validate without paying for a second process. The upload path is the case:
// ffprobe output was already captured. Probe wraps it for the common case.
func ParseProbeJSON(out []byte) (*ProbeReport, error) {
	var raw struct {
		Streams []struct {
			CodecType    string `json:"codec_type"`
			CodecName    string `json:"codec_name"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			AvgFrameRate string `json:"avg_frame_rate"`
			RFrameRate   string `json:"r_frame_rate"`
			Duration     string `json:"duration"`
			Disposition  struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse ffprobe json: %w", err)
	}
	report := &ProbeReport{}
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			// Cover art (an MP3 with embedded album art, an M4A with a
			// thumbnail) shows up as a "video" stream too: a single PNG/MJPEG
			// frame flagged attached_pic, with avg_frame_rate 0/0 and an
			// r_frame_rate equal to the container timebase (90000/1). It is
			// not a video, must not make an audio file count as one, and its
			// timebase must never be read as "90000 fps".
			if s.Disposition.AttachedPic != 0 || isStillImageCodec(s.CodecName) {
				continue
			}
			report.HasVideo = true
			if s.Width > report.Width {
				report.Width = s.Width
			}
			if s.Height > report.Height {
				report.Height = s.Height
			}
			if fps := streamFrameRate(s.AvgFrameRate, s.RFrameRate); fps > report.FPS {
				report.FPS = fps
			}
			if report.VideoCodec == "" {
				report.VideoCodec = s.CodecName
			}
		case "audio":
			report.HasAudio = true
			if report.AudioCodec == "" {
				report.AudioCodec = s.CodecName
			}
		}
	}
	duration := raw.Format.Duration
	if duration == "" {
		for _, s := range raw.Streams {
			if s.Duration != "" {
				duration = s.Duration
				break
			}
		}
	}
	if d, err := strconv.ParseFloat(strings.TrimSpace(duration), 64); err == nil {
		report.DurationSeconds = d
	}
	if !report.HasVideo && !report.HasAudio {
		return report, errors.New("ffprobe found neither video nor audio streams")
	}
	return report, nil
}

// streamFrameRate picks the frame rate to validate a video stream by.
//
// avg_frame_rate is the real rate (frames / duration). r_frame_rate is only
// "the smallest rate every timestamp fits in", which for variable-frame-rate
// sources and for anything demuxed with a 90 kHz timebase can be absurdly high
// (1000/1, 90000/1) while the picture plays at 30 fps. It is used only when the
// average is unknown (a stream with no duration yet, e.g. a raw elementary
// stream), never to override a known average.
func streamFrameRate(avg, r string) float64 {
	if fps := parseFrameRate(avg); fps > 0 {
		return fps
	}
	return parseFrameRate(r)
}

// isStillImageCodec reports whether a "video" stream is really a single picture
// (album art, a poster frame, an embedded thumbnail). Older ffprobe builds do
// not always set disposition.attached_pic for these, so the codec is checked as
// well.
func isStillImageCodec(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "png", "mjpeg", "bmp", "gif", "tiff", "webp", "jpegls", "jpeg2000", "ppm", "pgm", "pam", "targa", "sgi", "heic", "avif":
		return true
	}
	return false
}

// parseFrameRate parses ffprobe's fractional fps string e.g. "30000/1001".
func parseFrameRate(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if idx := strings.IndexByte(s, '/'); idx > 0 {
		num, err1 := strconv.ParseFloat(s[:idx], 64)
		den, err2 := strconv.ParseFloat(s[idx+1:], 64)
		if err1 == nil && err2 == nil && den > 0 {
			return num / den
		}
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}
