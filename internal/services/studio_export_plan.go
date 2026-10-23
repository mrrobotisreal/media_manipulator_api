package services

import (
	"sort"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// EDL flattening + plan assembly, extracted from the export handler (part 13)
// so the parity-render CLI (cmd/parity-render) drives the EXACT plan-building
// path the real export uses — the handler and the harness share this seam
// rather than duplicating the compiler.

// StudioClipRef pairs a clip with its resolved asset and the track context the
// exporter needs (kind for video-vs-audio routing, index for overlay order,
// muted to drop a track's audio).
//
// Audio: FadeIn/FadeOut are the equal-power crossfade windows, derived (part
// 14) from the ACTUAL same-track overlaps — whenever two audio-bearing clips
// overlap on one track the crossfade is automatic, typed transition or not.
// Video: TransIn* is the clip's typed entrance transition (legacy
// transitionInSeconds upgraded to crossDissolve); TransOut* is the NEXT
// clip-on-track's transition into this clip's tail (needed for pushes, which
// move the outgoing clip too).
type StudioClipRef struct {
	Clip       models.StudioClip
	Asset      *models.StudioAsset
	TrackID    string
	TrackKind  models.StudioTrackKind
	TrackIndex int
	TrackMuted bool
	FadeIn     float64
	FadeOut    float64

	TransInType   string
	TransInDur    float64
	TransOutType  string
	TransOutStart float64
	TransOutDur   float64
}

// resolveTransition returns the clip's typed entrance transition as
// (type, duration): the typed field wins, a legacy transitionInSeconds > 0
// upgrades to crossDissolve (mirrors the sanitizer + the TS clipTransitionOf).
func resolveTransition(c models.StudioClip) (string, float64) {
	if c.Transition != nil && c.Transition.DurationSeconds > 0 {
		return c.Transition.Type, c.Transition.DurationSeconds
	}
	if c.TransitionInSeconds != nil && *c.TransitionInSeconds > 0 {
		return models.StudioTransitionCrossDissolve, *c.TransitionInSeconds
	}
	return "", 0
}

// CollectExportRefs flattens the EDL into render refs and the timeline length.
// A ref is kept only if it produces output: every video clip (mute affects only
// audio), plus audio-bearing clips on non-muted tracks with volume > 0.
// Per-track clips are walked in timeline order so transitions and crossfades
// can be paired with their neighbours. Duration spans all clips so the export
// length matches the timeline.
func CollectExportRefs(p *models.StudioProject, assets map[string]*models.StudioAsset) ([]StudioClipRef, float64, bool) {
	refs := make([]StudioClipRef, 0)
	var duration float64
	for _, track := range p.Tracks {
		ordered := append([]models.StudioClip(nil), track.Clips...)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].TimelineStart < ordered[j].TimelineStart })

		for i, clip := range ordered {
			dur := clipEffectiveDur(clip)
			if end := clip.TimelineStart + dur; end > duration {
				duration = end
			}
			// Title clips (part 16) are assetless (AssetID "" XOR Title,
			// sanitizer-enforced): they render on video tracks from their layer
			// stack. A synthetic inert asset keeps every downstream r.Asset
			// deref safe (HasAudio false, no S3 key → no download, no input).
			asset, ok := assets[clip.AssetID]
			if !ok {
				if clip.Title == nil || track.Kind != models.StudioTrackKindVideo {
					continue
				}
				asset = &models.StudioAsset{ID: clip.ID, MediaKind: models.StudioMediaKindVideo}
			}
			volume := 1.0
			if clip.Volume != nil {
				volume = *clip.Volume
			}
			contributesAudio := !track.Muted && asset.HasAudio &&
				(volume > 0 || len(clip.VolumeKeyframes) > 0 || clipHasVolumeLane(clip))
			if track.Kind != models.StudioTrackKindVideo && !contributesAudio {
				continue // muted/silent audio clip — nothing to render
			}

			// Audio crossfades follow the real overlaps (clamped to both
			// clips' durations), mirroring the preview's prev/nextClipOverlap.
			fadeIn := 0.0
			if i > 0 {
				prev := ordered[i-1]
				overlap := prev.TimelineStart + clipEffectiveDur(prev) - clip.TimelineStart
				fadeIn = clampFade(clampFade(overlap, clipEffectiveDur(prev)), dur)
			}
			fadeOut := 0.0
			transOutType := ""
			transOutStart := 0.0
			transOutDur := 0.0
			if i+1 < len(ordered) {
				next := ordered[i+1]
				overlap := clip.TimelineStart + dur - next.TimelineStart
				fadeOut = clampFade(clampFade(overlap, clipEffectiveDur(next)), dur)
				if ty, d := resolveTransition(next); ty != "" {
					transOutType = ty
					transOutStart = next.TimelineStart
					transOutDur = clampFade(d, clipEffectiveDur(next))
				}
			}
			transInType, transInDur := resolveTransition(clip)
			transInDur = clampFade(transInDur, dur)

			refs = append(refs, StudioClipRef{
				Clip:          clip,
				Asset:         asset,
				TrackID:       track.ID,
				TrackKind:     track.Kind,
				TrackIndex:    track.Index,
				TrackMuted:    track.Muted,
				FadeIn:        fadeIn,
				FadeOut:       fadeOut,
				TransInType:   transInType,
				TransInDur:    transInDur,
				TransOutType:  transOutType,
				TransOutStart: transOutStart,
				TransOutDur:   transOutDur,
			})
		}
	}
	if len(refs) == 0 || duration <= 0 {
		return nil, 0, false
	}
	return refs, duration, true
}

