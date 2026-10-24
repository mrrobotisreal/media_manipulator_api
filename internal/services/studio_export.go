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

	// EDL v3 typed transitions (part 14). TransitionType/Dur describe this
	// clip's NON-dissolve entrance (dips/wipes/pushes/slides; crossDissolve
	// rides the legacy FadeIn path). TransOut* is the next clip-on-track's
	// transition into this clip's tail — pushes translate this clip out over
	// [TransOutStart, TransOutStart+TransOutDur]. Directional rule everywhere:
	// the type names WHERE THE ENTERING CLIP COMES FROM.
	TransitionType string
	TransitionDur  float64
	TransOutType   string
	TransOutStart  float64
	TransOutDur    float64

	// EDL v2.
	Transform *models.StudioTransform
	Crop      *models.StudioCrop
	BlendMode string
	Effects   []models.StudioEffect
	// LutPaths maps a lut effect's lutAssetId → local .cube path (downloaded by
	// the export job). A missing entry means the LUT is skipped.
	LutPaths map[string]string

	// EDL v3 per-property keyframes (part 15). Motion/opacity lanes become
	// per-frame filter expressions; effect-param lanes are driven by the
	// sendcmd sidecar (GenerateStudioSendCmd), whose filter-instance names are
	// derived from this seg's SORTED index (sortVideoSegs order).
	ClipKeyframes *models.StudioClipKeyframes

	// EDL v3 title clip (part 16): when set, InputIndex is -1 and the chain
	// starts from a transparent color= source carrying the drawbox/drawtext
	// layer stack instead of an input trim. Adjustments/effects are NOT
	// emitted for title segs (ffmpeg's eq/colorbalance stages destroy alpha;
	// the inspector hides them for title clips).
	Title *models.StudioTitle

	// SourceKind selects the chain head: "" or "video" trims the input, "image"
	// holds a looped still (no trim; the input is bounded by -t), "title"
	// starts from a color= source. Empty means video so existing literals and
	// goldens are unchanged.
	SourceKind string

	// Speed controls (part 17). Speed is the constant rate (0 = unset = 1x);
	// Reverse plays the trimmed span backwards; FreezeAt (source seconds) turns
	// the seg into a held frame lasting SourceOut-SourceIn. All zero values
	// leave the chain byte-identical to pre-v3 graphs.
	Speed    float64
	Reverse  bool
	FreezeAt *float64
}

// timelineDur is the seg's on-timeline length: the source span divided by the
// speed (a freeze or an unset speed is 1x).
func (vc StudioExportVideoSeg) timelineDur() float64 {
	return speedAdjustedDur(vc.SourceIn, vc.SourceOut, vc.Speed)
}

// speedAdjustedDur is (out-in)/speed with speed<=0 meaning 1x, clamped >= 0.
func speedAdjustedDur(in, out, speed float64) float64 {
	d := clampDurExport(in, out)
	if speed > 0 && speed != 1 {
		return d / speed
	}
	return d
}

// Seg source kinds (StudioExportVideoSeg.SourceKind).
const (
	StudioSegKindVideo = "video"
	StudioSegKindImage = "image"
	StudioSegKindTitle = "title"
)

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

	// EDL v3 per-property keyframes (part 15): `keyframes.volume` (eased,
	// wins over legacy VolumeKeyframes) and `keyframes.pan` (sendcmd-driven —
	// stereotools has no expression support but is commandable).
	ClipKeyframes *models.StudioClipKeyframes
	// SegIndex is this seg's index within plan.Audio — the stable suffix for
	// its stereotools@pan<i> instance name (chain labels vary with ducking).
	SegIndex int

	// TrackID (part 19) names the seg's EDL track when that track has an active
	// mixer stage (a StudioExportAudioTrack in plan.AudioTracks); "" otherwise.
	TrackID string

	// Speed controls (part 17): see StudioExportVideoSeg. PitchOff selects the
	// resample path (asetrate+aresample) over the pitch-preserving atempo chain.
	Speed    float64
	Reverse  bool
	PitchOff bool
}

// timelineDur is the seg's on-timeline length (source span / speed).
func (ac StudioExportAudioSeg) timelineDur() float64 {
	return speedAdjustedDur(ac.SourceIn, ac.SourceOut, ac.Speed)
}

// ApplyAudioSpeed copies a clip's speed controls onto its audio seg.
func ApplyAudioSpeed(seg *StudioExportAudioSeg, c models.StudioClip) {
	if sp := models.ClipSpeed(c); sp != 1 {
		seg.Speed = sp
	}
	seg.Reverse = c.Reverse && c.Freeze == nil
	seg.PitchOff = !models.ClipMaintainsPitch(c)
}

// StudioExportAudioTrack is one track's mixer stage (part 19): its clips are
// mixed together, then EQ -> compressor -> gain -> pan, then fed to the
// ducking/master stages. The order mirrors the preview's Web Audio chain
// (clipGain -> clipPan -> EQ -> compressor -> trackGain -> trackPan ->
// duckGain -> master), so the composition is clip x track x master.
type StudioExportAudioTrack struct {
	ID         string
	Gain       float64 // 0..2
	Pan        float64 // -1..1
	EQ         *models.StudioTrackEQ
	Compressor *models.StudioTrackCompressor
}

// eqActive reports whether any EQ band is non-flat.
func (t StudioExportAudioTrack) eqActive() bool {
	return t.EQ != nil && (t.EQ.LowGainDb != 0 || t.EQ.MidGainDb != 0 || t.EQ.HighGainDb != 0)
}

// active reports whether the track needs its own mixer stage.
func (t StudioExportAudioTrack) active() bool {
	return t.Gain != 1 || t.Pan != 0 || t.eqActive() || t.Compressor != nil
}

// StudioDucking is the resolved auto-ducking config for an export.
type StudioDucking struct {
	AmountDb  float64
	AttackMs  float64
	ReleaseMs float64
}

// ExportInput is one ffmpeg input: a local path plus the input options that
// must precede its -i (e.g. "-loop 1 -framerate 30 -t N" for a still image).
// Pre is nil for video/audio, so their argv is byte-identical to the bare path.
type ExportInput struct {
	Path string
	Pre  []string
}

// StudioExportPlan is the resolved, render-ready EDL.
type StudioExportPlan struct {
	Inputs   []ExportInput
	Video    []StudioExportVideoSeg
	Audio    []StudioExportAudioSeg
	Width    int
	Height   int
	FPS      float64
	Duration float64
	// EDL v2.
	Ducking  *StudioDucking
	// AudioTracks (part 19): the tracks with an active mixer stage. MasterGain
	// is the project master fader (nil = unity), applied after ducking and
	// before loudnorm.
	// BlankVideo (part 19): a hidden video track existed, so keep the video
	// output (black canvas) even when no visible video seg remains — the
	// preview shows black there, not an audio-only file.
	BlankVideo  bool
	AudioTracks []StudioExportAudioTrack
	MasterGain  *float64
	Loudness    string // '' | streaming | podcast | broadcast
	// CaptionsASSPath, when set, is burned in as the final video filter (Phase 7).
	CaptionsASSPath string
	// SendCmdPath, when set, is the keyframe sidecar (part 15) written by the
	// runner from GenerateStudioSendCmd. It rides ONE timeline-clocked sendcmd
	// instance on [vbase] (asendcmd on the mix for audio-only projects) and
	// addresses filters graph-wide by instance name.
	SendCmdPath string
}

