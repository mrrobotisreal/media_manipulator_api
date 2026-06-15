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

// EDL v2 effect parameter ranges/names mirror lib/studio/effectRegistry.ts (the
// single TS source of truth) and the Zod schema in lib/studioTypes.ts. The Go
// structs live in internal/models/studio.go and are clamped by
// models.SanitizeTracks before they reach this compiler, so every value here is
// already in range.
//
// SHARED TRANSFORM SPEC (mirrors lib/studio/glCompositor.ts verbatim):
//   crop → fit (force_original_aspect_ratio=decrease into W*scale × H*scale) →
//   center at (W/2 + x*W, H/2 + y*H) → rotate clockwise by rotationDeg.
//   Fragment order: eq(legacy) → lumetri → lut3d → format=yuva → chromakey →
//   opacity → drawtext → fade → rotate, then overlay/blend.
//
// StudioExportService renders an EDL to a single MP4 via NVENC on the Content
// Studio GPU. The arg slice is built by hand (no Go ffmpeg binding), mirroring
// the other services.
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
	// Cross-dissolve in: alpha ramps 0→1 over this many seconds.
	FadeIn       float64
	Adjustments  *models.StudioAdjustments
	TextOverlays []models.StudioTextOverlay

	// EDL v2.
	Transform *models.StudioTransform
	Crop      *models.StudioCrop
	BlendMode string
	Effects   []models.StudioEffect
	// LutPaths maps a lut effect's lutAssetId → local .cube path (downloaded by
	// the export job). A missing entry means the LUT is skipped.
	LutPaths map[string]string
}

// StudioExportAudioSeg is one audio-contributing clip (a video clip's embedded
// audio or an audio-track clip). Muted tracks contribute none.
type StudioExportAudioSeg struct {
	InputIndex    int
	SourceIn      float64
	SourceOut     float64
	TimelineStart float64
	Volume        float64 // 0..2 (flat); overridden by VolumeKeyframes when present
	// Audio crossfade. FadeIn pairs with this clip's transition; FadeOut pairs
	// with the next clip's transition into it.
	FadeIn  float64
	FadeOut float64

	// EDL v2.
	VolumeKeyframes []models.StudioVolumeKeyframe
	Pan             float64
	// Voice marks a seg on the ducking voice track (the sidechain key source).
	Voice bool
}

// StudioDucking is the resolved auto-ducking config for an export.
type StudioDucking struct {
	AmountDb  float64
	AttackMs  float64
	ReleaseMs float64
}

// StudioExportPlan is the resolved, render-ready EDL.
type StudioExportPlan struct {
	Inputs   []string
	Video    []StudioExportVideoSeg
	Audio    []StudioExportAudioSeg
	Width    int
	Height   int
	FPS      float64
	Duration float64
	// EDL v2.
	Ducking  *StudioDucking
	Loudness string // '' | streaming | podcast | broadcast
	// CaptionsASSPath, when set, is burned in as the final video filter (Phase 7).
	CaptionsASSPath string
}

// RunExport renders the plan to outputPath, reporting progress on jobID.
func (s *StudioExportService) RunExport(ctx context.Context, jobID string, plan StudioExportPlan, quality, outputPath string) error {
	encoder := studioH264Encoder(s.cfg)
	args := buildMultiTrackExportArgs(plan, encoder, quality, s.cfg.ContentStudioFontFile, outputPath)
	return runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, plan.Duration, args...)
}

// RunAudioMix renders the plan's audio to a timeline-aligned PCM WAV (no video),
// used to feed whisper for caption generation. Returns an error if the plan has
// no audio.
func (s *StudioExportService) RunAudioMix(ctx context.Context, jobID string, plan StudioExportPlan, outputPath string) error {
	args, ok := buildAudioMixArgs(plan, outputPath)
	if !ok {
		return fmt.Errorf("project has no audio to transcribe")
	}
	return runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, plan.Duration, args...)
}

