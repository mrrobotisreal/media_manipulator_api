package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func TestValidateRestoreClipWindow(t *testing.T) {
	const maxSeconds = 15.0
	tests := []struct {
		name    string
		start   float64
		end     float64
		wantErr string // substring; "" means valid
	}{
		{name: "valid mid-range", start: 12.4, end: 22.4, wantErr: ""},
		{name: "boundary exactly max", start: 0, end: 15, wantErr: ""},
		{name: "boundary exactly min", start: 1, end: 1.5, wantErr: ""},
		{name: "too long", start: 0, end: 15.01, wantErr: "too long"},
		{name: "zero window", start: 5, end: 5, wantErr: "after clip start"},
		{name: "negative window", start: 10, end: 5, wantErr: "after clip start"},
		{name: "negative start", start: -1, end: 4, wantErr: "negative"},
		{name: "below minimum window", start: 0, end: 0.4, wantErr: "too short"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRestoreClipWindow(tt.start, tt.end, maxSeconds)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid window, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestNormalizeRestoreModels(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []models.RestoreModelID
		wantErr bool
	}{
		{
			name:  "all six",
			input: []string{"realesrgan", "swinir", "hat", "basicvsrpp", "rvrt", "vrt"},
			want: []models.RestoreModelID{
				models.RestoreModelRealESRGAN, models.RestoreModelSwinIR, models.RestoreModelHAT,
				models.RestoreModelBasicVSRPP, models.RestoreModelRVRT, models.RestoreModelVRT,
			},
		},
		{
			name:  "dedupe preserves first-seen order",
			input: []string{"vrt", "realesrgan", "vrt", "realesrgan"},
			want:  []models.RestoreModelID{models.RestoreModelVRT, models.RestoreModelRealESRGAN},
		},
		{
			name:  "case and whitespace normalized",
			input: []string{" RealESRGAN ", "SWINIR"},
			want:  []models.RestoreModelID{models.RestoreModelRealESRGAN, models.RestoreModelSwinIR},
		},
		{name: "empty list", input: nil, wantErr: true},
		{name: "unknown model", input: []string{"realesrgan", "topaz"}, wantErr: true},
		{name: "injection attempt", input: []string{"realesrgan; rm -rf /"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeRestoreModels(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("expected %v, got %v", tt.want, got)
				}
			}
		})
	}
}

func TestResolveRestoreScale(t *testing.T) {
	tests := []struct {
		name         string
		scale        int
		sourceHeight int
		want         int
		wantErr      bool
	}{
		{name: "auto low-res gets 4x", scale: 0, sourceHeight: 480, want: 4},
		{name: "auto 540p boundary gets 4x", scale: 0, sourceHeight: 540, want: 4},
		{name: "auto 541p gets 2x", scale: 0, sourceHeight: 541, want: 2},
		{name: "auto 1080p gets 2x", scale: 0, sourceHeight: 1080, want: 2},
		{name: "explicit 2x always allowed", scale: 2, sourceHeight: 1080, want: 2},
		{name: "explicit 4x at 1080p allowed", scale: 4, sourceHeight: 1080, want: 4},
		{name: "explicit 4x at 720p allowed", scale: 4, sourceHeight: 720, want: 4},
		{name: "explicit 4x above 1080p rejected", scale: 4, sourceHeight: 1081, wantErr: true},
		{name: "invalid scale 3", scale: 3, sourceHeight: 480, wantErr: true},
		{name: "invalid negative scale", scale: -2, sourceHeight: 480, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveRestoreScale(tt.scale, tt.sourceHeight)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got scale %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected scale %d, got %d", tt.want, got)
			}
		})
	}
}

