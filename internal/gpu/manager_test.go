package gpu

import (
	"context"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func TestParseDeviceSpec(t *testing.T) {
	cases := []struct {
		raw     string
		ok      bool
		key     string
		backend string
		index   int
		totalMB int64
	}{
		{"cuda:0:RTX5060Ti:8192", true, "cuda:0", "cuda", 0, 8192},
		{"vulkan:1:RTX5080:16384", true, "vulkan:1", "vulkan", 1, 16384},
		{"ollama:0", true, "ollama:0", "ollama", 0, 0},
		{"bogus", false, "", "", 0, 0},
		{"cuda:nope", false, "", "", 0, 0},
	}
	for _, tc := range cases {
		got, ok := parseDeviceSpec(tc.raw)
		if ok != tc.ok {
			t.Errorf("parseDeviceSpec(%q) ok=%v want %v", tc.raw, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Key != tc.key {
			t.Errorf("%q key = %q want %q", tc.raw, got.Key, tc.key)
		}
		if got.Backend != tc.backend {
			t.Errorf("%q backend = %q want %q", tc.raw, got.Backend, tc.backend)
		}
		if got.Index != tc.index {
			t.Errorf("%q index = %d want %d", tc.raw, got.Index, tc.index)
		}
		if got.TotalMemoryMB != tc.totalMB {
			t.Errorf("%q totalMB = %d want %d", tc.raw, got.TotalMemoryMB, tc.totalMB)
		}
	}
}

func TestManager_AcquireRelease_NoStore(t *testing.T) {
	cfg := &config.Config{
		GPUSchedulerEnabled:              true,
		GPUSchedulerDevices:              []string{"cuda:0:Fake:8192", "cuda:1:Bigger:16384"},
		GPUSchedulerDefaultWhisperDevice: "cuda:1",
	}
	m := NewManager(cfg, nil, nil, nil)
	if !m.Enabled() {
		t.Fatalf("expected enabled manager")
	}
	lease, err := m.Acquire(context.Background(), TaskWhisper, "test", "", "")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease.Device().Key != "cuda:1" {
		t.Errorf("expected cuda:1, got %q", lease.Device().Key)
	}
	lease.Release(context.Background(), nil)
}

func TestManager_Disabled_FallsThrough(t *testing.T) {
	cfg := &config.Config{GPUSchedulerEnabled: false}
	m := NewManager(cfg, nil, nil, nil)
	lease, err := m.Acquire(context.Background(), TaskWhisper, "test", "", "")
	if err != nil {
		t.Fatalf("acquire on disabled manager should succeed: %v", err)
	}
	lease.Release(context.Background(), nil)
}

// MM77: TaskVoices honours GPU_SCHEDULER_DEFAULT_VOICES_DEVICE and defaults to
// cuda:0 (the card mm-voices is pinned to), even when another card is bigger.
func TestManager_PickDevice_Voices(t *testing.T) {
	devices := []string{"cuda:0:RTX5080:16303", "cuda:1:RTX5080:16304"}
	cases := []struct {
		pref, want string
	}{
		{"", "cuda:0"},
		{"cuda:0", "cuda:0"},
		{"cuda:1", "cuda:1"},
	}
	for _, tc := range cases {
		cfg := &config.Config{GPUSchedulerEnabled: true, GPUSchedulerDevices: devices, GPUSchedulerDefaultVoicesDevice: tc.pref}
		m := NewManager(cfg, nil, nil, nil)
		lease, err := m.Acquire(context.Background(), TaskVoices, "voice_tts", "", "")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if got := lease.Device().Key; got != tc.want {
			t.Errorf("pref %q: got %q want %q", tc.pref, got, tc.want)
		}
		lease.Release(context.Background(), nil)
	}
	if TaskVoices != "voices" {
		t.Fatalf("TaskVoices = %q", TaskVoices)
	}
}
