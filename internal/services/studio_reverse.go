package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// StudioReverseSegmentSeconds bounds ffmpeg's `reverse` buffer. The filter holds
// every frame of its input in memory (a 720p frame is ~1.4 MB raw), so the
// proxy is reversed in short segments that are then concatenated back to front.
const StudioReverseSegmentSeconds = 8.0

// ReverseSegment is one slice of the source, [Start, Start+Len).
type ReverseSegment struct {
	Start float64
	Len   float64
}

// PlanReverseSegments splits [0, duration) into segSeconds slices and returns
// them LAST FIRST — the order in which their reversed renderings are
// concatenated. A zero/negative duration yields one open-ended segment (Len 0
// means "to the end of the input") so unknown-duration sources still work, with
// the memory bound then falling back to the whole file.
func PlanReverseSegments(duration, segSeconds float64) []ReverseSegment {
	if duration <= 0 || segSeconds <= 0 {
		return []ReverseSegment{{Start: 0, Len: 0}}
	}
	var segs []ReverseSegment
	for start := 0.0; start < duration-1e-6; start += segSeconds {
		l := segSeconds
		if start+l > duration {
			l = duration - start
		}
		segs = append(segs, ReverseSegment{Start: start, Len: l})
	}
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return segs
}

// reverseSegmentArgs is the ffmpeg argv that renders one reversed segment of
// the (already proxy-sized) input: `reverse` for video, `areverse` for audio.
// The encode matches the ingest proxy's so the segments concat with -c copy.
func reverseSegmentArgs(encoder string, kind models.StudioMediaKind, hasAudio bool, in, out string, seg ReverseSegment) []string {
	args := []string{"-y"}
	if seg.Start > 0 {
		args = append(args, "-ss", formatSeconds(seg.Start))
	}
	if seg.Len > 0 {
		args = append(args, "-t", formatSeconds(seg.Len))
	}
	args = append(args, "-i", in)
	if kind == models.StudioMediaKindAudio {
		return append(args, "-vn", "-af", "areverse", "-c:a", "aac", "-b:a", "192k", out)
	}
	args = append(args, "-vf", "reverse")
	if hasAudio {
		args = append(args, "-af", "areverse")
	} else {
		args = append(args, "-an")
	}
	args = append(args, videoProxyEncodeArgs(encoder)...)
	if hasAudio {
		args = append(args, "-c:a", "aac", "-b:a", "128k")
	}
	return append(args, "-movflags", "+faststart", out)
}

// GenerateReversedProxy renders a reversed copy of an asset's PROXY (already
// 720p, so this is cheap and bounded) and uploads it beside the original.
// Returns the S3 key. totalSeconds drives the segment plan and progress.
//
// proxyKey is the asset's current proxy key: the reversed key follows its
// generation (studioReversedProxyKey), so it is never overwritten in place.
func (s *StudioIngestService) GenerateReversedProxy(ctx context.Context, jobID, originalKey, assetID, proxyKey string, kind models.StudioMediaKind, hasAudio bool, proxyPath string, totalSeconds float64) (string, error) {
	if s.s3Client == nil {
		return "", fmt.Errorf("S3 client not configured")
	}
	workDir := filepath.Join(s.cfg.TempDir, "studio_reverse_"+assetID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	ext, mime := "mp4", "video/mp4"
	if kind == models.StudioMediaKindAudio {
		ext, mime = "m4a", "audio/mp4"
	}
	encoder := studioH264Encoder(s.cfg)
	segs := PlanReverseSegments(totalSeconds, StudioReverseSegmentSeconds)
	var list strings.Builder
	for i, seg := range segs {
		part := filepath.Join(workDir, fmt.Sprintf("seg_%04d.%s", i, ext))
		args := reverseSegmentArgs(encoder, kind, hasAudio, proxyPath, part, seg)
		if err := runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, seg.Len, args...); err != nil {
			return "", fmt.Errorf("reverse segment %d: %w", i, err)
		}
		list.WriteString("file '" + strings.ReplaceAll(part, "'", `'\''`) + "'\n")
	}
	listPath := filepath.Join(workDir, "list.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o644); err != nil {
		return "", err
	}
	outLocal := filepath.Join(workDir, "reversed."+ext)
	concat := []string{"-y", "-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", "-movflags", "+faststart", outLocal}
	if err := runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, 0, concat...); err != nil {
		return "", fmt.Errorf("reverse concat: %w", err)
	}
	key := studioReversedProxyKey(originalKey, assetID, proxyKey, ext)
	if err := s.uploadFile(ctx, outLocal, key, originalKey, mime); err != nil {
		return "", fmt.Errorf("upload reversed proxy: %w", err)
	}
	return key, nil
}