func TestValidateRestoreFrameBudget(t *testing.T) {
	const maxFrames = 450
	tests := []struct {
		name    string
		window  float64
		fps     float64
		wantErr bool
	}{
		{name: "10s at 30fps fits", window: 10, fps: 30, wantErr: false},
		{name: "15s at 30fps boundary fits", window: 15, fps: 30, wantErr: false},
		{name: "15s at 60fps exceeds", window: 15, fps: 60, wantErr: true},
		{name: "8s at 60fps fits", window: 7.5, fps: 60, wantErr: false},
		{name: "ntsc fractional fps fits", window: 15, fps: 29.97, wantErr: false},
		{name: "unknown fps passes (checked later via real count)", window: 15, fps: 0, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRestoreFrameBudget(tt.window, tt.fps, maxFrames)
			if tt.wantErr && err == nil {
				t.Fatalf("expected frame-budget error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	// The rejection message must tell the user the max duration for THEIR fps.
	err := ValidateRestoreFrameBudget(15, 60, maxFrames)
	if err == nil {
		t.Fatal("expected error for 15s at 60fps")
	}
	if !strings.Contains(err.Error(), "60") || !strings.Contains(err.Error(), "7.5") {
		t.Fatalf("expected fps-aware message with limit 7.5s at 60 fps, got %q", err.Error())
	}
}

// restoreTestService builds a RestoreService whose model paths point at real
// temp files, so availability checks pass deterministically.
func restoreTestService(t *testing.T, basicVSRPPEnabled bool) *RestoreService {
	t.Helper()
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("stub"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
		return p
	}
	cfg := &config.Config{
		RestoreEnabled:           true,
		RestoreBasicVSRPPEnabled: basicVSRPPEnabled,
		RestoreMaxClipSeconds:    15,
		RestoreMaxFrames:         450,
		RestoreMaxConcurrentJobs: 1,
		RestoreModelTimeout:      30 * time.Second,
		RestoreResultPresignTTL:  6 * time.Hour,
		RealESRGANBin:            mk("realesrgan-ncnn-vulkan"),
		AIRestorePython:          mk("python-sr"),
		AIRestoreMMPython:        mk("python-mm"),
		AIRestoreFramesScript:    mk("restore_frames.py"),
		AIRestoreVideoScript:     mk("restore_video.py"),
		RestoreEstSecondsPerFrame: map[string]float64{
			"realesrgan": 0.8, "swinir": 3.5, "hat": 5.0,
			"basicvsrpp": 1.0, "rvrt": 3.0, "vrt": 7.0,
		},
		RestoreVRAMMiB: map[string]int64{
			"realesrgan": 3000, "swinir": 9000, "hat": 10000,
			"basicvsrpp": 11000, "rvrt": 12000, "vrt": 14000,
		},
	}
	return NewRestoreService(cfg, NewJobManager(), nil, nil, nil, nil)
}

func TestModelAvailabilityBasicVSRPPFlag(t *testing.T) {
	svc := restoreTestService(t, false)
	available, reason := svc.ModelAvailability(models.RestoreModelBasicVSRPP)
	if available {
		t.Fatal("expected basicvsrpp to be unavailable when RESTORE_BASICVSRPP_ENABLED=false")
	}
	if !strings.Contains(reason, "disabled") {
		t.Fatalf("expected a clear disabled reason, got %q", reason)
	}

	svc = restoreTestService(t, true)
	available, reason = svc.ModelAvailability(models.RestoreModelBasicVSRPP)
	if !available {
		t.Fatalf("expected basicvsrpp available with flag on and paths present, got reason %q", reason)
	}
}

func TestModelAvailabilityMissingPaths(t *testing.T) {
	svc := restoreTestService(t, true)
	svc.cfg.RealESRGANBin = filepath.Join(t.TempDir(), "missing-bin")
	if available, _ := svc.ModelAvailability(models.RestoreModelRealESRGAN); available {
		t.Fatal("expected realesrgan unavailable when binary is missing")
	}
	svc.cfg.AIRestorePython = filepath.Join(t.TempDir(), "missing-python")
	for _, id := range []models.RestoreModelID{models.RestoreModelSwinIR, models.RestoreModelHAT, models.RestoreModelRVRT, models.RestoreModelVRT} {
		if available, _ := svc.ModelAvailability(id); available {
			t.Fatalf("expected %s unavailable when python venv is missing", id)
		}
	}
}

func TestCapabilitiesShape(t *testing.T) {
	svc := restoreTestService(t, true)
	caps := svc.Capabilities()
	if !caps.Enabled {
		t.Fatal("expected enabled=true")
	}
	if len(caps.Models) != 6 {
		t.Fatalf("expected 6 models, got %d", len(caps.Models))
	}
	if caps.Models[0].ID != models.RestoreModelRealESRGAN || caps.Models[0].Group != models.RestoreGroupFrame {
		t.Fatalf("unexpected first model: %+v", caps.Models[0])
	}
	if got := caps.Models[0].Scales; len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("expected realesrgan scales [2 4], got %v", got)
	}
	for _, m := range caps.Models[1:] {
		if len(m.Scales) != 1 || m.Scales[0] != 4 {
			t.Fatalf("expected %s scales [4], got %v", m.ID, m.Scales)
		}
	}
	if caps.Models[3].ID != models.RestoreModelBasicVSRPP || caps.Models[3].Group != models.RestoreGroupVideo {
		t.Fatalf("unexpected fourth model: %+v", caps.Models[3])
	}
}

func TestOrderRestoreModels(t *testing.T) {
	selected := []models.RestoreModelID{
		models.RestoreModelVRT, models.RestoreModelRealESRGAN, models.RestoreModelHAT, models.RestoreModelBasicVSRPP,
	}
	got := orderRestoreModels(selected)
	want := []models.RestoreModelID{
		models.RestoreModelRealESRGAN, models.RestoreModelBasicVSRPP, models.RestoreModelHAT, models.RestoreModelVRT,
	}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected run order %v, got %v", want, got)
		}
	}
}
