// Package lab is Mitch's private Lab (MM138, docs/design/OPERATOR_LAB.md,
// ADR-0021): a registry of models the product cannot sell (non-commercial,
// research, deal-required, copyleft or too big for the home cards), the gate
// that only LAB_EMAILS passes, and the runner that launches them on a burst GPU
// (or a free home card for FitsHome models).
//
// Licence isolation is the point. This package is the only place a LabOnly
// burst profile exists; internal/burst's product table never lists one, and the
// tests in isolation_test.go fail the build when a product path could reach
// one.
package lab

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// ToolPrefix starts the tool (and job mode) of every Lab job: "lab_<model key>".
const ToolPrefix = "lab_"

// Licences.
const (
	LicenceNonCommercial = "noncommercial"
	LicenceResearch      = "research"
	LicenceDealRequired  = "deal_required"
	LicenceCopyleft      = "copyleft"
	LicencePermissive    = "permissive"
)

// Areas.
const (
	AreaVoice = "voice"
	AreaImage = "image"
	AreaVideo = "video"
)

// Input is one input a model takes (an upload, or the result of a previous job).
type Input struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // image|audio|video
	Label    string `json:"label"`
	Required bool   `json:"required"`
}

// Output is one thing a model produces.
type Output struct {
	Kind  string `json:"kind"` // image|audio|video|file
	Label string `json:"label"`
}

// CommandCtx is what a model's Command builds a Spec from. Every path is
// absolute and server-generated; Params is validated against ParamsSchema with
// defaults applied.
type CommandCtx struct {
	Cfg       *config.Config
	JobID     string
	RequestID string
	Params    map[string]any
	// Inputs maps an Input name to its local file.
	Inputs map[string]string
	// OutDir is the job's output directory (<OUTPUT_DIR>/<jobId>).
	OutDir string
	// Card is the GPU index the tool is told to use: the scheduler's card at
	// home, the configured local card on the way to a burst instance (the
	// remote executor rewrites a declared CardArgs to the worker's card).
	Card int
}

// LabModel is one registry row (ticket decision 3). Adding a Lab model is
// adding one of these to registry() in this file; nothing else changes.
type LabModel struct {
	// Key is the model key ("codeformer"); the tool is "lab_" + Key.
	Key  string
	Name string
	Area string
	// Licence is one of the Licence* constants; LicenceURL and SourceURL are
	// shown on the card.
	Licence    string
	LicenceURL string
	SourceURL  string
	// Family is the Lab worker image family ("lab-voice", "lab-image",
	// "lab-video"); BURST_AMIS maps "<family>:x86_64" to its AMI.
	Family string
	// InstanceCandidates are the ordered instance types for a burst run.
	InstanceCandidates []string
	MinGPUMemGiB       int
	// FitsHome models may run on a free home card (LAB_ALLOW_LOCAL).
	FitsHome    bool
	HomeVRAMMiB int64
	// ParamsSchema is a JSON Schema (draft-07 subset: an object of string,
	// number, integer and boolean properties with enum, default, minimum,
	// maximum, title, description and required). It drives both the server's
	// validation and the UI's generated form.
	ParamsSchema json.RawMessage
	Inputs       []Input
	Outputs      []Output
	// ResultFile is the one file the job's result is served from, written into
	// CommandCtx.OutDir by the command.
	ResultFile string
	// Command builds the Spec: steps plus every path they touch, so the same
	// Spec runs at home (exec.Local) or remotely (burst.Remote).
	Command func(c CommandCtx) (exec.Spec, error)
	// EstSeconds is the typical run time of one job, excluding boot.
	EstSeconds int
}

// Tool is the job tool / mode name of the model.
func (m LabModel) Tool() string { return ToolPrefix + m.Key }

// Profile is the model's burst profile, generated from the row, LabOnly.
func (m LabModel) Profile() burst.Profile {
	timeout := m.EstSeconds * 4
	if timeout < 1800 {
		timeout = 1800
	}
	return burst.Profile{
		Key: m.Tool(), Family: m.Family, MinGPUMemGiB: m.MinGPUMemGiB,
		Candidates: append([]string(nil), m.InstanceCandidates...),
		RootGiB:    200, ScratchGiB: 100, SeedHoldSeconds: m.EstSeconds, RemoteTimeout: timeout,
		LabOnly: true,
	}
}

