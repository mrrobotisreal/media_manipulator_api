package services

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Specialized tool modes. These are recognized in the conversion handler's
// `processConversion` dispatch and consumed by SpecializedTools below. Each
// mode is a short, snake_case identifier that the frontend sets on
// `options.mode` when posting through the standard /api/upload (or video
// presign) flow — no separate endpoint is required for these because they
// take exactly one media file.
const (
	SpecializedModeAudioWaveform   = "audio_waveform"
	SpecializedModeExtractAudio    = "extract_audio"
	SpecializedModeExtractVideoOnly = "extract_video_only"
	SpecializedModeExtractFrames   = "extract_frames"
)

// SpecializedToolsService runs the small set of FFmpeg-driven utilities that
// don't fit cleanly inside the main converter (waveform render, raw stream
// extraction, frame export). It mirrors the structure of the existing
// Converter / TranscriptionService and writes its result to the standard
// per-job output directory so the existing /api/download/:jobId handler
// keeps working unchanged.
type SpecializedToolsService struct {
	cfg        *config.Config
	jobManager *JobManager
}

func NewSpecializedToolsService(cfg *config.Config, jm *JobManager) *SpecializedToolsService {
	return &SpecializedToolsService{cfg: cfg, jobManager: jm}
}

// Run dispatches a job to the right specialized tool based on the mode option.
// outputPath is the final artifact path with the extension already resolved
// by the handler (see getOutputExtension in the conversion handler).
func (s *SpecializedToolsService) Run(ctx context.Context, job *models.ConversionJob, mode, inputPath, outputPath string) error {
	switch mode {
	case SpecializedModeAudioWaveform:
		return s.runAudioWaveform(ctx, job, inputPath, outputPath)
	case SpecializedModeExtractAudio:
		return s.runExtractAudio(ctx, job, inputPath, outputPath)
	case SpecializedModeExtractVideoOnly:
		return s.runExtractVideoOnly(ctx, job, inputPath, outputPath)
	case SpecializedModeExtractFrames:
		return s.runExtractFrames(ctx, job, inputPath, outputPath)
	default:
		return fmt.Errorf("unsupported specialized tool mode: %s", mode)
	}
}

func (s *SpecializedToolsService) progress(jobID string, percent int) {
	if s.jobManager == nil {
		return
	}
	s.jobManager.SendProgressUpdate(jobID, percent)
}

// ----------------------------------------------------------------------- //
// AUDIO WAVEFORM
// ----------------------------------------------------------------------- //

// AudioWaveformOptions captures the validated, structured options that this
// tool accepts. The handler is responsible for unmarshalling the raw
// options map into this struct (via mapToStruct in the dispatcher) — we
// re-validate every field here regardless, because untrusted JSON should
// never be turned into a raw FFmpeg filter string.
type AudioWaveformOptions struct {
	// OutputSelection: "video" (default), "image", or "both". "both" returns a
	// ZIP containing the waveform .mp4 + .png so the existing single-file
	// download handler remains the source of truth.
	OutputSelection string `json:"outputSelection"`
	// VideoFormat: mp4 (default), webm. Only validated values are accepted.
	VideoFormat string `json:"videoFormat"`
	// ImageFormat: png (default), webp.
	ImageFormat string `json:"imageFormat"`
	// Width / Height: positive even integers. Defaults: 1600 x 160 (10:1
	// wide-waveform shape, our recommended default for podcast/music clips).
	Width  int `json:"width"`
	Height int `json:"height"`
	// Mode: showwaves --mode value. point/line/p2p/cline. Default: point.
	Mode string `json:"mode"`
	// FrameRate: showwaves --rate, frames per second. Mutually exclusive with
	// N. Default: 25.
	FrameRate float64 `json:"rate"`
	// N: showwaves --n, samples per column. Mutually exclusive with rate.
	N int `json:"n"`
	// SplitChannels: stereo channel split on/off.
	SplitChannels bool `json:"splitChannels"`
	// ColorPrimary / ColorSecondary: validated hex colors. Defaults pick from
	// theme-compatible cyan/magenta cyber palette.
	ColorPrimary   string `json:"colorPrimary"`
	ColorSecondary string `json:"colorSecondary"`
	// Scale: lin / log / sqrt / cbrt. Default: lin.
	Scale string `json:"scale"`
	// Draw: scale / full. Default: scale.
	Draw string `json:"draw"`
}

