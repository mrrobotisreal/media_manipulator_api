package services

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// Content Studio centralizes its GPU/NVENC policy here so every ffmpeg
// invocation (proxy, filmstrip, export) pins to the same dedicated device and
// makes the same encoder choice. The host has NVIDIA GPUs; Content Studio work
// runs on cfg.ContentStudioGPUIndex (default 1, the 16GB RTX 5080) so it never
// contends with whisper/RIFE on GPU 0.

// studioFFmpegEnv returns the process environment with CUDA pinned to the
// Content Studio GPU. Mirrors the whisper/AI convention (CUDA_DEVICE_ORDER +
// a single visible device) so NVENC lands on the same physical card operators
// see in `nvidia-smi -L`. CUDA_VISIBLE_DEVICES masks the rest, so ffmpeg's
// `-gpu 0` / default maps to the pinned device.
func studioFFmpegEnv(gpuIndex int) []string {
	return append(os.Environ(),
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		fmt.Sprintf("CUDA_VISIBLE_DEVICES=%d", gpuIndex),
	)
}

var (
	studioNVENCOnce  sync.Once
	studioNVENCWorks bool
)

// studioH264Encoder returns "h264_nvenc" when an NVENC H.264 encode actually
// succeeds on the configured GPU, otherwise "libx264". The probe runs a real
// (tiny) encode rather than just listing `-encoders`, because h264_nvenc can be
// compiled in yet fail at runtime when the driver/device is unavailable. The
// result is cached for the process lifetime.
func studioH264Encoder(cfg *config.Config) string {
	studioNVENCOnce.Do(func() {
		studioNVENCWorks = probeNVENCH264(cfg)
	})
	if studioNVENCWorks {
		return "h264_nvenc"
	}
	return "libx264"
}

func probeNVENCH264(cfg *config.Config) bool {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=64x64:d=0.1",
		"-c:v", "h264_nvenc", "-f", "null", "-")
	cmd.Env = studioFFmpegEnv(cfg.ContentStudioGPUIndex)
	return cmd.Run() == nil
}

// h264EncodeArgs returns the codec + tuning args for the given encoder and
// quality preset (low|medium|high). NVENC uses constant-quality VBR (-cq);
// libx264 uses -crf. Both pin yuv420p for broad playback compatibility.
func h264EncodeArgs(encoder, quality string) []string {
	switch encoder {
	case "h264_nvenc":
		preset, cq := "p4", "23"
		switch strings.ToLower(strings.TrimSpace(quality)) {
		case "high":
			preset, cq = "p5", "19"
		case "low":
			preset, cq = "p4", "28"
		}
		return []string{
			"-c:v", "h264_nvenc",
			"-preset", preset,
			"-rc", "vbr",
			"-cq", cq,
			"-b:v", "0",
			"-pix_fmt", "yuv420p",
		}
	default: // libx264
		preset, crf := "medium", "23"
		switch strings.ToLower(strings.TrimSpace(quality)) {
		case "high":
			preset, crf = "slow", "18"
		case "low":
			preset, crf = "veryfast", "28"
		}
		return []string{
			"-c:v", "libx264",
			"-preset", preset,
			"-crf", crf,
			"-pix_fmt", "yuv420p",
		}
	}
}

var (
	studioDurationRegex = regexp.MustCompile(`Duration: (\d{2}):(\d{2}):(\d{2})\.(\d{2})`)
	studioTimeRegex     = regexp.MustCompile(`time=(\d{2}):(\d{2}):(\d{2})\.(\d{2})`)
)

// runStudioFFmpeg runs ffmpeg pinned to the Content Studio GPU, parsing stderr
// for progress and forwarding it to the JobManager (same machinery the
// converter uses). When knownTotalSeconds > 0 it is used as the progress
// denominator — correct for trimmed/segment encodes where ffmpeg's reported
// input `Duration:` would overshoot the real output length. Otherwise the
// runner falls back to the input's `Duration:` line.
func runStudioFFmpeg(ctx context.Context, jm *JobManager, jobID string, gpuIndex int, knownTotalSeconds float64, args ...string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg is required for Content Studio but was not found on PATH — install FFmpeg or see https://ffmpeg.org/download.html")
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Env = studioFFmpegEnv(gpuIndex)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	totalDuration := knownTotalSeconds
	for scanner.Scan() {
		line := scanner.Text()
		stderrBuf.WriteString(line + "\n")

		if knownTotalSeconds <= 0 {
			if m := studioDurationRegex.FindStringSubmatch(line); m != nil {
				totalDuration = hmsToSeconds(m[1], m[2], m[3], m[4])
			}
		}
		if m := studioTimeRegex.FindStringSubmatch(line); m != nil && totalDuration > 0 && jm != nil {
			current := hmsToSeconds(m[1], m[2], m[3], m[4])
			progress := int((current / totalDuration) * 100)
			if progress > 100 {
				progress = 100
			}
			if progress < 0 {
				progress = 0
			}
			jm.SendProgressUpdate(jobID, progress)
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg timed out: %w", ctx.Err())
		}
		if out := commandTail(stderrBuf.String(), 8000); out != "" {
			return fmt.Errorf("%w. ffmpeg stderr: %s", err, out)
		}
		return err
	}
	return nil
}

func hmsToSeconds(h, m, s, cs string) float64 {
	hours, _ := strconv.ParseFloat(h, 64)
	minutes, _ := strconv.ParseFloat(m, 64)
	seconds, _ := strconv.ParseFloat(s, 64)
	centi, _ := strconv.ParseFloat(cs, 64)
	return hours*3600 + minutes*60 + seconds + centi/100
}
