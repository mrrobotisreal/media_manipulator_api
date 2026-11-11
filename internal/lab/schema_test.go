package lab

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The schema drives both the server's validation and the UI's generated form;
// every registry row must carry a schema that compiles, validates its own
// defaults and refuses what it should.
func TestValidateParams_SchemaForm(t *testing.T) {
	m, _ := Get("codeformer")

	got, err := ValidateParams(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["fidelity"] != 0.7 || got["upscale"] != float64(2) {
		t.Fatalf("defaults not applied: %v", got)
	}
	got, err = ValidateParams(m, map[string]any{"fidelity": 0.25, "upscale": 4})
	if err != nil || got["fidelity"] != 0.25 || got["upscale"] != float64(4) {
		t.Fatalf("explicit params: %v %v", got, err)
	}
	for name, bad := range map[string]map[string]any{
		"above maximum":     {"fidelity": 1.5},
		"below minimum":     {"fidelity": -0.1},
		"not in the enum":   {"upscale": 3},
		"wrong type":        {"fidelity": "high"},
		"unknown parameter": {"sneaky": true},
	} {
		_, err := ValidateParams(m, bad)
		var pe *ParamsError
		if !errors.As(err, &pe) {
			t.Errorf("%s: want a ParamsError, got %v", name, err)
		}
	}
}

func TestRegistry_RowsAreValidAndLabOnly(t *testing.T) {
	if err := validateRegistry(); err != nil {
		t.Fatal(err)
	}
	for _, m := range Models() {
		if p := m.Profile(); !p.LabOnly || p.Key != ToolPrefix+m.Key || p.Never {
			t.Errorf("%s: profile %+v", m.Key, p)
		}
		if _, err := ValidateParams(m, requiredPlaceholders(m)); err != nil {
			t.Errorf("%s: defaults must validate: %v", m.Key, err)
		}
		if m.Licence == LicencePermissive {
			continue
		}
		if m.LicenceURL == "" || m.SourceURL == "" {
			t.Errorf("%s: a restricted licence needs its licence and source URLs", m.Key)
		}
	}
	if m, ok := Get("codeformer"); !ok || m.Licence != LicenceNonCommercial || !m.FitsHome || m.Tool() != "lab_codeformer" {
		t.Fatalf("codeformer row: %+v", m)
	}
}

func TestEstimate_HomeIsFreeBurstIsPriced(t *testing.T) {
	m, _ := Get("codeformer")
	rig := newRig(t, true, false)
	if got := EstimateMicros(m, rig.r.Cfg); got != 0 {
		t.Fatalf("home estimate %d", got)
	}
	rig.r.Cfg.Lab.AllowLocal = false
	got := EstimateMicros(m, rig.r.Cfg)
	// g6e.xlarge $1.86/h x (300 s boot + 60 s) = $0.186 plus 200 GiB EBS for 6 min.
	if got < 180_000 || got > 260_000 {
		t.Fatalf("burst estimate %d micros", got)
	}
}

// requiredPlaceholders fills the required properties that have no default (text,
// the consent confirmation) so a schema's own defaults can be validated.
func requiredPlaceholders(m LabModel) map[string]any {
	var sch struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(m.ParamsSchema, &sch)
	out := map[string]any{}
	for _, k := range sch.Required {
		switch sch.Properties[k].Type {
		case "boolean":
			out[k] = true
		case "string":
			out[k] = "x"
		}
	}
	return out
}

// lib.sh mirrors the Lab image's root size (the builder launches with it, and an
// AMI cannot be launched with a smaller root than its snapshot).
func TestLibRootSizeMatchesTheLabProfile(t *testing.T) {
	b, err := os.ReadFile("../../scripts/burst/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s+lab-voice\) echo (\d+) ;;`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("lib.sh has no lab-voice root size")
	}
	for _, mm := range Models() {
		if mm.Family == "lab-voice" && strconv.Itoa(mm.Profile().RootGiB) != m[1] {
			t.Errorf("%s: profile root %d GiB, lib.sh %s GiB", mm.Key, mm.Profile().RootGiB, m[1])
		}
	}
}