const (
	waveformDefaultWidth  = 1600
	waveformDefaultHeight = 160
	waveformMinDim        = 64
	waveformMaxDim        = 7680
	waveformMaxFPS        = 120
	waveformMinFPS        = 1
	waveformDefaultRate   = 25
	waveformDefaultPrimary   = "#22D3EE" // cyan-400 — pairs with the sci-fi UI theme
	waveformDefaultSecondary = "#A855F7" // purple-500 — secondary stereo channel
)

var hexColorRegexp = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// applyDefaults clamps unset / out-of-range options to safe defaults and
// reports a structured error if the input is unrecoverable. This is the
// single point of validation between an untrusted client and the FFmpeg
// command line — never weaken it.
func (o *AudioWaveformOptions) applyDefaults() error {
	o.OutputSelection = strings.ToLower(strings.TrimSpace(o.OutputSelection))
	if o.OutputSelection == "" {
		o.OutputSelection = "video"
	}
	switch o.OutputSelection {
	case "video", "image", "both":
	default:
		return fmt.Errorf("invalid outputSelection: %q (expected video|image|both)", o.OutputSelection)
	}

	o.VideoFormat = strings.ToLower(strings.TrimSpace(o.VideoFormat))
	if o.VideoFormat == "" {
		o.VideoFormat = "mp4"
	}
	switch o.VideoFormat {
	case "mp4", "webm":
	default:
		return fmt.Errorf("invalid videoFormat: %q (expected mp4|webm)", o.VideoFormat)
	}

	o.ImageFormat = strings.ToLower(strings.TrimSpace(o.ImageFormat))
	if o.ImageFormat == "" {
		o.ImageFormat = "png"
	}
	switch o.ImageFormat {
	case "png", "webp":
	default:
		return fmt.Errorf("invalid imageFormat: %q (expected png|webp)", o.ImageFormat)
	}

	if o.Width <= 0 {
		o.Width = waveformDefaultWidth
	}
	if o.Height <= 0 {
		o.Height = waveformDefaultHeight
	}
	if o.Width < waveformMinDim || o.Width > waveformMaxDim {
		return fmt.Errorf("invalid width: %d (allowed range %d-%d)", o.Width, waveformMinDim, waveformMaxDim)
	}
	if o.Height < waveformMinDim || o.Height > waveformMaxDim {
		return fmt.Errorf("invalid height: %d (allowed range %d-%d)", o.Height, waveformMinDim, waveformMaxDim)
	}
	// Force even dimensions for h264/vp9 friendliness.
	if o.Width%2 != 0 {
		o.Width++
	}
	if o.Height%2 != 0 {
		o.Height++
	}

	o.Mode = strings.ToLower(strings.TrimSpace(o.Mode))
	if o.Mode == "" {
		o.Mode = "point"
	}
	switch o.Mode {
	case "point", "line", "p2p", "cline":
	default:
		return fmt.Errorf("invalid mode: %q (expected point|line|p2p|cline)", o.Mode)
	}

	// rate vs. n are mutually exclusive in showwaves. If both are set we honor
	// rate (the documented default) and drop n with a clear validation error
	// rather than silently picking one.
	if o.FrameRate != 0 && o.N != 0 {
		return errors.New("specify either rate or n, not both")
	}
	if o.FrameRate == 0 && o.N == 0 {
		o.FrameRate = waveformDefaultRate
	}
	if o.FrameRate != 0 && (o.FrameRate < waveformMinFPS || o.FrameRate > waveformMaxFPS) {
		return fmt.Errorf("invalid rate: %v (allowed range %d-%d)", o.FrameRate, waveformMinFPS, waveformMaxFPS)
	}
	if o.N != 0 && (o.N < 1 || o.N > 1024) {
		return fmt.Errorf("invalid n: %d (allowed range 1-1024)", o.N)
	}

	if o.ColorPrimary == "" {
		o.ColorPrimary = waveformDefaultPrimary
	}
	if o.ColorSecondary == "" {
		o.ColorSecondary = waveformDefaultSecondary
	}
	if !hexColorRegexp.MatchString(o.ColorPrimary) {
		return fmt.Errorf("invalid colorPrimary: %q (expected #RRGGBB)", o.ColorPrimary)
	}
	if !hexColorRegexp.MatchString(o.ColorSecondary) {
		return fmt.Errorf("invalid colorSecondary: %q (expected #RRGGBB)", o.ColorSecondary)
	}

	o.Scale = strings.ToLower(strings.TrimSpace(o.Scale))
	if o.Scale == "" {
		o.Scale = "lin"
	}
	switch o.Scale {
	case "lin", "log", "sqrt", "cbrt":
	default:
		return fmt.Errorf("invalid scale: %q (expected lin|log|sqrt|cbrt)", o.Scale)
	}

	o.Draw = strings.ToLower(strings.TrimSpace(o.Draw))
	if o.Draw == "" {
		o.Draw = "scale"
	}
	switch o.Draw {
	case "scale", "full":
	default:
		return fmt.Errorf("invalid draw: %q (expected scale|full)", o.Draw)
	}
	return nil
}

