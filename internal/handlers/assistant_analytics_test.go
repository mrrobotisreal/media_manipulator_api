package handlers

import (
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

// MM107: server_assistant_turn_counted carries closed vocabularies and numbers
// only, the caller's analytics ids, and the tier in the context.
func TestAssistantTurnEvent(t *testing.T) {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	ev := assistantTurnEvent(assistant.TurnReport{Surface: "photo", ModelTier: "copilot", Outcome: "edits",
		CostUSD: 0.0234567, InputTokens: 10000, CachedTokens: 8000, Tier: "premium",
		VisitorID: "v-1", SessionID: "s-1"}, at)
	if ev.EventName != analyticsemit.ServerAssistantTurnCounted || analyticsemit.ServerAssistantTurnCounted != "server_assistant_turn_counted" {
		t.Fatalf("name = %q", ev.EventName)
	}
	if ev.VisitorID != "v-1" || ev.SessionID != "s-1" || !ev.EventTS.Equal(at) {
		t.Errorf("identity/time = %q %q %v", ev.VisitorID, ev.SessionID, ev.EventTS)
	}
	if ev.Context["tier"] != "premium" {
		t.Errorf("context = %v", ev.Context)
	}
	want := map[string]any{"surface": "photo", "model_tier": "copilot", "outcome": "edits",
		"cost_usd_micros": int64(23457), "cost_bucket": "<0.05", "cached_ratio_bucket": "76-100"}
	if len(ev.Properties) != len(want) {
		t.Errorf("props = %v", ev.Properties)
	}
	for k, v := range want {
		if ev.Properties[k] != v {
			t.Errorf("%s = %v (%T), want %v", k, ev.Properties[k], ev.Properties[k], v)
		}
	}
}

func TestAssistantBuckets_CostAndCache(t *testing.T) {
	for usd, want := range map[float64]string{0: "0", -1: "0", 0.0005: "<0.001", 0.001: "<0.01", 0.0099: "<0.01",
		0.02: "<0.05", 0.08: "<0.2", 0.2: "0.2+", 3: "0.2+"} {
		if got := assistantCostBucket(usd); got != want {
			t.Errorf("cost %v = %q, want %q", usd, got, want)
		}
	}
	cases := []struct {
		cached, input int
		want          string
	}{{0, 100, "0"}, {5, 0, "0"}, {1, 100, "1-25"}, {25, 100, "1-25"}, {26, 100, "26-75"},
		{75, 100, "26-75"}, {76, 100, "76-100"}, {100, 100, "76-100"}}
	for _, c := range cases {
		if got := assistantCachedRatioBucket(c.cached, c.input); got != c.want {
			t.Errorf("cached %d/%d = %q, want %q", c.cached, c.input, got, c.want)
		}
	}
}

// A nil or disabled emitter leaves the engine hook unset.
func TestAssistantSetAnalyticsEmitterDisabled(t *testing.T) {
	var nilH *AssistantHandler
	nilH.SetAnalyticsEmitter(nil)
	h := &AssistantHandler{engine: &assistant.Engine{}}
	h.SetAnalyticsEmitter(nil)
	if h.engine.OnTurnCounted != nil {
		t.Error("a nil emitter must not install the hook")
	}
}
