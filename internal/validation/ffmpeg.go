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

// ValidateVideo enforces the per-config caps against a probed video file.
func ValidateVideo(cfg *config.Config, p *ProbeReport) error {
	if cfg == nil || p == nil {
		return nil
	}
	if p.HasVideo {
		if cfg.MaxVideoDurationSeconds > 0 && p.DurationSeconds > float64(cfg.MaxVideoDurationSeconds) {
			return &Rejection{
				Reason:          "video_duration",
				UserMessage:     fmt.Sprintf("Video is too long. Max allowed is %d minutes.", cfg.MaxVideoDurationSeconds/60),
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
				UserMessage: fmt.Sprintf("Video resolution %dx%d exceeds the current free limit (max %d pixels).", p.Width, p.Height, cfg.MaxVideoPixels),
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
	if p.HasAudio && !p.HasVideo && cfg.MaxAudioDurationSeconds > 0 && p.DurationSeconds > float64(cfg.MaxAudioDurationSeconds) {
		return &Rejection{
			Reason:          "audio_duration",
			UserMessage:     fmt.Sprintf("Audio is too long. Max allowed is %d minutes.", cfg.MaxAudioDurationSeconds/60),
			HTTPStatus:      413,
			DurationSeconds: p.DurationSeconds,
		}
	}
	return nil
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
	var raw struct {
		Streams []struct {
			CodecType    string `json:"codec_type"`
			CodecName    string `json:"codec_name"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			AvgFrameRate string `json:"avg_frame_rate"`
			RFrameRate   string `json:"r_frame_rate"`
			Duration     string `json:"duration"`
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
			report.HasVideo = true
			if s.Width > report.Width {
				report.Width = s.Width
			}
			if s.Height > report.Height {
				report.Height = s.Height
			}
			if fps := parseFrameRate(s.AvgFrameRate); fps > report.FPS {
				report.FPS = fps
			} else if fps := parseFrameRate(s.RFrameRate); fps > report.FPS {
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
