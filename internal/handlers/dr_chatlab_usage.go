package handlers

import (
	"context"
	"log"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// Usage-event recording: every OpenRouter call becomes ONE immutable
// dr_chat_usage_events row — chat turns (persistAssistantMessage) AND the
// background title/memory completions (they cost money too). The table has no
// FKs on purpose: sessions/projects hard-delete, the financial record must
// not; session_title/project_name are snapshots taken here.

// usageEventInsert carries one event. Cost semantics: CostUSD nil = provider
// returned no cost AND no catalog estimate was possible; CostEstimated true =
// computed from catalog pricing rather than provider-reported.
type usageEventInsert struct {
	Kind          string // "chat" | "title" | "memory"
	Model         string
	SessionID     *string
	SessionTitle  *string
	ProjectID     *string
	ProjectName   *string
	MessageID     *string
	UserUID       string
	UserEmail     string
	Usage         *openrouter.Usage
	CostUSD       *float64
	CostEstimated bool
	// Per-response performance metrics (see dr_chatlab_perf.go). All nil for
	// events recorded before the metrics landed; title/memory events carry
	// only DurationMs + RequestType ("text").
	DurationMs   *int
	ReasoningMs  *int
	FirstTokenMs *int
	RequestType  *string // "text"|"file"|"image"|"pdf"|"audio"|"mixed"
}

// estimateCostUSD resolves an event's cost: the provider-reported cost when
// present (estimated=false); otherwise a catalog-pricing estimate
// promptTokens×promptUsdPerMTok/1e6 + completionTokens×completionUsdPerMTok/1e6
// (estimated=true); nil when there is no usage or the model isn't in the
// catalog. Pure; unit-tested.
func estimateCostUSD(usage *openrouter.Usage, model *models.DrChatLabModel) (*float64, bool) {
	if usage == nil {
		return nil, false
	}
	if usage.Cost != 0 {
		cost := usage.Cost
		return &cost, false
	}
	if model == nil {
		return nil, false
	}
	est := float64(usage.PromptTokens)*model.Pricing.PromptUsdPerMTok/1e6 +
		float64(usage.CompletionTokens)*model.Pricing.CompletionUsdPerMTok/1e6
	return &est, true
}

// recordUsageEvent inserts one event. Failures are logged, never surfaced —
// analytics must not break the calling path (message persist / titling /
// memory).
func (h *DrChatLabHandler) recordUsageEvent(ctx context.Context, ev usageEventInsert) {
	if h.pool == nil {
		return
	}
	var promptTokens, completionTokens, reasoningTokens int
	if ev.Usage != nil {
		promptTokens = ev.Usage.PromptTokens
		completionTokens = ev.Usage.CompletionTokens
		reasoningTokens = ev.Usage.ReasoningTokens()
	}
	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_chat_usage_events
    (kind, model, session_id, session_title, project_id, project_name, message_id,
     user_uid, user_email, prompt_tokens, completion_tokens, reasoning_tokens, cost_usd, cost_estimated,
     duration_ms, reasoning_ms, first_token_ms, request_type)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, lower($9), $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		ev.Kind, ev.Model, ev.SessionID, ev.SessionTitle, ev.ProjectID, ev.ProjectName, ev.MessageID,
		ev.UserUID, ev.UserEmail, promptTokens, completionTokens, reasoningTokens, ev.CostUSD, ev.CostEstimated,
		ev.DurationMs, ev.ReasoningMs, ev.FirstTokenMs, ev.RequestType); err != nil {
		log.Printf("dr chatlab usage: record %s event: %v", ev.Kind, err)
	}
}

// cachedCatalogModel is a lookup against the ALREADY-CACHED catalog only — the
// background title/memory paths must never block on (or fail from) a live
// catalog refresh just to estimate a cost.
func (h *DrChatLabHandler) cachedCatalogModel(id string) *models.DrChatLabModel {
	h.catalog.mu.Lock()
	defer h.catalog.mu.Unlock()
	for i := range h.catalog.models {
		if strings.EqualFold(h.catalog.models[i].ID, id) {
			return &h.catalog.models[i]
		}
	}
	return nil
}
