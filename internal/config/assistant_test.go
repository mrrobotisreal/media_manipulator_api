package config

import "testing"

func TestAssistantConfigDefaults(t *testing.T) {
	a := Load().Assistant
	if a.Enabled || a.Configured() {
		t.Fatal("Medi ships dark")
	}
	if a.ModelGuide != "anthropic/claude-haiku-5.5" || a.ModelCopilot != "anthropic/claude-sonnet-5.5" ||
		a.ModelPlan != "anthropic/claude-opus-5.5" || a.ModelTitle != "anthropic/claude-haiku-5.5" {
		t.Errorf("model defaults = %+v", a)
	}
	if a.DailySpendCapUSD != 10 || a.ToolMaxRounds != 4 || a.MaxEditRounds != 2 || a.PlanTurnCost != 3 || a.MaxFrameBytes != 307200 {
		t.Errorf("limits = %+v", a)
	}
	if a.TierAnonTurns != 0 || a.TierFreeTurns != 5 || a.TierPremiumTurns != 50 || a.TierProTurns != 150 {
		t.Errorf("turns = %d/%d/%d/%d", a.TierAnonTurns, a.TierFreeTurns, a.TierPremiumTurns, a.TierProTurns)
	}
	// No copilot answers until its surface is listed (2026-11-05: the old
	// default switched on all three before any eval had passed).
	if !a.GuideEnabled || len(a.CopilotSurfaces) != 0 {
		t.Errorf("surfaces = %v", a.CopilotSurfaces)
	}
	if len(a.ProviderOrder) != 1 || a.ProviderOrder[0] != "anthropic" {
		t.Errorf("provider order = %v", a.ProviderOrder)
	}
	// Reasoning budget (>= 1024, a share of max_tokens) must stay below max_tokens.
	for _, n := range []int{a.MaxTokensGuide, a.MaxTokensCopilot, a.MaxTokensPlan} {
		if n <= 1024 {
			t.Errorf("max tokens %d leaves no room after the 1024-token reasoning floor", n)
		}
	}
}

// The DR chat lab's key never enables Medi (decision 10).
func TestAssistantKeyNeverFallsBack(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "chat-lab-key")
	t.Setenv("ASSISTANT_ENABLED", "true")
	a := Load().Assistant
	if a.APIKey != "" || a.Configured() {
		t.Fatal("ASSISTANT_OPENROUTER_API_KEY must be set on its own")
	}
	t.Setenv("ASSISTANT_OPENROUTER_API_KEY", "medi-key")
	if !Load().Assistant.Configured() {
		t.Fatal("enabled + own key = configured")
	}
}

func TestAssistantCopilotSurfacesExplicit(t *testing.T) {
	t.Setenv("ASSISTANT_COPILOT_SURFACES", "")
	if a := Load().Assistant; len(a.CopilotSurfaces) != 0 {
		t.Fatalf("set-but-empty must mean none, got %v", a.CopilotSurfaces)
	}
	t.Setenv("ASSISTANT_COPILOT_SURFACES", " Photo , studio ")
	a := Load().Assistant
	if !a.CopilotSurface("photo") || !a.CopilotSurface("studio") || a.CopilotSurface("vector") {
		t.Fatalf("surfaces = %v", a.CopilotSurfaces)
	}
}
