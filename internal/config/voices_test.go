package config

import (
	"testing"
	"time"
)

func TestVoicesConfigDefaults(t *testing.T) {
	for _, k := range []string{"VOICES_ENABLED", "VOICES_URL", "VOICES_TIMEOUT_SECONDS", "VOICES_SCRATCH_DIR",
		"VOICES_VRAM_MIB", "GPU_SCHEDULER_DEFAULT_VOICES_DEVICE", "VOICES_RATE_LIMIT_PER_SESSION_PER_HOUR",
		"VOICES_RATE_LIMIT_PER_IP_PER_HOUR", "VOICES_NO_SPEECH_THRESHOLD"} {
		t.Setenv(k, "")
	}
	c := Load()
	v := c.Voices
	if v.Enabled {
		t.Error("voices must ship dark")
	}
	if v.URL != "http://127.0.0.1:8791" || v.Timeout != 300*time.Second || v.VRAMMiB != 6000 {
		t.Errorf("defaults: %+v", v)
	}
	if v.ScratchDir != "/opt/media-manipulator-ai/tmp/voices" {
		t.Errorf("scratch: %q", v.ScratchDir)
	}
	if c.GPUSchedulerDefaultVoicesDevice != "cuda:0" || v.CUDAIndex != 0 {
		t.Errorf("device: %q %d", c.GPUSchedulerDefaultVoicesDevice, v.CUDAIndex)
	}
	if v.RateLimitPerSessionPerHour != 5 || v.RateLimitPerIPPerHour != 20 || v.PreviewPerDescriptionPerHour != 3 {
		t.Errorf("buckets: %+v", v)
	}
	if v.NoSpeechThreshold != 0.6 || !v.SpeechCheck {
		t.Errorf("speech check: %+v", v)
	}
}

func TestVoicesConfigEnv(t *testing.T) {
	t.Setenv("VOICES_ENABLED", "true")
	t.Setenv("VOICES_URL", "http://127.0.0.1:9999/")
	t.Setenv("VOICES_TIMEOUT_SECONDS", "42")
	t.Setenv("GPU_SCHEDULER_DEFAULT_VOICES_DEVICE", "cuda:1")
	t.Setenv("VOICES_VRAM_MIB", "7000")
	c := Load()
	if !c.Voices.Enabled || c.Voices.URL != "http://127.0.0.1:9999" || c.Voices.Timeout != 42*time.Second {
		t.Errorf("env not applied: %+v", c.Voices)
	}
	if c.Voices.CUDAIndex != 1 || c.Voices.VRAMMiB != 7000 {
		t.Errorf("device/vram: %+v", c.Voices)
	}
}

func TestCudaIndexOf(t *testing.T) {
	for in, want := range map[string]int{"cuda:0": 0, "cuda:1": 1, "CUDA:3": 3, "2": 2, "": 9, "vulkan:1": 9, "cuda:x": 9} {
		if got := cudaIndexOf(in, 9); got != want {
			t.Errorf("cudaIndexOf(%q) = %d want %d", in, got, want)
		}
	}
}
