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

// hlsExtras carries the optional extras the master playlist needs to wire up
// (caption renditions, I-frame playlist, signature). It is built outside the
// HLS encoder and passed in so the encoder stays focused on FFmpeg work.
type hlsExtras struct {
	Captions        []CaptionTrack // captions/<lang>/subs.m3u8 already written
	IFramePlaylist  string         // relative path of iframes/iframes.m3u8 or ""
	IFrameBandwidth int
	IFrameWidth     int
	IFrameHeight    int
	SignatureKVs    []hlsSessionData
}

type hlsSessionData struct {
	DataID string
	Value  string
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
	if err := writeHLSMaster(masterPath, results, hasAudio, hlsExtras{}); err != nil {
		return nil, "", fmt.Errorf("write master playlist: %w", err)
	}
	return results, masterPath, nil
}

// rewriteHLSMaster overwrites the master.m3u8 we wrote during transcodeToHLS
// once the late-binding extras (captions / I-frames / signature) are known.
// This is what the orchestrator calls after generating all the auxiliary files.
func rewriteHLSMaster(outputDir string, entries []hlsVariantResult, hasAudio bool, extras hlsExtras) error {
	masterPath := filepath.Join(outputDir, "hls", "master.m3u8")
	return writeHLSMaster(masterPath, entries, hasAudio, extras)
}

// writeHLSMaster composes master.m3u8. CODECS attribute defaults to H.264 High
// + AAC LC; if hasAudio is false we omit the AAC codec hint.
//
// extras carry caption renditions (EXT-X-MEDIA TYPE=SUBTITLES), the optional
// I-frame playlist (EXT-X-I-FRAME-STREAM-INF), and free-form signature blobs
// (EXT-X-SESSION-DATA). Pass an empty hlsExtras{} to emit a bare master.
//
// HLS path notes: all variant paths in this master are relative to the
// master.m3u8 location at <pkg>/hls/master.m3u8. Captions live at
// <pkg>/captions/<lang>/subs.m3u8 — outside hls/ — so we prefix "../" to
// reach them. I-frame playlists live at <pkg>/hls/iframes/iframes.m3u8 so
// they don't need the "../" prefix.
func writeHLSMaster(dest string, entries []hlsVariantResult, hasAudio bool, extras hlsExtras) error {
	if len(entries) == 0 {
		return fmt.Errorf("no entries for master playlist")
	}
	sorted := append([]hlsVariantResult{}, entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Profile.Height < sorted[j].Profile.Height
	})
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	// Signature comments + EXT-X-SESSION-DATA so any HLS parser keeps the
	// attribution intact.
	if len(extras.SignatureKVs) > 0 {
		for _, kv := range extras.SignatureKVs {
			b.WriteString(fmt.Sprintf("# %s: %s\n", kv.DataID, kv.Value))
		}
		for _, kv := range extras.SignatureKVs {
			b.WriteString(fmt.Sprintf("#EXT-X-SESSION-DATA:DATA-ID=\"%s\",VALUE=\"%s\"\n",
				escapeM3UAttr(kv.DataID), escapeM3UAttr(kv.Value)))
		}
	}

	// Caption renditions. The captions/ folder lives one level above hls/, so
	// every URI gets a "../" prefix.
	hasCaptions := len(extras.Captions) > 0
	if hasCaptions {
		for _, track := range extras.Captions {
			defaultFlag := "NO"
			autoselectFlag := "NO"
			if track.IsPrimary {
				defaultFlag = "YES"
				autoselectFlag = "YES"
			}
			b.WriteString(fmt.Sprintf(
				"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"%s\",LANGUAGE=\"%s\",DEFAULT=%s,AUTOSELECT=%s,FORCED=NO,URI=\"../%s\"\n",
				escapeM3UAttr(track.DisplayName),
				escapeM3UAttr(track.Language),
				defaultFlag, autoselectFlag,
				track.HLSWrapper,
			))
		}
	}

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
		captionsAttr := ""
		if hasCaptions {
			captionsAttr = ",SUBTITLES=\"subs\""
		}
		b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d%s,CODECS=\"%s\"%s%s,NAME=\"%s\"\n",
			bandwidth, resolution, codecs, frameRate, captionsAttr, entry.Profile.Label,
		))
		b.WriteString(entry.PlaylistRel)
		b.WriteString("\n")
	}

	// I-frame playlist for scrubbing — single entry referencing a separate
	// I-frame-only stream so players can seek without downloading full segments.
	if extras.IFramePlaylist != "" {
		bandwidth := extras.IFrameBandwidth
		if bandwidth <= 0 {
			bandwidth = 200_000
		}
		resolution := ""
		if extras.IFrameWidth > 0 && extras.IFrameHeight > 0 {
			resolution = fmt.Sprintf(",RESOLUTION=%dx%d", extras.IFrameWidth, extras.IFrameHeight)
		}
		b.WriteString(fmt.Sprintf("#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=%d%s,CODECS=\"avc1.640028\",URI=\"%s\"\n",
			bandwidth, resolution, extras.IFramePlaylist,
		))
	}

	return os.WriteFile(dest, []byte(b.String()), 0o644)
}

// escapeM3UAttr escapes characters that would break a quoted-string attribute
// in an M3U tag. The HLS spec disallows raw " and newlines inside quoted
// attribute values, so we strip them aggressively.
func escapeM3UAttr(s string) string {
	s = strings.ReplaceAll(s, "\"", "'")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}
