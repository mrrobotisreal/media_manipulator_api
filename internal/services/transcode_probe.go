package services

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type ffprobeStream struct {
	Index              int               `json:"index"`
	CodecType          string            `json:"codec_type"`
	CodecName          string            `json:"codec_name"`
	Width              int               `json:"width"`
	Height             int               `json:"height"`
	AvgFrameRate       string            `json:"avg_frame_rate"`
	RFrameRate         string            `json:"r_frame_rate"`
	BitRate            string            `json:"bit_rate"`
	DisplayAspectRatio string            `json:"display_aspect_ratio"`
	Duration           string            `json:"duration"`
	SampleRate         string            `json:"sample_rate"`
	Channels           int               `json:"channels"`
	Tags               map[string]string `json:"tags"`
	Disposition        struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

type ffprobeFormat struct {
	Duration   string            `json:"duration"`
	FormatName string            `json:"format_name"`
	BitRate    string            `json:"bit_rate"`
	Tags       map[string]string `json:"tags"`
}

type ffprobeResult struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// ProbeVideoReport runs ffprobe against a local file and returns a structured
// report the UI can render directly. Anything the probe can't determine
// becomes a zero-value field rather than an error — we want to keep showing
// the user the form even when one or two fields are missing.
func ProbeVideoReport(ctx context.Context, inputPath string) (*models.VideoProbeResponse, error) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return nil, fmt.Errorf("ffprobe not found in PATH")
	}
	stdout, stderr, err := runCommand(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		inputPath,
	)
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w: %s", err, strings.TrimSpace(stderr))
	}
	var res ffprobeResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		return nil, fmt.Errorf("ffprobe parse: %w", err)
	}

	out := &models.VideoProbeResponse{}
	out.FormatName = strings.TrimSpace(res.Format.FormatName)
	out.ContainerFormat = primaryFormatName(out.FormatName)
	if duration, err := strconv.ParseFloat(strings.TrimSpace(res.Format.Duration), 64); err == nil {
		out.DurationSeconds = duration
	}

	for _, stream := range res.Streams {
		switch stream.CodecType {
		case "video":
			// Embedded cover art / poster frames are "video" streams to ffprobe
			// (one PNG/MJPEG picture, attached_pic=1, r_frame_rate = the 90 kHz
			// timebase). Skip them: they are not the picture being transcoded
			// and their timebase is not a frame rate.
			if stream.Disposition.AttachedPic != 0 {
				continue
			}
			if out.Width == 0 && stream.Width > 0 {
				out.Width = stream.Width
			}
			if out.Height == 0 && stream.Height > 0 {
				out.Height = stream.Height
			}
			if out.FPS == 0 {
				if r := parseFractionFrameRate(stream.AvgFrameRate); r > 0 {
					out.FPS = r
					out.FrameRate = stream.AvgFrameRate
				} else if r := parseFractionFrameRate(stream.RFrameRate); r > 0 {
					out.FPS = r
					out.FrameRate = stream.RFrameRate
				}
			}
			if out.VideoCodec == "" {
				out.VideoCodec = strings.TrimSpace(stream.CodecName)
			}
			if b, _ := strconv.ParseInt(stream.BitRate, 10, 64); b > 0 && out.VideoBitrateBps == 0 {
				out.VideoBitrateBps = b
			}
		case "audio":
			out.HasAudio = true
			if out.AudioCodec == "" {
				out.AudioCodec = strings.TrimSpace(stream.CodecName)
			}
			if b, _ := strconv.ParseInt(stream.BitRate, 10, 64); b > 0 && out.AudioBitrateBps == 0 {
				out.AudioBitrateBps = b
			}
		}
		out.Streams = append(out.Streams, streamInfoFromFFprobe(stream))
	}

	// If the format-level duration was missing, fall back to the longest stream duration.
	if out.DurationSeconds == 0 {
		var maxDur float64
		for _, s := range res.Streams {
			if d, err := strconv.ParseFloat(strings.TrimSpace(s.Duration), 64); err == nil && d > maxDur {
				maxDur = d
			}
		}
		out.DurationSeconds = maxDur
	}

	out.MaxQualityLabel = classifyMaxQuality(out.Height)
	// The ladder defaults to the most restrictive tier. Callers that know who
	// is asking re-apply it with ApplyLadderAccess; callers that do not (the
	// restore and Studio pipelines, which never show a rung picker) keep the
	// safe answer.
	ApplyLadderAccess(out, AnonymousLadderAccess())

	if !out.HasAudio {
		out.Warnings = append(out.Warnings, captionsNoAudioTip)
	}
	return out, nil
}

// ApplyLadderAccess recomputes the selectable/disabled rung split and the
// source-too-small warning for a specific caller's tier.
//
// This is separate from ProbeVideoReport because the ffprobe run is the
// expensive part and is tier-independent: exactly one probe per request, then
// the cheap tier-dependent view on top of it.
func ApplyLadderAccess(report *models.VideoProbeResponse, access LadderAccess) {
	if report == nil {
		return
	}
	enabled, disabled := splitRungs(buildQualityRungs(report.Height, access))
	report.SelectableRungs = enabled
	report.DisabledRungs = disabled

	report.SourceTooSmall = report.Height > 0 && access.MinHeight > 0 && report.Height < access.MinHeight
	// Drop any previously-attached minimum-height warning before deciding
	// again, so re-applying a different tier never leaves a stale one behind.
	report.Warnings = withoutMinHeightWarnings(report.Warnings)
	if report.SourceTooSmall {
		report.Warnings = append(report.Warnings, access.minHeightMessage())
	}
}

// withoutMinHeightWarnings strips the warning ApplyLadderAccess owns. Matching
// on the shared prefix keeps this working if the wording is retuned.
func withoutMinHeightWarnings(warnings []string) []string {
	out := warnings[:0]
	for _, w := range warnings {
		if strings.HasPrefix(w, "This video is below the ") {
			continue
		}
		out = append(out, w)
	}
	return out
}

func streamInfoFromFFprobe(stream ffprobeStream) models.FFProbeStreamInfo {
	info := models.FFProbeStreamInfo{
		Index:      stream.Index,
		CodecType:  stream.CodecType,
		CodecName:  strings.TrimSpace(stream.CodecName),
		Width:      stream.Width,
		Height:     stream.Height,
		SampleRate: strings.TrimSpace(stream.SampleRate),
		Channels:   stream.Channels,
	}
	if r := parseFractionFrameRate(stream.AvgFrameRate); r > 0 {
		info.FrameRate = math.Round(r*1000) / 1000
	}
	if b, _ := strconv.ParseInt(stream.BitRate, 10, 64); b > 0 {
		info.BitrateBps = b
	}
	if d, err := strconv.ParseFloat(strings.TrimSpace(stream.Duration), 64); err == nil {
		info.Duration = d
	}
	return info
}

func primaryFormatName(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if idx := strings.Index(raw, ","); idx >= 0 {
		return strings.TrimSpace(raw[:idx])
	}
	return raw
}