// buildAudioMixArgs reuses the audio section to produce a timeline-aligned PCM
// WAV (-vn -c:a pcm_s16le). Ducking/loudness are intentionally ignored here so
// the transcript sees a clean, full-level mix. Returns ok=false when silent.
func buildAudioMixArgs(plan StudioExportPlan, outputPath string) (args []string, ok bool) {
	durStr := formatSeconds(plan.Duration)
	// Strip ducking/loudness for a flat transcription mix.
	mixPlan := plan
	mixPlan.Ducking = nil
	mixPlan.Loudness = ""
	stmts, audioOut := buildAudioSection(mixPlan, durStr)
	if audioOut == "" {
		return nil, false
	}
	args = []string{"-y"}
	for _, in := range plan.Inputs {
		args = append(args, "-i", in)
	}
	args = append(args, "-filter_complex", strings.Join(stmts, ";"))
	args = append(args, "-map", "["+audioOut+"]", "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-t", durStr, outputPath)
	return args, true
}

// buildMultiTrackExportArgs constructs the ffmpeg arg slice for the full EDL.
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

	stmts := make([]string, 0, len(plan.Video)*2+len(plan.Audio)+4)

	// --- video composite ---
	videoOut := ""
	if len(plan.Video) > 0 {
		segs := sortVideoSegs(plan.Video)
		stmts = append(stmts, fmt.Sprintf("color=c=black:s=%dx%d:r=%s:d=%s,format=yuv420p[vbase]", width, height, fpsStr, durStr))
		last := "vbase"
		for k, vc := range segs {
			clipStmts, clipLabel := buildVideoClipChain(vc, k, width, height, fpsStr, fontFile)
			stmts = append(stmts, clipStmts...)

			outL := "vout"
			if k < len(segs)-1 || plan.CaptionsASSPath != "" {
				outL = fmt.Sprintf("vov%d", k)
			}
			stmts = append(stmts, compositeStep(last, clipLabel, vc, k, width, height, durStr, fpsStr, outL)...)
			last = outL
		}
		// Caption burn-in is the final video filter (Phase 7).
		if plan.CaptionsASSPath != "" {
			stmts = append(stmts, fmt.Sprintf("[%s]subtitles=filename='%s'[vout]", last, filterPathEscape(plan.CaptionsASSPath)))
			last = "vout"
		}
		videoOut = last
	}

	// --- audio mix ---
	audioStmts, audioOut := buildAudioSection(plan, durStr)
	stmts = append(stmts, audioStmts...)

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
	args = append(args, "-t", durStr, "-movflags", "+faststart", outputPath)
	return args
}

// buildVideoClipChain builds the filter statements for one video clip and
// returns them plus the final label (vc<k>). The common case is a single
// comma-joined statement; a LUT with intensity < 1 expands into a
// split/lut3d/blend sub-graph.
func buildVideoClipChain(vc StudioExportVideoSeg, k, width, height int, fpsStr, fontFile string) ([]string, string) {
	tsStr := formatSeconds(vc.TimelineStart)
	scale := 1.0
	if vc.Transform != nil && vc.Transform.Scale > 0 {
		scale = vc.Transform.Scale
	}
	boxW := int(math.Round(float64(width) * scale))
	boxH := int(math.Round(float64(height) * scale))

	chain := []string{
		fmt.Sprintf("[%d:v]trim=start=%s:end=%s", vc.InputIndex, formatSeconds(vc.SourceIn), formatSeconds(vc.SourceOut)),
		fmt.Sprintf("setpts=PTS-STARTPTS+%s/TB", tsStr),
	}
	if c := cropArg(vc.Crop); c != "" {
		chain = append(chain, c)
	}
	chain = append(chain,
		fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", boxW, boxH),
		"setsar=1",
		fmt.Sprintf("fps=%s", fpsStr),
	)
	if vc.Adjustments != nil {
		chain = append(chain, eqArg(vc.Adjustments))
	}

	lumetri, lut, chroma := pickEffects(vc.Effects)
	if lumetri != nil {
		chain = append(chain, lumetriArgs(lumetri)...)
	}

	var stmts []string
	finalLabel := fmt.Sprintf("vc%d", k)
	chainHead := "" // input-label prefix for the final statement when restarted

	if lut != nil {
		lutPath := ""
		if lut.LutAssetID != nil {
			lutPath = vc.LutPaths[*lut.LutAssetID]
		}
		if lutPath != "" {
			intensity := optF(lut.Intensity, 1)
			if intensity >= 1 {
				chain = append(chain, lut3dPart(lutPath))
			} else {
				// split + blend to mix the graded result at `intensity`.
				pre := fmt.Sprintf("vc%da", k)
				stmts = append(stmts, strings.Join(chain, ",")+"["+pre+"]")
				stmts = append(stmts, fmt.Sprintf("[%s]split[%sx][%sy]", pre, pre, pre))
				stmts = append(stmts, fmt.Sprintf("[%sy]%s[%sl]", pre, lut3dPart(lutPath), pre))
				gLabel := fmt.Sprintf("vc%dg", k)
				stmts = append(stmts, fmt.Sprintf("[%sx][%sl]blend=all_mode=normal:all_opacity=%s[%s]", pre, pre, formatGain(intensity), gLabel))
				chain = nil
				chainHead = "[" + gLabel + "]"
			}
		}
	}

	chain = append(chain, "format=yuva420p")
	if chroma != nil {
		chain = append(chain, chromaArgs(chroma)...)
	}
	if vc.Opacity > 0 && vc.Opacity < 1 {
		chain = append(chain, fmt.Sprintf("colorchannelmixer=aa=%s", formatGain(vc.Opacity)))
	}
	for _, ov := range vc.TextOverlays {
		if strings.TrimSpace(ov.Text) == "" {
			continue
		}
		chain = append(chain, drawtextArg(ov, fontFile))
	}
	if vc.FadeIn > 0 {
		// Video alpha fade stays LINEAR (no qsin): a linear alpha ramp is the
		// correct visual cross-dissolve; qsin is for equal-power AUDIO crossfades.
		chain = append(chain, fmt.Sprintf("fade=t=in:st=%s:d=%s:alpha=1", tsStr, formatSeconds(vc.FadeIn)))
	}
	if r := rotateArg(vc); r != "" {
		chain = append(chain, r)
	}
	stmts = append(stmts, chainHead+strings.Join(chain, ",")+"["+finalLabel+"]")
	return stmts, finalLabel
}