// hexToFFmpegColor converts a #RRGGBB string into FFmpeg's 0xRRGGBB color
// syntax. FFmpeg also accepts the #RRGGBB literal in most builds, but the
// 0x form sidesteps shell-meta confusion when filter strings are inspected
// in logs.
func hexToFFmpegColor(hex string) string {
	return "0x" + strings.TrimPrefix(hex, "#")
}

func (s *SpecializedToolsService) runAudioWaveform(ctx context.Context, job *models.ConversionJob, inputPath, outputPath string) error {
	opts, err := parseAudioWaveformOptions(job.Options)
	if err != nil {
		return err
	}
	if err := opts.applyDefaults(); err != nil {
		return err
	}

	s.progress(job.ID, 10)

	// We always render into a job-local workdir and then move the result(s)
	// into place. For "both" we zip the workdir contents.
	workDir, err := os.MkdirTemp(filepath.Dir(outputPath), "waveform-")
	if err != nil {
		return fmt.Errorf("create waveform workdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	wantVideo := opts.OutputSelection == "video" || opts.OutputSelection == "both"
	wantImage := opts.OutputSelection == "image" || opts.OutputSelection == "both"

	var videoOut, imageOut string

	if wantVideo {
		videoOut = filepath.Join(workDir, "waveform."+opts.VideoFormat)
		if err := renderWaveformVideo(ctx, inputPath, videoOut, opts); err != nil {
			return fmt.Errorf("waveform video: %w", err)
		}
		s.progress(job.ID, 60)
	}
	if wantImage {
		imageOut = filepath.Join(workDir, "waveform."+opts.ImageFormat)
		if err := renderWaveformImage(ctx, inputPath, imageOut, opts); err != nil {
			return fmt.Errorf("waveform image: %w", err)
		}
		s.progress(job.ID, 85)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	switch opts.OutputSelection {
	case "video":
		if err := os.Rename(videoOut, outputPath); err != nil {
			return fmt.Errorf("finalize waveform video: %w", err)
		}
	case "image":
		if err := os.Rename(imageOut, outputPath); err != nil {
			return fmt.Errorf("finalize waveform image: %w", err)
		}
	case "both":
		if err := zipFiles(outputPath, []string{videoOut, imageOut}); err != nil {
			return fmt.Errorf("package waveform zip: %w", err)
		}
	}
	s.progress(job.ID, 100)
	return nil
}

func parseAudioWaveformOptions(raw map[string]any) (*AudioWaveformOptions, error) {
	out := &AudioWaveformOptions{}
	if raw == nil {
		return out, nil
	}
	// Nested options under "waveform" let the conversion form keep "mode" /
	// "format" reserved for the top-level dispatcher.
	if nested, ok := raw["waveform"].(map[string]any); ok {
		raw = nested
	}
	if v, ok := raw["outputSelection"].(string); ok {
		out.OutputSelection = v
	}
	if v, ok := raw["videoFormat"].(string); ok {
		out.VideoFormat = v
	}
	if v, ok := raw["imageFormat"].(string); ok {
		out.ImageFormat = v
	}
	out.Width = intFromAny(raw["width"])
	out.Height = intFromAny(raw["height"])
	if v, ok := raw["mode"].(string); ok {
		out.Mode = v
	}
	if v, ok := raw["rate"].(float64); ok {
		out.FrameRate = v
	} else if v, ok := raw["rate"].(int); ok {
		out.FrameRate = float64(v)
	}
	out.N = intFromAny(raw["n"])
	if v, ok := raw["splitChannels"].(bool); ok {
		out.SplitChannels = v
	}
	if v, ok := raw["colorPrimary"].(string); ok {
		out.ColorPrimary = v
	}
	if v, ok := raw["colorSecondary"].(string); ok {
		out.ColorSecondary = v
	}
	if v, ok := raw["scale"].(string); ok {
		out.Scale = v
	}
	if v, ok := raw["draw"].(string); ok {
		out.Draw = v
	}
	return out, nil
}

func intFromAny(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return 0
}

func renderWaveformVideo(ctx context.Context, inputPath, outputPath string, opts *AudioWaveformOptions) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg not found in PATH")
	}
	colorList := waveformColorList(opts)
	splitChannels := "0"
	if opts.SplitChannels {
		splitChannels = "1"
	}
	rateOrN := ""
	if opts.FrameRate > 0 {
		rateOrN = fmt.Sprintf("r=%v", opts.FrameRate)
	} else if opts.N > 0 {
		rateOrN = fmt.Sprintf("n=%d", opts.N)
	} else {
		rateOrN = fmt.Sprintf("r=%d", waveformDefaultRate)
	}
	// showwaves builds the animated waveform; format=yuv420p ensures broad
	// MP4 compatibility (some players reject yuv444).
	filter := fmt.Sprintf(
		"showwaves=s=%dx%d:mode=%s:%s:split_channels=%s:colors=%s:scale=%s:draw=%s,format=yuv420p",
		opts.Width, opts.Height, opts.Mode, rateOrN, splitChannels, colorList, opts.Scale, opts.Draw,
	)
	args := []string{"-y", "-i", inputPath, "-filter_complex", filter}
	if opts.VideoFormat == "webm" {
		args = append(args, "-c:v", "libvpx-vp9", "-b:v", "1M", "-an")
	} else {
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "medium", "-crf", "20", "-an")
	}
	args = append(args, outputPath)
	if _, stderr, err := runCommand(ctx, "ffmpeg", args...); err != nil {
		return fmt.Errorf("ffmpeg showwaves failed: %w (%s)", err, tail(stderr, 1500))
	}
	return nil
}