// StudioRenderStats describes a render for the part-10 server telemetry
// (studio_render_completed/failed). Facts about the ffmpeg invocation only —
// nothing content-derived.
type StudioRenderStats struct {
	// Encoder in the analytics vocabulary: "nvenc" | "x264".
	Encoder string
	// FilterCount is the number of statements in the -filter_complex graph,
	// the proxy for compositing complexity.
	FilterCount int
}

// RunExport renders the plan to outputPath, reporting progress on jobID.
// The returned stats are valid even when the render fails — the failure
// telemetry wants the same encoder/complexity dimensions as success.
func (s *StudioExportService) RunExport(ctx context.Context, jobID string, plan StudioExportPlan, quality, outputPath string) (StudioRenderStats, error) {
	assertStudioSendCmdSupport() // one-time ffmpeg ≥ 4.4 check, log-only (part 15)
	encoder := studioH264Encoder(s.cfg)
	args := buildMultiTrackExportArgs(plan, encoder, quality, s.cfg.ContentStudioFontFile, s.cfg.ContentStudioFontsDir, outputPath)
	stats := StudioRenderStats{Encoder: encoderAnalyticsName(encoder), FilterCount: countFilterStatements(args)}
	return stats, runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, plan.Duration, args...)
}

// encoderAnalyticsName maps the ffmpeg encoder id onto the two-value analytics
// vocabulary the catalog documents.
func encoderAnalyticsName(encoder string) string {
	if encoder == "h264_nvenc" {
		return "nvenc"
	}
	return "x264"
}

// countFilterStatements counts the ';'-joined statements in the arg slice's
// -filter_complex value. Counting the built args (rather than re-deriving from
// the plan) means the number can never disagree with what ffmpeg actually ran.
func countFilterStatements(args []string) int {
	for i, a := range args {
		if a == "-filter_complex" && i+1 < len(args) {
			return strings.Count(args[i+1], ";") + 1
		}
	}
	return 0
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
		args = append(args, in.Pre...)
		args = append(args, "-i", in.Path)
	}
	args = append(args, "-filter_complex", strings.Join(stmts, ";"))
	args = append(args, "-map", "["+audioOut+"]", "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-t", durStr, outputPath)
	return args, true
}

