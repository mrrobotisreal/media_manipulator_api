package services

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// QualityProfile mirrors the CreaTV ladder rung definition: a height plus the
// codec-independent bitrate targets we plug into FFmpeg for any protocol.
type QualityProfile struct {
	Label            string
	Height           int
	VideoBitrateKbps int
	AudioBitrateKbps int
	CRF              int
	Preset           string
}

// ladderProfiles is the full canonical adaptive ladder. The Media Manipulator
// product currently only sells the 360p/480p/720p subset to free users — the
// premium rungs (144p/240p/1080p/2160p) are returned by buildQualityRungs as
// disabled+premium so the UI can render them grayed-out behind a tooltip.
func ladderProfiles() []QualityProfile {
	return []QualityProfile{
		{Label: "144p", Height: 144, VideoBitrateKbps: 200, AudioBitrateKbps: 64, CRF: 26, Preset: "veryfast"},
		{Label: "240p", Height: 240, VideoBitrateKbps: 400, AudioBitrateKbps: 64, CRF: 25, Preset: "veryfast"},
		{Label: "360p", Height: 360, VideoBitrateKbps: 800, AudioBitrateKbps: 96, CRF: 23, Preset: "veryfast"},
		{Label: "480p", Height: 480, VideoBitrateKbps: 1200, AudioBitrateKbps: 128, CRF: 23, Preset: "veryfast"},
		{Label: "720p", Height: 720, VideoBitrateKbps: 2800, AudioBitrateKbps: 128, CRF: 22, Preset: "veryfast"},
		{Label: "1080p", Height: 1080, VideoBitrateKbps: 5000, AudioBitrateKbps: 192, CRF: 22, Preset: "veryfast"},
		{Label: "2160p", Height: 2160, VideoBitrateKbps: 12000, AudioBitrateKbps: 256, CRF: 21, Preset: "slow"},
	}
}

// freeRungLabels lists the rungs available to free users. Changing this set is
// the single place to flip a rung from premium to free (or back).
var freeRungLabels = map[string]bool{
	"360p": true,
	"480p": true,
	"720p": true,
}

const (
	premiumTooltip       = "Sign up for a Premium account to get full transcode access! (Premium sign-up Coming Soon!)"
	sourceTooSmallTip    = "The source video is not high enough resolution for this output rung."
	captionsNoAudioTip   = "Captions require an audio track. This video does not appear to have audio."
	freeMinHeightTooltip = "This video is below the free transcode minimum of 360p. Premium support for ultra-low-quality sources is coming soon. Please load a video that is 360p or higher."
)

// ProfileByLabel finds the canonical ladder rung for a given label.
// Returns ok=false for unknown labels — callers must reject the request.
func ProfileByLabel(label string) (QualityProfile, bool) {
	label = strings.ToLower(strings.TrimSpace(label))
	for _, p := range ladderProfiles() {
		if p.Label == label {
			return p, true
		}
	}
	return QualityProfile{}, false
}

// classifyMaxQuality returns the rung label that best describes the largest
// rendition this source can produce without upscaling. Used purely for UI copy.
func classifyMaxQuality(height int) string {
	if height <= 0 {
		return "unknown"
	}
	best := ""
	for _, p := range ladderProfiles() {
		if p.Height <= height {
			best = p.Label
		}
	}
	if best == "" {
		// Source is smaller than the smallest ladder rung — call it sub-144p.
		return "sub-144p"
	}
	return best
}

// parseFractionFrameRate handles the "30000/1001", "30/1", "29.97", "" formats
// FFprobe returns. Returns 0 on anything we can't decode so callers can fall
// back to whatever they treat as default.
func parseFractionFrameRate(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0/0" {
		return 0
	}
	if strings.Contains(raw, "/") {
		parts := strings.SplitN(raw, "/", 2)
		num, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		den, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 != nil || err2 != nil || den == 0 {
			return 0
		}
		return num / den
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return value
}