// compositeStep overlays (normal) or blends (non-normal) the clip onto the
// running composite, honoring the transform position + the enable window.
func compositeStep(last, clipLabel string, vc StudioExportVideoSeg, k, width, height int, durStr, fpsStr, outL string) []string {
	tsStr := formatSeconds(vc.TimelineStart)
	teStr := formatSeconds(vc.TimelineStart + clampDurExport(vc.SourceIn, vc.SourceOut))
	xExpr, yExpr := overlayPos(vc, width, height)

	mode := vc.BlendMode
	if mode == "" || mode == models.StudioBlendNormal {
		return []string{fmt.Sprintf(
			"[%s][%s]overlay=x=%s:y=%s:enable='between(t,%s,%s)':eof_action=pass[%s]",
			last, clipLabel, xExpr, yExpr, tsStr, teStr, outL,
		)}
	}

	// Non-normal blend: position the clip onto a full-frame transparent layer,
	// then blend it against the running composite within the clip's window.
	// CAVEAT: ffmpeg `blend` ignores source alpha for some modes, so the
	// blend+dissolve interaction can differ slightly from the WebGL preview.
	bg := fmt.Sprintf("bg%d", k)
	pos := fmt.Sprintf("pos%d", k)
	return []string{
		fmt.Sprintf("color=c=black@0:s=%dx%d:r=%s:d=%s,format=yuva420p[%s]", width, height, fpsStr, durStr, bg),
		fmt.Sprintf("[%s][%s]overlay=x=%s:y=%s[%s]", bg, clipLabel, xExpr, yExpr, pos),
		fmt.Sprintf("[%s][%s]blend=all_mode=%s:enable='between(t,%s,%s)'[%s]", last, pos, mode, tsStr, teStr, outL),
	}
}