// StudioPlanSpec carries everything AssembleExportPlan needs beyond the refs:
// the project geometry, the resolved project-level audio config, and the media
// localization maps built by the caller (S3 downloads for the real export;
// fixture paths for the parity CLI).
type StudioPlanSpec struct {
	Width    int
	Height   int
	FPS      float64
	Duration float64
	Ducking  *StudioDucking
	// VoiceTrackID marks the ducking sidechain-key track (with Ducking set).
	VoiceTrackID string
	Loudness     string
	// LocalByKey maps an asset's S3KeyOriginal to the local media path used as
	// its ffmpeg input.
	LocalByKey map[string]string
	// LutPaths maps a lut effect's lutAssetId to a local .cube path. A missing
	// entry skips that LUT.
	LutPaths map[string]string
}

// AssembleExportPlan builds the render-ready plan from refs: one ffmpeg input
// per ref (shared assets repeat the same local path). Video clips composite;
// audio-bearing clips on non-muted tracks mix in.
func AssembleExportPlan(refs []StudioClipRef, spec StudioPlanSpec) StudioExportPlan {
	plan := StudioExportPlan{
		Width: spec.Width, Height: spec.Height, FPS: spec.FPS, Duration: spec.Duration,
		Ducking: spec.Ducking, Loudness: spec.Loudness,
	}
	for _, r := range refs {
		// Title clips consume no ffmpeg input — their chain starts from a
		// color= source (buildVideoClipChain); InputIndex -1 marks that.
		idx := -1
		sourceKind := ""
		if r.Clip.Title != nil {
			sourceKind = StudioSegKindTitle
		} else {
			in := ExportInput{Path: spec.LocalByKey[r.Asset.S3KeyOriginal]}
			if r.Asset.MediaKind == models.StudioMediaKindImage {
				// Still image: loop the single frame at the project rate for
				// exactly the hold length (+1 frame for fps rounding). -t bounds
				// the demuxer so a very long hold is not decoded unboundedly.
				// Every image clip owns its input (hold lengths differ even for
				// a shared asset). The ORIGINAL file is used (design §6); ffmpeg
				// autorotates EXIF orientation on stills by default (6+/7), which
				// the parity photo.jpg fixture covers.
				sourceKind = StudioSegKindImage
				in.Pre = imageInputPre(spec.FPS, r.Clip.SourceOut-r.Clip.SourceIn)
			}
			idx = len(plan.Inputs)
			plan.Inputs = append(plan.Inputs, in)
		}
		opacity := 1.0
		if r.Clip.Opacity != nil {
			opacity = *r.Clip.Opacity
		}
		volume := 1.0
		if r.Clip.Volume != nil {
			volume = *r.Clip.Volume
		}
		if r.TrackKind == models.StudioTrackKindVideo {
			seg := StudioExportVideoSeg{
				InputIndex: idx, SourceIn: r.Clip.SourceIn, SourceOut: r.Clip.SourceOut,
				TimelineStart: r.Clip.TimelineStart, Opacity: opacity, TrackIndex: r.TrackIndex,
				Adjustments: r.Clip.Adjustments, TextOverlays: r.Clip.TextOverlays,
				Transform: r.Clip.Transform, Crop: r.Clip.Crop, BlendMode: r.Clip.BlendMode,
				Effects: r.Clip.Effects, LutPaths: spec.LutPaths,
				ClipKeyframes: r.Clip.Keyframes,
				Title:         r.Clip.Title,
				SourceKind:    sourceKind,
			}
			// Typed entrance transition (part 14): crossDissolve keeps the
			// legacy linear-alpha FadeIn path (byte-stable v1/v2 graphs); every
			// other type is carried by TransitionType/Dur and emitted by the
			// compiler's per-type expressions. TransOut* moves this clip only
			// for pushes, but is carried whenever the next clip transitions.
			switch r.TransInType {
			case "":
				// no transition
			case models.StudioTransitionCrossDissolve:
				seg.FadeIn = r.TransInDur
			default:
				seg.TransitionType = r.TransInType
				seg.TransitionDur = r.TransInDur
			}
			seg.TransOutType = r.TransOutType
			seg.TransOutStart = r.TransOutStart
			seg.TransOutDur = r.TransOutDur
			plan.Video = append(plan.Video, seg)
		}
		if !r.TrackMuted && r.Asset.HasAudio &&
			(volume > 0 || len(r.Clip.VolumeKeyframes) > 0 || clipHasVolumeLane(r.Clip)) {
			pan := 0.0
			if r.Clip.Pan != nil {
				pan = *r.Clip.Pan
			}
			plan.Audio = append(plan.Audio, StudioExportAudioSeg{
				InputIndex: idx, SourceIn: r.Clip.SourceIn, SourceOut: r.Clip.SourceOut,
				TimelineStart: r.Clip.TimelineStart, Volume: volume,
				FadeIn: r.FadeIn, FadeOut: r.FadeOut,
				VolumeKeyframes: r.Clip.VolumeKeyframes, Pan: pan,
				Voice:         spec.Ducking != nil && r.TrackID == spec.VoiceTrackID,
				ClipKeyframes: r.Clip.Keyframes,
				SegIndex:      len(plan.Audio),
			})
		}
	}
	return plan
}

// clipHasVolumeLane reports a non-empty v3 keyframes.volume lane (part 15) —
// it makes a clip audible even at flat volume 0, like legacy VolumeKeyframes.
func clipHasVolumeLane(c models.StudioClip) bool {
	return c.Keyframes != nil && len(c.Keyframes.Volume) > 0
}

// clampFade keeps a transition no longer than the clip it applies to.
func clampFade(d, clipDur float64) float64 {
	if d <= 0 {
		return 0
	}
	if d > clipDur {
		return clipDur
	}
	return d
}

func clipEffectiveDur(c models.StudioClip) float64 {
	if d := c.SourceOut - c.SourceIn; d > 0 {
		return d
	}
	return 0
}

// imageInputPre is the per-input option block for a looped still image:
// -loop 1 -framerate <fps> -t <hold + 1/fps>.
func imageInputPre(fps, hold float64) []string {
	fpsStr := formatFrameRate(fps)
	if fpsStr == "" {
		fpsStr = "30"
	}
	frame := 1.0 / 30.0
	if fps > 0 {
		frame = 1.0 / fps
	}
	if hold < 0 {
		hold = 0
	}
	return []string{"-loop", "1", "-framerate", fpsStr, "-t", formatSeconds(hold + frame)}
}