func renderWaveformImage(ctx context.Context, inputPath, outputPath string, opts *AudioWaveformOptions) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg not found in PATH")
	}
	colorList := waveformColorList(opts)
	splitChannels := "0"
	if opts.SplitChannels {
		splitChannels = "1"
	}
	// showwavespic is the still-image equivalent. mode/n/rate don't apply
	// here — those are showwaves animation controls — so we omit them.
	filter := fmt.Sprintf(
		"showwavespic=s=%dx%d:split_channels=%s:colors=%s:scale=%s:draw=%s",
		opts.Width, opts.Height, splitChannels, colorList, opts.Scale, opts.Draw,
	)
	args := []string{"-y", "-i", inputPath, "-filter_complex", filter, "-frames:v", "1", outputPath}
	if _, stderr, err := runCommand(ctx, "ffmpeg", args...); err != nil {
		return fmt.Errorf("ffmpeg showwavespic failed: %w (%s)", err, tail(stderr, 1500))
	}
	return nil
}

func waveformColorList(opts *AudioWaveformOptions) string {
	if opts.SplitChannels {
		return hexToFFmpegColor(opts.ColorPrimary) + "|" + hexToFFmpegColor(opts.ColorSecondary)
	}
	return hexToFFmpegColor(opts.ColorPrimary)
}

// ----------------------------------------------------------------------- //
// EXTRACT AUDIO
// ----------------------------------------------------------------------- //

type ExtractAudioOptions struct {
	// Format: mp3 (default), wav, m4a, aac, flac, ogg.
	Format string `json:"format"`
}

