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
// muted to drop a track's audio). FadeIn is the clip's own cross-dissolve;
// FadeOut is the next clip-on-track's dissolve into it (for audio crossfade).
type StudioClipRef struct {
	Clip       models.StudioClip
	Asset      *models.StudioAsset
	TrackID    string
	TrackKind  models.StudioTrackKind
	TrackIndex int
	TrackMuted bool
	FadeIn     float64
	FadeOut    float64
}

// CollectExportRefs flattens the EDL into render refs and the timeline length.
// A ref is kept only if it produces output: every video clip (mute affects only
// audio), plus audio-bearing clips on non-muted tracks with volume > 0.
// Per-track clips are walked in timeline order so transition fades can be paired
// with their neighbours. Duration spans all clips so the export length matches
// the timeline.
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
			asset, ok := assets[clip.AssetID]
			if !ok {
				continue
			}
			volume := 1.0
			if clip.Volume != nil {
				volume = *clip.Volume
			}
			contributesAudio := !track.Muted && asset.HasAudio && (volume > 0 || len(clip.VolumeKeyframes) > 0)
			if track.Kind != models.StudioTrackKindVideo && !contributesAudio {
				continue // muted/silent audio clip — nothing to render
			}

			fadeIn := clampFade(transitionOf(clip), dur)
			fadeOut := 0.0
			if i+1 < len(ordered) {
				fadeOut = clampFade(transitionOf(ordered[i+1]), dur)
			}

			refs = append(refs, StudioClipRef{
				Clip:       clip,
				Asset:      asset,
				TrackID:    track.ID,
				TrackKind:  track.Kind,
				TrackIndex: track.Index,
				TrackMuted: track.Muted,
				FadeIn:     fadeIn,
				FadeOut:    fadeOut,
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
		idx := len(plan.Inputs)
		plan.Inputs = append(plan.Inputs, spec.LocalByKey[r.Asset.S3KeyOriginal])
		opacity := 1.0
		if r.Clip.Opacity != nil {
			opacity = *r.Clip.Opacity
		}
		volume := 1.0
		if r.Clip.Volume != nil {
			volume = *r.Clip.Volume
		}
		if r.TrackKind == models.StudioTrackKindVideo {
			plan.Video = append(plan.Video, StudioExportVideoSeg{
				InputIndex: idx, SourceIn: r.Clip.SourceIn, SourceOut: r.Clip.SourceOut,
				TimelineStart: r.Clip.TimelineStart, Opacity: opacity, TrackIndex: r.TrackIndex,
				FadeIn: r.FadeIn, Adjustments: r.Clip.Adjustments, TextOverlays: r.Clip.TextOverlays,
				Transform: r.Clip.Transform, Crop: r.Clip.Crop, BlendMode: r.Clip.BlendMode,
				Effects: r.Clip.Effects, LutPaths: spec.LutPaths,
			})
		}
		if !r.TrackMuted && r.Asset.HasAudio && (volume > 0 || len(r.Clip.VolumeKeyframes) > 0) {
			pan := 0.0
			if r.Clip.Pan != nil {
				pan = *r.Clip.Pan
			}
			plan.Audio = append(plan.Audio, StudioExportAudioSeg{
				InputIndex: idx, SourceIn: r.Clip.SourceIn, SourceOut: r.Clip.SourceOut,
				TimelineStart: r.Clip.TimelineStart, Volume: volume,
				FadeIn: r.FadeIn, FadeOut: r.FadeOut,
				VolumeKeyframes: r.Clip.VolumeKeyframes, Pan: pan,
				Voice: spec.Ducking != nil && r.TrackID == spec.VoiceTrackID,
			})
		}
	}
	return plan
}

func transitionOf(c models.StudioClip) float64 {
	if c.TransitionInSeconds != nil && *c.TransitionInSeconds > 0 {
		return *c.TransitionInSeconds
	}
	return 0
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
