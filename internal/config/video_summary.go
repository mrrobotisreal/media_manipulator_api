package config

import "time"

// VideoSummaryConfig is the video summary (MM74, docs/design/VIDEO_SUMMARY.md,
// ADR-0014) configuration. No value is required; with nothing set the feature
// is off (SUMMARY_ENABLED=false) and every other default is the design's.
type VideoSummaryConfig struct {
	// Enabled is SUMMARY_ENABLED (default false). Off: capabilities report
	// enabled:false and /api/summarize/start answers 503 before any charge.
	Enabled bool
	// OllamaURL is the daemon serving the VLM and the standard summary model:
	// the shared OLLAMA_URL (both models live on the same GPU-1 daemon).
	OllamaURL string
	// SummaryModel is OLLAMA_SUMMARY_MODEL (default qwen3.5:9b-q8_0, the text
	// model already installed as OLLAMA_TEXT_MODEL).
	SummaryModel string
	// DeepModel / DeepURL are OLLAMA_SUMMARY_DEEP_MODEL and
	// OLLAMA_SUMMARY_DEEP_URL (default empty = deep off). Both must be set for
	// deep synthesis to exist; it is then offered to tiers with SummaryDeep.
	DeepModel string
	DeepURL   string
	// MaxInputTokens is OLLAMA_SUMMARY_MAX_INPUT_TOKENS (default 12000): the
	// synthesis input budget before notes are dropped / the transcript condensed.
	MaxInputTokens int
	// Workers is SUMMARY_WORKERS (default 1): a serial queue like
	// ANALYSIS_WORKERS so two summaries never load models at once.
	Workers int
	// StageTimeout is SUMMARY_STAGE_TIMEOUT_SECONDS (default 1800): the bound
	// on each heavy stage (audio, frames, vision, synthesis). A timeout fails
	// the job, which refunds its ai_ops unit.
	StageTimeout time.Duration
	// OllamaTimeout is the per-call HTTP timeout, OLLAMA_TIMEOUT_SECONDS
	// (default 300, shared with the analysis queue).
	OllamaTimeout time.Duration
	// RateLimitPerSessionPerHour / PerIPPerHour are the summarize_start route
	// bucket (SUMMARY_RATE_LIMIT_PER_SESSION_PER_HOUR 6, _PER_IP_PER_HOUR 12).
	RateLimitPerSessionPerHour int
	RateLimitPerIPPerHour      int
	// Tier frame budgets (TIER_<T>_SUMMARY_MAX_FRAMES): anonymous 0 (the
	// feature requires sign-in), free 24, premium 48, pro 96.
	TierAnonMaxFrames    int
	TierFreeMaxFrames    int
	TierPremiumMaxFrames int
	TierProMaxFrames     int
}

// DeepConfigured reports whether deep synthesis has both a model and a daemon.
func (v VideoSummaryConfig) DeepConfigured() bool {
	return v.DeepModel != "" && v.DeepURL != ""
}

func loadVideoSummaryConfig() VideoSummaryConfig {
	return VideoSummaryConfig{
		Enabled:                    getEnvBool("SUMMARY_ENABLED", false),
		OllamaURL:                  getEnv("OLLAMA_URL", "http://localhost:11434"),
		SummaryModel:               getEnv("OLLAMA_SUMMARY_MODEL", "qwen3.5:9b-q8_0"),
		DeepModel:                  getEnv("OLLAMA_SUMMARY_DEEP_MODEL", ""),
		DeepURL:                    getEnv("OLLAMA_SUMMARY_DEEP_URL", ""),
		MaxInputTokens:             getEnvInt("OLLAMA_SUMMARY_MAX_INPUT_TOKENS", 12000),
		Workers:                    getEnvInt("SUMMARY_WORKERS", 1),
		StageTimeout:               time.Duration(getEnvInt("SUMMARY_STAGE_TIMEOUT_SECONDS", 1800)) * time.Second,
		OllamaTimeout:              time.Duration(getEnvInt("OLLAMA_TIMEOUT_SECONDS", 300)) * time.Second,
		RateLimitPerSessionPerHour: getEnvInt("SUMMARY_RATE_LIMIT_PER_SESSION_PER_HOUR", 6),
		RateLimitPerIPPerHour:      getEnvInt("SUMMARY_RATE_LIMIT_PER_IP_PER_HOUR", 12),
		// getEnvIntDefault: 0 is a valid budget (sign-in required).
		TierAnonMaxFrames:    nonNegative(getEnvIntDefault("TIER_ANON_SUMMARY_MAX_FRAMES", 0)),
		TierFreeMaxFrames:    nonNegative(getEnvIntDefault("TIER_FREE_SUMMARY_MAX_FRAMES", 24)),
		TierPremiumMaxFrames: nonNegative(getEnvIntDefault("TIER_PREMIUM_SUMMARY_MAX_FRAMES", 48)),
		TierProMaxFrames:     nonNegative(getEnvIntDefault("TIER_PRO_SUMMARY_MAX_FRAMES", 96)),
	}
}

func nonNegative(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
