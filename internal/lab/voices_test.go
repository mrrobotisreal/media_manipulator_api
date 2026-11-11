package lab

import (
	"errors"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
)

func voiceCtx(t *testing.T, key string, params map[string]any, inputs map[string]string) (exec.Spec, error) {
	t.Helper()
	g := newRig(t, false, true)
	m, ok := Get(key)
	if !ok {
		t.Fatalf("no model %s", key)
	}
	p, err := ValidateParams(m, params)
	if err != nil {
		return exec.Spec{}, err
	}
	return m.Command(CommandCtx{Cfg: g.r.Cfg, JobID: "j", Params: p, Inputs: inputs, OutDir: "/out/j", Card: 1})
}

// Every voice row validates its defaults once the required fields are given,
// refuses a missing consent at the schema, and is Lab-only on the lab-voice family.
func TestVoiceRowsSchemaAndConsent(t *testing.T) {
	if err := validateRegistry(); err != nil {
		t.Fatal(err)
	}
	required := map[string]map[string]any{
		"fish_s2":    {"text": "Hello there", "reference_text": "Hi"},
		"indextts2":  {"text": "Hello there"},
		"seedvc":     {},
		"cosyvoice3": {"text": "Hello there", "reference_text": "Hi"},
	}
	for key, base := range required {
		m, ok := Get(key)
		if !ok || m.Family != "lab-voice" || m.Area != AreaVoice || !m.RequiresConsent() || m.FitsHome {
			t.Fatalf("%s row: %+v", key, m)
		}
		without := map[string]any{}
		for k, v := range base {
			without[k] = v
		}
		var pe *ParamsError
		if _, err := ValidateParams(m, without); !errors.As(err, &pe) {
			t.Errorf("%s: no consent must fail the schema, got %v", key, err)
		}
		with := map[string]any{"consent": true}
		for k, v := range base {
			with[k] = v
		}
		if _, err := ValidateParams(m, with); err != nil {
			t.Errorf("%s: with consent and its required fields the defaults must validate: %v", key, err)
		}
		// Commands refuse consent=false even if the schema were bypassed.
		if _, err := m.Command(CommandCtx{Cfg: newRig(t, false, true).r.Cfg, Params: map[string]any{"consent": false, "text": "x"}, Inputs: map[string]string{"reference": "/x"}}); err == nil {
			t.Errorf("%s: Command must refuse without consent", key)
		}
	}
	for _, bad := range []map[string]any{{"text": ""}, {"text": "x", "temperature": 9}, {"text": "x", "sneaky": 1}} {
		bad["consent"], bad["reference_text"] = true, "r"
		if _, err := ValidateParams(mustGet(t, "fish_s2"), bad); err == nil {
			t.Errorf("fish_s2 accepted %v", bad)
		}
	}
	if m := mustGet(t, "indextts2"); true {
		if _, err := ValidateParams(m, map[string]any{"text": "x", "consent": true, "version": "3"}); err == nil {
			t.Error("indextts2 accepted version 3")
		}
	}
	// Licences as verified on 2026-11-11 (MM139 ticket log).
	want := map[string]string{"fish_s2": LicenceNonCommercial, "indextts2": LicenceNonCommercial, "seedvc": LicenceCopyleft, "cosyvoice3": LicencePermissive}
	for k, l := range want {
		if mustGet(t, k).Licence != l {
			t.Errorf("%s licence %s, want %s", k, mustGet(t, k).Licence, l)
		}
	}
}

func mustGet(t *testing.T, k string) LabModel {
	t.Helper()
	m, ok := Get(k)
	if !ok {
		t.Fatalf("no model %s", k)
	}
	return m
}

func TestFishS2Spec(t *testing.T) {
	sp, err := voiceCtx(t, "fish_s2", map[string]any{"text": "Hello there", "reference_text": "Hi it is me", "style": "whisper", "seed": 7, "consent": true},
		map[string]string{"reference": "/up/j/reference_a.wav"})
	if err != nil {
		t.Fatal(err)
	}
	c := sp.Steps[0]
	want := "/opt/media-manipulator-ai/scripts/lab/voice_fish_s2.py --ref /up/j/reference_a.wav --ref-text Hi it is me --text Hello there --temperature 0.7 --top-p 0.7 --seed 7 --style whisper --out /out/j/result.wav"
	if c.Exe != "/opt/media-manipulator-ai/venvs/lab-voice-fish_s2/bin/python" || strings.Join(c.Args, " ") != want {
		t.Fatalf("argv %q %q", c.Exe, strings.Join(c.Args, " "))
	}
	if c.Card == nil || c.Card.Local != 1 || len(c.Card.Env) != 1 || c.Card.Env[0] != "CUDA_VISIBLE_DEVICES" {
		t.Fatalf("card %+v", c.Card)
	}
	if len(sp.Inputs) != 1 || len(sp.Outputs) != 1 || !sp.Outputs[0].Dir || sp.Workload != "lab_fish_s2" {
		t.Fatalf("spec %+v", sp)
	}
	if _, err := voiceCtx(t, "fish_s2", map[string]any{"text": "x", "reference_text": "y", "consent": true}, map[string]string{}); err == nil {
		t.Error("a missing reference must be refused")
	}
}

