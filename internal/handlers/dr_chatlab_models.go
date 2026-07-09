package handlers

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// Model catalog for the chat-lab picker: the live OpenRouter GET /models list
// filtered by DR_CHATLAB_MODEL_RULES, mapped to the UI DTO, cached in-memory
// for an hour. Single process, mutex-guarded — same philosophy as the feedback
// broadcaster. On upstream failure the stale cache (if any) is served with a
// log line; with no cache at all the endpoint 502s.

const drChatLabCatalogTTL = time.Hour

type chatLabCatalogCache struct {
	mu        sync.Mutex
	models    []models.DrChatLabModel
	fetchedAt time.Time
}

// filterModels returns the catalog entries whose id passes any allow rule. A
// rule ending in '/' is a provider prefix match (the rule already carries the
// trailing slash, so a plain HasPrefix suffices); any other rule is an exact,
// case-insensitive id match. Rules are pre-lowercased at config load.
//
// Variant-suffixed ids (OpenRouter appends ":free", ":extended", ":thinking",
// ":online" etc. after the base slug — see the models API reference) are
// excluded by prefix rules so the picker stays clean; an exact rule naming the
// full variant id still admits it.
func filterModels(list []openrouter.Model, rules []string) []openrouter.Model {
	out := make([]openrouter.Model, 0, len(list))
	for _, m := range list {
		id := strings.ToLower(strings.TrimSpace(m.ID))
		if id == "" {
			continue
		}
		isVariant := strings.Contains(id, ":")
		for _, rule := range rules {
			if rule == "" {
				continue
			}
			if strings.HasSuffix(rule, "/") {
				if !isVariant && strings.HasPrefix(id, rule) {
					out = append(out, m)
					break
				}
				continue
			}
			if id == rule {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// parsePerTokenUSD converts OpenRouter's per-token decimal price string to USD
// per million tokens. Unparseable/empty → 0.
func parsePerTokenUSD(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || v < 0 {
		return 0
	}
	return v * 1_000_000
}

// stripProviderPrefix removes OpenRouter's redundant "Provider: " prefix from
// a catalog display name ("Anthropic: Claude Opus 4.8" → "Claude Opus 4.8").
// The picker already groups models under provider section labels, so the
// embedded prefix only eats row width. Matching is defensive: the text before
// the first ": " must equal the provider slug once both are normalized
// (lowercased, all non-alphanumeric runes dropped) — so "xAI"/"x-ai",
// "Z.AI"/"z-ai", and "Moonshot AI"/"moonshotai" all strip, while a colon
// segment that is NOT the provider (e.g. "Llama 4: Scout") passes through
// unchanged. Pure; unit-tested.
func stripProviderPrefix(name, provider string) string {
	prefix, rest, found := strings.Cut(name, ": ")
	if !found {
		return name
	}
	normalize := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	if normalize(prefix) != normalize(provider) {
		return name
	}
	return strings.TrimSpace(rest)
}

// buildChatLabModel maps one raw catalog entry to the UI DTO.
//
// SupportedEfforts heuristic: the models payload advertises WHETHER a model
// takes the `reasoning` parameter (supported_parameters) but not which effort
// levels it honours, so we expose the common ["low","medium","high"] set for
// every reasoning-capable model and add "xhigh" only when the model's own
// description advertises it. Servers accept the full enum regardless; unknown
// levels are mapped upstream by OpenRouter's token-budget approximation.
func buildChatLabModel(m openrouter.Model) models.DrChatLabModel {
	provider := m.ID
	if i := strings.Index(m.ID, "/"); i > 0 {
		provider = m.ID[:i]
	}
	supportsImages := false
	supportsAudio := false
	for _, mod := range m.Architecture.InputModalities {
		if strings.EqualFold(mod, "image") {
			supportsImages = true
		}
		if strings.EqualFold(mod, "audio") {
			supportsAudio = true
		}
	}
	supportsReasoning := false
	supportsTools := false
	for _, p := range m.SupportedParameters {
		if strings.EqualFold(p, "reasoning") {
			supportsReasoning = true
		}
		if strings.EqualFold(p, "tools") {
			supportsTools = true
		}
	}
	var efforts []string
	if supportsReasoning {
		efforts = []string{"low", "medium", "high"}
		if strings.Contains(strings.ToLower(m.Description), "xhigh") {
			efforts = append(efforts, "xhigh")
		}
	}
	return models.DrChatLabModel{
		ID: m.ID,
		// The redundant "Provider: " prefix is stripped server-side so every
		// consumer (picker rows, message badges, stats labels) benefits; the
		// raw id keeps the provider, so picker search still matches on it.
		Name:              stripProviderPrefix(m.Name, provider),
		Description:       m.Description,
		Provider:          strings.ToLower(provider),
		ContextLength:     m.ContextLength,
		SupportsImages:    supportsImages,
		SupportsReasoning: supportsReasoning,
		SupportsTools:     supportsTools,
		SupportsAudio:     supportsAudio,
		SupportedEfforts:  efforts,
		Pricing: models.DrChatLabModelPricing{
			PromptUsdPerMTok:     parsePerTokenUSD(m.Pricing.Prompt),
			CompletionUsdPerMTok: parsePerTokenUSD(m.Pricing.Completion),
		},
		Created: m.Created,
	}
}

// providerRank orders provider groups: Anthropic → OpenAI → Google → Qwen →
// xAI → everyone else alphabetically.
func providerRank(provider string) int {
	switch provider {
	case "anthropic":
		return 0
	case "openai":
		return 1
	case "google":
		return 2
	case "qwen":
		return 3
	case "x-ai":
		// OpenRouter's xAI provider slug is hyphenated (openrouter.ai/x-ai).
		return 4
	default:
		return 5
	}
}

// sortChatLabModels sorts in place: provider group order Anthropic → OpenAI →
// others alphabetical; within a provider, newest `created` first.
func sortChatLabModels(list []models.DrChatLabModel) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		ra, rb := providerRank(a.Provider), providerRank(b.Provider)
		if ra != rb {
			return ra < rb
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Created != b.Created {
			return a.Created > b.Created
		}
		return a.ID < b.ID
	})
}

// loadCatalog returns the filtered, sorted catalog, refreshing the cache when
// stale. On upstream failure a stale cache is served (with a log); with no
// cache the error is returned.
func (h *DrChatLabHandler) loadCatalog(ctx context.Context) ([]models.DrChatLabModel, error) {
	h.catalog.mu.Lock()
	defer h.catalog.mu.Unlock()
	if h.catalog.models != nil && time.Since(h.catalog.fetchedAt) < drChatLabCatalogTTL {
		return h.catalog.models, nil
	}
	raw, err := h.or.ListModels(ctx)
	if err != nil {
		if h.catalog.models != nil {
			log.Printf("dr chatlab: model catalog refresh failed, serving stale cache: %v", err)
			return h.catalog.models, nil
		}
		return nil, err
	}
	filtered := filterModels(raw, h.cfg.DRChatLabModelRules)
	dtos := make([]models.DrChatLabModel, 0, len(filtered))
	for _, m := range filtered {
		dtos = append(dtos, buildChatLabModel(m))
	}
	sortChatLabModels(dtos)
	h.catalog.models = dtos
	h.catalog.fetchedAt = time.Now()
	return dtos, nil
}

// catalogModel resolves one model id (case-insensitive) against the current
// catalog, refreshing it if stale.
func (h *DrChatLabHandler) catalogModel(ctx context.Context, id string) (*models.DrChatLabModel, error) {
	catalog, err := h.loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	for i := range catalog {
		if strings.EqualFold(catalog[i].ID, id) {
			return &catalog[i], nil
		}
	}
	return nil, nil
}

// ----------------------------------------------------------------------- //
// GET /chatlab/models
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) ListModels(c *gin.Context) {
	if !h.orReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	catalog, err := h.loadCatalog(ctx)
	if err != nil {
		// The raw error (which may include upstream body text) goes to the
		// logs only — never to the client.
		log.Printf("dr chatlab: load model catalog: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to load model catalog"})
		return
	}
	c.JSON(http.StatusOK, models.DrChatLabModelsResponse{
		Models: catalog,
		// The feedback category catalog rides along on this "lab config"
		// fetch so the UI never hardcodes the option ids.
		FeedbackCategories: chatLabFeedbackCategories(),
	})
}
