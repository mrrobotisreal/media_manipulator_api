package services

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// StudioExportService renders an EDL to a single MP4 via NVENC on the Content
// Studio GPU. Phase 1 implemented the single-clip case; Phase 2 adds multi-clip
// concatenation on one video track (per-clip trim/setpts → concat). The full
// multi-track overlay + audio-mix graph lands in Phase 3.
type StudioExportService struct {
	cfg        *config.Config
	jobManager *JobManager
}

func NewStudioExportService(cfg *config.Config, jm *JobManager) *StudioExportService {
	return &StudioExportService{cfg: cfg, jobManager: jm}
}

// StudioExportSegment is one resolved clip in timeline order: the local source
// path plus its in/out points and whether the source carries audio.
type StudioExportSegment struct {
	InputPath string
	SourceIn  float64
	SourceOut float64
	HasAudio  bool
}

// RunTimeline renders the ordered segments to outputPath. A single segment uses
// the fast stream-trim path (preserves source resolution); multiple segments go
// through the concat filter graph (each normalized to the project frame).
func (s *StudioExportService) RunTimeline(ctx context.Context, jobID string, segments []StudioExportSegment, width, height int, fps float64, quality, outputPath string) error {
	encoder := studioH264Encoder(s.cfg)
	var total float64
	for _, seg := range segments {
		if d := seg.SourceOut - seg.SourceIn; d > 0 {
			total += d
		}
	}
	var args []string
	if len(segments) == 1 {
		args = buildSingleClipExportArgs(segments[0].InputPath, segments[0].SourceIn, segments[0].SourceOut, encoder, quality, outputPath)
	} else {
		args = buildConcatExportArgs(segments, width, height, fps, encoder, quality, outputPath)
	}
	return runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, total, args...)
}

// buildSingleClipExportArgs builds the ffmpeg arg slice for a single-clip trim
// + re-encode. We use a fast input seek (-ss before -i) plus -t duration, which
// is unambiguous — unlike -to combined with input seeking, where -to is
// interpreted against the original timeline. Keeps the source resolution; audio
// (if any) is re-encoded to AAC. Pure + deterministic for tests.
func buildSingleClipExportArgs(inputPath string, sourceIn, sourceOut float64, encoder, quality, outputPath string) []string {
	dur := sourceOut - sourceIn
	if dur < 0 {
		dur = 0
	}
	args := []string{"-y"}
	if sourceIn > 0 {
		args = append(args, "-ss", formatSeconds(sourceIn))
	}
	args = append(args, "-i", inputPath)
	if dur > 0 {
		args = append(args, "-t", formatSeconds(dur))
	}
	args = append(args, h264EncodeArgs(encoder, quality)...)
	args = append(args,
		"-c:a", "aac",
		"-b:a", "192k",
		"-movflags", "+faststart",
		outputPath,
	)
	return args
}

// buildConcatExportArgs builds a filter_complex that trims each segment, resets
// its PTS, normalizes it to the project frame (scale + pad + setsar + fps +
// yuv420p) and a common audio format (48k stereo, silence synthesized for
// silent sources), then concatenates everything into one video + one audio
// stream. Normalization is required because the concat filter demands matching
// geometry/format across inputs. One -i per clip (the same file repeats when
// clips share an asset). Pure + deterministic so the graph is unit-testable.
func buildConcatExportArgs(segments []StudioExportSegment, width, height int, fps float64, encoder, quality, outputPath string) []string {
	args := []string{"-y"}
	for _, seg := range segments {
		args = append(args, "-i", seg.InputPath)
	}

	fpsStr := formatFrameRate(fps)
	if fpsStr == "" {
		fpsStr = "30"
	}
	var fc strings.Builder
	var concatIn strings.Builder
	for i, seg := range segments {
		dur := seg.SourceOut - seg.SourceIn
		if dur < 0 {
			dur = 0
		}
		// Video: trim → reset PTS → fit to WxH letterboxed → square pixels → fps → yuv420p.
		fc.WriteString(fmt.Sprintf(
			"[%d:v]trim=start=%s:end=%s,setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%s,format=yuv420p[v%d];",
			i, formatSeconds(seg.SourceIn), formatSeconds(seg.SourceOut), width, height, width, height, fpsStr, i,
		))
		// Audio: real track when present, otherwise synthesized silence of the
		// clip's duration so every concat segment has matching streams.
		if seg.HasAudio {
			fc.WriteString(fmt.Sprintf(
				"[%d:a]atrim=start=%s:end=%s,asetpts=PTS-STARTPTS,aformat=sample_rates=48000:channel_layouts=stereo[a%d];",
				i, formatSeconds(seg.SourceIn), formatSeconds(seg.SourceOut), i,
			))
		} else {
			fc.WriteString(fmt.Sprintf(
				"anullsrc=r=48000:cl=stereo,atrim=duration=%s,asetpts=PTS-STARTPTS[a%d];",
				formatSeconds(dur), i,
			))
		}
		concatIn.WriteString(fmt.Sprintf("[v%d][a%d]", i, i))
	}
	fc.WriteString(fmt.Sprintf("%sconcat=n=%d:v=1:a=1[outv][outa]", concatIn.String(), len(segments)))

	args = append(args, "-filter_complex", fc.String(), "-map", "[outv]", "-map", "[outa]")
	args = append(args, h264EncodeArgs(encoder, quality)...)
	args = append(args, "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", outputPath)
	return args
}

func formatSeconds(s float64) string {
	return strconv.FormatFloat(s, 'f', 3, 64)
}