func parseExtractAudioOptions(raw map[string]any) ExtractAudioOptions {
	out := ExtractAudioOptions{}
	if raw == nil {
		return out
	}
	if v, ok := raw["format"].(string); ok {
		out.Format = strings.ToLower(strings.TrimSpace(v))
	}
	return out
}

func (s *SpecializedToolsService) runExtractAudio(ctx context.Context, job *models.ConversionJob, inputPath, outputPath string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg not found in PATH")
	}
	// Confirm the video actually has an audio stream — otherwise FFmpeg
	// silently produces an empty container which surprises users.
	if !ffprobeHasStream(ctx, inputPath, "a") {
		return errors.New("this video has no audio stream — there is nothing to extract")
	}
	opts := parseExtractAudioOptions(job.Options)
	ext := strings.TrimPrefix(filepath.Ext(outputPath), ".")
	if opts.Format == "" {
		opts.Format = ext
	}
	if opts.Format == "" {
		opts.Format = "mp3"
	}
	s.progress(job.ID, 20)
	args := []string{"-y", "-i", inputPath, "-map", "0:a:0", "-vn"}
	switch opts.Format {
	case "mp3":
		args = append(args, "-c:a", "libmp3lame", "-q:a", "2")
	case "wav":
		args = append(args, "-c:a", "pcm_s16le")
	case "m4a", "aac":
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	case "flac":
		args = append(args, "-c:a", "flac")
	case "ogg":
		args = append(args, "-c:a", "libvorbis", "-q:a", "5")
	default:
		return fmt.Errorf("unsupported extract-audio format: %s", opts.Format)
	}
	args = append(args, outputPath)
	if _, stderr, err := runCommand(ctx, "ffmpeg", args...); err != nil {
		return fmt.Errorf("ffmpeg extract audio failed: %w (%s)", err, tail(stderr, 1500))
	}
	s.progress(job.ID, 100)
	return nil
}

// ----------------------------------------------------------------------- //
// EXTRACT VIDEO ONLY (REMOVE AUDIO)
// ----------------------------------------------------------------------- //

func (s *SpecializedToolsService) runExtractVideoOnly(ctx context.Context, job *models.ConversionJob, inputPath, outputPath string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg not found in PATH")
	}
	if !ffprobeHasStream(ctx, inputPath, "v") {
		return errors.New("this file has no video stream — there is nothing to extract")
	}
	s.progress(job.ID, 20)
	// Stream-copy video, drop audio. For containers that don't accept the
	// source video codec FFmpeg will error, so we fall back to a re-encode in
	// that case — measured on a 2-pass try to avoid wasting GPU on the happy
	// path.
	args := []string{"-y", "-i", inputPath, "-map", "0:v:0", "-an", "-c:v", "copy", outputPath}
	if _, stderr, err := runCommand(ctx, "ffmpeg", args...); err != nil {
		// Re-encode fallback. h264 + yuv420p is the broadest baseline.
		os.Remove(outputPath)
		args = []string{"-y", "-i", inputPath, "-map", "0:v:0", "-an", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "medium", "-crf", "20", outputPath}
		if _, stderr2, err2 := runCommand(ctx, "ffmpeg", args...); err2 != nil {
			return fmt.Errorf("ffmpeg remove audio failed: %w (copy: %s | reencode: %s)", err2, tail(stderr, 1000), tail(stderr2, 1000))
		}
	}
	s.progress(job.ID, 100)
	return nil
}

// ----------------------------------------------------------------------- //
// EXTRACT FRAMES
// ----------------------------------------------------------------------- //

type ExtractFramesOptions struct {
	// Mode: every_n_seconds (default), fps, timestamp.
	Mode string `json:"mode"`
	// IntervalSeconds: used when Mode == every_n_seconds. Must be > 0.
	IntervalSeconds float64 `json:"intervalSeconds"`
	// FPS: used when Mode == fps. Must be > 0.
	FPS float64 `json:"fps"`
	// Timestamps: used when Mode == timestamp. List of seconds (>= 0).
	Timestamps []float64 `json:"timestamps"`
	// Format: jpg, png, webp.
	Format string `json:"format"`
	// MaxFrames: safety guardrail. We clamp this server-side.
	MaxFrames int `json:"maxFrames"`
}