// buildQualityRungs returns the full rendered rung list for the UI. Rungs that
// cannot be used are still returned but marked Enabled=false with a tooltip
// reason — that's what lets the UI render greyed-out chips with hover help.
//
// isPremium=false (the only path today) means the free-tier rules apply:
//   - 360p, 480p, 720p are selectable if the source is tall enough;
//   - 144p/240p/1080p/2160p are disabled "premium coming soon";
//   - sources < 360p mark every rung as sourceTooSmall.
func buildQualityRungs(sourceHeight int, isPremium bool) []models.TranscodeQualityRung {
	rungs := make([]models.TranscodeQualityRung, 0, 7)
	defaultSelected := map[string]bool{"360p": true, "480p": true, "720p": true}
	for _, p := range ladderProfiles() {
		rung := models.TranscodeQualityRung{
			Label:            p.Label,
			Height:           p.Height,
			BitrateKbps:      p.VideoBitrateKbps,
			AudioBitrateKbps: p.AudioBitrateKbps,
		}
		isFreeRung := freeRungLabels[p.Label]
		canFit := sourceHeight >= p.Height

		switch {
		case !canFit:
			rung.Enabled = false
			rung.SourceTooSmall = true
			rung.DisabledReason = sourceTooSmallTip
		case !isFreeRung && !isPremium:
			rung.Enabled = false
			rung.PremiumOnly = true
			rung.DisabledReason = premiumTooltip
		default:
			rung.Enabled = true
			if defaultSelected[p.Label] {
				rung.Selected = true
			}
		}
		rungs = append(rungs, rung)
	}
	return rungs
}

// splitRungs returns (enabled, disabled) so the UI can render two clearly
// labeled groups without doing the filtering on the client.
func splitRungs(all []models.TranscodeQualityRung) ([]models.TranscodeQualityRung, []models.TranscodeQualityRung) {
	enabled := make([]models.TranscodeQualityRung, 0, len(all))
	disabled := make([]models.TranscodeQualityRung, 0, len(all))
	for _, r := range all {
		if r.Enabled {
			enabled = append(enabled, r)
		} else {
			disabled = append(disabled, r)
		}
	}
	return enabled, disabled
}

// ValidateSelectedRungs enforces the same rules as buildQualityRungs but at
// job-start time. The UI is untrusted: a client could send a 1080p rung even
// when we never offered it. This function rejects that.
//
// Returns a normalized, sorted, unique list of profiles to actually transcode.
func ValidateSelectedRungs(sourceHeight int, selected []string, isPremium bool) ([]QualityProfile, error) {
	if sourceHeight < 360 && !isPremium {
		return nil, fmt.Errorf("%s", freeMinHeightTooltip)
	}
	seen := map[string]bool{}
	out := make([]QualityProfile, 0, len(selected))
	for _, raw := range selected {
		label := strings.ToLower(strings.TrimSpace(raw))
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		profile, ok := ProfileByLabel(label)
		if !ok {
			return nil, fmt.Errorf("unknown quality rung %q", raw)
		}
		if sourceHeight < profile.Height {
			return nil, fmt.Errorf("source video is too small for %s (source=%dp)", profile.Label, sourceHeight)
		}
		if !isPremium && !freeRungLabels[profile.Label] {
			return nil, fmt.Errorf("%s requires a premium account", profile.Label)
		}
		out = append(out, profile)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one quality rung must be selected")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Height < out[j].Height })
	return out, nil
}

// computeVariantWidth scales the source dimensions to the requested height
// preserving aspect ratio and forcing an even width (ffmpeg yuv420p
// constraint). Returns 0 if the source dimensions are unusable.
func computeVariantWidth(sourceWidth, sourceHeight, targetHeight int) int {
	if sourceWidth <= 0 || sourceHeight <= 0 || targetHeight <= 0 {
		return 0
	}
	scaled := float64(sourceWidth) * float64(targetHeight) / float64(sourceHeight)
	w := int(math.Round(scaled))
	if w%2 != 0 {
		w++
	}
	return w
}

// keyframeIntervalFrames returns the GOP size for a given output fps and
// segment duration so that segment boundaries always coincide with keyframes.
func keyframeIntervalFrames(outputFPS float64, segmentSeconds int) int {
	if segmentSeconds <= 0 {
		segmentSeconds = 2
	}
	if math.IsNaN(outputFPS) || math.IsInf(outputFPS, 0) || outputFPS <= 0 {
		outputFPS = 30
	}
	frames := int(math.Round(outputFPS * float64(segmentSeconds)))
	if frames < 1 {
		return 1
	}
	return frames
}

// formatFrameRate gives us a compact decimal representation of an fps value
// safe to drop into FFmpeg's `-r` flag (e.g. 23.976 stays unrounded).
func formatFrameRate(value float64) string {
	if value <= 0 {
		return "30"
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", value), "0"), ".")
}

// forceKeyFramesExpr is the standard "force a keyframe every N seconds" expr.
func forceKeyFramesExpr(segmentSeconds int) string {
	if segmentSeconds <= 0 {
		segmentSeconds = 2
	}
	return fmt.Sprintf("expr:gte(t,n_forced*%d)", segmentSeconds)
}
