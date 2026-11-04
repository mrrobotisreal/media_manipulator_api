package handlers

import (
	"math"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

// SetAnalyticsEmitter attaches the server-truth analytics feed (MM107): every
// finished Medi turn becomes one server_assistant_turn_counted. A nil or
// disabled emitter sends nothing.
func (h *AssistantHandler) SetAnalyticsEmitter(e *analyticsemit.Emitter) {
	if h == nil || h.engine == nil || !e.Enabled() {
		return
	}
	h.engine.OnTurnCounted = func(r assistant.TurnReport) {
		e.Emit(assistantTurnEvent(r, time.Now().UTC()))
	}
}

// assistantTurnEvent builds server_assistant_turn_counted. Every property is
// a closed vocabulary or a number; the tier also travels in the context block.
func assistantTurnEvent(r assistant.TurnReport, at time.Time) analyticsemit.Event {
	micros := int64(math.Round(math.Max(r.CostUSD, 0) * 1e6))
	return analyticsemit.Event{
		EventName: analyticsemit.ServerAssistantTurnCounted,
		EventTS:   at,
		VisitorID: r.VisitorID,
		SessionID: r.SessionID,
		Context:   analyticsemit.Context("", "", "", r.Tier),
		Properties: map[string]any{
			"surface":             r.Surface,
			"model_tier":          r.ModelTier,
			"outcome":             r.Outcome,
			"cost_usd_micros":     micros,
			"cost_bucket":         assistantCostBucket(r.CostUSD),
			"cached_ratio_bucket": assistantCachedRatioBucket(r.CachedTokens, r.InputTokens),
		},
	}
}

// assistantCostBucket is the catalog's cost_bucket (USD):
// 0 | <0.001 | <0.01 | <0.05 | <0.2 | 0.2+.
func assistantCostBucket(usd float64) string {
	switch {
	case usd <= 0:
		return "0"
	case usd < 0.001:
		return "<0.001"
	case usd < 0.01:
		return "<0.01"
	case usd < 0.05:
		return "<0.05"
	case usd < 0.2:
		return "<0.2"
	}
	return "0.2+"
}

// assistantCachedRatioBucket is the share of input tokens read from the prompt
// cache, in percent: 0 | 1-25 | 26-75 | 76-100.
func assistantCachedRatioBucket(cached, input int) string {
	if cached <= 0 || input <= 0 {
		return "0"
	}
	pct := int(math.Ceil(float64(cached) * 100 / float64(input)))
	switch {
	case pct <= 25:
		return "1-25"
	case pct <= 75:
		return "26-75"
	}
	return "76-100"
}
