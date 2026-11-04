package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// assistantHandler is the /api/assistant handler main builds (MM104 Medi);
// setupRouter mounts it. nil (tests) mounts nothing.
var assistantHandler *handlers.AssistantHandler

// newAssistantHandler wires Medi: the OpenRouter provider over Medi's own key
// (ASSISTANT_OPENROUTER_API_KEY; unset = off), the MM105 embedded knowledge,
// the embedded op schemas, the daily spend cap over assistant_usage_events,
// and the user-delete sweeper for conversation deletes.
// Nothing here touches the GPU scheduler or Ollama.
func newAssistantHandler(appCtx context.Context, cfg *config.Config, store *telemetry.Store, registry *tiers.Registry,
	deleter handlers.Expirer, logger *slog.Logger) *handlers.AssistantHandler {
	ac := cfg.Assistant
	var st assistant.Store
	var spendSrc assistant.SpendSource
	if store.Enabled() {
		st, spendSrc = store, store
	}
	// MM105: the generated corpus (knowledge/, `npm run export:medi`). A
	// malformed export logs and falls back to the stub (no docs, all in scope).
	var knowledge assistant.Knowledge = assistant.NewStubKnowledge()
	if k, err := assistant.NewEmbeddedKnowledge(); err != nil {
		logger.Error("assistant: knowledge corpus failed to load; Medi answers without docs", "error", err.Error())
	} else {
		knowledge = k
		logger.Info("assistant: knowledge loaded", "docs", len(k.Corpus().Docs))
	}
	engine := &assistant.Engine{
		Provider:  assistant.NewOpenRouterProvider(ac.BaseURL, ac.APIKey, ac.AttributionURL, ac.ProviderOrder),
		Knowledge: knowledge,
		Ops:       assistant.NewOpsValidator(),
		Store:     st,
		Spend:     assistant.NewSpendCap(ac.DailySpendCapUSD, spendSrc, nil),
		Cfg:       ac,
		Logger:    logger,
		Shutdown:  appCtx.Done(),
	}
	h := handlers.NewAssistantHandler(ac, engine, st, registry, logger)
	h.SetDeletion(deleter)
	if ac.Enabled && !ac.Configured() {
		logger.Warn("assistant: ASSISTANT_ENABLED is set but ASSISTANT_OPENROUTER_API_KEY is not; Medi stays off")
	}
	return h
}

// assistantBuckets is the Medi rate limit (design §10), chosen by the body's
// surface: assistant_guide 20/h per session and 40/h per IP anonymous, 60/h per
// signed-in user; assistant_copilot 60/h per user (a burst guard, the daily
// metric is the real limit). The body is peeked once (bounded by the route's
// own 512 KB cap) and restored for the handler. Operators are exempt through
// limiter.RouteExempt.
func assistantBuckets(cfg *config.Config, guard func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc) gin.HandlerFunc {
	ac := cfg.Assistant
	guideAnon := guard("assistant_guide", "assistant", ac.GuidePerSessionPerHour, ac.GuidePerIPPerHour)
	guideUser := guard("assistant_guide", "assistant", ac.GuidePerUserPerHour, 0)
	copilot := guard("assistant_copilot", "assistant", ac.CopilotPerUserPerHour, 0)
	return func(c *gin.Context) {
		if assistantSurfaceOf(c) != assistant.SurfaceGuide {
			copilot(c)
			return
		}
		if res, ok := middleware.ResolveTier(c); ok && res.User != nil {
			guideUser(c)
			return
		}
		guideAnon(c)
	}
}

// assistantSurfaceOf reads `surface` from the JSON body and puts the bytes
// back. Anything unreadable counts as the guide (the handler then answers 400).
func assistantSurfaceOf(c *gin.Context) string {
	if c.Request.Body == nil {
		return assistant.SurfaceGuide
	}
	head, err := io.ReadAll(io.LimitReader(c.Request.Body, assistant.MaxBodyBytes+1))
	c.Request.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), c.Request.Body), c.Request.Body}
	if err != nil {
		return assistant.SurfaceGuide
	}
	var peek struct {
		Surface string `json:"surface"`
	}
	if json.Unmarshal(head, &peek) != nil || !assistant.ValidSurface(peek.Surface) {
		return assistant.SurfaceGuide
	}
	return peek.Surface
}
