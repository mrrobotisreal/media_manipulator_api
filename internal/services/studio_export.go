package services

import (
	"context"
	"strconv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// StudioExportService renders an EDL to a single MP4 via NVENC on the Content
// Studio GPU. Phase 1 implements the single-clip case (one input, trimmed to
// [sourceIn, sourceOut]); the multi-track filter_complex graph lands in Phase 3.
type StudioExportService struct {
	cfg        *config.Config
	jobManager *JobManager
}

func NewStudioExportService(cfg *config.Config, jm *JobManager) *StudioExportService {
	return &StudioExportService{cfg: cfg, jobManager: jm}
}

// StudioExportClip is a resolved single-clip export: the local source path plus
// the in/out points within it.
type StudioExportClip struct {
	InputPath string
	SourceIn  float64
	SourceOut float64
}

// RunSingleClip trims + re-encodes one clip to outputPath, reporting progress
// on jobID. The encoder is chosen once (NVENC when the GPU supports it, else
// libx264).
func (s *StudioExportService) RunSingleClip(ctx context.Context, jobID string, clip StudioExportClip, quality, outputPath string) error {
	encoder := studioH264Encoder(s.cfg)
	args := buildSingleClipExportArgs(clip.InputPath, clip.SourceIn, clip.SourceOut, encoder, quality, outputPath)
	dur := clip.SourceOut - clip.SourceIn
	return runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, dur, args...)
}

// buildSingleClipExportArgs builds the ffmpeg arg slice for a single-clip trim
// + re-encode. We use a fast input seek (-ss before -i) plus -t duration, which
// is unambiguous — unlike -to combined with input seeking, where -to is
// interpreted against the original timeline. Phase 1 keeps the source
// resolution; audio (if any) is re-encoded to AAC. Pure + deterministic so the
// graph is unit-testable without invoking ffmpeg.
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

func formatSeconds(s float64) string {
	return strconv.FormatFloat(s, 'f', 3, 64)
}
