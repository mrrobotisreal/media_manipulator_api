package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	dashSegmentSeconds   = 2
	dashAudioBitrateKbps = 128
	dashAudioSampleRate  = 48000
	dashAudioChannels    = 2
	dashAudioInit        = "init.mp4"
	dashAudioSegment     = "seg-$Number%05d$.m4s"
)

// dashVariantResult collects per-rendition artifacts for manifest assembly.
type dashVariantResult struct {
	Profile      QualityProfile
	Height       int
	Width        int
	FPS          float64
	BasePath     string // "<label>" — relative to dash/ root
	InitName     string
	SegmentTpl   string
	SegmentCount int
	OutputBytes  int64
	VideoCodec   string // "av1" or "vp9"
}

type dashAudioResult struct {
	BasePath     string
	InitName     string
	SegmentTpl   string
	SegmentCount int
	OutputBytes  int64
	BitrateKbps  int
	SampleRate   int
	Channels     int
}

// transcodeToDASH packages a VOD MPEG-DASH bundle. codec is "av1" or "vp9".
// AV1 picks the best available encoder via av1Encoders(); vp9 always uses
// libvpx-vp9. Audio (if present) is encoded once as AAC and shared across
// representations through manifest references.
func transcodeToDASH(ctx context.Context, inputPath string, profiles []QualityProfile, sourceFPS float64, hasAudio bool, codec string, outputDir string, onVariantProgress func(label string, percent int)) ([]dashVariantResult, *dashAudioResult, string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, nil, "", fmt.Errorf("ffmpeg not found in PATH")
	}
	dashRoot := filepath.Join(outputDir, "dash")
	if err := os.MkdirAll(dashRoot, 0o755); err != nil {
		return nil, nil, "", err
	}

	codec = strings.ToLower(strings.TrimSpace(codec))
	if codec != "av1" && codec != "vp9" {
		codec = "av1"
	}
	encoder := ""
	if codec == "av1" {
		encoders, err := availableFFmpegEncoders(ctx)
		if err != nil {
			return nil, nil, "", err
		}
		for _, name := range []string{"av1_nvenc", "libsvtav1", "libaom-av1"} {
			if _, ok := encoders[name]; ok {
				encoder = name
				break
			}
		}
		if encoder == "" {
			return nil, nil, "", fmt.Errorf("no supported AV1 encoder found (expected av1_nvenc, libsvtav1, or libaom-av1)")
		}
	} else {
		encoder = "libvpx-vp9"
	}

	results := make([]dashVariantResult, 0, len(profiles))
	for _, profile := range profiles {
		variantDir := filepath.Join(dashRoot, profile.Label)
		if err := os.MkdirAll(variantDir, 0o755); err != nil {
			return nil, nil, "", err
		}
		outputFPS := sourceFPS
		if outputFPS <= 0 {
			outputFPS = 30
		}
		gop := keyframeIntervalFrames(outputFPS, dashSegmentSeconds)
		args := []string{"-y", "-i", inputPath, "-map", "0:v:0", "-an"}
		if outputFPS > 0 {
			args = append(args, "-r", formatFrameRate(outputFPS))
		}
		args = append(args, "-vf", fmt.Sprintf("scale=-2:%d", profile.Height))
		videoBitrate := fmt.Sprintf("%dk", profile.VideoBitrateKbps)
		maxrate := fmt.Sprintf("%dk", int(float64(profile.VideoBitrateKbps)*12/10))
		bufsize := fmt.Sprintf("%dk", profile.VideoBitrateKbps*2)
		switch encoder {
		case "av1_nvenc":
			args = append(args,
				"-c:v", "av1_nvenc",
				"-preset", "p5",
				"-rc", "vbr",
				"-cq", strconv.Itoa(profile.CRF),
				"-g", strconv.Itoa(gop),
				"-b:v", videoBitrate,
				"-maxrate", maxrate,
				"-bufsize", bufsize,
			)
		case "libsvtav1":
			args = append(args,
				"-c:v", "libsvtav1",
				"-preset", "8",
				"-svtav1-params", "rc=1",
				"-g", strconv.Itoa(gop),
				"-b:v", videoBitrate,
			)
		case "libaom-av1":
			args = append(args,
				"-c:v", "libaom-av1",
				"-cpu-used", "4",
				"-crf", strconv.Itoa(profile.CRF),
				"-g", strconv.Itoa(gop),
				"-b:v", videoBitrate,
				"-maxrate", maxrate,
				"-bufsize", bufsize,
			)
		default: // libvpx-vp9
			args = append(args,
				"-c:v", "libvpx-vp9",
				"-quality", "good",
				"-speed", "1",
				"-tile-columns", "2",
				"-frame-parallel", "1",
				"-auto-alt-ref", "1",
				"-lag-in-frames", "25",
				"-row-mt", "1",
				"-g", strconv.Itoa(gop),
				"-keyint_min", strconv.Itoa(gop),
				"-crf", strconv.Itoa(profile.CRF),
				"-b:v", videoBitrate,
				"-maxrate", maxrate,
				"-bufsize", bufsize,
			)
		}
		args = append(args,
			"-force_key_frames", forceKeyFramesExpr(dashSegmentSeconds),
			"-f", "dash",
			"-seg_duration", strconv.Itoa(dashSegmentSeconds),
			"-use_template", "1",
			"-use_timeline", "0",
			"-dash_segment_type", "mp4",
			"-single_file", "0",
			"-init_seg_name", "init.mp4",
			"-media_seg_name", "seg-$Number%05d$.m4s",
			"manifest.mpd",
		)
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		cmd.Dir = variantDir
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, nil, "", fmt.Errorf("ffmpeg DASH %s/%s: %w: %s", codec, profile.Label, err, tail(stderr.String(), 1500))
		}
		// We don't need the per-variant manifest — the unified manifest.mpd at the dash/ root will reference variants by path.
		_ = os.Remove(filepath.Join(variantDir, "manifest.mpd"))
		segments, _ := filepath.Glob(filepath.Join(variantDir, segmentGlobFromPattern("seg-$Number%05d$.m4s")))
		sort.Strings(segments)
		var bytesTotal int64
		for _, seg := range segments {
			if st, err := os.Stat(seg); err == nil {
				bytesTotal += st.Size()
			}
		}
		if init, err := os.Stat(filepath.Join(variantDir, "init.mp4")); err == nil {
			bytesTotal += init.Size()
		}
		results = append(results, dashVariantResult{
			Profile:      profile,
			Height:       profile.Height,
			FPS:          outputFPS,
			BasePath:     profile.Label,
			InitName:     "init.mp4",
			SegmentTpl:   "seg-$Number%05d$.m4s",
			SegmentCount: len(segments),
			OutputBytes:  bytesTotal,
			VideoCodec:   codec,
		})
		if onVariantProgress != nil {
			onVariantProgress(profile.Label, 100)
		}
	}

	var audio *dashAudioResult
	if hasAudio {
		audioDir := filepath.Join(dashRoot, "audio", "128k")
		if err := os.MkdirAll(audioDir, 0o755); err != nil {
			return nil, nil, "", err
		}
		args := []string{"-y", "-i", inputPath, "-map", "0:a:0", "-vn",
			"-c:a", "aac",
			"-b:a", fmt.Sprintf("%dk", dashAudioBitrateKbps),
			"-ac", strconv.Itoa(dashAudioChannels),
			"-ar", strconv.Itoa(dashAudioSampleRate),
			"-f", "dash",
			"-seg_duration", strconv.Itoa(dashSegmentSeconds),
			"-use_template", "1",
			"-use_timeline", "0",
			"-dash_segment_type", "mp4",
			"-single_file", "0",
			"-init_seg_name", dashAudioInit,
			"-media_seg_name", dashAudioSegment,
			"manifest.mpd",
		}
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		cmd.Dir = audioDir
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, nil, "", fmt.Errorf("ffmpeg DASH audio: %w: %s", err, tail(stderr.String(), 1500))
		}
		_ = os.Remove(filepath.Join(audioDir, "manifest.mpd"))
		segs, _ := filepath.Glob(filepath.Join(audioDir, segmentGlobFromPattern(dashAudioSegment)))
		sort.Strings(segs)
		var bytesTotal int64
		for _, seg := range segs {
			if st, err := os.Stat(seg); err == nil {
				bytesTotal += st.Size()
			}
		}
		if init, err := os.Stat(filepath.Join(audioDir, dashAudioInit)); err == nil {
			bytesTotal += init.Size()
		}
		audio = &dashAudioResult{
			BasePath:     "audio/128k",
			InitName:     dashAudioInit,
			SegmentTpl:   dashAudioSegment,
			SegmentCount: len(segs),
			OutputBytes:  bytesTotal,
			BitrateKbps:  dashAudioBitrateKbps,
			SampleRate:   dashAudioSampleRate,
			Channels:     dashAudioChannels,
		}
	}

	manifestPath := filepath.Join(dashRoot, "manifest.mpd")
	if err := writeDashManifest(manifestPath, results, audio, dashSegmentSeconds); err != nil {
		return nil, nil, "", fmt.Errorf("write dash manifest: %w", err)
	}
	return results, audio, manifestPath, nil
}