// buildAudioSection builds the audio mix statements + the output label. Returns
// ("", "") when there is no audio.
func buildAudioSection(plan StudioExportPlan, durStr string) ([]string, string) {
	if len(plan.Audio) == 0 {
		return nil, ""
	}
	var stmts []string

	// Final label: when a loudness preset is set, mix to a premaster then
	// loudnorm into [aout]; otherwise mix directly into [aout].
	finalLabel := "aout"
	if plan.Loudness != "" {
		finalLabel = "apm"
	}

	ducking := plan.Ducking
	voice, bed := splitVoiceBed(plan.Audio)
	useDuck := ducking != nil && len(voice) > 0 && len(bed) > 0

	if !useDuck {
		// Standard path (byte-identical to v1 when no v2 fields are present).
		if len(plan.Audio) == 1 {
			stmts = append(stmts, audioSegChain(plan.Audio[0], finalLabel))
		} else {
			var mixIn strings.Builder
			for k, ac := range plan.Audio {
				label := fmt.Sprintf("ac%d", k)
				stmts = append(stmts, audioSegChain(ac, label))
				mixIn.WriteString("[" + label + "]")
			}
			stmts = append(stmts, fmt.Sprintf("%samix=inputs=%d:normalize=0:dropout_transition=0[%s]", mixIn.String(), len(plan.Audio), finalLabel))
		}
	} else {
		// Ducking: bed group → sidechaincompress keyed by the voice group.
		bedLabel := mixGroup(&stmts, bed, "bed")
		voiceLabel := mixGroup(&stmts, voice, "voice")
		stmts = append(stmts, fmt.Sprintf("[%s]asplit[%smix][%ssc]", voiceLabel, voiceLabel, voiceLabel))
		stmts = append(stmts, fmt.Sprintf(
			"[%s][%ssc]sidechaincompress=threshold=0.02:ratio=%s:attack=%s:release=%s:makeup=1[duckedbed]",
			bedLabel, voiceLabel, formatGain(duckRatio(ducking.AmountDb)), formatGain(ducking.AttackMs), formatGain(ducking.ReleaseMs),
		))
		stmts = append(stmts, fmt.Sprintf("[duckedbed][%smix]amix=inputs=2:normalize=0:dropout_transition=0[%s]", voiceLabel, finalLabel))
	}

	if plan.Loudness != "" {
		stmts = append(stmts, fmt.Sprintf("[%s]%s[aout]", finalLabel, loudnormArg(plan.Loudness)))
	}
	return stmts, "aout"
}

// mixGroup writes one chain per seg in the group and amixes them into a single
// labeled output (or passes a single seg through). Returns the group label.
func mixGroup(stmts *[]string, group []StudioExportAudioSeg, name string) string {
	if len(group) == 1 {
		*stmts = append(*stmts, audioSegChain(group[0], name))
		return name
	}
	var mixIn strings.Builder
	for k, ac := range group {
		label := fmt.Sprintf("%s%d", name, k)
		*stmts = append(*stmts, audioSegChain(ac, label))
		mixIn.WriteString("[" + label + "]")
	}
	*stmts = append(*stmts, fmt.Sprintf("%samix=inputs=%d:normalize=0:dropout_transition=0[%s]", mixIn.String(), len(group), name))
	return name
}

// audioSegChain builds one audio clip's chain → [label].
func audioSegChain(ac StudioExportAudioSeg, label string) string {
	delayMs := int(math.Round(ac.TimelineStart * 1000))
	if delayMs < 0 {
		delayMs = 0
	}
	dur := clampDurExport(ac.SourceIn, ac.SourceOut)
	parts := []string{
		fmt.Sprintf("[%d:a]atrim=start=%s:end=%s", ac.InputIndex, formatSeconds(ac.SourceIn), formatSeconds(ac.SourceOut)),
		"asetpts=PTS-STARTPTS",
		"aformat=sample_rates=48000:channel_layouts=stereo",
	}
	if len(ac.VolumeKeyframes) > 0 {
		// t is post-asetpts clip-local time, matching the keyframe times.
		parts = append(parts, fmt.Sprintf("volume='%s':eval=frame", volumeExpr(ac.VolumeKeyframes)))
	} else {
		parts = append(parts, fmt.Sprintf("volume=%s", formatGain(ac.Volume)))
	}
	// Equal-power (qsin) crossfades so overlapping audio sums without a dip.
	if ac.FadeIn > 0 {
		parts = append(parts, fmt.Sprintf("afade=t=in:st=0:d=%s:curve=qsin", formatSeconds(ac.FadeIn)))
	}
	if ac.FadeOut > 0 && dur > ac.FadeOut {
		parts = append(parts, fmt.Sprintf("afade=t=out:st=%s:d=%s:curve=qsin", formatSeconds(dur-ac.FadeOut), formatSeconds(ac.FadeOut)))
	}
	if ac.Pan != 0 {
		parts = append(parts, fmt.Sprintf("stereotools=balance_in=%s", formatGain(ac.Pan)))
	}
	parts = append(parts, fmt.Sprintf("adelay=%d|%d", delayMs, delayMs))
	return strings.Join(parts, ",") + "[" + label + "]"
}

