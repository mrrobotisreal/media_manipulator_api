package lab

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
)

// MM139: the Lab voice models. Each is a thin wrapper (scripts/lab/voice_<m>.py)
// that the lab-voice burst image carries, run inside the model's own venv:
//
//	/opt/media-manipulator-ai/venvs/lab-voice-<m>/bin/python /opt/media-manipulator-ai/scripts/lab/voice_<m>.py ...
//
// Only venvs/ and scripts/ are subtrees the burst executor lets a Spec name, so
// the commands reference nothing else under /opt/media-manipulator-ai.

// VoiceConsentText is the product's consent copy (VOICES.md section 2, the
// voices feature's checkbox). A Lab job that uses a reference voice must carry
// it as consent=true; it is stored on the job (options.consent*).
const VoiceConsentText = "This is my voice, or I have the explicit permission of the person whose voice this is"

// VoiceConsentVersion versions that copy for the job record.
const VoiceConsentVersion = "2026-10-28"

const (
	voiceRoot   = "/opt/media-manipulator-ai"
	voiceResult = "result.wav"
)

// consentProp is the schema property every reference-voice model carries. It is
// a plain boolean so the generated form renders a switch with the consent text;
// the handler (and each Command) refuses anything but true.
const consentProp = `"consent": {"type": "boolean", "title": "` + VoiceConsentText + `", "description": "Required. Lab voice outputs are personal use only, with no watermark."}`