// IsLabTool reports whether a tool, mode or kind names a Lab model.
func IsLabTool(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), ToolPrefix)
}

var codeformerSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "fidelity": {"type": "number", "minimum": 0, "maximum": 1, "default": 0.7, "title": "Fidelity", "description": "0 restores more, 1 stays closer to the input face."},
    "upscale": {"type": "integer", "enum": [1, 2, 4], "default": 2, "title": "Upscale", "description": "Output scale factor."}
  },
  "additionalProperties": false
}`)

// models is the registry. Add a row here (and its Command) to add a model.
func registry() []LabModel {
	return append([]LabModel{
		{
			Key: "codeformer", Name: "CodeFormer", Area: AreaImage,
			Licence:    LicenceNonCommercial,
			LicenceURL: "https://github.com/sczhou/CodeFormer/blob/master/LICENSE",
			SourceURL:  "https://github.com/sczhou/CodeFormer",
			Family:     "lab-image", InstanceCandidates: []string{"g6e.xlarge"}, MinGPUMemGiB: 16,
			FitsHome: true, HomeVRAMMiB: 6000,
			ParamsSchema: codeformerSchema,
			Inputs:       []Input{{Name: "image", Kind: "image", Label: "Photo with faces", Required: true}},
			Outputs:      []Output{{Kind: "image", Label: "Restored image"}},
			ResultFile:   "restored.png",
			Command:      codeformerCommand,
			EstSeconds:   60,
		},
	}, voiceModels()...)
}

// Models returns a copy of the registry, ordered by area then name.
func Models() []LabModel {
	out := registry()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Area != out[j].Area {
			return out[i].Area < out[j].Area
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Get returns the model with that key.
func Get(key string) (LabModel, bool) {
	for _, m := range registry() {
		if m.Key == key {
			return m, true
		}
	}
	return LabModel{}, false
}

// ProfileForKind resolves a "lab_<key>" Kind to its LabOnly profile. It is
// installed as burst.LabProfileLookup so the planner's Terminate and the
// reaper can price a Lab run; burst.Lookup (the product table) never sees it.
func ProfileForKind(kind string) (burst.Profile, bool) {
	if !IsLabTool(kind) {
		return burst.Profile{}, false
	}
	m, ok := Get(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(kind)), ToolPrefix))
	if !ok {
		return burst.Profile{}, false
	}
	return m.Profile(), true
}

func init() { burst.LabProfileLookup = ProfileForKind }

// validateRegistry is the start-up self-check (and a test): keys unique, a
// command and schema present, licence and area known.
func validateRegistry() error {
	seen := map[string]bool{}
	for _, m := range registry() {
		switch {
		case m.Key == "" || strings.ContainsAny(m.Key, " /."):
			return fmt.Errorf("lab: bad model key %q", m.Key)
		case seen[m.Key]:
			return fmt.Errorf("lab: duplicate model key %q", m.Key)
		case m.Command == nil || len(m.ParamsSchema) == 0 || len(m.Inputs) == 0 || m.ResultFile == "":
			return fmt.Errorf("lab: model %s is incomplete", m.Key)
		case !map[string]bool{LicenceNonCommercial: true, LicenceResearch: true, LicenceDealRequired: true, LicenceCopyleft: true, LicencePermissive: true}[m.Licence]:
			return fmt.Errorf("lab: model %s has an unknown licence %q", m.Key, m.Licence)
		case !map[string]bool{AreaVoice: true, AreaImage: true, AreaVideo: true}[m.Area]:
			return fmt.Errorf("lab: model %s has an unknown area %q", m.Key, m.Area)
		case !m.Profile().LabOnly:
			return fmt.Errorf("lab: model %s profile is not LabOnly", m.Key)
		}
		seen[m.Key] = true
	}
	return nil
}
