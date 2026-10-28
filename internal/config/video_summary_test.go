package config

import (
	"testing"
	"time"
)

// MM74: every summary setting is optional and defaults to the design.
func TestVideoSummaryConfigDefaults(t *testing.T) {
	v := Load().VideoSummary
	if v.Enabled {
		t.Error("SUMMARY_ENABLED must default to false")
	}
	if v.SummaryModel != "qwen3.5:9b-q8_0" || v.DeepModel != "" || v.DeepURL != "" || v.DeepConfigured() {
		t.Errorf("models: %+v", v)
	}
	if v.MaxInputTokens != 12000 || v.Workers != 1 || v.StageTimeout != 30*time.Minute {
		t.Errorf("limits: %+v", v)
	}
	if v.RateLimitPerSessionPerHour != 6 || v.RateLimitPerIPPerHour != 12 {
		t.Errorf("rate limits: %+v", v)
	}
	if v.TierAnonMaxFrames != 0 || v.TierFreeMaxFrames != 24 || v.TierPremiumMaxFrames != 48 || v.TierProMaxFrames != 96 {
		t.Errorf("frame budgets: %+v", v)
	}
}

func TestVideoSummaryConfigOverrides(t *testing.T) {
	t.Setenv("SUMMARY_ENABLED", "true")
	t.Setenv("OLLAMA_SUMMARY_MODEL", "other:7b")
	t.Setenv("OLLAMA_SUMMARY_DEEP_MODEL", "qwen3.8:27b")
	t.Setenv("OLLAMA_SUMMARY_DEEP_URL", "http://127.0.0.1:11436")
	t.Setenv("OLLAMA_SUMMARY_MAX_INPUT_TOKENS", "20000")
	t.Setenv("SUMMARY_WORKERS", "2")
	t.Setenv("SUMMARY_RATE_LIMIT_PER_SESSION_PER_HOUR", "3")
	t.Setenv("SUMMARY_RATE_LIMIT_PER_IP_PER_HOUR", "9")
	t.Setenv("TIER_ANON_SUMMARY_MAX_FRAMES", "4")
	t.Setenv("TIER_FREE_SUMMARY_MAX_FRAMES", "0")
	t.Setenv("TIER_PRO_SUMMARY_MAX_FRAMES", "-5")
	v := Load().VideoSummary
	if !v.Enabled || v.SummaryModel != "other:7b" || !v.DeepConfigured() || v.MaxInputTokens != 20000 || v.Workers != 2 {
		t.Errorf("overrides: %+v", v)
	}
	if v.RateLimitPerSessionPerHour != 3 || v.RateLimitPerIPPerHour != 9 {
		t.Errorf("rate limits: %+v", v)
	}
	if v.TierAnonMaxFrames != 4 || v.TierFreeMaxFrames != 0 || v.TierProMaxFrames != 0 {
		t.Errorf("frame budgets (0 allowed, negative clamps to 0): %+v", v)
	}
	// Deep needs both halves.
	t.Setenv("OLLAMA_SUMMARY_DEEP_URL", "")
	if Load().VideoSummary.DeepConfigured() {
		t.Error("deep with no URL must be off")
	}
}