// buildMultiTrackExportArgs constructs the ffmpeg arg slice for the full EDL.
// Pure + deterministic so the graph is unit-testable without invoking ffmpeg
// (title font resolution consults the fonts directory on disk, falling back to
// fontFile — pass a fixed fontsDir in tests).
func buildMultiTrackExportArgs(plan StudioExportPlan, encoder, quality, fontFile, fontsDir, outputPath string) []string {
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
		args = append(args, in.Pre...)
		args = append(args, "-i", in.Path)
	}

	stmts := make([]string, 0, len(plan.Video)*2+len(plan.Audio)+4)

	// --- video composite ---
	videoOut := ""
	if len(plan.Video) > 0 || plan.BlankVideo {
		segs := sortVideoSegs(plan.Video)
		// The keyframe sidecar rides the base color source: CFR at the project
		// rate with PTS = timeline time, so its intervals fire deterministically
		// and one instance drives every commandable filter in the graph
		// (research ticket 01's design).
		base := fmt.Sprintf("color=c=black:s=%dx%d:r=%s:d=%s,format=yuv420p", width, height, fpsStr, durStr)
		if plan.SendCmdPath != "" {
			base += fmt.Sprintf(",sendcmd=f='%s'", filterPathEscape(plan.SendCmdPath))
		}
		stmts = append(stmts, base+"[vbase]")
		last := "vbase"
		for k, vc := range segs {
			clipStmts, clipLabel := buildVideoClipChain(vc, k, width, height, fpsStr, fontFile, fontsDir)
			stmts = append(stmts, clipStmts...)

			outL := "vout"
			if k < len(segs)-1 || plan.CaptionsASSPath != "" {
				outL = fmt.Sprintf("vov%d", k)
			}
			if isDipTransition(vc.TransitionType) && vc.TransitionDur > 0 {
				// Dip (part 14): composite the clip normally (enable shifted to
				// the midpoint inside compositeStep), then layer the full-canvas
				// color quad peaking at mid-transition on top.
				mid := fmt.Sprintf("vtr%d", k)
				stmts = append(stmts, compositeStep(last, clipLabel, vc, k, width, height, durStr, fpsStr, mid)...)
				stmts = append(stmts, dipStatements(vc, k, width, height, fpsStr, durStr, mid, outL)...)
			} else {
				stmts = append(stmts, compositeStep(last, clipLabel, vc, k, width, height, durStr, fpsStr, outL)...)
			}
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

	// Audio-only project with keyframed pan: no [vbase] exists to carry the
	// sidecar, so an asendcmd on the (timeline-clocked) mix output drives it.
	if videoOut == "" && audioOut != "" && plan.SendCmdPath != "" {
		stmts = append(stmts, fmt.Sprintf("[%s]asendcmd=f='%s'[aoutsc]", audioOut, filterPathEscape(plan.SendCmdPath)))
		audioOut = "aoutsc"
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
	args = append(args, "-t", durStr, "-movflags", "+faststart", outputPath)
	return args
}

// buildVideoClipChain builds the filter statements for one video clip and
// returns them plus the final label (vc<k>). The common case is a single
// comma-joined statement; a LUT with intensity < 1 expands into a
// split/lut3d/blend sub-graph. Title clips (part 16) swap the input trim for a
// transparent color= source carrying the drawbox/drawtext layer stack, then
// ride the identical downstream chain (crop/scale/opacity/fade/rotate/wipe).
func buildVideoClipChain(vc StudioExportVideoSeg, k, width, height int, fpsStr, fontFile, fontsDir string) ([]string, string) {
	tsStr := formatSeconds(vc.TimelineStart)
	kf := keyframesOf(vc.ClipKeyframes)
	scale := 1.0
	if vc.Transform != nil && vc.Transform.Scale > 0 {
		scale = vc.Transform.Scale
	}
	// Animated scale (part 15): the static fit targets the MAX keyframed scale
	// (so this stage stays deterministic), and a per-frame eval=frame downscale
	// after `rotate` animates s(t)/sMax. Uniform scale and rotation about the
	// center commute, so the reordering matches the preview's quad math.
	scaleLane := kf.Scale
	sMax := scale
	if hasLane(scaleLane) {
		sMax = maxLaneValue(scaleLane, scale)
	}
	boxW := int(math.Round(float64(width) * sMax))
	boxH := int(math.Round(float64(height) * sMax))

	var chain []string
	isImage := vc.SourceKind == StudioSegKindImage && vc.Title == nil
	if vc.Title != nil {
		// Transparent full-frame source at the clip's duration; the layer
		// stack draws onto it (bottom → top, same order as the raster), then
		// the standard setpts shifts it onto the timeline.
		chain = append(chain,
			fmt.Sprintf("color=c=black@0:s=%dx%d:r=%s:d=%s", width, height, fpsStr, formatSeconds(clampDurExport(vc.SourceIn, vc.SourceOut))),
			"format=yuva420p",
		)
		chain = append(chain, titleLayerParts(vc.Title, width, height, fontsDir, fontFile)...)
		chain = append(chain, fmt.Sprintf("setpts=PTS-STARTPTS+%s/TB", tsStr))
	} else if isImage {
		// Looped still: the input is already bounded by -t (see
		// imageInputPre), so no trim — a trim on a looped frame stream would
		// only re-cut what -t produced. rgba keeps PNG alpha through scale.
		chain = append(chain,
			fmt.Sprintf("[%d:v]format=rgba", vc.InputIndex),
			fmt.Sprintf("setpts=PTS-STARTPTS+%s/TB", tsStr),
		)
	} else if vc.FreezeAt != nil {
		chain = append(chain, freezeChainParts(vc, tsStr)...)
	} else {
		chain = append(chain,
			fmt.Sprintf("[%d:v]trim=start=%s:end=%s", vc.InputIndex, formatSeconds(vc.SourceIn), formatSeconds(vc.SourceOut)),
		)
		chain = append(chain, speedPtsParts(vc.Speed, vc.Reverse, tsStr)...)
	}
	if c := cropArg(vc.Crop); c != "" {
		chain = append(chain, c)
	}
	scaleOpts := ""
	if isImage {
		// Image segs only (video chains stay byte-stable): pin the RGB->YUV
		// matrix to bt709 like the preview's sRGB path.
		scaleOpts = ":in_color_matrix=auto:out_color_matrix=bt709"
	}
	chain = append(chain,
		fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease%s", boxW, boxH, scaleOpts),
		"setsar=1",
		fmt.Sprintf("fps=%s", fpsStr),
	)
	// Adjustments/effects are skipped for title segs: eq/colorbalance/vibrance
	// have no alpha-capable pixel formats, so ffmpeg would silently convert
	// away the title's transparency. The inspector hides these for title clips.
	if vc.Adjustments != nil && vc.Title == nil {
		chain = append(chain, eqArg(vc.Adjustments))
	}

	lumetri, lut, chroma := pickEffects(vc.Effects)
	if vc.Title != nil {
		lumetri, lut, chroma = nil, nil, nil
	}
	if lumetri != nil {
		chain = append(chain, lumetriChainArgs(lumetri, k, vc.ClipKeyframes)...)
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
				// split + blend to mix the graded result at `intensity`. blend
				// normal computes top*opacity + bottom*(1-opacity) (vf_blend), so
				// the GRADED stream is the TOP input — out = graded*intensity +
				// ungraded*(1-intensity), the same linear mix as the preview
				// shader's mix(c, graded, intensity) (parity closure, part 12).
				pre := fmt.Sprintf("vc%da", k)
				stmts = append(stmts, strings.Join(chain, ",")+"["+pre+"]")
				stmts = append(stmts, fmt.Sprintf("[%s]split[%sx][%sy]", pre, pre, pre))
				stmts = append(stmts, fmt.Sprintf("[%sy]%s[%sl]", pre, lut3dPart(lutPath), pre))
				gLabel := fmt.Sprintf("vc%dg", k)
				stmts = append(stmts, fmt.Sprintf("[%sl][%sx]blend=all_mode=normal:all_opacity=%s[%s]", pre, pre, formatGain(intensity), gLabel))
				chain = nil
				chainHead = "[" + gLabel + "]"
			}
		}
	}

	chain = append(chain, "format=yuva420p")
	if chroma != nil {
		chain = append(chain, chromaChainArgs(chroma, k, vc.ClipKeyframes)...)
	}
	if hasLane(kf.Opacity) {
		// Keyframed opacity keeps the part-12 alpha path (colorchannelmixer aa,
		// not expression-capable but commandable): the instance-named filter is
		// seeded with the lane's value at clip start and driven per frame by
		// the sendcmd sidecar.
		chain = append(chain, fmt.Sprintf("colorchannelmixer@op%d=aa=%s", k, formatGain(clamp01(KeyframeValueAt(kf.Opacity, 0, vc.Opacity)))))
	} else if vc.Opacity > 0 && vc.Opacity < 1 {
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
	if hasLane(scaleLane) {
		// Per-frame downscale by s(t)/sMax (≤ 1 by construction). The only
		// consumer of the varying frame size is `overlay`, which already
		// handles per-frame sizes (part 14's wipe crops rely on the same).
		// trunc(x/2)*2 keeps subsampled dimensions even.
		r := fmt.Sprintf("(%s)/%s", KeyframeExpr(scaleLane, videoClipTimeExpr(vc.TimelineStart)), formatGain(sMax))
		chain = append(chain, fmt.Sprintf(
			"scale=w=%s:h=%s:eval=frame",
			quoteChainExpr(fmt.Sprintf("max(2,trunc(iw*%s/2)*2)", r)),
			quoteChainExpr(fmt.Sprintf("max(2,trunc(ih*%s/2)*2)", r)),
		))
	}
	if isWipeTransition(vc.TransitionType) && vc.TransitionDur > 0 {
		// Wipe (part 14): animated crop reveals the entering clip from its
		// named edge — dimensions/offsets are per-frame expressions of t
		// (progress p clamped 0→1 across the overlap; identity once p=1).
		chain = append(chain, wipeCropArg(vc))
	}
	stmts = append(stmts, chainHead+strings.Join(chain, ",")+"["+finalLabel+"]")
	return stmts, finalLabel
}

// compositeStep overlays (normal) or blends (non-normal) the clip onto the
// running composite, honoring the transform position + the enable window.
// Part 14: push/slide/wipe transitions animate the overlay position (see
// overlayPosWithTransitions), and dips shift the enable start to the overlap
// midpoint — the switch happens under the fully opaque color layer.
func compositeStep(last, clipLabel string, vc StudioExportVideoSeg, k, width, height int, durStr, fpsStr, outL string) []string {
	enableStart := vc.TimelineStart
	if isDipTransition(vc.TransitionType) && vc.TransitionDur > 0 {
		enableStart += vc.TransitionDur / 2
	}
	tsStr := formatSeconds(enableStart)
	teStr := formatSeconds(vc.TimelineStart + vc.timelineDur())
	xExpr, yExpr := overlayPosWithTransitions(vc, width, height)

	mode := vc.BlendMode
	if mode == "" || mode == models.StudioBlendNormal {
		return []string{fmt.Sprintf(
			"[%s][%s]overlay=x=%s:y=%s:enable='between(t,%s,%s)':eof_action=pass[%s]",
			last, clipLabel, xExpr, yExpr, tsStr, teStr, outL,
		)}
	}

	// Non-normal blend (parity closure, part 12): both pipelines state ONE
	// semantic — out = mix(backdrop, blendRGB(backdrop, src), layerAlpha) —
	// which the WebGL preview computes directly in its fragment shader.
	// The graph expresses it exactly:
	//   1. position the clip on a transparent full-frame layer ([pos], yuva420p;
	//      its alpha carries the spatial window × clip opacity × dissolve fade ×
	//      chroma key);
	//   2. blend backdrop×clip per RGB channel in gbrp (vf_blend on planar RGB is
	//      per-channel math identical to the shader; top input = backdrop so the
	//      `overlay` mode conditions on the backdrop, like the shader);
	//   3. give the blended frame the layer's alpha (alphamerge) and composite it
	//      onto the untouched backdrop with plain straight-alpha overlay, gated
	//      by the clip's enable window.
	bg := fmt.Sprintf("bg%d", k)
	pos := fmt.Sprintf("pos%d", k)
	posc := fmt.Sprintf("posc%d", k)
	posa := fmt.Sprintf("posa%d", k)
	mask := fmt.Sprintf("mask%d", k)
	bk := fmt.Sprintf("bk%d", k)   // backdrop copy for the final composite
	bt := fmt.Sprintf("bt%d", k)   // backdrop copy fed to blend (as top input)
	bln := fmt.Sprintf("bln%d", k) // RGB blend result
	lay := fmt.Sprintf("lay%d", k) // blend result + layer alpha
	return []string{
		fmt.Sprintf("color=c=black@0:s=%dx%d:r=%s:d=%s,format=yuva420p[%s]", width, height, fpsStr, durStr, bg),
		fmt.Sprintf("[%s][%s]overlay=x=%s:y=%s[%s]", bg, clipLabel, xExpr, yExpr, pos),
		fmt.Sprintf("[%s]split[%s][%s]", pos, posc, posa),
		fmt.Sprintf("[%s]alphaextract[%s]", posa, mask),
		fmt.Sprintf("[%s]format=gbrp[%sr]", posc, posc),
		fmt.Sprintf("[%s]split[%s][%s]", last, bk, bt),
		fmt.Sprintf("[%s]format=gbrp[%sr]", bt, bt),
		fmt.Sprintf("[%sr][%sr]blend=all_mode=%s[%s]", bt, posc, mode, bln),
		fmt.Sprintf("[%s][%s]alphamerge,format=yuva420p[%s]", bln, mask, lay),
		fmt.Sprintf("[%s][%s]overlay=x=0:y=0:enable='between(t,%s,%s)':eof_action=pass[%s]", bk, lay, tsStr, teStr, outL),
	}
}

// buildAudioSection builds the audio mix statements + the output label. Returns
// ("", "") when there is no audio.
//
// Stage order (part 19, mirrored by the preview graph): per-clip chain ->
// per-track mixer stage (EQ, compressor, gain, pan) -> ducking -> master gain
// -> loudnorm.
func buildAudioSection(plan StudioExportPlan, durStr string) ([]string, string) {
	if len(plan.Audio) == 0 {
		return nil, ""
	}
	var stmts []string

	// Final label: when a loudness preset is set, mix to a premaster then
	// loudnorm into [aout]; otherwise mix directly into [aout]. A master gain
	// adds one more stage between the mix and the loudness/out label.
	finalLabel := "aout"
	if plan.Loudness != "" {
		finalLabel = "apm"
	}
	mixLabel := finalLabel
	masterGain := masterGainOf(plan)
	if masterGain != 1 {
		mixLabel = "amixpre"
	}

	ducking := plan.Ducking
	voice, bed := splitVoiceBed(plan.Audio)
	useDuck := ducking != nil && len(voice) > 0 && len(bed) > 0

	if !useDuck {
		// Standard path (byte-identical to v1 when no v2/mixer fields are present).
		mixSegs(&stmts, plan, plan.Audio, "ac", mixLabel)
	} else {
		// Ducking: bed group -> sidechaincompress keyed by the voice group.
		bedLabel := mixSegs(&stmts, plan, bed, "bed", "bed")
		voiceLabel := mixSegs(&stmts, plan, voice, "voice", "voice")
		stmts = append(stmts, fmt.Sprintf("[%s]asplit[%smix][%ssc]", voiceLabel, voiceLabel, voiceLabel))
		stmts = append(stmts, fmt.Sprintf(
			"[%s][%ssc]sidechaincompress=threshold=0.02:ratio=%s:attack=%s:release=%s:makeup=1[duckedbed]",
			bedLabel, voiceLabel, formatGain(duckRatio(ducking.AmountDb)), formatGain(ducking.AttackMs), formatGain(ducking.ReleaseMs),
		))
		stmts = append(stmts, fmt.Sprintf("[duckedbed][%smix]amix=inputs=2:normalize=0:dropout_transition=0[%s]", voiceLabel, mixLabel))
	}

	if masterGain != 1 {
		stmts = append(stmts, fmt.Sprintf("[%s]volume=%s[%s]", mixLabel, formatGain(masterGain), finalLabel))
	}
	if plan.Loudness != "" {
		stmts = append(stmts, fmt.Sprintf("[%s]%s[aout]", finalLabel, loudnormArg(plan.Loudness)))
	}
	return stmts, "aout"
}

func masterGainOf(plan StudioExportPlan) float64 {
	if plan.MasterGain == nil {
		return 1
	}
	return *plan.MasterGain
}

// trackStage finds the mixer stage for a seg's track.
func trackStage(plan StudioExportPlan, id string) (StudioExportAudioTrack, bool) {
	if id == "" {
		return StudioExportAudioTrack{}, false
	}
	for _, t := range plan.AudioTracks {
		if t.ID == id {
			return t, true
		}
	}
	return StudioExportAudioTrack{}, false
}

// mixSegs writes one chain per seg and amixes them into `out` (or passes a
// single seg straight through). Segs on a track with an active mixer stage are
// first mixed together and run through trackStageChain; the stage output joins
// the plain segs in the group mix. With no active stages this reduces to the
// pre-mixer wiring byte-for-byte (labels `<prefix><k>`). Returns `out`.
func mixSegs(stmts *[]string, plan StudioExportPlan, group []StudioExportAudioSeg, prefix, out string) string {
	var plain []StudioExportAudioSeg
	var order []string
	byTrack := map[string][]StudioExportAudioSeg{}
	for _, ac := range group {
		if _, ok := trackStage(plan, ac.TrackID); ok {
			if _, seen := byTrack[ac.TrackID]; !seen {
				order = append(order, ac.TrackID)
			}
			byTrack[ac.TrackID] = append(byTrack[ac.TrackID], ac)
		} else {
			plain = append(plain, ac)
		}
	}
	if len(order) == 0 {
		mixPlain(stmts, plain, prefix, out)
		return out
	}
	// Each track stage becomes one input of the group mix.
	var inputs []string
	for _, id := range order {
		stage, _ := trackStage(plan, id)
		label := fmt.Sprintf("%st%d", prefix, len(inputs))
		raw := label + "m"
		mixPlain(stmts, byTrack[id], label+"s", raw)
		*stmts = append(*stmts, fmt.Sprintf("[%s]%s[%s]", raw, strings.Join(trackStageParts(stage), ","), label))
		inputs = append(inputs, label)
	}
	for k, ac := range plain {
		label := fmt.Sprintf("%sp%d", prefix, k)
		*stmts = append(*stmts, audioSegChain(ac, label))
		inputs = append(inputs, label)
	}
	if len(inputs) == 1 {
		// Rename the lone stage output to `out` via anull.
		*stmts = append(*stmts, fmt.Sprintf("[%s]anull[%s]", inputs[0], out))
		return out
	}
	var mixIn strings.Builder
	for _, l := range inputs {
		mixIn.WriteString("[" + l + "]")
	}
	*stmts = append(*stmts, fmt.Sprintf("%samix=inputs=%d:normalize=0:dropout_transition=0[%s]", mixIn.String(), len(inputs), out))
	return out
}

// mixPlain is the pre-mixer group wiring: a single seg is chained straight into
// `out`; several are chained into `<prefix><k>` and amixed into `out`.
func mixPlain(stmts *[]string, group []StudioExportAudioSeg, prefix, out string) {
	if len(group) == 1 {
		*stmts = append(*stmts, audioSegChain(group[0], out))
		return
	}
	var mixIn strings.Builder
	for k, ac := range group {
		label := fmt.Sprintf("%s%d", prefix, k)
		*stmts = append(*stmts, audioSegChain(ac, label))
		mixIn.WriteString("[" + label + "]")
	}
	*stmts = append(*stmts, fmt.Sprintf("%samix=inputs=%d:normalize=0:dropout_transition=0[%s]", mixIn.String(), len(group), out))
}

// Track EQ band centers/topology. These constants are mirrored by
// lib/studio/audioPresets.ts (EQ_BANDS) for the BiquadFilter chain: low shelf
// 120 Hz, peaking 1 kHz (Q 1), high shelf 8 kHz, RBJ shelf slope 1.
const (
	trackEQLowHz  = 120
	trackEQMidHz  = 1000
	trackEQHighHz = 8000
	// acompressor's knee is a linear ratio (default 2.82843 ~ 9 dB); the
	// preview's DynamicsCompressorNode knee is set to 9 dB to match.
	trackCompKnee = 2.82843
)

// trackStageParts is the comma-joined filter list for one track's mixer stage:
// EQ -> compressor -> gain -> pan. Flat EQ bands and unity gain / centre pan
// emit nothing.
func trackStageParts(t StudioExportAudioTrack) []string {
	var parts []string
	if t.EQ != nil {
		if g := t.EQ.LowGainDb; g != 0 {
			parts = append(parts, fmt.Sprintf("lowshelf=f=%d:t=s:w=1:g=%s", trackEQLowHz, formatGain(g)))
		}
		if g := t.EQ.MidGainDb; g != 0 {
			parts = append(parts, fmt.Sprintf("equalizer=f=%d:t=q:w=1:g=%s", trackEQMidHz, formatGain(g)))
		}
		if g := t.EQ.HighGainDb; g != 0 {
			parts = append(parts, fmt.Sprintf("highshelf=f=%d:t=s:w=1:g=%s", trackEQHighHz, formatGain(g)))
		}
	}
	if c := t.Compressor; c != nil {
		parts = append(parts, fmt.Sprintf("acompressor=threshold=%s:ratio=%s:attack=%s:release=%s:makeup=%s:knee=%s",
			formatLinear(dbToLinear(c.ThresholdDb)), formatGain(c.Ratio), formatGain(c.AttackMs), formatGain(c.ReleaseMs),
			formatGain(math.Max(1, dbToLinear(c.MakeupDb))), formatLinear(trackCompKnee)))
	}
	if t.Gain != 1 {
		parts = append(parts, fmt.Sprintf("volume=%s", formatGain(t.Gain)))
	}
	if t.Pan != 0 {
		parts = append(parts, fmt.Sprintf("stereotools=balance_in=%s", formatGain(t.Pan)))
	}
	if len(parts) == 0 {
		parts = append(parts, "anull")
	}
	return parts
}

func dbToLinear(db float64) float64 { return math.Pow(10, db/20) }

// formatLinear keeps 6 decimals: acompressor's threshold floor (0.000976563)
// would round to 0.001 at 3 decimals.
func formatLinear(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// audioSegChain builds one audio clip's chain → [label].
func audioSegChain(ac StudioExportAudioSeg, label string) string {
	delayMs := int(math.Round(ac.TimelineStart * 1000))
	if delayMs < 0 {
		delayMs = 0
	}
	dur := ac.timelineDur()
	parts := []string{
		fmt.Sprintf("[%d:a]atrim=start=%s:end=%s", ac.InputIndex, formatSeconds(ac.SourceIn), formatSeconds(ac.SourceOut)),
		"asetpts=PTS-STARTPTS",
		"aformat=sample_rates=48000:channel_layouts=stereo",
	}
	parts = append(parts, audioSpeedParts(ac.Speed, ac.Reverse, ac.PitchOff)...)
	if v3vol := keyframesOf(ac.ClipKeyframes).Volume; hasLane(v3vol) {
		// v3 lane (part 15): eased piecewise expression; wins over the legacy
		// linear lane (the client keeps both mirrored). t is post-asetpts
		// clip-local time, matching the keyframe times.
		parts = append(parts, fmt.Sprintf("volume='%s':eval=frame", KeyframeExpr(v3vol, "t")))
	} else if len(ac.VolumeKeyframes) > 0 {
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
	if panLane := keyframesOf(ac.ClipKeyframes).Pan; hasLane(panLane) {
		// Keyframed pan (part 15): stereotools is not expression-capable but IS
		// commandable — the instance-named filter is seeded with the lane's
		// value at clip start and driven by the sendcmd sidecar.
		parts = append(parts, fmt.Sprintf("stereotools@pan%d=balance_in=%s", ac.SegIndex, formatGain(KeyframeValueAt(panLane, 0, ac.Pan))))
	} else if ac.Pan != 0 {
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

// speedPtsParts is the video retime tail of a trimmed clip chain (part 17):
// optional `reverse` (after the trim, so the buffered span is clip-length), then
// the setpts that divides timeline time by the speed and places the clip. With
// no speed and no reverse it is the exact pre-v3 `setpts=PTS-STARTPTS+<ts>/TB`.
func speedPtsParts(speed float64, reverse bool, tsStr string) []string {
	var out []string
	if reverse {
		out = append(out, "setpts=PTS-STARTPTS", "reverse")
	}
	if speed > 0 && speed != 1 {
		out = append(out, fmt.Sprintf("setpts=(PTS-STARTPTS)/%s+%s/TB", formatTempo(speed), tsStr))
	} else {
		out = append(out, fmt.Sprintf("setpts=PTS-STARTPTS+%s/TB", tsStr))
	}
	return out
}

// freezeChainParts is a held-frame clip: trim to the single frame at
// freeze.sourceTime, then `tpad=stop_mode=clone` extends it to the clip's
// duration. tpad was chosen over `loop` (which buffers the frame count it is
// told to and needs a size argument); it is stateless and cheap, and the
// project-rate `fps` stage that follows normalizes the cloned frames.
func freezeChainParts(vc StudioExportVideoSeg, tsStr string) []string {
	return []string{
		fmt.Sprintf("[%d:v]trim=start=%s", vc.InputIndex, formatSeconds(*vc.FreezeAt)),
		"setpts=PTS-STARTPTS",
		"trim=end_frame=1",
		fmt.Sprintf("tpad=stop_mode=clone:stop_duration=%s", formatSeconds(clampDurExport(vc.SourceIn, vc.SourceOut))),
		fmt.Sprintf("setpts=PTS-STARTPTS+%s/TB", tsStr),
	}
}

// audioSpeedParts retimes a trimmed, 48k-stereo audio clip (part 17). Reverse
// comes first (clip-length buffer). Pitch preserved (default): an `atempo`
// chain factored into steps within atempo's portable 0.5..2.0 range. Pitch
// off: resample trick (asetrate to speed*48k, aresample back), which shifts
// pitch with speed like a tape.
func audioSpeedParts(speed float64, reverse, pitchOff bool) []string {
	var out []string
	if reverse {
		out = append(out, "areverse")
	}
	if speed > 0 && speed != 1 {
		if pitchOff {
			out = append(out,
				fmt.Sprintf("asetrate=%d", int(math.Round(48000*speed))),
				"aresample=48000",
			)
		} else {
			for _, f := range atempoFactors(speed) {
				out = append(out, "atempo="+formatTempo(f))
			}
		}
	}
	if len(out) > 0 {
		// Re-zero so volume/fade expressions keep reading clip-local TIMELINE time.
		out = append(out, "asetpts=PTS-STARTPTS")
	}
	return out
}

// atempoFactors splits a rate into atempo steps each within [0.5, 2.0] whose
// product is the rate: whole 2.0 (or 0.5) steps, then one remainder step.
func atempoFactors(speed float64) []float64 {
	if speed <= 0 || speed == 1 {
		return nil
	}
	var fs []float64
	rem := speed
	for rem > 2.0+1e-9 {
		fs = append(fs, 2.0)
		rem /= 2.0
	}
	for rem < 0.5-1e-9 {
		fs = append(fs, 0.5)
		rem /= 0.5
	}
	if math.Abs(rem-1) > 1e-9 {
		fs = append(fs, rem)
	}
	return fs
}

// formatTempo prints a rate with enough precision for the setpts/atempo math
// (formatGain's 3 decimals would drift a factored chain).
func formatTempo(v float64) string {
	return strconv.FormatFloat(v, 'f', 6, 64)
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

// lumetriChainArgs maps a lumetri effect onto ffmpeg filters. The WebGL
// preview shader mirrors this chain stage-for-stage (exposure → colorbalance
// midtones → vibrance → eq contrast/saturation), including colorbalance's
// midtone-weight curve and vibrance's formula — see applyLumetri in
// lib/studio/glCompositor.ts and the reference math in lib/studio/colorMath.ts
// (parity closure, part 12).
//
// Part 15: a stage with any KEYFRAMED param is always emitted, carries an
// instance name derived from the seg's sorted index k (the sendcmd sidecar's
// command target), and is seeded with the lane's value at clip start. Stages
// without keyframes keep the pre-15 byte-identical static emission.
func lumetriChainArgs(e *models.StudioEffect, k int, clipKfs *models.StudioClipKeyframes) []string {
	var out []string
	expLane := effectLane(clipKfs, e.ID, "exposure")
	exp := KeyframeValueAt(expLane, 0, optF(e.Exposure, 0))
	if hasLane(expLane) {
		g := math.Pow(2, exp)
		out = append(out, fmt.Sprintf("colorchannelmixer@lume%d=rr=%s:gg=%s:bb=%s", k, formatGain(g), formatGain(g), formatGain(g)))
	} else if exp != 0 {
		g := math.Pow(2, exp) // matches the shader's c *= 2^exposure
		out = append(out, fmt.Sprintf("colorchannelmixer=rr=%s:gg=%s:bb=%s", formatGain(g), formatGain(g), formatGain(g)))
	}
	tempLane := effectLane(clipKfs, e.ID, "temperature")
	tintLane := effectLane(clipKfs, e.ID, "tint")
	temp := KeyframeValueAt(tempLane, 0, optF(e.Temperature, 0))
	tint := KeyframeValueAt(tintLane, 0, optF(e.Tint, 0))
	if hasLane(tempLane) || hasLane(tintLane) {
		out = append(out, fmt.Sprintf("colorbalance@lumb%d=rm=%s:gm=%s:bm=%s", k, formatGain(temp/100), formatGain(-tint/100), formatGain(-temp/100)))
	} else if temp != 0 || tint != 0 {
		out = append(out, fmt.Sprintf("colorbalance=rm=%s:gm=%s:bm=%s", formatGain(temp/100), formatGain(-tint/100), formatGain(-temp/100)))
	}
	vibLane := effectLane(clipKfs, e.ID, "vibrance")
	vib := KeyframeValueAt(vibLane, 0, optF(e.Vibrance, 0))
	if hasLane(vibLane) {
		out = append(out, fmt.Sprintf("vibrance@lumv%d=intensity=%s", k, formatGain(vib)))
	} else if vib != 0 {
		out = append(out, fmt.Sprintf("vibrance=intensity=%s", formatGain(vib)))
	}
	conLane := effectLane(clipKfs, e.ID, "contrast")
	satLane := effectLane(clipKfs, e.ID, "saturation")
	con := KeyframeValueAt(conLane, 0, optF(e.Contrast, 1))
	sat := KeyframeValueAt(satLane, 0, optF(e.Saturation, 1))
	if hasLane(conLane) || hasLane(satLane) {
		out = append(out, fmt.Sprintf("eq@lumq%d=contrast=%s:saturation=%s", k, formatGain(con), formatGain(sat)))
	} else if con != 1 || sat != 1 {
		out = append(out, fmt.Sprintf("eq=contrast=%s:saturation=%s", formatGain(con), formatGain(sat)))
	}
	return out
}

func lut3dPart(localPath string) string {
	return fmt.Sprintf("lut3d=file='%s':interp=trilinear", filterPathEscape(localPath))
}

// chromaChainArgs emits chromakey + despill (despill type chosen by the
// dominant channel of the key color). Part 15: keyframed params (backend
// capability only — the UI deliberately doesn't expose them, ramping a key
// mid-shot causes edge crawl) get instance names + sidecar-driven values,
// mirroring lumetriChainArgs.
func chromaChainArgs(e *models.StudioEffect, k int, clipKfs *models.StudioClipKeyframes) []string {
	key := "#00FF00"
	if e.KeyColor != nil {
		key = *e.KeyColor
	}
	simLane := effectLane(clipKfs, e.ID, "similarity")
	blendLane := effectLane(clipKfs, e.ID, "blend")
	despillLane := effectLane(clipKfs, e.ID, "despill")
	sim := KeyframeValueAt(simLane, 0, optF(e.Similarity, 0.1))
	blend := KeyframeValueAt(blendLane, 0, optF(e.Blend, 0.1))
	despill := KeyframeValueAt(despillLane, 0, optF(e.Despill, 0.5))
	var out []string
	if hasLane(simLane) || hasLane(blendLane) {
		out = append(out, fmt.Sprintf("chromakey@ck%d=color=%s:similarity=%s:blend=%s", k, hexToFFColor(key), formatGain(sim), formatGain(blend)))
	} else {
		out = append(out, fmt.Sprintf("chromakey=color=%s:similarity=%s:blend=%s", hexToFFColor(key), formatGain(sim), formatGain(blend)))
	}
	if hasLane(despillLane) {
		out = append(out, fmt.Sprintf("despill@dsp%d=type=%s:mix=%s", k, despillType(key), formatGain(despill)))
	} else if despill > 0 {
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
// clockwise convention); transparent corners via c=black@0. A keyframed
// rotation animates the angle per frame; the output stays a CONSTANT bounding
// box (the diagonal — the max over any angle) so every downstream filter but
// overlay sees a fixed frame size, and (W-w)/2 keeps the center in place.
func rotateArg(vc StudioExportVideoSeg) string {
	rotLane := keyframesOf(vc.ClipKeyframes).Rotation
	if hasLane(rotLane) {
		deg := KeyframeExpr(rotLane, videoClipTimeExpr(vc.TimelineStart))
		diag := "ceil(hypot(iw,ih))"
		return fmt.Sprintf("rotate=a=%s:c=black@0:ow=%s:oh=%s",
			quoteChainExpr(fmt.Sprintf("(%s)*PI/180", deg)), quoteChainExpr(diag), quoteChainExpr(diag))
	}
	if vc.Transform == nil || vc.Transform.RotationDeg == 0 {
		return ""
	}
	rad := vc.Transform.RotationDeg * math.Pi / 180
	return fmt.Sprintf("rotate=a=%s:c=black@0:ow=rotw(%s):oh=roth(%s)", formatGain(rad), formatGain(rad), formatGain(rad))
}

// overlayPos returns the overlay x/y expressions, centered + transform offset.
// Keyframed position lanes (part 15) become per-frame expressions of the
// normalized offset × frame size; overlay re-evaluates x/y per frame, so
// (W-w)/2 recentering also tracks an animated-scale clip's varying w/h.
func overlayPos(vc StudioExportVideoSeg, width, height int) (string, string) {
	kf := keyframesOf(vc.ClipKeyframes)
	x, y := "(W-w)/2", "(H-h)/2"
	if hasLane(kf.PositionX) {
		x = fmt.Sprintf("(W-w)/2+W*(%s)", KeyframeExpr(kf.PositionX, videoClipTimeExpr(vc.TimelineStart)))
	} else if vc.Transform != nil {
		if dx := vc.Transform.X * float64(width); dx != 0 {
			x = fmt.Sprintf("(W-w)/2+(%s)", formatGain(dx))
		}
	}
	if hasLane(kf.PositionY) {
		y = fmt.Sprintf("(H-h)/2+H*(%s)", KeyframeExpr(kf.PositionY, videoClipTimeExpr(vc.TimelineStart)))
	} else if vc.Transform != nil {
		if dy := vc.Transform.Y * float64(height); dy != 0 {
			y = fmt.Sprintf("(H-h)/2+(%s)", formatGain(dy))
		}
	}
	return x, y
}

// --- typed transitions (part 14) --------------------------------------------

func isDipTransition(t string) bool {
	return t == models.StudioTransitionDipToBlack || t == models.StudioTransitionDipToWhite
}

func isWipeTransition(t string) bool {
	switch t {
	case models.StudioTransitionWipeLeft, models.StudioTransitionWipeRight,
		models.StudioTransitionWipeUp, models.StudioTransitionWipeDown:
		return true
	}
	return false
}

// transitionProgressExpr is p(t): 0→1 across [start, start+dur], clamped — the
// same clamp the preview's transitionFrameState applies.
func transitionProgressExpr(start, dur float64) string {
	return fmt.Sprintf("min(1,max(0,(t-%s)/%s))", formatSeconds(start), formatSeconds(dur))
}

// quoteFilterExpr single-quotes an option value when it contains characters
// the filtergraph parser would otherwise split on. Plain arithmetic like
// (W-w)/2 stays unquoted so pre-part-14 graphs remain byte-identical.
func quoteFilterExpr(e string) string {
	if strings.ContainsAny(e, ",") {
		return "'" + e + "'"
	}
	return e
}

// wipeCropArg crops the entering clip to its revealed region, growing from the
// named edge (= where the clip comes from). w/h/x/y are per-frame expressions;
// the 2px floor keeps the frame valid at p≈0 (the first frame of the overlap).
func wipeCropArg(vc StudioExportVideoSeg) string {
	p := transitionProgressExpr(vc.TimelineStart, vc.TransitionDur)
	switch vc.TransitionType {
	case models.StudioTransitionWipeLeft:
		return fmt.Sprintf("crop=w='max(2,iw*%s)':h=ih:x=0:y=0", p)
	case models.StudioTransitionWipeRight:
		return fmt.Sprintf("crop=w='max(2,iw*%s)':h=ih:x=iw-ow:y=0", p)
	case models.StudioTransitionWipeUp:
		return fmt.Sprintf("crop=w=iw:h='max(2,ih*%s)':x=0:y=0", p)
	case models.StudioTransitionWipeDown:
		return fmt.Sprintf("crop=w=iw:h='max(2,ih*%s)':x=0:y=ih-oh", p)
	}
	return ""
}

// overlayPosWithTransitions wraps overlayPos with the animated position terms:
//   - the clip's own entrance: pushes/slides translate it in from its edge
//     (offset W·(1−p) toward home), wipes anchor the shrinking crop to the
//     canvas edge it reveals from while the transition runs;
//   - the outgoing side: the NEXT clip's push translates this clip out by
//     W·p over the overlap window (slides leave it static by design).
//
// The full-canvas anchoring assumes the clip fits the frame (the editing case
// transitions exist for); letterboxed sources land within the same bounded
// tolerance row — see transition_* in lib/studio/parityTolerances.ts.
func overlayPosWithTransitions(vc StudioExportVideoSeg, width, height int) (string, string) {
	x, y := overlayPos(vc, width, height)
	if vc.TransitionDur > 0 {
		p := transitionProgressExpr(vc.TimelineStart, vc.TransitionDur)
		teStr := formatSeconds(vc.TimelineStart + vc.TransitionDur)
		switch vc.TransitionType {
		case models.StudioTransitionPushLeft, models.StudioTransitionSlideLeft:
			x = fmt.Sprintf("%s-W*(1-%s)", x, p)
		case models.StudioTransitionPushRight, models.StudioTransitionSlideRight:
			x = fmt.Sprintf("%s+W*(1-%s)", x, p)
		case models.StudioTransitionPushUp, models.StudioTransitionSlideUp:
			y = fmt.Sprintf("%s-H*(1-%s)", y, p)
		case models.StudioTransitionPushDown, models.StudioTransitionSlideDown:
			y = fmt.Sprintf("%s+H*(1-%s)", y, p)
		case models.StudioTransitionWipeLeft:
			x = fmt.Sprintf("if(lt(t,%s),0,%s)", teStr, x)
		case models.StudioTransitionWipeRight:
			x = fmt.Sprintf("if(lt(t,%s),W-w,%s)", teStr, x)
		case models.StudioTransitionWipeUp:
			y = fmt.Sprintf("if(lt(t,%s),0,%s)", teStr, y)
		case models.StudioTransitionWipeDown:
			y = fmt.Sprintf("if(lt(t,%s),H-h,%s)", teStr, y)
		}
	}
	if vc.TransOutDur > 0 {
		po := transitionProgressExpr(vc.TransOutStart, vc.TransOutDur)
		switch vc.TransOutType {
		case models.StudioTransitionPushLeft:
			x = fmt.Sprintf("%s+W*%s", x, po)
		case models.StudioTransitionPushRight:
			x = fmt.Sprintf("%s-W*%s", x, po)
		case models.StudioTransitionPushUp:
			y = fmt.Sprintf("%s+H*%s", y, po)
		case models.StudioTransitionPushDown:
			y = fmt.Sprintf("%s-H*%s", y, po)
		}
	}
	return quoteFilterExpr(x), quoteFilterExpr(y)
}

// dipStatements layers the dip color quad over the freshly composited clip:
// a full-canvas color source whose alpha fades in over the first half of the
// overlap and out over the second (peak 1.0 at the midpoint, where the
// entering clip's enable window begins).
func dipStatements(vc StudioExportVideoSeg, k, width, height int, fpsStr, durStr, inLabel, outLabel string) []string {
	col := "black"
	if vc.TransitionType == models.StudioTransitionDipToWhite {
		col = "white"
	}
	ts := vc.TimelineStart
	half := vc.TransitionDur / 2
	dip := fmt.Sprintf("dip%d", k)
	return []string{
		fmt.Sprintf(
			"color=c=%s:s=%dx%d:r=%s:d=%s,format=yuva420p,fade=t=in:st=%s:d=%s:alpha=1,fade=t=out:st=%s:d=%s:alpha=1[%s]",
			col, width, height, fpsStr, durStr,
			formatSeconds(ts), formatSeconds(half), formatSeconds(ts+half), formatSeconds(half), dip,
		),
		fmt.Sprintf(
			"[%s][%s]overlay=x=0:y=0:enable='between(t,%s,%s)':eof_action=pass[%s]",
			inLabel, dip, formatSeconds(ts), formatSeconds(ts+vc.TransitionDur), outLabel,
		),
	}
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

// --- title clips (part 16) ---------------------------------------------------
//
// titleLayerParts renders a title's layer stack as comma-chain filter parts on
// the transparent color source — array order = bottom → top, the same order
// the browser raster paints (SHARED TITLE LAYOUT SPEC in
// lib/studio/titleRaster.ts; change both or neither):
//   - text splits into lines on '\n' (no auto-wrap); one drawtext per line;
//   - the anchor is (x·W, y·H); line i centers at
//     anchorY − blockH/2 + (i+0.5)·lineH with lineH = lineHeight·fontSize;
//   - align hangs the line off the anchor X (left = starts at, center =
//     centered on, right = ends at) via x/text_w expressions;
//   - stroke = borderw/bordercolor, shadow = shadowx/y/color, background =
//     per-line box=1 with uniform boxborderw.
//
// KNOWN BOUNDED DIVERGENCES from the preview raster (title_* tolerance rows in
// lib/studio/parityTolerances.ts): letter-spacing (no drawtext knob), shadow
// blur (drawtext shadows are hard), background corner radius + distinct X/Y
// padding (drawbox/boxborderw limitations), and diagonal lines (staircase of
// drawbox segments vs an antialiased canvas stroke).

// titleStaircaseSegments bounds the drawbox staircase for diagonal lines.
const titleStaircaseSegments = 24

func titleLayerParts(title *models.StudioTitle, width, height int, fontsDir, fallbackFont string) []string {
	var parts []string
	for _, l := range title.Layers {
		switch l.Type {
		case models.StudioTitleLayerRect:
			parts = append(parts, titleRectPart(l, width, height))
		case models.StudioTitleLayerLine:
			parts = append(parts, titleLinePartsFor(l, width, height)...)
		case models.StudioTitleLayerText:
			parts = append(parts, titleTextParts(l, width, height, fontsDir, fallbackFont)...)
		}
	}
	return parts
}

// ffColorAlpha renders "#RRGGBB"+opacity as ffmpeg's 0xRRGGBB@a.aaa form.
func ffColorAlpha(hex string, alpha float64) string {
	return fmt.Sprintf("%s@%s", hexToFFColor(hex), formatGain(clamp01(alpha)))
}

func titleRectPart(l models.StudioTitleLayer, width, height int) string {
	x := int(math.Round(optF(l.X, 0) * float64(width)))
	y := int(math.Round(optF(l.Y, 0) * float64(height)))
	w := int(math.Round(optF(l.W, 0) * float64(width)))
	h := int(math.Round(optF(l.H, 0) * float64(height)))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	color := "#FFFFFF"
	if l.Color != nil {
		color = *l.Color
	}
	// replace=1 writes color+alpha instead of blending — the source is
	// transparent, so a plate must REPLACE the empty pixels. Corner radius is
	// dropped here (preview-only; bounded tolerance).
	return fmt.Sprintf("drawbox=x=%d:y=%d:w=%d:h=%d:color=%s:t=fill:replace=1",
		x, y, w, h, ffColorAlpha(color, optF(l.Opacity, 1)))
}

// titleLinePartsFor draws a line layer. Axis-aligned lines are one exact
// drawbox; diagonals approximate with a bounded staircase of segments.
func titleLinePartsFor(l models.StudioTitleLayer, width, height int) []string {
	x1 := optF(l.X1, 0) * float64(width)
	y1 := optF(l.Y1, 0) * float64(height)
	x2 := optF(l.X2, 1) * float64(width)
	y2 := optF(l.Y2, 1) * float64(height)
	th := optF(l.ThicknessPx, 4)
	color := "#FFFFFF"
	if l.Color != nil {
		color = *l.Color
	}
	col := ffColorAlpha(color, 1)
	box := func(cx, cy, w, h float64) string {
		bw := int(math.Max(1, math.Round(w)))
		bh := int(math.Max(1, math.Round(h)))
		return fmt.Sprintf("drawbox=x=%d:y=%d:w=%d:h=%d:color=%s:t=fill:replace=1",
			int(math.Round(cx-w/2)), int(math.Round(cy-h/2)), bw, bh, col)
	}
	dx := x2 - x1
	dy := y2 - y1
	if math.Abs(dy) < 0.5 { // horizontal
		return []string{box((x1+x2)/2, y1, math.Abs(dx), th)}
	}
	if math.Abs(dx) < 0.5 { // vertical
		return []string{box(x1, (y1+y2)/2, th, math.Abs(dy))}
	}
	out := make([]string, 0, titleStaircaseSegments)
	for i := 0; i < titleStaircaseSegments; i++ {
		f := (float64(i) + 0.5) / titleStaircaseSegments
		segW := math.Max(math.Abs(dx)/titleStaircaseSegments, th)
		segH := math.Max(math.Abs(dy)/titleStaircaseSegments, th)
		out = append(out, box(x1+dx*f, y1+dy*f, segW, segH))
	}
	return out
}

func titleTextParts(l models.StudioTitleLayer, width, height int, fontsDir, fallbackFont string) []string {
	text := ""
	if l.Text != nil {
		text = *l.Text
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	family := models.StudioTitleFontDefault
	if l.FontFamily != nil {
		family = *l.FontFamily
	}
	weight := 400
	if l.FontWeight != nil {
		weight = *l.FontWeight
	}
	fontPath := resolveTitleFontFile(fontsDir, family, weight, fallbackFont)
	fontSize := optF(l.FontSizePx, 64)
	fill := "#FFFFFF"
	if l.FillColor != nil {
		fill = *l.FillColor
	}
	align := "center"
	if l.Align != nil {
		align = *l.Align
	}
	anchorX := optF(l.X, 0.5) * float64(width)
	anchorY := optF(l.Y, 0.5) * float64(height)
	lineH := optF(l.LineHeight, 1.2) * fontSize
	lines := strings.Split(text, "\n")
	blockH := float64(len(lines)) * lineH

	styled := []string{
		fmt.Sprintf("fontfile='%s'", drawtextEscape(fontPath)),
		"", // text placeholder — filled per line below (kept second for readability)
		fmt.Sprintf("fontsize=%d", int(math.Round(fontSize))),
		fmt.Sprintf("fontcolor=%s", hexToFFColor(fill)),
	}
	if l.StrokeColor != nil && optF(l.StrokeWidthPx, 0) > 0 {
		styled = append(styled,
			fmt.Sprintf("borderw=%d", int(math.Round(optF(l.StrokeWidthPx, 0)))),
			fmt.Sprintf("bordercolor=%s", hexToFFColor(*l.StrokeColor)),
		)
	}
	if l.Shadow != nil {
		// Blur has no drawtext equivalent — offset + color only (tolerance).
		styled = append(styled,
			fmt.Sprintf("shadowx=%d", int(math.Round(l.Shadow.XPx))),
			fmt.Sprintf("shadowy=%d", int(math.Round(l.Shadow.YPx))),
			fmt.Sprintf("shadowcolor=%s", hexToFFColor(l.Shadow.Color)),
		)
	}
	if l.Background != nil && l.Background.Opacity > 0 {
		// One uniform boxborderw (older-ffmpeg-safe): the mean of the X/Y
		// paddings; radius is dropped (both bounded tolerances).
		pad := int(math.Round((l.Background.PaddingXPx + l.Background.PaddingYPx) / 2))
		styled = append(styled,
			"box=1",
			fmt.Sprintf("boxcolor=%s", ffColorAlpha(l.Background.Color, l.Background.Opacity)),
			fmt.Sprintf("boxborderw=%d", pad),
		)
	}

	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lineCenterY := anchorY - blockH/2 + (float64(i)+0.5)*lineH
		var xExpr string
		switch align {
		case "left":
			xExpr = formatGain(anchorX)
		case "right":
			xExpr = fmt.Sprintf("%s-text_w", formatGain(anchorX))
		default:
			xExpr = fmt.Sprintf("%s-text_w/2", formatGain(anchorX))
		}
		opts := append([]string(nil), styled...)
		opts[1] = fmt.Sprintf("text='%s'", drawtextEscape(line))
		opts = append(opts,
			fmt.Sprintf("x=%s", xExpr),
			fmt.Sprintf("y=%s-text_h/2", formatGain(lineCenterY)),
		)
		out = append(out, "drawtext="+strings.Join(opts, ":"))
	}
	return out
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
