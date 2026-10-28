package config

import (
	"strings"
	"time"
)

// VoicesConfig is the voices feature (MM77, ADR-0015, design VOICES.md §3):
// the Go side of the resident mm-voices service on the GPU host. Kept in its
// own struct and file so the feature's keys sit together and the main Config
// literal stays untouched by it.
type VoicesConfig struct {
	// Enabled is the kill switch (VOICES_ENABLED, default false). Off, every
	// /api/voices/* job route answers 503 voices_unavailable and capabilities
	// reports enabled:false.
	Enabled bool
	// URL is the service base URL (VOICES_URL, default http://127.0.0.1:8791).
	URL string
	// Timeout bounds one synthesis / conversion call (VOICES_TIMEOUT_SECONDS,
	// default 300: covers a 10 to 20 s cold load plus a 15-minute Pro convert).
	Timeout time.Duration
	// ScratchDir is the directory shared with the service (VOICES_SCRATCH_DIR).
	// The API writes inputs there and the service writes outputs there; the
	// service refuses any path outside it. Must equal the unit's value.
	ScratchDir string
	// VRAMMiB sizes the TaskVoices GPU lease (VOICES_VRAM_MIB, default 6000).
	VRAMMiB int64
	// CUDAIndex is the physical GPU index the service is pinned to, parsed from
	// GPU_SCHEDULER_DEFAULT_VOICES_DEVICE ("cuda:0" → 0). The lease is forced
	// onto that card so it serialises against Whisper there.
	CUDAIndex int
	// RateLimitPerSessionPerHour / PerIPPerHour are the voices_create bucket on
	// POST /api/voices and POST /api/voices/design.
	RateLimitPerSessionPerHour int
	RateLimitPerIPPerHour      int
	// PreviewPerDescriptionPerHour caps uncounted design previews per
	// description hash per session (design §5: 3).
	PreviewPerDescriptionPerHour int
	// ConsentTextVersion is stored with every cloned voice so the consent copy
	// the user agreed to can be identified later (VOICES_CONSENT_TEXT_VERSION).
	ConsentTextVersion string
	// SpeechCheck turns the create-voice Whisper no_speech_prob pass on
	// (VOICES_SPEECH_CHECK, default true). NoSpeechThreshold is the mean
	// no_speech_prob above which a reference is refused (default 0.6).
	SpeechCheck       bool
	NoSpeechThreshold float64
	// WhisperModel is the model the speech check uses (VOICES_SPEECH_CHECK_MODEL,
	// default "small": the check only needs no_speech_prob, not a transcript).
	WhisperModel string
}

// GPUSchedulerDefaultVoicesDevice is read by internal/gpu.pickDevice for
// TaskVoices; it lives beside the other GPU_SCHEDULER_DEFAULT_*_DEVICE keys in
// spirit but is loaded here so Config's literal is untouched.
func loadVoicesConfig() (VoicesConfig, string) {
	device := getEnv("GPU_SCHEDULER_DEFAULT_VOICES_DEVICE", "cuda:0")
	return VoicesConfig{
		Enabled:                      getEnvBool("VOICES_ENABLED", false),
		URL:                          strings.TrimRight(getEnv("VOICES_URL", "http://127.0.0.1:8791"), "/"),
		Timeout:                      time.Duration(getEnvInt("VOICES_TIMEOUT_SECONDS", 300)) * time.Second,
		ScratchDir:                   getEnv("VOICES_SCRATCH_DIR", "/opt/media-manipulator-ai/tmp/voices"),
		VRAMMiB:                      getEnvInt64("VOICES_VRAM_MIB", 6000),
		CUDAIndex:                    cudaIndexOf(device, 0),
		RateLimitPerSessionPerHour:   getEnvInt("VOICES_RATE_LIMIT_PER_SESSION_PER_HOUR", 5),
		RateLimitPerIPPerHour:        getEnvInt("VOICES_RATE_LIMIT_PER_IP_PER_HOUR", 20),
		PreviewPerDescriptionPerHour: getEnvInt("VOICES_DESIGN_PREVIEWS_PER_HOUR", 3),
		ConsentTextVersion:           getEnv("VOICES_CONSENT_TEXT_VERSION", "2026-10-28"),
		SpeechCheck:                  getEnvBool("VOICES_SPEECH_CHECK", true),
		NoSpeechThreshold:            getEnvFloat("VOICES_NO_SPEECH_THRESHOLD", 0.6),
		WhisperModel:                 getEnv("VOICES_SPEECH_CHECK_MODEL", "small"),
	}, device
}

// cudaIndexOf parses "cuda:N" (or a bare "N"); anything else is fallback.
func cudaIndexOf(device string, fallback int) int {
	d := strings.TrimSpace(strings.ToLower(device))
	d = strings.TrimPrefix(d, "cuda:")
	n := 0
	if d == "" {
		return fallback
	}
	for _, r := range d {
		if r < '0' || r > '9' {
			return fallback
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// applyVoicesConfig fills the voices block. Called from Load after the main
// literal, like applyProTierDefaults.
func applyVoicesConfig(c *Config) {
	c.Voices, c.GPUSchedulerDefaultVoicesDevice = loadVoicesConfig()
}
