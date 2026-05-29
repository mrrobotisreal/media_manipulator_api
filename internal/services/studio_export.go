package services

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// StudioExportService renders an EDL to a single MP4 via NVENC on the Content
// Studio GPU. Phase 3 builds the full multi-track graph: video clips are
// composited (bottom track → top, honoring opacity + timeline position over a
// black base) and audio is mixed (per-clip volume, delayed to its timeline
// position, muted tracks dropped). The arg slice is built by hand (no Go ffmpeg
// binding), mirroring the other services.
type StudioExportService struct {
	cfg        *config.Config
	jobManager *JobManager
}

func NewStudioExportService(cfg *config.Config, jm *JobManager) *StudioExportService {
	return &StudioExportService{cfg: cfg, jobManager: jm}
}

// StudioExportVideoSeg is one clip placed on a video track. InputIndex points at
// the ffmpeg input (one -i per clip, so the same file repeats for shared
// assets). Overlay order is bottom track first (TrackIndex asc, then start).
type StudioExportVideoSeg struct {
	InputIndex    int
	SourceIn      float64
	SourceOut     float64
	TimelineStart float64
	Opacity       float64 // 0..1
	TrackIndex    int
	// Phase 5 effects.
	FadeIn       float64 // cross-dissolve in: alpha ramps 0→1 over this many seconds
	Adjustments  *models.StudioAdjustments
	TextOverlays []models.StudioTextOverlay
}

// StudioExportAudioSeg is one audio-contributing clip (a video clip's embedded
// audio or an audio-track clip). Muted tracks contribute none.
type StudioExportAudioSeg struct {
	InputIndex    int
	SourceIn      float64
	SourceOut     float64
	TimelineStart float64
	Volume        float64 // 0..1
	// Phase 5: audio crossfade. FadeIn pairs with this clip's own transition;
	// FadeOut pairs with the next clip's transition into it.
	FadeIn  float64
	FadeOut float64
}

// StudioExportPlan is the resolved, render-ready EDL: the deduped-but-repeated
// input list plus the video + audio segments and the output frame parameters.
type StudioExportPlan struct {
	Inputs   []string
	Video    []StudioExportVideoSeg
	Audio    []StudioExportAudioSeg
	Width    int
	Height   int
	FPS      float64
	Duration float64
}

// RunExport renders the plan to outputPath, reporting progress on jobID.
func (s *StudioExportService) RunExport(ctx context.Context, jobID string, plan StudioExportPlan, quality, outputPath string) error {
	encoder := studioH264Encoder(s.cfg)
	args := buildMultiTrackExportArgs(plan, encoder, quality, s.cfg.ContentStudioFontFile, outputPath)
	return runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, plan.Duration, args...)
}

