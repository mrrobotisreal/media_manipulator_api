package services

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// MM120 (design DARKROOM_OUTPUTS_AND_CAPTIONS.md §5): the Pro-only video
// encoders, AV1 and ProRes 422, and the predictable-size estimate behind the
// ProRes output guard. The rows live in studio_outputs.go like every other.

// --- AV1 ---------------------------------------------------------------------

// StudioEncoderSVTAV1 is the ffmpeg id of the CPU AV1 fallback. It is
// CPU-bound, so it takes a slot of the CPU encode gate (MM119) like VP9.
const StudioEncoderSVTAV1 = "libsvtav1"

// StudioEncoderAV1NVENC is the RTX 5080's AV1 encoder.
const StudioEncoderAV1NVENC = "av1_nvenc"

// ErrStudioAV1Unavailable is the job error for an AV1 export on a host with
// neither a working av1_nvenc nor libsvtav1. Loud on purpose, like HEVC: a
// file named AV1 that holds another codec would be worse than a failure. It
// says "unsupported" (failure class unsupported_input) and avoids
// "nvenc"/"gpu", which would misfile it.
//
// RUNBOOK: if AV1 is wanted on a host where this fires, check
// `ffmpeg -encoders | grep -E "av1_nvenc|libsvtav1"` and the NVENC probe in
// probeNVENCAV1 (a real 256x256 encode on ContentStudioGPUIndex; AV1 NVENC
// needs an Ada or newer GPU).
var ErrStudioAV1Unavailable = fmt.Errorf("AV1 export is unsupported on this server: no usable AV1 encoder is installed")

var (
	studioAV1Once  sync.Once
	studioAV1NVENC bool
	studioAV1SVT   bool
)

// pickAV1Encoder is the pure AV1 encoder choice: NVENC when its probe
// succeeded, else libsvtav1 when the build has it, else ErrStudioAV1Unavailable.
func pickAV1Encoder(nvencOK, svtOK bool) (string, error) {
	switch {
	case nvencOK:
		return StudioEncoderAV1NVENC, nil
	case svtOK:
		return StudioEncoderSVTAV1, nil
	}
	return "", ErrStudioAV1Unavailable
}

// studioAV1Encoder resolves (and caches for the process lifetime) the AV1
// encoder for this host.
func studioAV1Encoder(cfg *config.Config) (string, error) {
	studioAV1Once.Do(func() {
		studioAV1NVENC = probeNVENCAV1(cfg)
		studioAV1SVT = probeEncoderListed(StudioEncoderSVTAV1)
	})
	return pickAV1Encoder(studioAV1NVENC, studioAV1SVT)
}

// probeNVENCAV1 runs a real 256x256 av1_nvenc encode on the studio GPU (a
// listed encoder can still fail at runtime: pre-Ada GPU, driver build).
func probeNVENCAV1(cfg *config.Config) bool {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=256x256:d=0.1",
		"-c:v", StudioEncoderAV1NVENC, "-f", "null", "-")
	cmd.Env = studioFFmpegEnv(cfg.ContentStudioGPUIndex)
	return cmd.Run() == nil
}

// probeEncoderListed reports whether this ffmpeg build lists an encoder.
func probeEncoderListed(name string) bool {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	return err == nil && encodersListHas(string(out), name)
}

// encodersListHas scans `ffmpeg -encoders` output for an encoder row.
func encodersListHas(listing, name string) bool {
	for _, line := range strings.Split(listing, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == name {
			return true
		}
	}
	return false
}

// av1EncodeArgs is AV1 in constant-quality mode. NVENC: preset p5, -cq
// low/medium/high = 38/32/28 with -b:v 0 (as decided; -rc vbr is NVENC's
// default with -cq, the server run produced byte-identical files with and
// without it), plus a -maxrate 120M ceiling: without one av1_nvenc's VBR
// applies its own default cap and the server run's 2160p cq 28 and cq 32
// files came out the same ~31 Mb/s (with it: 58 and 35 Mb/s, design §9).
// libsvtav1: -preset 10/8/7 with -crf 38/32/28 (the server run moved high
// from the proposed preset 6, which encoded 2160p at 3 fps; preset 7 runs at
// 8 fps, VP9-high speed, for a near-identical size). Both 8-bit yuv420p like
// every other encoder (the graph is 8-bit).
func av1EncodeArgs(encoder, quality string) []string {
	q := strings.ToLower(strings.TrimSpace(quality))
	if encoder == StudioEncoderSVTAV1 {
		preset, crf := "8", "32"
		switch q {
		case "high":
			preset, crf = "7", "28"
		case "low":
			preset, crf = "10", "38"
		}
		return []string{
			"-c:v", StudioEncoderSVTAV1,
			"-preset", preset,
			"-crf", crf,
			"-pix_fmt", "yuv420p",
		}
	}
	cq := "32"
	switch q {
	case "high":
		cq = "28"
	case "low":
		cq = "38"
	}
	return []string{
		"-c:v", StudioEncoderAV1NVENC,
		"-preset", "p5",
		"-cq", cq,
		"-b:v", "0",
		"-maxrate", "120M",
		"-bufsize", "240M",
		"-pix_fmt", "yuv420p",
	}
}

