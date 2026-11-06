package handlers

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// AssistantExports is Medi's assistant.ExportAdvisor (MM118): it checks a
// studio suggest_action export's params against the output table and the
// caller's plan, the same two rules the export route enforces, before the
// action streams. Pure and read-only: it never starts or charges anything.
type AssistantExports struct {
	Registry *tiers.Registry
}

// assistantExportParams are the only params an export suggestion may carry.
var assistantExportParams = map[string]bool{
	"preset": true, "container": true, "videoCodec": true, "audioCodec": true,
	"audioBitrate": true, "audioChannels": true, "resolution": true, "captionsMode": true,
}

// AdviseExport implements assistant.ExportAdvisor. An invalid field, value or
// combination is an error for the model; an output above the plan is replaced
// by the nearest preset the plan allows, with upgrade {required, option}.
func (a *AssistantExports) AdviseExport(tier string, params map[string]any) (map[string]any, error) {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !assistantExportParams[k] {
			return nil, fmt.Errorf("export has no param %q (use preset, container, videoCodec, audioCodec, audioBitrate, audioChannels, resolution, captionsMode)", k)
		}
	}
	str := func(k string) (string, error) {
		v, ok := params[k]
		if !ok || v == nil {
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("export param %s must be a string", k)
		}
		return strings.ToLower(strings.TrimSpace(s)), nil
	}
	num := func(k string) (int, error) {
		v, ok := params[k]
		if !ok || v == nil {
			return 0, nil
		}
		f, ok := v.(float64)
		if !ok {
			if i, isInt := v.(int); isInt {
				return i, nil
			}
			return 0, fmt.Errorf("export param %s must be a number", k)
		}
		if f != float64(int(f)) {
			return 0, fmt.Errorf("export param %s must be a whole number", k)
		}
		return int(f), nil
	}
	var req models.StudioExportRequest
	var err error
	if req.OutputPreset, err = str("preset"); err != nil {
		return nil, err
	}
	if req.Container, err = str("container"); err != nil {
		return nil, err
	}
	if req.VideoCodec, err = str("videoCodec"); err != nil {
		return nil, err
	}
	if req.AudioCodec, err = str("audioCodec"); err != nil {
		return nil, err
	}
	if req.CaptionsMode, err = str("captionsMode"); err != nil {
		return nil, err
	}
	if req.AudioBitrate, err = num("audioBitrate"); err != nil {
		return nil, err
	}
	if req.AudioChannels, err = num("audioChannels"); err != nil {
		return nil, err
	}
	if req.Resolution, err = num("resolution"); err != nil {
		return nil, err
	}
	out, msg := validateExportRequest(&req)
	if msg != "" {
		return nil, fmt.Errorf("invalid export: %s", msg)
	}

	t := tiers.Tier(tier)
	limits := tiers.Limits{}
	if a.Registry != nil {
		limits = a.Registry.For(t)
	}
	requiredFor := studioRequiredFor(a.Registry)
	var upgrade map[string]any
	if a.Registry != nil {
		if ref := tiers.CheckStudioOutput(t, limits, out.Tokens(), out.Advanced, requiredFor); ref != nil {
			upgrade = map[string]any{"option": ref.Option}
			if ref.Required != "" {
				upgrade["required"] = ref.Required
			}
			nearest, ok := nearestAllowedPreset(t, limits, out, requiredFor)
			if !ok {
				return nil, fmt.Errorf("no export format is available on the %s plan", tier)
			}
			out = nearest
		}
	}

	// MM119: the embedded captions mode is its own token. When the plan lacks
	// it, the nearest delivery is a sidecar file (every plan; the player
	// toggles it too) and the upgrade names subs_embedded, unless the output
	// itself already needed one.
	captionsMode := req.CaptionsMode
	if a.Registry != nil && captionsMode == models.StudioCaptionsEmbedded {
		if ref := tiers.CheckStudioOutput(t, limits, []string{services.StudioTokenSubsEmbedded}, false, requiredFor); ref != nil {
			captionsMode = models.StudioCaptionsSidecar
			if upgrade == nil {
				upgrade = map[string]any{"option": ref.Option}
				if ref.Required != "" {
					upgrade["required"] = ref.Required
				}
			}
		}
	}

	res := map[string]any{
		"container": out.Container, "videoCodec": out.VideoCodec, "audioCodec": out.AudioCodec,
		"audioChannels": out.AudioChannels, "format": out.Label(),
	}
	if out.Preset != "" {
		res["preset"] = out.Preset
	}
	if out.AudioBitrate > 0 {
		res["audioBitrate"] = out.AudioBitrate
	}
	if !out.AudioOnly() {
		if _, set := params["resolution"]; set {
			res["resolution"] = req.Resolution
		}
		if captionsMode != "" {
			res["captionsMode"] = captionsMode
		}
	}
	if w := out.Warnings(); len(w) > 0 {
		res["warnings"] = w
	}
	if upgrade != nil {
		res["upgrade"] = upgrade
	}
	return res, nil
}

// nearestAllowedPreset is the closest preset the plan allows to want: same
// kind first (video vs audio-only), then the same video codec, the same
// container, the same audio codec; table order breaks ties.
func nearestAllowedPreset(t tiers.Tier, l tiers.Limits, want services.StudioOutput, requiredFor func(string) tiers.Tier) (services.StudioOutput, bool) {
	best, bestScore := services.StudioOutput{}, -1
	for _, p := range services.StudioOutputs().Presets {
		o, err := services.ResolveStudioOutput(services.StudioOutputRequest{Preset: p.Key})
		if err != nil || tiers.CheckStudioOutput(t, l, o.Tokens(), o.Advanced, requiredFor) != nil {
			continue
		}
		score := 0
		if o.AudioOnly() == want.AudioOnly() {
			score += 100
		}
		if o.VideoCodec == want.VideoCodec {
			score += 10
		}
		if o.Container == want.Container {
			score += 5
		}
		if o.AudioCodec == want.AudioCodec {
			score++
		}
		if score > bestScore {
			best, bestScore = o, score
		}
	}
	return best, bestScore >= 0
}
