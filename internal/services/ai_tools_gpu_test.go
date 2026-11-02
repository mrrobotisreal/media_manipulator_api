package services

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MM94 S3: every upload AI tool runs its GPU subprocess inside a slot pinned
// to the card the tool is configured for, and the card in the subprocess's
// env/argv is that same card — so one setting (AI_CUDA_GPU, AI_VULKAN_GPU,
// AI_RIFE_GPU) rules both.

type aiGPURecorder struct {
	mu   sync.Mutex
	held int // slots currently held
	reqs []GPURequest
	runs []aiRun
}

type aiRun struct {
	label string
	env   []string
	name  string
	args  []string
	held  int // slots held while it ran
}

func (r *aiGPURecorder) acquire(_ context.Context, _ *JobManager, _ string, req GPURequest) (int, func(), error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.held++
	r.mu.Unlock()
	idx := -1
	if req.ForceIndex != nil {
		idx = *req.ForceIndex
	}
	return idx, func() {
		r.mu.Lock()
		r.held--
		r.mu.Unlock()
	}, nil
}

func (r *aiGPURecorder) run(_ context.Context, label string, env []string, name string, args ...string) error {
	r.mu.Lock()
	r.runs = append(r.runs, aiRun{label: label, env: env, name: name, args: args, held: r.held})
	r.mu.Unlock()
	// Leave the outputs the post-processing looks for.
	switch label {
	case "deepFilter":
		dir := argAfter(args, "--output-dir")
		_ = os.WriteFile(filepath.Join(dir, "input_48k_mono_DeepFilterNet3.wav"), []byte("wav"), 0o644)
	case "demucs":
		dir := filepath.Join(argAfter(args, "-o"), "htdemucs", "in")
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, AIAudioDemucsTrackVocals), []byte("wav"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, AIAudioDemucsTrackNoVox), []byte("wav"), 0o644)
	}
	return nil
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func envValue(env []string, key string) (string, bool) {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return strings.TrimPrefix(e, key+"="), true
		}
	}
	return "", false
}

// gpuRun is the one run of r that held a slot.
func (r *aiGPURecorder) gpuRun(t *testing.T) aiRun {
	t.Helper()
	var out []aiRun
	for _, run := range r.runs {
		if run.held > 0 {
			out = append(out, run)
		}
	}
	if len(out) != 1 || len(r.reqs) != 1 {
		t.Fatalf("want exactly one slot and one subprocess under it, got %d slots, runs %+v", len(r.reqs), r.runs)
	}
	for _, run := range r.runs {
		if run.held == 0 && run.name != "ffmpeg" {
			t.Fatalf("GPU subprocess %q ran without a slot", run.label)
		}
	}
	return out[0]
}

func newAITestService(t *testing.T, cuda, vulkan, rife int) (*AIService, *aiGPURecorder) {
	t.Helper()
	dir := t.TempDir()
	touch := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	modelDir := filepath.Join(dir, "models", "rife-v4.6")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		TempDir:                     dir,
		AICUDAGPU:                   cuda,
		AIVulkanGPU:                 vulkan,
		AIRIFEGPU:                   rife,
		DeepFilterBin:               "deepFilter",
		DemucsBin:                   "demucs",
		VisionPython:                "python",
		FacePrivacyScript:           "face_privacy.py",
		TextRedactScript:            "redact.py",
		RembgBin:                    "rembg",
		RembgEnvScript:              "/opt/env/onnxruntime-cuda-bg-remove.sh",
		RembgModelDir:               "/opt/models/rembg",
		RealESRGANBin:               "realesrgan",
		LamaPython:                  "lama-python",
		RemoveObjectScript:          "remove_object.py",
		AIFrameInterpolationEnabled: true,
		AIFrameInterpolationScript:  touch("rife.py"),
		AIRIFEBin:                   touch("rife-bin"),
		AIRIFEModel:                 modelDir,
		AIToolVRAMMiB:               map[string]int64{aiGPUKeyRemoveBackground: 1234},
	}
	rec := &aiGPURecorder{}
	a := NewAIService(cfg)
	jm := NewJobManager()
	a.SetJobManager(jm)
	a.acquire = rec.acquire
	a.run = rec.run
	return a, rec
}

