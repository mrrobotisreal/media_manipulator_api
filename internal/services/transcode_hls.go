package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	hlsSegmentSeconds = 2
	hlsSegmentDir     = "segments"
)

// hlsVariantResult collects the artifacts of a single HLS rendition for use
// when writing the master playlist later.
type hlsVariantResult struct {
	Profile      QualityProfile
	Width        int
	Height       int
	FPS          float64
	PlaylistRel  string // path of variant index.m3u8 relative to hls/ root
	OutputBytes  int64
	SegmentCount int
}

// transcodeToHLS produces an H.264/AAC HLS VOD package at outputDir/hls/ and
// returns one entry per variant. It is safe to call with hasAudio=false — the
// FFmpeg invocation skips audio mapping in that case.
func transcodeToHLS(ctx context.Context, inputPath string, profiles []QualityProfile, sourceFPS float64, hasAudio bool, outputDir string, onVariantProgress func(label string, percent int)) ([]hlsVariantResult, string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, "", fmt.Errorf("ffmpeg not found in PATH")
	}
	hlsRoot := filepath.Join(outputDir, "hls")
	if err := os.MkdirAll(hlsRoot, 0o755); err != nil {
		return nil, "", err
	}
	results := make([]hlsVariantResult, 0, len(profiles))
	for _, profile := range profiles {
		variantDir := filepath.Join(hlsRoot, profile.Label)
		if err := os.MkdirAll(filepath.Join(variantDir, hlsSegmentDir), 0o755); err != nil {
			return nil, "", err
		}
		outputFPS := sourceFPS
		if outputFPS <= 0 {
			outputFPS = 30
		}
		gop := keyframeIntervalFrames(outputFPS, hlsSegmentSeconds)
		segmentPattern := filepath.Join(hlsSegmentDir, fmt.Sprintf("%s_%%05d.ts", profile.Label))
		args := []string{"-y", "-i", inputPath}
		args = append(args, "-map", "0:v:0", "-vf", fmt.Sprintf("scale=-2:%d", profile.Height))
		if outputFPS > 0 {
			args = append(args, "-r", formatFrameRate(outputFPS))
		}
		args = append(args,
			"-c:v", "libx264",
			"-profile:v", "high",
			"-level", "4.1",
			"-preset", profile.Preset,
			"-crf", strconv.Itoa(profile.CRF),
			"-sc_threshold", "0",
			"-g", strconv.Itoa(gop),
			"-keyint_min", strconv.Itoa(gop),
			"-b:v", fmt.Sprintf("%dk", profile.VideoBitrateKbps),
			"-maxrate", fmt.Sprintf("%dk", int(float64(profile.VideoBitrateKbps)*12/10)),
			"-bufsize", fmt.Sprintf("%dk", profile.VideoBitrateKbps*2),
			"-force_key_frames", forceKeyFramesExpr(hlsSegmentSeconds),
		)
		if hasAudio {
			args = append(args,
				"-map", "0:a:0?",
				"-c:a", "aac",
				"-b:a", fmt.Sprintf("%dk", profile.AudioBitrateKbps),
				"-ac", "2",
				"-ar", "48000",
			)
		} else {
			args = append(args, "-an")
		}
		args = append(args,
			"-hls_time", strconv.Itoa(hlsSegmentSeconds),
			"-hls_playlist_type", "vod",
			"-hls_segment_filename", segmentPattern,
			"-hls_flags", "independent_segments",
			"index.m3u8",
		)
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		cmd.Dir = variantDir
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, "", fmt.Errorf("ffmpeg HLS %s: %w: %s", profile.Label, err, tail(stderr.String(), 1500))
		}
		segments, _ := filepath.Glob(filepath.Join(variantDir, hlsSegmentDir, fmt.Sprintf("%s_*.ts", profile.Label)))
		sort.Strings(segments)
		var bytesTotal int64
		for _, seg := range segments {
			if st, err := os.Stat(seg); err == nil {
				bytesTotal += st.Size()
			}
		}
		if pl, err := os.Stat(filepath.Join(variantDir, "index.m3u8")); err == nil {
			bytesTotal += pl.Size()
		}
		w := computeVariantWidth(0, 0, profile.Height) // unknown source dims here; left to manifest writer to fill
		results = append(results, hlsVariantResult{
			Profile:      profile,
			Width:        w,
			Height:       profile.Height,
			FPS:          outputFPS,
			PlaylistRel:  filepath.ToSlash(filepath.Join(profile.Label, "index.m3u8")),
			OutputBytes:  bytesTotal,
			SegmentCount: len(segments),
		})
		if onVariantProgress != nil {
			onVariantProgress(profile.Label, 100)
		}
	}

	masterPath := filepath.Join(hlsRoot, "master.m3u8")
	if err := writeHLSMaster(masterPath, results, hasAudio); err != nil {
		return nil, "", fmt.Errorf("write master playlist: %w", err)
	}
	return results, masterPath, nil
}

// writeHLSMaster composes master.m3u8. CODECS attribute defaults to H.264 High
// + AAC LC; if hasAudio is false we omit the AAC codec hint.
func writeHLSMaster(dest string, entries []hlsVariantResult, hasAudio bool) error {
	if len(entries) == 0 {
		return fmt.Errorf("no entries for master playlist")
	}
	sorted := append([]hlsVariantResult{}, entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Profile.Height < sorted[j].Profile.Height
	})
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	for _, entry := range sorted {
		bandwidth := entry.Profile.VideoBitrateKbps * 1000
		if hasAudio {
			bandwidth += entry.Profile.AudioBitrateKbps * 1000
		}
		codecs := "avc1.640028"
		if hasAudio {
			codecs += ",mp4a.40.2"
		}
		resolution := ""
		if entry.Profile.Height > 0 {
			width := entry.Width
			if width <= 0 {
				// 16:9 default if width unknown
				width = (entry.Profile.Height * 16) / 9
				if width%2 != 0 {
					width++
				}
			}
			resolution = fmt.Sprintf(",RESOLUTION=%dx%d", width, entry.Profile.Height)
		}
		frameRate := ""
		if entry.FPS > 0 {
			frameRate = fmt.Sprintf(",FRAME-RATE=%s", formatFrameRate(entry.FPS))
		}
		b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d%s,CODECS=\"%s\"%s,NAME=\"%s\"\n",
			bandwidth, resolution, codecs, frameRate, entry.Profile.Label,
		))
		b.WriteString(entry.PlaylistRel)
		b.WriteString("\n")
	}
	return os.WriteFile(dest, []byte(b.String()), 0o644)
}
