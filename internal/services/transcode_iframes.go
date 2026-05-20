package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// iframePlaylistResult is what we hand back to the master-playlist writer so
// it can emit a correctly-attributed EXT-X-I-FRAME-STREAM-INF entry.
type iframePlaylistResult struct {
	RelativePath string // path of iframes.m3u8 from the master.m3u8 location
	Width        int
	Height       int
	BandwidthBps int // approximate average — purely for player ABR ranking
}

// generateHLSIFramePlaylist runs a lightweight second ffmpeg pass that emits
// an I-frame-only HLS playlist suitable for scrubber/seek previews. We follow
// the Apple-style convention: one shared I-frame stream at a small fixed
// resolution (~240p), not one per variant. Most players use this purely to
// draw thumbnails, so a single low-res stream is sufficient and far cheaper
// than re-encoding once per quality rung.
//
// The output lives at <packageDir>/hls/iframes/iframes.m3u8 with mpegts
// segments alongside it. The selector filter (`select=eq(pict_type,I)`)
// drops every frame that is not a key frame, so the resulting file is
// minuscule even for long videos.
func generateHLSIFramePlaylist(ctx context.Context, inputPath string, sourceHeight int, packageDir string) (*iframePlaylistResult, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH")
	}
	// Clamp target height so we never upscale. 240 is canonical for thumbnail
	// scrubbing; fall back to the source height if smaller.
	targetHeight := 240
	if sourceHeight > 0 && sourceHeight < targetHeight {
		targetHeight = sourceHeight
	}

	dir := filepath.Join(packageDir, "hls", "iframes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	segmentPattern := "iframe_%05d.ts"
	args := []string{
		"-y", "-i", inputPath,
		"-an",
		// Keep only key frames; vsync vfr lets ffmpeg honor that selection
		// without inventing duplicate frames.
		"-vf", fmt.Sprintf("select=eq(pict_type\\,I),scale=-2:%d", targetHeight),
		"-fps_mode", "vfr",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "30",
		"-profile:v", "high",
		"-level", "4.1",
		"-f", "hls",
		"-hls_time", "4",
		"-hls_playlist_type", "vod",
		"-hls_flags", "iframes_only+independent_segments",
		"-hls_segment_filename", segmentPattern,
		"iframes.m3u8",
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg iframes: %w: %s", err, tail(stderr.String(), 1200))
	}
	playlist := filepath.Join(dir, "iframes.m3u8")
	if _, err := os.Stat(playlist); err != nil {
		return nil, fmt.Errorf("iframes playlist missing: %w", err)
	}

	// Approximate bandwidth: total bytes / duration in seconds * 8.
	totalBytes := int64(0)
	segments, _ := filepath.Glob(filepath.Join(dir, "iframe_*.ts"))
	for _, seg := range segments {
		if st, err := os.Stat(seg); err == nil {
			totalBytes += st.Size()
		}
	}
	approxBandwidth := 0
	if totalBytes > 0 {
		// Without a parsed duration, assume each segment is at most hls_time=4s
		// and use the number of segments as a coarse divisor. Off by a small
		// constant — only matters for ABR ranking, which never picks I-frame
		// streams for playback anyway.
		seconds := len(segments) * 4
		if seconds > 0 {
			approxBandwidth = int(totalBytes*8) / seconds
		}
	}
	if approxBandwidth <= 0 {
		approxBandwidth = 200_000
	}

	// Guess width assuming 16:9. Players largely ignore the width attribute on
	// I-frame streams and only use the URI, so this is best-effort.
	width := (targetHeight * 16) / 9
	if width%2 != 0 {
		width++
	}

	return &iframePlaylistResult{
		RelativePath: "iframes/iframes.m3u8",
		Width:        width,
		Height:       targetHeight,
		BandwidthBps: approxBandwidth,
	}, nil
}