// The emotion reference is optional; 2.5-only flags appear only for 2.5.
func TestIndexTTS2Spec(t *testing.T) {
	ins := map[string]string{"reference": "/up/j/reference_a.wav", "emotion_reference": "/up/j/emotion_reference_b.wav"}
	sp, err := voiceCtx(t, "indextts2", map[string]any{"text": "Hello", "consent": true, "emotion_vector": "0,0,0,0,0,0,0.6,0.4"}, ins)
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Join(sp.Steps[0].Args, " ")
	if !strings.Contains(a, "--version 2 ") || !strings.Contains(a, "--emo-ref /up/j/emotion_reference_b.wav") || !strings.Contains(a, "--emo-vector 0,0,0,0,0,0,0.6,0.4") || strings.Contains(a, "--lang") || strings.Contains(a, "--duration-factor") {
		t.Fatalf("v2 argv: %s", a)
	}
	if len(sp.Inputs) != 2 {
		t.Fatalf("both audio inputs must be declared: %+v", sp.Inputs)
	}
	sp, err = voiceCtx(t, "indextts2", map[string]any{"text": "Hello", "consent": true, "version": "2.5", "language": "ES", "duration_factor": 1.2, "emotion_text": "calm and warm"},
		map[string]string{"reference": "/up/j/reference_a.wav"})
	if err != nil {
		t.Fatal(err)
	}
	a = strings.Join(sp.Steps[0].Args, " ")
	if !strings.Contains(a, "--version 2.5") || !strings.Contains(a, "--lang ES") || !strings.Contains(a, "--duration-factor 1.2") || !strings.Contains(a, "--emo-text calm and warm") || strings.Contains(a, "--emo-ref") {
		t.Fatalf("v2.5 argv: %s", a)
	}
	if len(sp.Inputs) != 1 {
		t.Fatalf("an absent optional input must not be declared: %+v", sp.Inputs)
	}
}

func TestSeedVCAndCosyVoiceSpecs(t *testing.T) {
	sp, err := voiceCtx(t, "seedvc", map[string]any{"consent": true, "diffusion_steps": 10, "f0_condition": true, "semitone_shift": -2},
		map[string]string{"source": "/up/j/source_a.wav", "reference": "/up/j/reference_b.wav"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sp.Steps[0].Args, " "); got != "/opt/media-manipulator-ai/scripts/lab/voice_seedvc.py --source /up/j/source_a.wav --ref /up/j/reference_b.wav --diffusion-steps 10 --length-adjust 1 --cfg-rate 0.7 --f0-condition true --auto-f0-adjust false --semitone-shift -2 --out /out/j/result.wav" {
		t.Fatalf("seedvc argv: %s", got)
	}
	if len(sp.Inputs) != 2 {
		t.Fatalf("seedvc inputs %+v", sp.Inputs)
	}
	if _, err := voiceCtx(t, "cosyvoice3", map[string]any{"consent": true, "text": "Hi"}, map[string]string{"reference": "/up/j/reference_a.wav"}); err == nil {
		t.Error("zero_shot without the transcript must be refused")
	}
	sp, err = voiceCtx(t, "cosyvoice3", map[string]any{"consent": true, "text": "Hi", "mode": "cross_lingual"}, map[string]string{"reference": "/up/j/reference_a.wav"})
	if err != nil || !strings.Contains(strings.Join(sp.Steps[0].Args, " "), "--mode cross_lingual") {
		t.Fatalf("cosyvoice3 cross_lingual: %v %+v", err, sp)
	}
}

// Every voice Spec passes the executor's validator: only the declared data paths
// and the runtime subtrees the burst worker may name (venvs, scripts).
func TestVoiceSpecsPassTheExecutorValidator(t *testing.T) {
	g := newRig(t, false, true)
	cases := map[string]struct {
		params map[string]any
		inputs map[string]string
	}{
		"fish_s2":    {map[string]any{"text": "x", "reference_text": "y"}, map[string]string{"reference": g.r.Cfg.UploadDir + "/j/reference_a.wav"}},
		"indextts2":  {map[string]any{"text": "x"}, map[string]string{"reference": g.r.Cfg.UploadDir + "/j/reference_a.wav", "emotion_reference": g.r.Cfg.UploadDir + "/j/e.wav"}},
		"seedvc":     {map[string]any{}, map[string]string{"source": g.r.Cfg.UploadDir + "/j/s.wav", "reference": g.r.Cfg.UploadDir + "/j/r.wav"}},
		"cosyvoice3": {map[string]any{"text": "x", "reference_text": "y"}, map[string]string{"reference": g.r.Cfg.UploadDir + "/j/reference_a.wav"}},
	}
	for key, c := range cases {
		m := mustGet(t, key)
		c.params["consent"] = true
		p, err := ValidateParams(m, c.params)
		if err != nil {
			t.Fatal(err)
		}
		sp, err := m.Command(CommandCtx{Cfg: g.r.Cfg, JobID: "j", Params: p, Inputs: c.inputs, OutDir: g.r.Cfg.OutputDir + "/j", Card: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.r.Validator.Validate(sp); err != nil {
			t.Errorf("%s: %v", key, err)
		}
		if _, err := exec.NewMapper("", "run1", sp); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
}
