package tiers

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Darkroom (code name `studio`) output gating (MM118, ADR-0019, design
// DARKROOM_OUTPUTS_AND_CAPTIONS.md §3.4). The output TABLE (which combinations
// exist) lives in internal/services/studio_outputs.go; this file holds only the
// gated token vocabulary and the plan decision, so tiers, config and the
// handlers share one list. TestStudioOutputTokensMatchTiers (handlers) pins the
// two lists and the default ladder against each other.

// AllStudioFormats is every gated output token in canonical order: container
// keys, then video and audio codec keys, then feature tokens. "mp3" and
// "opus" are both an audio-only container and the codec; "none" (no video) is
// never gated. MM119 added webm, opus, vp9, vorbis and subs_embedded (the
// embedded captions mode); MM120 the Pro-only flac (container and codec),
// av1, prores and pcm_s24le.
func AllStudioFormats() []string {
	return []string{
		"mp4", "mov", "mkv", "webm", "mp3", "wav", "opus", "flac",
		"h264", "hevc", "vp9", "av1", "prores",
		"aac", "pcm_s16le", "vorbis", "pcm_s24le",
		"subs_embedded",
	}
}

// StudioFormatSet is a tier's Darkroom output capability set: a canonical
// comma-joined string (Limits stays comparable), serialised as a JSON array in
// AllStudioFormats order (`"studioFormats": ["mp4", ...]` on /api/tiers).
type StudioFormatSet string

// NewStudioFormatSet builds a set from any list: unknown names are dropped,
// duplicates collapse, order is canonical.
func NewStudioFormatSet(formats ...string) StudioFormatSet {
	want := make(map[string]bool, len(formats))
	for _, f := range formats {
		want[strings.ToLower(strings.TrimSpace(f))] = true
	}
	out := make([]string, 0, len(want))
	for _, f := range AllStudioFormats() {
		if want[f] {
			out = append(out, f)
		}
	}
	return StudioFormatSet(strings.Join(out, ","))
}

// List returns the tokens in canonical order (never nil).
func (s StudioFormatSet) List() []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(string(s), ",")
}

// Has reports whether f is in the set.
func (s StudioFormatSet) Has(f string) bool {
	for _, k := range s.List() {
		if k == f {
			return true
		}
	}
	return false
}

// Union is s ∪ o.
func (s StudioFormatSet) Union(o StudioFormatSet) StudioFormatSet {
	return NewStudioFormatSet(append(s.List(), o.List()...)...)
}

// MarshalJSON renders the set as a JSON array.
func (s StudioFormatSet) MarshalJSON() ([]byte, error) { return json.Marshal(s.List()) }

// UnmarshalJSON accepts the array form.
func (s *StudioFormatSet) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	*s = NewStudioFormatSet(list...)
	return nil
}

// ErrCodeFormatNotInPlan is the `error` of the 403 an export (or render-again)
// gets when its output uses a token, or Advanced-only values, the caller's plan
// does not include.
const ErrCodeFormatNotInPlan = "format_not_in_plan"

// LimitKeyStudioFormat is the 403's limitKey (the UI's LIMIT_FIELDS key).
const LimitKeyStudioFormat = "studio_format"

// StudioOptionAdvanced is the `option` of a refusal for Advanced-only values
// (a combination no preset names, or a non-default bitrate / channel count).
const StudioOptionAdvanced = "advanced"

// StudioFormatRefusal is the JSON body of a 403 format_not_in_plan.
// Required is the lowest public plan that unlocks Option ("free" means a free
// account, i.e. sign up); UpgradePath mirrors the other 403s ("signup" for an
// anonymous caller or a free-account option, else "premium" | "pro"), omitted
// when nothing public unlocks it.
type StudioFormatRefusal struct {
	Error       string `json:"error"`
	Message     string `json:"message"`
	LimitKey    string `json:"limitKey"`
	Required    string `json:"required,omitempty"`
	Option      string `json:"option"`
	UpgradePath string `json:"upgradePath,omitempty"`
}

// StudioFormatRequired is the lowest PUBLIC tier whose set has token ("" when
// none does, e.g. an operator-only env mistake).
func (r *Registry) StudioFormatRequired(token string) Tier {
	if r == nil {
		return ""
	}
	for _, t := range All() {
		if r.For(t).StudioFormats.Has(token) {
			return t
		}
	}
	return ""
}

// StudioAdvancedRequired is the lowest public tier with the Advanced picker.
func (r *Registry) StudioAdvancedRequired() Tier {
	if r == nil {
		return ""
	}
	for _, t := range All() {
		if r.For(t).StudioAdvanced {
			return t
		}
	}
	return ""
}

// CheckStudioOutput is the one plan decision for a Darkroom export: every
// token must be in the tier's StudioFormats, and Advanced-only values need
// StudioAdvanced. Tokens are checked in order (container, video, audio), then
// Advanced, so the refusal names the first thing the plan lacks. nil = allowed.
// The operator is never refused. requiredFor is consulted for the body's
// `required` (nil or "" leaves it out).
func CheckStudioOutput(tier Tier, l Limits, tokens []string, advanced bool, requiredFor func(option string) Tier) *StudioFormatRefusal {
	if tier == TierOperator {
		return nil
	}
	option := ""
	for _, tok := range tokens {
		if !l.StudioFormats.Has(tok) {
			option = tok
			break
		}
	}
	if option == "" && advanced && !l.StudioAdvanced {
		option = StudioOptionAdvanced
	}
	if option == "" {
		return nil
	}
	ref := &StudioFormatRefusal{Error: ErrCodeFormatNotInPlan, LimitKey: LimitKeyStudioFormat, Option: option}
	if requiredFor != nil {
		if req := requiredFor(option); req != "" && Rank(req) > Rank(tier) {
			ref.Required = string(req)
			// An anonymous caller signs up first whatever unlocks it (the
			// output-height refusal's rule); `required` still names the plan.
			if req == TierFree || tier == TierAnonymous {
				ref.UpgradePath = "signup"
			} else {
				ref.UpgradePath = string(req)
			}
		}
	}
	what := strings.ToUpper(option)
	switch option {
	case StudioOptionAdvanced:
		what = "Advanced export settings"
	case "subs_embedded":
		what = "Embedded subtitle"
	case "prores":
		what = "ProRes"
	case "pcm_s24le":
		what = "24-bit WAV"
	}
	switch ref.Required {
	case "free":
		ref.Message = fmt.Sprintf("%s exports need a free account.", what)
	case "premium", "pro":
		ref.Message = fmt.Sprintf("%s exports need %s.", what, strings.ToUpper(ref.Required[:1])+ref.Required[1:])
	default:
		ref.Message = fmt.Sprintf("%s exports are not available on your plan.", what)
	}
	return ref
}

// RequiredFor is CheckStudioOutput's requiredFor over this registry.
func (r *Registry) RequiredFor(option string) Tier {
	if option == StudioOptionAdvanced {
		return r.StudioAdvancedRequired()
	}
	return r.StudioFormatRequired(option)
}
