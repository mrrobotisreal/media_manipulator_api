package handlers

import (
	"path/filepath"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// The 2026-11-02 stopgap (81258aa) skipped the background analysis for AI-tool
// jobs because its Ollama vision model filled the AI GPU and made
// remove_background fail. MM94 schedules every AI tool and Ollama call through
// the GPU scheduler (the analysis at background priority), so AI-tool jobs are
// analysed again — inverted deliberately here — but only AFTER the job has
// completed, never alongside it (TestAnalysisTimingFor).
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
		{"remove_background", ai(true, "remove_background"), true},
		{"upscale", ai(true, "ai_upscale"), true},
		{"clean voice", ai(true, "clean_voice"), true},
		{"isolate vocals", ai(true, "isolate_vocals"), true},
		{"frame interpolation", ai(true, "frame_interpolation"), true},
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

// Plain conversions are analysed at upload (background priority); AI-tool jobs
// only after they complete, so the analysis never runs next to the job's own
// GPU step.
func TestAnalysisTimingFor(t *testing.T) {
	ai := func(enabled bool, op string) map[string]interface{} {
		return map[string]interface{}{"ai": map[string]interface{}{"enabled": enabled, "operation": op}}
	}
	cases := []struct {
		name string
		opts map[string]interface{}
		want analysisTiming
	}{
		{"plain conversion", map[string]interface{}{"format": "png"}, analysisAtUpload},
		{"remove_background", ai(true, "remove_background"), analysisAfterJob},
		{"frame interpolation", ai(true, "frame_interpolation"), analysisAfterJob},
		{"ai disabled", ai(false, "remove_background"), analysisAtUpload},
		{"ai none", ai(true, "none"), analysisAtUpload},
		{"transcribe", map[string]interface{}{"mode": "transcribe"}, analysisNone},
		{"specialized", map[string]interface{}{"mode": "extract_audio"}, analysisNone},
	}
	for _, c := range cases {
		if got := analysisTimingFor(&models.ConversionJob{Options: c.opts}); got != c.want {
			t.Errorf("%s: analysisTimingFor = %v, want %v", c.name, got, c.want)
		}
	}
}

// An AI-tool job's analysis is held back at upload and dropped when the job
// fails; a plain conversion's goes to the queue at once.
func TestPlanAnalysisHoldsAIJobsUntilCompletion(t *testing.T) {
	var got []services.AnalysisJob
	jm := services.NewJobManager()
	h := &ConversionHandler{jobManager: jm, cfg: &config.Config{}, converter: services.NewConverter(&config.Config{}), enqueueAnalysis: func(aj services.AnalysisJob) { got = append(got, aj) }}

	plain := jm.CreateJob(models.OriginalFileInfo{Name: "a.png", Type: "image/png"}, map[string]interface{}{"format": "jpg"})
	if after := h.planAnalysis(plain, services.AnalysisJob{JobID: plain.ID}); after != nil || len(got) != 1 {
		t.Fatalf("plain conversion: after=%v enqueued=%d", after, len(got))
	}

	aiJob := jm.CreateJob(models.OriginalFileInfo{Name: "a.png", Type: "image/png"},
		map[string]interface{}{"format": "png", "ai": map[string]interface{}{"enabled": true, "operation": "remove_background"}})
	after := h.planAnalysis(aiJob, services.AnalysisJob{JobID: aiJob.ID})
	if after == nil || len(got) != 1 {
		t.Fatalf("AI job must not be enqueued at upload: after=%v enqueued=%d", after, len(got))
	}

	// The job fails (missing input): the held analysis is dropped.
	h.processConversion(aiJob, filepath.Join(t.TempDir(), "missing.png"), t.TempDir(), after)
	if len(got) != 1 {
		t.Fatalf("a failed AI job must not be analysed, enqueued=%d", len(got))
	}
	if j, _ := jm.GetJob(aiJob.ID); j.Status != models.StatusFailed {
		t.Fatalf("job should have failed, got %s", j.Status)
	}
}