func splitVoiceBed(segs []StudioExportAudioSeg) (voice, bed []StudioExportAudioSeg) {
	for _, s := range segs {
		if s.Voice {
			voice = append(voice, s)
		} else {
			bed = append(bed, s)
		}
	}
	return voice, bed
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

// --- effect emitters --------------------------------------------------------

// pickEffects returns the first enabled effect of each type (preview honors one
// of each; documented divergence from a fully ordered stack).
func pickEffects(effects []models.StudioEffect) (lumetri, lut, chroma *models.StudioEffect) {
	for i := range effects {
		e := &effects[i]
		if !e.Enabled {
			continue
		}
		switch e.Type {
		case models.StudioEffectLumetri:
			if lumetri == nil {
				lumetri = e
			}
		case models.StudioEffectLUT:
			if lut == nil {
				lut = e
			}
		case models.StudioEffectChromaKey:
			if chroma == nil {
				chroma = e
			}
		}
	}
	return lumetri, lut, chroma
}

func optF(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

// cropArg emits crop only when an edge is cropped; clamps w/h ≥ 16px via max().
func cropArg(c *models.StudioCrop) string {
	if c == nil || (c.Left == 0 && c.Top == 0 && c.Right == 0 && c.Bottom == 0) {
		return ""
	}
	return fmt.Sprintf("crop=w=max(16\\,iw*%s):h=max(16\\,ih*%s):x=iw*%s:y=ih*%s",
		formatGain(1-c.Left-c.Right), formatGain(1-c.Top-c.Bottom), formatGain(c.Left), formatGain(c.Top))
}

// lumetriArgs maps a lumetri effect onto ffmpeg filters. Order/constants are
// documented to diverge slightly from the WebGL preview (colorbalance vs the
// shader's linear temp/tint shift); both warm/cool the image.
func lumetriArgs(e *models.StudioEffect) []string {
	var out []string
	if exp := optF(e.Exposure, 0); exp != 0 {
		g := math.Pow(2, exp) // matches the shader's c *= 2^exposure
		out = append(out, fmt.Sprintf("colorchannelmixer=rr=%s:gg=%s:bb=%s", formatGain(g), formatGain(g), formatGain(g)))
	}
	temp, tint := optF(e.Temperature, 0), optF(e.Tint, 0)
	if temp != 0 || tint != 0 {
		out = append(out, fmt.Sprintf("colorbalance=rm=%s:gm=%s:bm=%s", formatGain(temp/100), formatGain(-tint/100), formatGain(-temp/100)))
	}
	if vib := optF(e.Vibrance, 0); vib != 0 {
		out = append(out, fmt.Sprintf("vibrance=intensity=%s", formatGain(vib)))
	}
	con, sat := optF(e.Contrast, 1), optF(e.Saturation, 1)
	if con != 1 || sat != 1 {
		out = append(out, fmt.Sprintf("eq=contrast=%s:saturation=%s", formatGain(con), formatGain(sat)))
	}
	return out
}

func lut3dPart(localPath string) string {
	return fmt.Sprintf("lut3d=file='%s':interp=trilinear", filterPathEscape(localPath))
}

// chromaArgs emits chromakey + despill (despill type chosen by the dominant
// channel of the key color).
func chromaArgs(e *models.StudioEffect) []string {
	key := "#00FF00"
	if e.KeyColor != nil {
		key = *e.KeyColor
	}
	sim := optF(e.Similarity, 0.1)
	blend := optF(e.Blend, 0.1)
	despill := optF(e.Despill, 0.5)
	out := []string{fmt.Sprintf("chromakey=color=%s:similarity=%s:blend=%s", hexToFFColor(key), formatGain(sim), formatGain(blend))}
	if despill > 0 {
		out = append(out, fmt.Sprintf("despill=type=%s:mix=%s", despillType(key), formatGain(despill)))
	}
	return out
}

// despillType picks "blue" when the key color's blue channel dominates green,
// else "green" (the common case).
func despillType(hex string) string {
	_, g, b := parseHexRGB(hex)
	if b > g {
		return "blue"
	}
	return "green"
}

// rotateArg rotates the alpha-padded clip clockwise (matching the shader's
// clockwise convention); transparent corners via c=black@0.
func rotateArg(vc StudioExportVideoSeg) string {
	if vc.Transform == nil || vc.Transform.RotationDeg == 0 {
		return ""
	}
	rad := vc.Transform.RotationDeg * math.Pi / 180
	return fmt.Sprintf("rotate=a=%s:c=black@0:ow=rotw(%s):oh=roth(%s)", formatGain(rad), formatGain(rad), formatGain(rad))
}

// overlayPos returns the overlay x/y expressions, centered + transform offset.
func overlayPos(vc StudioExportVideoSeg, width, height int) (string, string) {
	x, y := "(W-w)/2", "(H-h)/2"
	if vc.Transform != nil {
		if dx := vc.Transform.X * float64(width); dx != 0 {
			x = fmt.Sprintf("(W-w)/2+(%s)", formatGain(dx))
		}
		if dy := vc.Transform.Y * float64(height); dy != 0 {
			y = fmt.Sprintf("(H-h)/2+(%s)", formatGain(dy))
		}
	}
	return x, y
}

// volumeExpr builds a piecewise-linear ffmpeg volume expression from clip-local
// keyframes (held flat before the first / after the last). Sanitize caps the
// list at 64 points so the expression length stays bounded.
func volumeExpr(kfs []models.StudioVolumeKeyframe) string {
	n := len(kfs)
	if n == 0 {
		return formatGain(1)
	}
	if n == 1 {
		return formatGain(kfs[0].Gain)
	}
	expr := formatGain(kfs[n-1].Gain) // hold after the last point
	for i := n - 2; i >= 0; i-- {
		a, b := kfs[i], kfs[i+1]
		seg := lerpExpr(a, b)
		expr = fmt.Sprintf("if(lt(t,%s),%s,%s)", formatSeconds(b.T), seg, expr)
	}
	return fmt.Sprintf("if(lt(t,%s),%s,%s)", formatSeconds(kfs[0].T), formatGain(kfs[0].Gain), expr)
}

func lerpExpr(a, b models.StudioVolumeKeyframe) string {
	span := b.T - a.T
	if span <= 0 {
		return formatGain(b.Gain)
	}
	return fmt.Sprintf("(%s+(%s)*(t-%s)/(%s))", formatGain(a.Gain), formatGain(b.Gain-a.Gain), formatSeconds(a.T), formatSeconds(span))
}

func duckRatio(amountDb float64) float64 {
	r := 1 + amountDb // heuristic: more dB → harder compression (1..20)
	if r < 1 {
		r = 1
	}
	if r > 20 {
		r = 20
	}
	return r
}

func loudnormArg(preset string) string {
	switch preset {
	case "streaming":
		return "loudnorm=I=-14:TP=-1.5:LRA=11"
	case "podcast":
		return "loudnorm=I=-16:TP=-1.5:LRA=11"
	case "broadcast":
		return "loudnorm=I=-23:TP=-2:LRA=7"
	default:
		return ""
	}
}

// eqArg maps per-clip legacy adjustments onto ffmpeg's eq filter.
func eqArg(a *models.StudioAdjustments) string {
	return fmt.Sprintf("eq=brightness=%s:contrast=%s:saturation=%s",
		formatGain(a.Brightness), formatGain(a.Contrast), formatGain(a.Saturation))
}

// drawtextArg renders a text overlay onto the clip.
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

// filterPathEscape escapes a filesystem path for use inside a single-quoted
// filtergraph filename option (lut3d/subtitles): backslash, single quote, and
// the filtergraph-special ':' are escaped.
func filterPathEscape(p string) string {
	p = strings.ReplaceAll(p, "\\", "\\\\")
	p = strings.ReplaceAll(p, "'", "\\'")
	p = strings.ReplaceAll(p, ":", "\\:")
	return p
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

func parseHexRGB(hex string) (r, g, b int) {
	h := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(h) != 6 {
		return 0, 255, 0
	}
	rv, err1 := strconv.ParseInt(h[0:2], 16, 0)
	gv, err2 := strconv.ParseInt(h[2:4], 16, 0)
	bv, err3 := strconv.ParseInt(h[4:6], 16, 0)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 255, 0
	}
	return int(rv), int(gv), int(bv)
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
