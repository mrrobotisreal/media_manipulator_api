package config

import "strings"

// AssistantConfig is Medi's configuration (MM104, code name `assistant`,
// docs/design/MEDI_ASSISTANT.md §10, ADR-0018). With nothing set Medi is off:
// ASSISTANT_ENABLED defaults to false and an unset ASSISTANT_OPENROUTER_API_KEY
// keeps it off whatever the flag says (fail closed, like the DR chat lab).
type AssistantConfig struct {
	// Enabled is ASSISTANT_ENABLED (default false). Off: every /api/assistant
	// route 404s except capabilities, which reports enabled:false.
	Enabled bool
	// OperatorOnly is ASSISTANT_OPERATOR_ONLY (default false): only the
	// operator tier may chat (MM106 evals before launch); everyone else sees
	// enabled:false.
	OperatorOnly bool
	// GuideEnabled is ASSISTANT_GUIDE_ENABLED (default true).
	GuideEnabled bool
	// CopilotSurfaces is ASSISTANT_COPILOT_SURFACES (CSV, default
	// photo,studio,vector).
	CopilotSurfaces []string
	// APIKey is ASSISTANT_OPENROUTER_API_KEY, Medi's own key. Never falls back
	// to OPENROUTER_API_KEY. Never log it.
	APIKey string
	// BaseURL is the shared OPENROUTER_BASE_URL.
	BaseURL string
	// AttributionURL is ASSISTANT_ATTRIBUTION_URL (HTTP-Referer).
	AttributionURL string
	// Models by surface (ASSISTANT_MODEL_GUIDE|COPILOT|PLAN|TITLE). Defaults
	// are the OpenRouter slugs confirmed against the live catalog 2026-11-04.
	ModelGuide, ModelCopilot, ModelPlan, ModelTitle string
	// Max output tokens by surface (ASSISTANT_MAX_TOKENS_GUIDE|COPILOT|PLAN,
	// default 2048/8192/32768). Above the design's 1024/4096/8192 because
	// OpenRouter turns reasoning.effort into a thinking budget that is a share
	// of max_tokens (low 20 %, medium 50 %, high 80 %, floor 1024), so the
	// visible answer keeps roughly the design's numbers.
	MaxTokensGuide, MaxTokensCopilot, MaxTokensPlan int
	// ProviderOrder is ASSISTANT_PROVIDER_ORDER (CSV, default anthropic):
	// provider.order, with fallbacks disallowed and data collection denied.
	ProviderOrder []string
	// DailySpendCapUSD is ASSISTANT_DAILY_SPEND_CAP_USD (default 10).
	DailySpendCapUSD float64
	// ToolMaxRounds is ASSISTANT_TOOL_MAX_ROUNDS (default 4): server tool
	// rounds per request.
	ToolMaxRounds int
	// MaxEditRounds is ASSISTANT_MAX_EDIT_ROUNDS (default 2): editResult
	// continuations per turn.
	MaxEditRounds int
	// PlanTurnCost is ASSISTANT_PLAN_TURN_COST (default 3).
	PlanTurnCost int
	// MaxFrameBytes is ASSISTANT_MAX_FRAME_BYTES (default 307200).
	MaxFrameBytes int
	// Rate buckets (per hour). Guide: anonymous per session / per IP, and per
	// signed-in user. Copilot: per user.
	GuidePerSessionPerHour, GuidePerIPPerHour, GuidePerUserPerHour int
	CopilotPerUserPerHour                                          int
	// Tier allowances (TIER_<T>_ASSISTANT_TURNS_PER_DAY): anonymous 0 (sign-in),
	// free 5, premium 50, pro 150 (proposed, design §11). Operator uncounted.
	TierAnonTurns, TierFreeTurns, TierPremiumTurns, TierProTurns int
}

// Configured reports whether Medi may run at all: enabled and keyed.
func (a AssistantConfig) Configured() bool {
	return a.Enabled && strings.TrimSpace(a.APIKey) != ""
}