func TestAITools_SlotCardMatchesSubprocessCard(t *testing.T) {
	orig := imageDimensionsFn
	imageDimensionsFn = func(context.Context, string) (int, int, error) { return 64, 32, nil }
	t.Cleanup(func() { imageDimensionsFn = orig })

	const cuda, vulkan, rife = 3, 2, 5
	cases := []struct {
		kind string
		card int
		call func(a *AIService, out string) error
		// subprocessCard reads the card the subprocess was pointed at.
		subprocessCard func(t *testing.T, run aiRun) string
	}{
		{aiGPUKeyRemoveBackground, cuda, func(a *AIService, out string) error {
			return a.RemoveBackground(context.Background(), "job", "/in.png", out+".png", "")
		}, func(t *testing.T, run aiRun) string {
			// Exported after the env script is sourced, so it wins over the
			// script's own default.
			script := run.args[len(run.args)-1]
			src := strings.Index(script, "source ")
			exp := strings.Index(script, "CUDA_VISIBLE_DEVICES=")
			if src < 0 || exp < src {
				t.Fatalf("CUDA_VISIBLE_DEVICES must be exported after sourcing the env script: %s", script)
			}
			rest := script[exp+len("CUDA_VISIBLE_DEVICES="):]
			return rest[:strings.IndexAny(rest, "; ")]
		}},
		{aiGPUKeyUpscale, vulkan, func(a *AIService, out string) error {
			return a.UpscaleImage(context.Background(), "job", "/in.png", out+".png", 4, "")
		}, func(_ *testing.T, run aiRun) string { return argAfter(run.args, "-g") }},
		{aiGPUKeyFacePrivacy, cuda, func(a *AIService, out string) error {
			return a.FacePrivacy(context.Background(), "job", "/in.png", out+".png", "blur", "")
		}, cudaCard},
		{aiGPUKeyRedactText, cuda, func(a *AIService, out string) error {
			return a.RedactText(context.Background(), "job", "/in.png", out+".png", "", "")
		}, cudaCard},
		{aiGPUKeyRemoveObject, cuda, func(a *AIService, out string) error {
			return a.RemoveObject(context.Background(), "job", "/in.png", out+".png", []models.NormalizedRect{{X: 0.1, Y: 0.1, Width: 0.5, Height: 0.5}})
		}, cudaCard},
		{aiGPUKeyDeepFilter, cuda, func(a *AIService, out string) error {
			return a.CleanVoice(context.Background(), "job", "/in.wav", out+".wav", true)
		}, cudaCard},
		{aiGPUKeyDeepFilter, cuda, func(a *AIService, out string) error {
			return a.RemoveBackgroundNoise(context.Background(), "job", "/in.wav", out+".wav")
		}, cudaCard},
		{aiGPUKeyDemucs, cuda, func(a *AIService, out string) error {
			return a.SeparateVocals(context.Background(), "job", "/in.wav", out+".wav", AIAudioOpIsolateVocals)
		}, cudaCard},
		{aiGPUKeyDemucs, cuda, func(a *AIService, out string) error {
			return a.SeparateVocals(context.Background(), "job", "/in.wav", out+".wav", AIAudioOpRemoveVocals)
		}, cudaCard},
		{aiGPUKeyFrameInterpolation, rife, func(a *AIService, out string) error {
			return a.InterpolateFrames(context.Background(), "job", "/in.mp4", out+".mp4", models.AIFrameInterpolationOptions{})
		}, func(_ *testing.T, run aiRun) string { return argAfter(run.args, "--gpu") }},
		{aiGPUKeyFacePrivacy + "[detect]", cuda, func(a *AIService, _ string) error {
			_, err := a.DetectFaces(context.Background(), "/in.png", GPUPriorityPremium)
			if os.IsNotExist(err) || (err != nil && strings.Contains(err.Error(), "read detect json")) {
				return nil // the fake runner writes no JSON; only the slot matters here
			}
			return err
		}, cudaCard},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			a, rec := newAITestService(t, cuda, vulkan, rife)
			if err := c.call(a, filepath.Join(t.TempDir(), "out")); err != nil {
				t.Fatalf("op failed: %v", err)
			}
			run := rec.gpuRun(t)
			req := rec.reqs[0]
			if req.Kind != c.kind {
				t.Errorf("Kind = %q, want %q", req.Kind, c.kind)
			}
			if req.ForceIndex == nil || *req.ForceIndex != c.card {
				t.Fatalf("ForceIndex = %v, want %d", req.ForceIndex, c.card)
			}
			if got := c.subprocessCard(t, run); got != strconv.Itoa(*req.ForceIndex) {
				t.Fatalf("subprocess card %q != slot card %d", got, *req.ForceIndex)
			}
			if req.VRAMRequiredMiB <= 0 {
				t.Errorf("VRAMRequiredMiB must be set, got %d", req.VRAMRequiredMiB)
			}
			if rec.held != 0 {
				t.Errorf("slot not released (held=%d)", rec.held)
			}
		})
	}
}