const (
	extractFramesDefaultMax     = 300
	extractFramesAbsoluteMax    = 1000
	extractFramesMinIntervalSec = 0.1
	extractFramesMaxIntervalSec = 600.0
	extractFramesMinFPS         = 0.05
	extractFramesMaxFPS         = 60.0
)

func parseExtractFramesOptions(raw map[string]any) ExtractFramesOptions {
	out := ExtractFramesOptions{}
	if raw == nil {
		return out
	}
	// The outer dispatcher already consumed `mode` (set to "extract_frames").
	// We read the per-tool sub-mode from `frameMode` (preferred) and fall back
	// to legacy `mode` only when frameMode is missing AND mode is not the
	// dispatcher value, so we never confuse the two.
	if v, ok := raw["frameMode"].(string); ok && strings.TrimSpace(v) != "" {
		out.Mode = strings.ToLower(strings.TrimSpace(v))
	} else if v, ok := raw["mode"].(string); ok && !strings.EqualFold(strings.TrimSpace(v), "extract_frames") {
		out.Mode = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := raw["intervalSeconds"].(float64); ok {
		out.IntervalSeconds = v
	} else if v, ok := raw["intervalSeconds"].(int); ok {
		out.IntervalSeconds = float64(v)
	}
	if v, ok := raw["fps"].(float64); ok {
		out.FPS = v
	} else if v, ok := raw["fps"].(int); ok {
		out.FPS = float64(v)
	}
	if list, ok := raw["timestamps"].([]any); ok {
		for _, item := range list {
			switch t := item.(type) {
			case float64:
				out.Timestamps = append(out.Timestamps, t)
			case int:
				out.Timestamps = append(out.Timestamps, float64(t))
			case string:
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
					out.Timestamps = append(out.Timestamps, parsed)
				}
			}
		}
	}
	if v, ok := raw["format"].(string); ok {
		out.Format = strings.ToLower(strings.TrimSpace(v))
	}
	out.MaxFrames = intFromAny(raw["maxFrames"])
	return out
}

func (o *ExtractFramesOptions) applyDefaults() error {
	if o.Mode == "" {
		o.Mode = "every_n_seconds"
	}
	switch o.Mode {
	case "every_n_seconds", "fps", "timestamp":
	default:
		return fmt.Errorf("invalid mode: %q (expected every_n_seconds|fps|timestamp)", o.Mode)
	}
	if o.Format == "" {
		o.Format = "jpg"
	}
	switch o.Format {
	case "jpg", "jpeg", "png", "webp":
	default:
		return fmt.Errorf("invalid format: %q (expected jpg|png|webp)", o.Format)
	}
	if o.MaxFrames <= 0 || o.MaxFrames > extractFramesAbsoluteMax {
		o.MaxFrames = extractFramesDefaultMax
	}
	switch o.Mode {
	case "every_n_seconds":
		if o.IntervalSeconds <= 0 {
			o.IntervalSeconds = 5
		}
		if o.IntervalSeconds < extractFramesMinIntervalSec || o.IntervalSeconds > extractFramesMaxIntervalSec {
			return fmt.Errorf("invalid intervalSeconds: %v (allowed %v-%v)", o.IntervalSeconds, extractFramesMinIntervalSec, extractFramesMaxIntervalSec)
		}
	case "fps":
		if o.FPS <= 0 {
			o.FPS = 1
		}
		if o.FPS < extractFramesMinFPS || o.FPS > extractFramesMaxFPS {
			return fmt.Errorf("invalid fps: %v (allowed %v-%v)", o.FPS, extractFramesMinFPS, extractFramesMaxFPS)
		}
	case "timestamp":
		if len(o.Timestamps) == 0 {
			return errors.New("timestamps list cannot be empty for timestamp mode")
		}
		if len(o.Timestamps) > o.MaxFrames {
			return fmt.Errorf("too many timestamps: %d (max %d)", len(o.Timestamps), o.MaxFrames)
		}
		for _, ts := range o.Timestamps {
			if ts < 0 || math.IsNaN(ts) || math.IsInf(ts, 0) {
				return fmt.Errorf("invalid timestamp: %v", ts)
			}
		}
		sort.Float64s(o.Timestamps)
	}
	return nil
}