// CopilotSurface reports whether a copilot surface is switched on.
func (a AssistantConfig) CopilotSurface(surface string) bool {
	for _, s := range a.CopilotSurfaces {
		if s == surface {
			return true
		}
	}
	return false
}

func loadAssistantConfig() AssistantConfig {
	return AssistantConfig{
		Enabled:          getEnvBool("ASSISTANT_ENABLED", false),
		OperatorOnly:     getEnvBool("ASSISTANT_OPERATOR_ONLY", false),
		GuideEnabled:     getEnvBool("ASSISTANT_GUIDE_ENABLED", true),
		CopilotSurfaces:  splitCSVLower(getEnv("ASSISTANT_COPILOT_SURFACES", "photo,studio,vector")),
		APIKey:           getEnv("ASSISTANT_OPENROUTER_API_KEY", ""),
		BaseURL:          getEnv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
		AttributionURL:   getEnv("ASSISTANT_ATTRIBUTION_URL", "https://media-manipulator.com"),
		ModelGuide:       getEnv("ASSISTANT_MODEL_GUIDE", "anthropic/claude-haiku-5.5"),
		ModelCopilot:     getEnv("ASSISTANT_MODEL_COPILOT", "anthropic/claude-sonnet-5.5"),
		ModelPlan:        getEnv("ASSISTANT_MODEL_PLAN", "anthropic/claude-opus-5.5"),
		ModelTitle:       getEnv("ASSISTANT_MODEL_TITLE", "anthropic/claude-haiku-5.5"),
		MaxTokensGuide:   getEnvInt("ASSISTANT_MAX_TOKENS_GUIDE", 2048),
		MaxTokensCopilot: getEnvInt("ASSISTANT_MAX_TOKENS_COPILOT", 8192),
		MaxTokensPlan:    getEnvInt("ASSISTANT_MAX_TOKENS_PLAN", 32768),
		ProviderOrder:    splitCSVLower(getEnv("ASSISTANT_PROVIDER_ORDER", "anthropic")),
		DailySpendCapUSD: getEnvFloat("ASSISTANT_DAILY_SPEND_CAP_USD", 10),
		ToolMaxRounds:    getEnvInt("ASSISTANT_TOOL_MAX_ROUNDS", 4),
		MaxEditRounds:    getEnvInt("ASSISTANT_MAX_EDIT_ROUNDS", 2),
		PlanTurnCost:     getEnvInt("ASSISTANT_PLAN_TURN_COST", 3),
		MaxFrameBytes:    getEnvInt("ASSISTANT_MAX_FRAME_BYTES", 307200),

		GuidePerSessionPerHour: getEnvInt("RATE_LIMIT_ASSISTANT_GUIDE_PER_SESSION_PER_HOUR", 20),
		GuidePerIPPerHour:      getEnvInt("RATE_LIMIT_ASSISTANT_GUIDE_PER_IP_PER_HOUR", 40),
		GuidePerUserPerHour:    getEnvInt("RATE_LIMIT_ASSISTANT_GUIDE_PER_USER_PER_HOUR", 60),
		CopilotPerUserPerHour:  getEnvInt("RATE_LIMIT_ASSISTANT_COPILOT_PER_USER_PER_HOUR", 60),

		// getEnvIntDefault: 0 is a valid allowance; -1 unlimited.
		TierAnonTurns:    getEnvIntDefault("TIER_ANON_ASSISTANT_TURNS_PER_DAY", 0),
		TierFreeTurns:    getEnvIntDefault("TIER_FREE_ASSISTANT_TURNS_PER_DAY", 5),
		TierPremiumTurns: getEnvIntDefault("TIER_PREMIUM_ASSISTANT_TURNS_PER_DAY", 50),
		TierProTurns:     getEnvIntDefault("TIER_PRO_ASSISTANT_TURNS_PER_DAY", 150),
	}
}