// buildMultiTrackExportArgs constructs the ffmpeg arg slice for the full EDL.
//
// Video: a black base of the project size/duration, then each video clip is
// trimmed, PTS-shifted to its timeline position, scaled to fit (letterboxed via
// centered overlay so lower tracks show through), optionally alpha-faded, and
// overlaid bottom-track-first. Result label: vout.
//
// Audio: each audio source is trimmed, resampled to 48k stereo, volume-scaled,
// and delayed to its timeline position, then amix'd with normalize=0 (per-clip
// volume is authoritative). Result label: aout.
//
// Pure + deterministic so the graph is unit-testable without invoking ffmpeg.
func buildMultiTrackExportArgs(plan StudioExportPlan, encoder, quality, fontFile, outputPath string) []string {
	width, height := plan.Width, plan.Height
	if width <= 0 {
		width = 1920
	}
	if height <= 0 {
		height = 1080
	}
	fpsStr := formatFrameRate(plan.FPS)
	if fpsStr == "" {
		fpsStr = "30"
	}
	durStr := formatSeconds(plan.Duration)

	args := []string{"-y"}
	for _, in := range plan.Inputs {
		args = append(args, "-i", in)
	}

	stmts := make([]string, 0, len(plan.Video)*2+len(plan.Audio)+2)

	// --- video composite ---
	videoOut := ""
	if len(plan.Video) > 0 {
		segs := sortVideoSegs(plan.Video)
		stmts = append(stmts, fmt.Sprintf("color=c=black:s=%dx%d:r=%s:d=%s,format=yuv420p[vbase]", width, height, fpsStr, durStr))
		last := "vbase"
		for k, vc := range segs {
			clipL := fmt.Sprintf("vc%d", k)
			tsStr := formatSeconds(vc.TimelineStart)
			teStr := formatSeconds(vc.TimelineStart + clampDurExport(vc.SourceIn, vc.SourceOut))

			// Build the clip's filter chain piece by piece: trim → reset PTS to
			// its timeline position → fit to frame → fps → [color eq] → alpha →
			// [opacity] → [text overlays] → [dissolve-in].
			parts := []string{
				fmt.Sprintf("[%d:v]trim=start=%s:end=%s", vc.InputIndex, formatSeconds(vc.SourceIn), formatSeconds(vc.SourceOut)),
				fmt.Sprintf("setpts=PTS-STARTPTS+%s/TB", tsStr),
				fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", width, height),
				"setsar=1",
				fmt.Sprintf("fps=%s", fpsStr),
			}
			if vc.Adjustments != nil {
				parts = append(parts, eqArg(vc.Adjustments))
			}
			parts = append(parts, "format=yuva420p")
			if vc.Opacity > 0 && vc.Opacity < 1 {
				parts = append(parts, fmt.Sprintf("colorchannelmixer=aa=%s", formatGain(vc.Opacity)))
			}
			for _, ov := range vc.TextOverlays {
				if strings.TrimSpace(ov.Text) == "" {
					continue
				}
				parts = append(parts, drawtextArg(ov, fontFile))
			}
			if vc.FadeIn > 0 {
				parts = append(parts, fmt.Sprintf("fade=t=in:st=%s:d=%s:alpha=1", tsStr, formatSeconds(vc.FadeIn)))
			}
			stmts = append(stmts, strings.Join(parts, ",")+fmt.Sprintf("[%s]", clipL))

			outL := "vout"
			if k < len(segs)-1 {
				outL = fmt.Sprintf("vov%d", k)
			}
			stmts = append(stmts, fmt.Sprintf(
				"[%s][%s]overlay=x=(W-w)/2:y=(H-h)/2:enable='between(t,%s,%s)':eof_action=pass[%s]",
				last, clipL, tsStr, teStr, outL,
			))
			last = outL
		}
		videoOut = last
	}

	// --- audio mix ---
	audioOut := ""
	if len(plan.Audio) > 0 {
		var mixIn strings.Builder
		for k, ac := range plan.Audio {
			label := "aout"
			if len(plan.Audio) > 1 {
				label = fmt.Sprintf("ac%d", k)
			}
			delayMs := int(math.Round(ac.TimelineStart * 1000))
			if delayMs < 0 {
				delayMs = 0
			}
			dur := clampDurExport(ac.SourceIn, ac.SourceOut)
			parts := []string{
				fmt.Sprintf("[%d:a]atrim=start=%s:end=%s", ac.InputIndex, formatSeconds(ac.SourceIn), formatSeconds(ac.SourceOut)),
				"asetpts=PTS-STARTPTS",
				"aformat=sample_rates=48000:channel_layouts=stereo",
				fmt.Sprintf("volume=%s", formatGain(ac.Volume)),
			}
			// Crossfade: fade this clip in (its own transition) and out (the next
			// clip's transition into it) so overlapping audio sums cleanly.
			if ac.FadeIn > 0 {
				parts = append(parts, fmt.Sprintf("afade=t=in:st=0:d=%s", formatSeconds(ac.FadeIn)))
			}
			if ac.FadeOut > 0 && dur > ac.FadeOut {
				parts = append(parts, fmt.Sprintf("afade=t=out:st=%s:d=%s", formatSeconds(dur-ac.FadeOut), formatSeconds(ac.FadeOut)))
			}
			parts = append(parts, fmt.Sprintf("adelay=%d|%d", delayMs, delayMs))
			stmts = append(stmts, strings.Join(parts, ",")+fmt.Sprintf("[%s]", label))
			mixIn.WriteString("[" + label + "]")
		}
		if len(plan.Audio) > 1 {
			stmts = append(stmts, fmt.Sprintf("%samix=inputs=%d:normalize=0:dropout_transition=0[aout]", mixIn.String(), len(plan.Audio)))
		}
		audioOut = "aout"
	}

	args = append(args, "-filter_complex", strings.Join(stmts, ";"))
	if videoOut != "" {
		args = append(args, "-map", "["+videoOut+"]")
	}
	if audioOut != "" {
		args = append(args, "-map", "["+audioOut+"]")
	}
	if videoOut != "" {
		args = append(args, h264EncodeArgs(encoder, quality)...)
	}
	if audioOut != "" {
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	}
	// Bound the output to the timeline length (audio adelay can otherwise run
	// past the base video).
	args = append(args, "-t", durStr, "-movflags", "+faststart", outputPath)
	return args
}

// sortVideoSegs orders clips for overlay: lower track first (drawn underneath),
// then by timeline position. Returns a copy.
func sortVideoSegs(in []StudioExportVideoSeg) []StudioExportVideoSeg {
	out := append([]StudioExportVideoSeg(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.TrackIndex < b.TrackIndex || (a.TrackIndex == b.TrackIndex && a.TimelineStart <= b.TimelineStart) {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func clampDurExport(in, out float64) float64 {
	d := out - in
	if d < 0 {
		return 0
	}
	return d
}

// eqArg maps per-clip adjustments onto ffmpeg's eq filter.
func eqArg(a *models.StudioAdjustments) string {
	return fmt.Sprintf("eq=brightness=%s:contrast=%s:saturation=%s",
		formatGain(a.Brightness), formatGain(a.Contrast), formatGain(a.Saturation))
}

// drawtextArg renders a text overlay onto the clip. X/Y are normalized: the
// (w-text_w)*X form keeps the text fully inside the frame. A translucent box
// keeps labels legible over busy footage (e.g. drone shots).
func drawtextArg(ov models.StudioTextOverlay, fontFile string) string {
	size := int(math.Round(ov.FontSize))
	if size <= 0 {
		size = 32
	}
	return fmt.Sprintf(
		"drawtext=fontfile='%s':text='%s':x=(w-text_w)*%s:y=(h-text_h)*%s:fontsize=%d:fontcolor=%s:box=1:boxcolor=black@0.4:boxborderw=8",
		drawtextEscape(fontFile), drawtextEscape(ov.Text), formatGain(clamp01(ov.X)), formatGain(clamp01(ov.Y)), size, hexToFFColor(ov.Color),
	)
}

// drawtextEscape escapes a value for use inside a single-quoted drawtext field.
func drawtextEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// hexToFFColor converts "#RRGGBB" to ffmpeg's "0xRRGGBB" form, defaulting to
// white for anything unparseable.
func hexToFFColor(hex string) string {
	h := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(h) != 6 {
		return "white"
	}
	for _, r := range h {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return "white"
		}
	}
	return "0x" + strings.ToUpper(h)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func formatSeconds(s float64) string {
	return strconv.FormatFloat(s, 'f', 3, 64)
}

func formatGain(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}