func voiceSchema(props ...string) json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {` + strings.Join(append(props, consentProp), ",") + `}, "required": ["text", "consent"], "additionalProperties": false}`)
}

const textProp = `"text": {"type": "string", "minLength": 1, "maxLength": 1500, "title": "Text to speak"}`

var fishSchema = voiceSchema(textProp,
	`"reference_text": {"type": "string", "minLength": 1, "maxLength": 600, "title": "Transcript of the reference audio", "description": "Exactly what is said in the reference clip."}`,
	`"style": {"type": "string", "maxLength": 80, "title": "Style tag", "description": "Optional, put in front of the text as [tag]: whisper, excited, sad, laughing, professional broadcast tone ... You can also write [tags] inside the text."}`,
	`"temperature": {"type": "number", "minimum": 0.1, "maximum": 1.5, "default": 0.7, "title": "Temperature"}`,
	`"top_p": {"type": "number", "minimum": 0.1, "maximum": 1, "default": 0.7, "title": "Top-p"}`,
	`"seed": {"type": "integer", "minimum": 0, "maximum": 2147483647, "default": 42, "title": "Seed"}`)

var indexSchema = voiceSchema(textProp,
	`"version": {"type": "string", "enum": ["2", "2.5"], "default": "2", "title": "Model version", "description": "2.5 adds speed control and Japanese, Spanish, Arabic. Neither lists Russian."}`,
	`"language": {"type": "string", "enum": ["EN", "ZH", "JA", "ES", "AR"], "default": "EN", "title": "Language (2.5 only)"}`,
	`"emotion_alpha": {"type": "number", "minimum": 0, "maximum": 1, "default": 1, "title": "Emotion strength", "description": "Applies to the emotion reference, vector or text. 0.6 or lower is suggested with emotion text."}`,
	`"emotion_vector": {"type": "string", "maxLength": 80, "title": "Emotion vector", "description": "Optional: 8 numbers 0 to 1 for happy, angry, sad, afraid, disgusted, melancholic, surprised, calm. Example: 0,0,0,0,0,0,0.6,0.4"}`,
	`"emotion_text": {"type": "string", "maxLength": 200, "title": "Emotion in words", "description": "Optional: describe the emotion; a small Qwen model turns it into a vector."}`,
	`"duration_factor": {"type": "number", "minimum": 0.5, "maximum": 2, "default": 1, "title": "Speed (2.5 only)", "description": "Above 1 slows speech, below 1 speeds it up."}`,
	`"use_random": {"type": "boolean", "default": false, "title": "Random sampling", "description": "More varied, less faithful to the reference voice."}`)

var seedvcSchema = json.RawMessage(`{"type": "object", "properties": {
  "diffusion_steps": {"type": "integer", "minimum": 4, "maximum": 100, "default": 30, "title": "Diffusion steps", "description": "4 to 10 fastest, 30 to 50 for singing."},
  "length_adjust": {"type": "number", "minimum": 0.5, "maximum": 2, "default": 1, "title": "Length adjust"},
  "cfg_rate": {"type": "number", "minimum": 0, "maximum": 1, "default": 0.7, "title": "Reference strength (CFG)"},
  "f0_condition": {"type": "boolean", "default": false, "title": "Keep source pitch (singing model, 44.1 kHz)"},
  "auto_f0_adjust": {"type": "boolean", "default": false, "title": "Auto-match pitch level to the reference"},
  "semitone_shift": {"type": "integer", "minimum": -24, "maximum": 24, "default": 0, "title": "Pitch shift (semitones)", "description": "Only with the pitch-conditioned model."},
  ` + consentProp + `
}, "required": ["consent"], "additionalProperties": false}`)

var cosySchema = voiceSchema(textProp,
	`"mode": {"type": "string", "enum": ["zero_shot", "cross_lingual"], "default": "zero_shot", "title": "Mode", "description": "zero_shot needs the transcript; cross_lingual needs only the audio."}`,
	`"reference_text": {"type": "string", "maxLength": 600, "title": "Transcript of the reference audio", "description": "Needed for zero_shot."}`,
	`"speed": {"type": "number", "minimum": 0.5, "maximum": 2, "default": 1, "title": "Speed"}`)

func needConsent(p map[string]any, model string) error {
	if v, _ := p["consent"].(bool); !v {
		return fmt.Errorf("lab: %s needs the consent confirmation (%s)", model, VoiceConsentText)
	}
	return nil
}

func str(p map[string]any, k string) string { s, _ := p[k].(string); return strings.TrimSpace(s) }

func num(p map[string]any, k string, def float64) float64 {
	switch v := p[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return def
}

func boolp(p map[string]any, k string) bool { b, _ := p[k].(bool); return b }

func ff(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// voiceSpec builds the one-step Spec every voice model shares.
func voiceSpec(c CommandCtx, key string, inputs []string, args []string) exec.Spec {
	tool := ToolPrefix + key
	out := filepath.Join(c.OutDir, voiceResult)
	full := append([]string{voiceRoot + "/scripts/lab/voice_" + key + ".py"}, args...)
	full = append(full, "--out", out)
	var ins []exec.Path
	for _, in := range inputs {
		if in != "" {
			ins = append(ins, exec.P(in))
		}
	}
	return exec.Spec{
		Workload: tool, Tool: tool, JobID: c.JobID, RequestID: c.RequestID,
		Steps: []exec.Cmd{{
			Label: tool,
			Exe:   voiceRoot + "/venvs/lab-voice-" + key + "/bin/python",
			Args:  full,
			Env: []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=" + strconv.Itoa(c.Card),
				"PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True", "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "USE_MODELSCOPE=false"},
			Card: &exec.CardArgs{Local: c.Card, Env: []string{exec.CardEnvCUDA}},
		}},
		Inputs:  ins,
		Outputs: []exec.Path{exec.D(c.OutDir)},
	}
}

func fishS2Command(c CommandCtx) (exec.Spec, error) {
	if err := needConsent(c.Params, "Fish Audio S2 Pro"); err != nil {
		return exec.Spec{}, err
	}
	ref := c.Inputs["reference"]
	text, refText := str(c.Params, "text"), str(c.Params, "reference_text")
	if ref == "" || text == "" || refText == "" {
		return exec.Spec{}, fmt.Errorf("lab: fish_s2 needs a reference clip, its transcript and the text")
	}
	args := []string{"--ref", ref, "--ref-text", refText, "--text", text,
		"--temperature", ff(num(c.Params, "temperature", 0.7)), "--top-p", ff(num(c.Params, "top_p", 0.7)),
		"--seed", strconv.Itoa(int(num(c.Params, "seed", 42)))}
	if s := str(c.Params, "style"); s != "" {
		args = append(args, "--style", s)
	}
	return voiceSpec(c, "fish_s2", []string{ref}, args), nil
}

func indexTTS2Command(c CommandCtx) (exec.Spec, error) {
	if err := needConsent(c.Params, "IndexTTS-2"); err != nil {
		return exec.Spec{}, err
	}
	ref, emo := c.Inputs["reference"], c.Inputs["emotion_reference"]
	text := str(c.Params, "text")
	if ref == "" || text == "" {
		return exec.Spec{}, fmt.Errorf("lab: indextts2 needs a reference clip and the text")
	}
	version := str(c.Params, "version")
	if version != "2" && version != "2.5" {
		return exec.Spec{}, fmt.Errorf("lab: indextts2 version must be 2 or 2.5")
	}
	args := []string{"--ref", ref, "--text", text, "--version", version, "--emo-alpha", ff(num(c.Params, "emotion_alpha", 1))}
	if emo != "" {
		args = append(args, "--emo-ref", emo)
	}
	if v := str(c.Params, "emotion_vector"); v != "" {
		args = append(args, "--emo-vector", v)
	}
	if v := str(c.Params, "emotion_text"); v != "" {
		args = append(args, "--emo-text", v)
	}
	if boolp(c.Params, "use_random") {
		args = append(args, "--use-random")
	}
	if version == "2.5" {
		lang := str(c.Params, "language")
		if lang == "" {
			lang = "EN"
		}
		args = append(args, "--lang", lang, "--duration-factor", ff(num(c.Params, "duration_factor", 1)))
	}
	return voiceSpec(c, "indextts2", []string{ref, emo}, args), nil
}

func seedVCCommand(c CommandCtx) (exec.Spec, error) {
	if err := needConsent(c.Params, "Seed-VC"); err != nil {
		return exec.Spec{}, err
	}
	src, ref := c.Inputs["source"], c.Inputs["reference"]
	if src == "" || ref == "" {
		return exec.Spec{}, fmt.Errorf("lab: seedvc needs the source speech and a reference clip")
	}
	args := []string{"--source", src, "--ref", ref,
		"--diffusion-steps", strconv.Itoa(int(num(c.Params, "diffusion_steps", 30))),
		"--length-adjust", ff(num(c.Params, "length_adjust", 1)), "--cfg-rate", ff(num(c.Params, "cfg_rate", 0.7)),
		"--f0-condition", strconv.FormatBool(boolp(c.Params, "f0_condition")),
		"--auto-f0-adjust", strconv.FormatBool(boolp(c.Params, "auto_f0_adjust")),
		"--semitone-shift", strconv.Itoa(int(num(c.Params, "semitone_shift", 0)))}
	return voiceSpec(c, "seedvc", []string{src, ref}, args), nil
}

func cosyVoice3Command(c CommandCtx) (exec.Spec, error) {
	if err := needConsent(c.Params, "Fun-CosyVoice3"); err != nil {
		return exec.Spec{}, err
	}
	ref, text := c.Inputs["reference"], str(c.Params, "text")
	if ref == "" || text == "" {
		return exec.Spec{}, fmt.Errorf("lab: cosyvoice3 needs a reference clip and the text")
	}
	mode := str(c.Params, "mode")
	if mode != "zero_shot" && mode != "cross_lingual" {
		return exec.Spec{}, fmt.Errorf("lab: cosyvoice3 mode must be zero_shot or cross_lingual")
	}
	refText := str(c.Params, "reference_text")
	if mode == "zero_shot" && refText == "" {
		return exec.Spec{}, fmt.Errorf("lab: cosyvoice3 zero_shot needs the transcript of the reference audio")
	}
	args := []string{"--ref", ref, "--text", text, "--mode", mode, "--speed", ff(num(c.Params, "speed", 1))}
	if refText != "" {
		args = append(args, "--ref-text", refText)
	}
	return voiceSpec(c, "cosyvoice3", []string{ref}, args), nil
}

const (
	voiceFamily    = "lab-voice"
	fishLicenceURL = "https://huggingface.co/fishaudio/s2-pro/blob/main/LICENSE.md"
)

var wavOut = []Output{{Kind: "audio", Label: "Generated speech (WAV)"}}

// voiceModels are the MM139 registry rows. EstSeconds are estimates (not
// measured; the first live runs replace them, see the MM139 log). FitsHome is
// false for all: none is installed on the home GPU host, and Seed-VC's VRAM is
// not measured yet.
func voiceModels() []LabModel {
	big := []string{"g6e.xlarge", "g7.2xlarge"}
	small := []string{"g6.xlarge", "g6e.xlarge", "g7.2xlarge"}
	return []LabModel{
		{
			Key: "fish_s2", Name: "Fish Audio S2 Pro", Area: AreaVoice,
			Licence: LicenceNonCommercial, LicenceURL: fishLicenceURL, SourceURL: "https://huggingface.co/fishaudio/s2-pro",
			Family: voiceFamily, InstanceCandidates: big, MinGPUMemGiB: 24,
			ParamsSchema: fishSchema,
			Inputs:       []Input{{Name: "reference", Kind: "audio", Label: "Reference voice (5 to 60 s)", Required: true}},
			Outputs:      wavOut, ResultFile: voiceResult, Command: fishS2Command, EstSeconds: 90,
		},
		{
			Key: "indextts2", Name: "IndexTTS-2 / 2.5", Area: AreaVoice,
			// bilibili Model Use License: free below 100 M MAU / RMB 1 bn revenue, but the project's
			// DISCLAIMER forbids commercial use of the voices without authorisation and the README says
			// to contact indexspeech@bilibili.com for commercial use: "deal required" for a product.
			Licence: LicenceDealRequired, LicenceURL: "https://github.com/index-tts/index-tts/blob/main/LICENSE", SourceURL: "https://github.com/index-tts/index-tts",
			Family: voiceFamily, InstanceCandidates: big, MinGPUMemGiB: 16,
			ParamsSchema: indexSchema,
			Inputs: []Input{
				{Name: "reference", Kind: "audio", Label: "Reference voice (5 to 60 s)", Required: true},
				{Name: "emotion_reference", Kind: "audio", Label: "Emotion reference (optional)", Required: false},
			},
			Outputs: wavOut, ResultFile: voiceResult, Command: indexTTS2Command, EstSeconds: 90,
		},
		{
			Key: "seedvc", Name: "Seed-VC (voice conversion)", Area: AreaVoice,
			Licence: LicenceCopyleft, LicenceURL: "https://github.com/Plachtaa/seed-vc/blob/main/LICENSE", SourceURL: "https://github.com/Plachtaa/seed-vc",
			Family: voiceFamily, InstanceCandidates: small, MinGPUMemGiB: 12,
			ParamsSchema: seedvcSchema,
			Inputs: []Input{
				{Name: "source", Kind: "audio", Label: "Speech to convert", Required: true},
				{Name: "reference", Kind: "audio", Label: "Target voice reference (5 to 25 s)", Required: true},
			},
			Outputs: wavOut, ResultFile: voiceResult, Command: seedVCCommand, EstSeconds: 45,
		},
		{
			Key: "cosyvoice3", Name: "Fun-CosyVoice3 0.5B (comparator)", Area: AreaVoice,
			Licence: LicencePermissive, LicenceURL: "https://github.com/QwenAudio/CosyVoice/blob/main/LICENSE", SourceURL: "https://github.com/QwenAudio/CosyVoice",
			Family: voiceFamily, InstanceCandidates: small, MinGPUMemGiB: 12,
			ParamsSchema: cosySchema,
			Inputs:       []Input{{Name: "reference", Kind: "audio", Label: "Reference voice (5 to 30 s)", Required: true}},
			Outputs:      wavOut, ResultFile: voiceResult, Command: cosyVoice3Command, EstSeconds: 60,
		},
	}
}

// RequiresConsent reports whether the model takes a reference voice and so
// carries the consent property (every MM139 voice model does).
func (m LabModel) RequiresConsent() bool {
	return strings.Contains(string(m.ParamsSchema), `"consent"`)
}
