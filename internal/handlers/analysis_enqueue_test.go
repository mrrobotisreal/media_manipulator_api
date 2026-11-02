package handlers

import (
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// AI-tool jobs no longer get the background analysis: its Ollama vision model
// filled the AI GPU and made remove_background fail (2026-11-02).
func TestShouldEnqueueAnalysis(t *testing.T) {
	ai := func(enabled bool, op string) map[string]interface{} {
		return map[string]interface{}{"ai": map[string]interface{}{"enabled": enabled, "operation": op}}
	}
	cases := []struct {
		name string
		opts map[string]interface{}
		want bool
	}{
		{"plain conversion", map[string]interface{}{"format": "png"}, true},
		{"no options", nil, true},
		{"remove_background", ai(true, "remove_background"), false},
		{"upscale", ai(true, "ai_upscale"), false},
		{"clean voice", ai(true, "clean_voice"), false},
		{"isolate vocals", ai(true, "isolate_vocals"), false},
		{"frame interpolation", ai(true, "frame_interpolation"), false},
		{"ai disabled", ai(false, "remove_background"), true},
		{"ai none", ai(true, "none"), true},
		{"transcribe", map[string]interface{}{"mode": "transcribe"}, false},
		{"specialized", map[string]interface{}{"mode": "extract_audio"}, false},
	}
	for _, c := range cases {
		if got := shouldEnqueueAnalysis(&models.ConversionJob{Options: c.opts}); got != c.want {
			t.Errorf("%s: shouldEnqueueAnalysis = %v, want %v", c.name, got, c.want)
		}
	}
}
