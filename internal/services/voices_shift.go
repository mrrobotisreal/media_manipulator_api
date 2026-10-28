package services

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Voice shifter (MM77, design VOICES.md §2): pitch and formant through
// ffmpeg's rubberband filter. Deterministic, CPU only, no model, no
// watermark. The preset table is the one the UI mirrors.

// Formant modes rubberband accepts.
const (
	FormantPreserved = "preserved"
	FormantShifted   = "shifted"
)

// VoiceShiftMaxSemitones is the clamp on |semitones|.
const VoiceShiftMaxSemitones = 12.0

// VoiceShiftPreset is one named shifter setting.
type VoiceShiftPreset struct {
	ID        string  `json:"id"`
	Semitones float64 `json:"semitones"`
	Formant   string  `json:"formant"`
	// Filter is an extra ffmpeg audio filter appended after rubberband
	// (telephone's band-pass); empty for the pitch-only presets.
	Filter string `json:"filter,omitempty"`
}

// VoiceShiftPresets is the one table of shifter presets. Formant "shifted"
// moves the vocal-tract resonances with the pitch (a smaller or larger
// speaker); "preserved" keeps the speaker and changes only the pitch.
var VoiceShiftPresets = []VoiceShiftPreset{
	{ID: "deeper", Semitones: -4, Formant: FormantShifted},
	{ID: "higher", Semitones: 4, Formant: FormantShifted},
	{ID: "child", Semitones: 7, Formant: FormantShifted},
	{ID: "elder", Semitones: -2, Formant: FormantPreserved},
	{ID: "telephone", Semitones: 0, Formant: FormantPreserved, Filter: "highpass=f=300,lowpass=f=3400"},
}

// VoiceShiftPresetByID looks a preset up; ok=false when unknown.
func VoiceShiftPresetByID(id string) (VoiceShiftPreset, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range VoiceShiftPresets {
		if p.ID == id {
			return p, true
		}
	}
	return VoiceShiftPreset{}, false
}

// VoiceShiftSettings is a resolved shift: preset wins over explicit values.
type VoiceShiftSettings struct {
	Semitones float64
	Formant   string
	Filter    string
	Preset    string
}

// ResolveVoiceShift validates and resolves the request's shift options.
// semitones are clamped to ±12; formant defaults to preserved.
func ResolveVoiceShift(preset string, semitones float64, formant string) (VoiceShiftSettings, error) {
	if strings.TrimSpace(preset) != "" {
		p, ok := VoiceShiftPresetByID(preset)
		if !ok {
			return VoiceShiftSettings{}, fmt.Errorf("unknown shift preset")
		}
		return VoiceShiftSettings{Semitones: p.Semitones, Formant: p.Formant, Filter: p.Filter, Preset: p.ID}, nil
	}
	if math.IsNaN(semitones) || math.IsInf(semitones, 0) {
		return VoiceShiftSettings{}, fmt.Errorf("semitones must be a number")
	}
	semitones = math.Max(-VoiceShiftMaxSemitones, math.Min(VoiceShiftMaxSemitones, semitones))
	switch f := strings.ToLower(strings.TrimSpace(formant)); f {
	case "", FormantPreserved:
		formant = FormantPreserved
	case FormantShifted:
		formant = FormantShifted
	default:
		return VoiceShiftSettings{}, fmt.Errorf("formant must be preserved or shifted")
	}
	return VoiceShiftSettings{Semitones: semitones, Formant: formant}, nil
}

// VoicePitchRatio is 2^(semitones/12), formatted to six decimals as ffmpeg's
// rubberband `pitch` option takes it (+3 → 1.189207).
func VoicePitchRatio(semitones float64) string {
	return strconv.FormatFloat(math.Pow(2, semitones/12), 'f', 6, 64)
}

// VoiceShiftFilter is the -af filter graph for a resolved shift.
func VoiceShiftFilter(s VoiceShiftSettings) string {
	f := "rubberband=pitch=" + VoicePitchRatio(s.Semitones) + ":formant=" + s.Formant
	if s.Filter != "" {
		f += "," + s.Filter
	}
	return f
}

// VoiceShiftArgs is the ffmpeg argument list that shifts input into a 16-bit
// PCM WAV at output (audio only; a video input's picture is dropped).
func VoiceShiftArgs(input, output string, s VoiceShiftSettings) []string {
	return []string{"-hide_banner", "-nostdin", "-y", "-i", input, "-vn", "-sn", "-dn",
		"-af", VoiceShiftFilter(s), "-c:a", "pcm_s16le", output}
}

// VoiceMP3Args encodes a WAV to MP3 (libmp3lame, VBR quality 2).
func VoiceMP3Args(input, output string) []string {
	return []string{"-hide_banner", "-nostdin", "-y", "-i", input, "-vn", "-c:a", "libmp3lame", "-q:a", "2", output}
}

// VoiceWavArgs normalises any audio (or a video's audio track) to the mono
// 24 kHz 16-bit WAV the voice engines read. maxSeconds > 0 truncates.
func VoiceWavArgs(input, output string, maxSeconds int) []string {
	args := []string{"-hide_banner", "-nostdin", "-y", "-i", input, "-vn", "-sn", "-dn"}
	if maxSeconds > 0 {
		args = append(args, "-t", strconv.Itoa(maxSeconds))
	}
	return append(args, "-ac", "1", "-ar", "24000", "-c:a", "pcm_s16le", output)
}