// availableFFmpegEncoders parses `ffmpeg -hide_banner -encoders` and returns a
// set of encoder names. Used to pick the best AV1 encoder available locally.
func availableFFmpegEncoders(ctx context.Context) (map[string]struct{}, error) {
	stdout, stderr, err := runCommand(ctx, "ffmpeg", "-hide_banner", "-encoders")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg -encoders failed: %w: %s", err, strings.TrimSpace(stderr))
	}
	out := map[string]struct{}{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Encoders:") || strings.HasPrefix(line, "--") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			out[fields[1]] = struct{}{}
		}
	}
	return out, nil
}

// dashVideoCodecID maps the codec name we passed FFmpeg to the codecs attr
// the DASH manifest expects.
func dashVideoCodecID(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "av1":
		return "av01.0.08M.08"
	case "vp9":
		return "vp09.00.51.08"
	}
	return ""
}

// writeDashManifest emits a static VOD MPD that references each variant by
// the local path inside the package (the same relative path we'll embed in the tarball).
func writeDashManifest(dest string, videoEntries []dashVariantResult, audio *dashAudioResult, segmentSeconds int) error {
	if len(videoEntries) == 0 {
		return fmt.Errorf("no DASH video entries")
	}
	if segmentSeconds <= 0 {
		segmentSeconds = dashSegmentSeconds
	}
	durationISO := "PT3600S" // upper bound; players will use segment count anyway
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="%s" minBufferTime="PT4S" profiles="urn:mpeg:dash:profile:isoff-main:2011">`+"\n", durationISO))
	b.WriteString(fmt.Sprintf(`<Period duration="%s">`+"\n", durationISO))
	b.WriteString(`<AdaptationSet id="0" contentType="video" mimeType="video/mp4" segmentAlignment="true" bitstreamSwitching="true">` + "\n")
	sort.SliceStable(videoEntries, func(i, j int) bool { return videoEntries[i].Height < videoEntries[j].Height })
	for _, entry := range videoEntries {
		width := entry.Width
		if width <= 0 {
			width = (entry.Height * 16) / 9
			if width%2 != 0 {
				width++
			}
		}
		codecsAttr := dashVideoCodecID(entry.VideoCodec)
		if codecsAttr == "" {
			codecsAttr = "avc1.640028"
		}
		fps := entry.FPS
		if fps <= 0 {
			fps = 30
		}
		initPath := path.Join(entry.BasePath, entry.InitName)
		mediaPath := path.Join(entry.BasePath, entry.SegmentTpl)
		bandwidth := entry.Profile.VideoBitrateKbps * 1000
		b.WriteString(fmt.Sprintf(
			`  <Representation id="%s" bandwidth="%d" width="%d" height="%d" frameRate="%s" codecs="%s">`+"\n",
			entry.Profile.Label, bandwidth, width, entry.Height, formatFrameRate(fps), codecsAttr,
		))
		b.WriteString(fmt.Sprintf(
			`    <SegmentTemplate initialization="%s" media="%s" startNumber="1" timescale="1" duration="%d"/>`+"\n",
			initPath, mediaPath, segmentSeconds,
		))
		b.WriteString("  </Representation>\n")
	}
	b.WriteString("</AdaptationSet>\n")
	if audio != nil {
		b.WriteString(`<AdaptationSet id="1" contentType="audio" mimeType="audio/mp4" segmentAlignment="true" bitstreamSwitching="true">` + "\n")
		initPath := path.Join(audio.BasePath, audio.InitName)
		mediaPath := path.Join(audio.BasePath, audio.SegmentTpl)
		b.WriteString(fmt.Sprintf(
			`  <Representation id="audio-128k" bandwidth="%d" codecs="mp4a.40.2" audioSamplingRate="%d">`+"\n",
			audio.BitrateKbps*1000, audio.SampleRate,
		))
		b.WriteString(fmt.Sprintf(
			`    <AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="%d"/>`+"\n",
			audio.Channels,
		))
		b.WriteString(fmt.Sprintf(
			`    <SegmentTemplate initialization="%s" media="%s" startNumber="1" timescale="1" duration="%d"/>`+"\n",
			initPath, mediaPath, segmentSeconds,
		))
		b.WriteString("  </Representation>\n")
		b.WriteString("</AdaptationSet>\n")
	}
	b.WriteString("</Period>\n</MPD>\n")
	return os.WriteFile(dest, []byte(b.String()), 0o644)
}

// segmentGlobFromPattern converts FFmpeg's `seg-$Number%05d$.m4s` template
// into a filepath.Glob pattern matching every emitted segment.
func segmentGlobFromPattern(pattern string) string {
	value := strings.TrimSpace(pattern)
	if value == "" {
		return "*"
	}
	numberToken := regexp.MustCompile(`\$Number[^$]*\$`)
	value = numberToken.ReplaceAllString(value, "*")
	value = regexp.MustCompile(`%0?\d*d`).ReplaceAllString(value, "*")
	value = strings.ReplaceAll(value, "$", "")
	return value
}

// ListAV1Encoders is exposed for the capabilities endpoint.
func ListAV1Encoders(ctx context.Context) ([]string, string) {
	encs, err := availableFFmpegEncoders(ctx)
	if err != nil {
		return nil, ""
	}
	have := []string{}
	for _, name := range []string{"av1_nvenc", "libsvtav1", "libaom-av1"} {
		if _, ok := encs[name]; ok {
			have = append(have, name)
		}
	}
	selected := ""
	if len(have) > 0 {
		selected = have[0]
	}
	return have, selected
}

// HasVP9Encoder reports whether libvpx-vp9 is available.
func HasVP9Encoder(ctx context.Context) bool {
	encs, err := availableFFmpegEncoders(ctx)
	if err != nil {
		return false
	}
	_, ok := encs["libvpx-vp9"]
	return ok
}