func cudaCard(t *testing.T, run aiRun) string {
	t.Helper()
	if order, _ := envValue(run.env, "CUDA_DEVICE_ORDER"); order != "PCI_BUS_ID" {
		t.Fatalf("CUDA_DEVICE_ORDER must be PCI_BUS_ID (nvidia-smi order), env %v", run.env)
	}
	v, ok := envValue(run.env, "CUDA_VISIBLE_DEVICES")
	if !ok {
		t.Fatalf("no CUDA_VISIBLE_DEVICES in env %v", run.env)
	}
	return v
}

// VRAM comes from cfg.AIToolVRAMMiB (AI_VRAM_MIB_<KEY>) and the priority from
// the job's recorded tier priority.
func TestAITools_SlotVRAMAndPriority(t *testing.T) {
	a, rec := newAITestService(t, 1, 1, 1)
	a.jobManager.SetJobGPUPriority("job", GPUPriorityPremium)
	if err := a.RemoveBackground(context.Background(), "job", "/in.png", filepath.Join(t.TempDir(), "o.png"), ""); err != nil {
		t.Fatal(err)
	}
	if err := a.UpscaleImage(context.Background(), "other", "/in.png", filepath.Join(t.TempDir(), "o.png"), 2, ""); err != nil {
		t.Fatal(err)
	}
	if rec.reqs[0].VRAMRequiredMiB != 1234 || rec.reqs[0].Priority != GPUPriorityPremium {
		t.Fatalf("configured VRAM / Premium priority: %+v", rec.reqs[0])
	}
	if rec.reqs[1].VRAMRequiredMiB != defaultAIToolVRAMMiB || rec.reqs[1].Priority != GPUPriorityStandard {
		t.Fatalf("default VRAM / standard priority: %+v", rec.reqs[1])
	}
}

// With the scheduler disabled (acquire returns -1) the tool runs on its
// configured card, exactly as before MM94.
func TestAITools_DisabledSchedulerKeepsConfiguredCard(t *testing.T) {
	a, rec := newAITestService(t, 1, 1, 1)
	a.acquire = func(_ context.Context, _ *JobManager, _ string, req GPURequest) (int, func(), error) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, req)
		rec.held++
		rec.mu.Unlock()
		return -1, func() { rec.mu.Lock(); rec.held--; rec.mu.Unlock() }, nil
	}
	a.cfg.AICUDAGPU = 7
	if err := a.FacePrivacy(context.Background(), "job", "/in.png", filepath.Join(t.TempDir(), "o.png"), "blur", ""); err != nil {
		t.Fatal(err)
	}
	if got := cudaCard(t, rec.gpuRun(t)); got != "7" {
		t.Fatalf("disabled scheduler must keep AI_CUDA_GPU, got %s", got)
	}
}