func (s *SpecializedToolsService) runExtractFrames(ctx context.Context, job *models.ConversionJob, inputPath, outputPath string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg not found in PATH")
	}
	opts := parseExtractFramesOptions(job.Options)
	if err := opts.applyDefaults(); err != nil {
		return err
	}
	if !ffprobeHasStream(ctx, inputPath, "v") {
		return errors.New("this file has no video stream — there is nothing to extract frames from")
	}
	s.progress(job.ID, 10)

	workDir, err := os.MkdirTemp(filepath.Dir(outputPath), "frames-")
	if err != nil {
		return fmt.Errorf("create frames workdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	ext := opts.Format
	if ext == "jpg" {
		ext = "jpg"
	}
	pattern := filepath.Join(workDir, "frame_%05d."+ext)

	switch opts.Mode {
	case "every_n_seconds":
		// FFmpeg's `fps=1/N` filter samples every N seconds. Pair it with
		// `-vframes` for a hard cap on output count.
		filter := fmt.Sprintf("fps=1/%.4f", opts.IntervalSeconds)
		args := []string{"-y", "-i", inputPath, "-vf", filter, "-vframes", strconv.Itoa(opts.MaxFrames), pattern}
		if _, stderr, err := runCommand(ctx, "ffmpeg", args...); err != nil {
			return fmt.Errorf("ffmpeg frame extraction failed: %w (%s)", err, tail(stderr, 1500))
		}
	case "fps":
		filter := fmt.Sprintf("fps=%.4f", opts.FPS)
		args := []string{"-y", "-i", inputPath, "-vf", filter, "-vframes", strconv.Itoa(opts.MaxFrames), pattern}
		if _, stderr, err := runCommand(ctx, "ffmpeg", args...); err != nil {
			return fmt.Errorf("ffmpeg frame extraction failed: %w (%s)", err, tail(stderr, 1500))
		}
	case "timestamp":
		// One FFmpeg invocation per timestamp keeps the command-line simple,
		// avoids the "concat" filter, and produces deterministic per-frame
		// errors when one timestamp is invalid.
		for i, ts := range opts.Timestamps {
			frame := filepath.Join(workDir, fmt.Sprintf("frame_%05d.%s", i+1, ext))
			args := []string{"-y", "-ss", strconv.FormatFloat(ts, 'f', 3, 64), "-i", inputPath, "-frames:v", "1", "-q:v", "2", frame}
			if _, stderr, err := runCommand(ctx, "ffmpeg", args...); err != nil {
				return fmt.Errorf("ffmpeg frame at %vs failed: %w (%s)", ts, err, tail(stderr, 1000))
			}
		}
	}

	s.progress(job.ID, 80)

	entries, err := os.ReadDir(workDir)
	if err != nil {
		return fmt.Errorf("read frames workdir: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("no frames were produced — the video may be too short for the chosen interval")
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, filepath.Join(workDir, e.Name()))
		}
	}
	sort.Strings(files)
	if err := zipFiles(outputPath, files); err != nil {
		return fmt.Errorf("package frames zip: %w", err)
	}
	s.progress(job.ID, 100)
	return nil
}

// ----------------------------------------------------------------------- //
// Shared helpers
// ----------------------------------------------------------------------- //

// ffprobeHasStream reports whether the file has at least one stream of the
// given type ("a" for audio, "v" for video). We re-implement this rather
// than reusing MediaInspector.HasAudioStream so callers don't need the
// inspector dependency just to check.
func ffprobeHasStream(ctx context.Context, path, streamType string) bool {
	stdout, _, err := runCommand(ctx, "ffprobe", "-v", "error", "-select_streams", streamType, "-show_entries", "stream=index", "-of", "csv=p=0", path)
	return err == nil && strings.TrimSpace(stdout) != ""
}

// zipFiles writes the given file paths into a single .zip at outputPath,
// preserving each file's base name (no leading directories) to keep the
// archive predictable for end-users.
func zipFiles(outputPath string, paths []string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()
	for _, p := range paths {
		if err := addFileToZip(zw, p); err != nil {
			return err
		}
	}
	return nil
}

func addFileToZip(zw *zip.Writer, path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = filepath.Base(path)
	header.Method = zip.Deflate
	writer, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(writer, src)
	return err
}
