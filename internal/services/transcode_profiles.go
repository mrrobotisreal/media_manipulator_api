package services

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
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

// ladderProfiles is the full canonical adaptive ladder. Which rungs a given
// caller may actually select is decided by LadderAccess, from the tier matrix;
// the rest are returned by buildQualityRungs as disabled, carrying the name of
// the tier that would unlock them, so the UI can render them greyed-out behind
// an honest tooltip.
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

// ladderFloorHeight is the shortest rung a non-Premium caller may select.
// Sub-360p output is a specialist need (bandwidth-constrained distribution),
// not a cheaper one, so it sits with Premium rather than at the bottom of the
// free ladder.
const ladderFloorHeight = 360

// LadderAccess is the slice of the transcode ladder one caller may use, derived
// from their tier. It is the only thing buildQualityRungs and
// ValidateSelectedRungs consult, so the three-tier rules live in exactly one
// place — and the tall end of each tier comes from tiers.Limits.MaxLadderHeight
// (i.e. from configuration) rather than from a literal repeated here.
//
// Per the published matrix: anonymous ≤ 720p, free ≤ 1080p, premium the full
// 144p–2160p ladder.
type LadderAccess struct {
	// Tier is the caller's tier, used only to word the disabled tooltips.
	Tier tiers.Tier
	// MinHeight and MaxHeight bound the selectable rungs, inclusive.
	MinHeight int
	MaxHeight int
	// unlockedBy names, for each rung this caller cannot select, the cheapest
	// tier that could. Empty for rungs the caller can already use, and for
	// rungs no tier offers.
	unlockedBy map[string]tiers.Tier
}

// LadderAccessFor builds the access rules for one tier by asking the registry
// what every tier is allowed, so the "which tier unlocks this rung" copy stays
// correct automatically if the matrix is retuned via TIER_* env vars.
func LadderAccessFor(tier tiers.Tier, registry *tiers.Registry) LadderAccess {
	if registry == nil {
		return AnonymousLadderAccess()
	}
	access := bounds(tier, registry.For(tier))
	access.unlockedBy = map[string]tiers.Tier{}
	for _, p := range ladderProfiles() {
		if access.allows(p.Height) {
			continue
		}
		// tiers.All() is in ascending order of capability, so the first match
		// is the cheapest tier that would unlock this rung.
		for _, candidate := range tiers.All() {
			if bounds(candidate, registry.For(candidate)).allows(p.Height) {
				access.unlockedBy[p.Label] = candidate
				break
			}
		}
	}
	return access
}

// AnonymousLadderAccess is the safe default for any path that has not resolved
// a caller — the most restrictive rules, never the most permissive.
func AnonymousLadderAccess() LadderAccess {
	return LadderAccess{
		Tier:      tiers.TierAnonymous,
		MinHeight: ladderFloorHeight,
		MaxHeight: 720,
		unlockedBy: map[string]tiers.Tier{
			"144p":  tiers.TierPremium,
			"240p":  tiers.TierPremium,
			"1080p": tiers.TierFree,
			"2160p": tiers.TierPremium,
		},
	}
}

func bounds(tier tiers.Tier, limits tiers.Limits) LadderAccess {
	min := ladderFloorHeight
	// Operator inherits Premium's limits, so it gets Premium's floor too.
	if tier == tiers.TierPremium || tier == tiers.TierOperator {
		min = 0
	}
	return LadderAccess{Tier: tier, MinHeight: min, MaxHeight: limits.MaxLadderHeight}
}

func (a LadderAccess) allows(height int) bool {
	return height >= a.MinHeight && height <= a.MaxHeight
}

// tierLabel is the name the tooltips use for a tier.
func tierLabel(t tiers.Tier) string {
	switch t {
	case tiers.TierFree:
		return "free account"
	case tiers.TierPremium:
		return "Premium"
	default:
		return "a higher plan"
	}
}

// lockedReason words the disabled tooltip for a rung this caller cannot select.
// §4.9's target is the honest short form — "1080p — free account", "2160p —
// Premium" — not a sales pitch on every chip.
func (a LadderAccess) lockedReason(label string) string {
	if unlock, ok := a.unlockedBy[label]; ok {
		if unlock == tiers.TierPremium {
			return fmt.Sprintf("%s — Premium", label)
		}
		return fmt.Sprintf("%s — %s", label, tierLabel(unlock))
	}
	return fmt.Sprintf("%s is not available on this plan", label)
}

// minHeightMessage explains a source that is shorter than the caller's floor.
func (a LadderAccess) minHeightMessage() string {
	if a.MinHeight <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"This video is below the %dp transcode minimum for your plan. Premium supports ultra-low-quality sources; otherwise please load a video that is %dp or higher.",
		a.MinHeight, a.MinHeight,
	)
}

const (
	sourceTooSmallTip  = "The source video is not high enough resolution for this output rung."
	captionsNoAudioTip = "Captions require an audio track. This video does not appear to have audio."
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
// The rules, in order of precedence:
//   - a rung taller than the source is sourceTooSmall, whatever the tier;
//   - a rung outside the caller's LadderAccess is disabled and names the tier
//     that would unlock it;
//   - everything else is selectable, with 360p/480p/720p pre-selected where the
//     caller can actually use them.
func buildQualityRungs(sourceHeight int, access LadderAccess) []models.TranscodeQualityRung {
	rungs := make([]models.TranscodeQualityRung, 0, 7)
	defaultSelected := map[string]bool{"360p": true, "480p": true, "720p": true}
	for _, p := range ladderProfiles() {
		rung := models.TranscodeQualityRung{
			Label:            p.Label,
			Height:           p.Height,
			BitrateKbps:      p.VideoBitrateKbps,
			AudioBitrateKbps: p.AudioBitrateKbps,
		}
		canFit := sourceHeight >= p.Height

		switch {
		case !canFit:
			rung.Enabled = false
			rung.SourceTooSmall = true
			rung.DisabledReason = sourceTooSmallTip
		case !access.allows(p.Height):
			rung.Enabled = false
			// PremiumOnly is the flag the UI styles the "locked" chip from. It
			// stays true for anything the caller cannot reach, including rungs
			// a free account would unlock — the chip is identical, only the
			// tooltip differs.
			rung.PremiumOnly = true
			rung.DisabledReason = access.lockedReason(p.Label)
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
func ValidateSelectedRungs(sourceHeight int, selected []string, access LadderAccess) ([]QualityProfile, error) {
	if access.MinHeight > 0 && sourceHeight < access.MinHeight {
		return nil, fmt.Errorf("%s", access.minHeightMessage())
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
		if !access.allows(profile.Height) {
			return nil, fmt.Errorf("%s requires %s", profile.Label, tierLabel(access.unlockedBy[profile.Label]))
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