// --- ProRes 422 --------------------------------------------------------------

// StudioEncoderProRes is the ffmpeg id of the ProRes encoder.
const StudioEncoderProRes = "prores_ks"

// proResEncodeArgs is ProRes 422 (profile 2, Apple's "apcn" fourcc) with the
// Apple vendor id so Final Cut and QuickTime take it as their own. 10-bit
// 4:2:2 output from the 8-bit graph: the dialog says "from an 8-bit
// timeline". Quality presets do not apply (ProRes is a fixed-rate intra codec).
func proResEncodeArgs() []string {
	return []string{
		"-c:v", StudioEncoderProRes,
		"-profile:v", "2",
		"-vendor", "apl0",
		"-pix_fmt", "yuv422p10le",
	}
}

// --- predictable output size (the ProRes guard) -------------------------------

// studioEstimateRefPixelRate is the reference the video EstimateKbps is for:
// 1920x1080 at 30 fps.
const studioEstimateRefPixelRate = 1920.0 * 1080.0 * 30.0

// StudioOutputEstimateBytes is the predicted size of an output of `seconds`
// at width x height and fps, from the table's EstimateKbps: a video codec's
// rate scales with pixels x fps; an audio codec's is for stereo (mono halves
// it), else its chosen bitrate. ok=false when any part is not predictable
// (a constant-quality video codec, FLAC): no estimate, no guard.
func StudioOutputEstimateBytes(o StudioOutput, width, height int, fps, seconds float64) (int64, bool) {
	if seconds <= 0 {
		return 0, false
	}
	kbps := 0.0
	if o.VideoCodec != StudioVideoNone && o.VideoCodec != "" {
		vc, ok := studioCodec(o.VideoCodec)
		if !ok || vc.EstimateKbps <= 0 {
			return 0, false
		}
		if width <= 0 || height <= 0 {
			width, height = 1920, 1080
		}
		if fps <= 0 {
			fps = 30
		}
		kbps += float64(vc.EstimateKbps) * float64(width) * float64(height) * fps / studioEstimateRefPixelRate
	}
	if o.AudioCodec != StudioAudioNone && o.AudioCodec != "" {
		ac, ok := studioCodec(o.AudioCodec)
		switch {
		case !ok:
			return 0, false
		case ac.EstimateKbps > 0:
			a := float64(ac.EstimateKbps)
			if o.AudioChannels == 1 {
				a /= 2
			}
			kbps += a
		case o.AudioBitrate > 0:
			kbps += float64(o.AudioBitrate)
		default:
			return 0, false
		}
	}
	return int64(kbps * 1000 / 8 * seconds), true
}

// StudioOutputTooLarge is the ProRes size guard's decision: the estimate is
// above the per-file cap (maxBytes > 0) or, with the headroom the render
// keeps free, above the free disk (freeBytes >= 0; -1 = unknown). Unknown
// estimates are never refused.
func StudioOutputTooLarge(estimate int64, ok bool, maxBytes, freeBytes int64) (refuse bool, reason string) {
	if !ok || estimate <= 0 {
		return false, ""
	}
	if maxBytes > 0 && estimate > maxBytes {
		return true, "file_cap"
	}
	if freeBytes >= 0 && estimate+StudioExportDiskHeadroom > freeBytes {
		return true, "disk"
	}
	return false, ""
}

// StudioExportDiskHeadroom is what a predictable-size export must leave free
// on the output disk beyond its own estimate (2 GiB).
const StudioExportDiskHeadroom int64 = 2 << 30
