package handlers

import (
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// Pure unit tests for the stats/feedback/model-additions build — no DB, no
// network, no S3.

// ---- Feature A: the new model-rules default -------------------------------------

// drChatLabDefaultModelRulesFixture mirrors the config default (config.Load
// reads env, so the literal is duplicated here; if the default changes, update
// both).
const drChatLabDefaultModelRulesFixture = "anthropic/,openai/,z-ai/glm-5.2,moonshotai/kimi-k2.6," +
	"google/gemini-3.1-pro-preview,google/gemini-3-pro-preview,google/gemini-3.1-flash-lite,google/gemini-3.5-flash,qwen/qwen3.7-plus," +
	"google/gemini-3-flash-preview,google/gemini-2.5-flash,google/gemini-2.0-flash-001,qwen/qwen3.6-plus,qwen/qwen3.6-flash,qwen/qwen3.7-max,qwen/qwen3-vl-235b-a22b-instruct," +
	"xai/grok-4.5,xai/grok-4.3"

var drChatLabNewModelIDs = []string{
	"google/gemini-3.1-pro-preview",
	"google/gemini-3-pro-preview",
	"google/gemini-3.1-flash-lite",
	"google/gemini-3.5-flash",
	"qwen/qwen3.7-plus",
	"google/gemini-3-flash-preview",
	"google/gemini-2.5-flash",
	"google/gemini-2.0-flash-001",
	"qwen/qwen3.6-plus",
	"qwen/qwen3.6-flash",
	"qwen/qwen3.7-max",
	"qwen/qwen3-vl-235b-a22b-instruct",
	"xai/grok-4.5",
	"xai/grok-4.3",
}

func TestDefaultModelRulesAdmitNewModels(t *testing.T) {
	rules := strings.Split(strings.ToLower(drChatLabDefaultModelRulesFixture), ",")

	catalog := make([]openrouter.Model, 0)
	for _, id := range drChatLabNewModelIDs {
		catalog = append(catalog, orModel(id))
	}
	catalog = append(catalog,
		orModel("anthropic/claude-opus-4.8"),
		orModel("openai/gpt-5.2"),
		orModel("z-ai/glm-5.2"),
		orModel("moonshotai/kimi-k2.6"),
		// Pollution that exact-id rules must NOT admit (why google//qwen//xai/
		// are not prefixes):
		orModel("google/gemini-embedding-001"),
		orModel("google/imagen-4"),
		orModel("qwen/qwen-image-edit"),
		orModel("xai/grok-2-image-1212"),
		orModel("xai/grok-code-fast-1"),
		// A drifted/unknown preview slug that exists upstream but not in our
		// rules stays out:
		orModel("google/gemini-4-ultra-preview"),
		// Variant suffixes stay excluded from prefix rules:
		orModel("anthropic/claude-haiku-4.5:free"),
	)

	got := filterModels(catalog, rules)
	ids := map[string]bool{}
	for _, m := range got {
		ids[strings.ToLower(m.ID)] = true
	}
	for _, want := range drChatLabNewModelIDs {
		if !ids[want] {
			t.Fatalf("new model %s missing from filtered catalog", want)
		}
	}
	for _, banned := range []string{"google/gemini-embedding-001", "google/imagen-4", "qwen/qwen-image-edit", "xai/grok-2-image-1212", "xai/grok-code-fast-1", "google/gemini-4-ultra-preview", "anthropic/claude-haiku-4.5:free"} {
		if ids[banned] {
			t.Fatalf("%s must not pass the rules", banned)
		}
	}
	if len(got) != len(drChatLabNewModelIDs)+4 {
		t.Fatalf("filtered count = %d, want %d", len(got), len(drChatLabNewModelIDs)+4)
	}
}

func TestProviderRankGoogleQwen(t *testing.T) {
	order := []string{"anthropic", "openai", "google", "qwen", "xai", "mistralai"}
	for i := 1; i < len(order); i++ {
		if !(providerRank(order[i-1]) < providerRank(order[i])) &&
			providerRank(order[i-1]) == providerRank(order[i]) {
			continue // equal rank sorts alphabetically — only mistralai vs others hits this
		}
		if providerRank(order[i-1]) > providerRank(order[i]) {
			t.Fatalf("provider order broken: %s should sort before %s", order[i-1], order[i])
		}
	}
	if providerRank("google") >= providerRank("mistralai") {
		t.Fatal("google must rank before the alphabetical tail")
	}
}

// ---- Feature A: vision guard ------------------------------------------------------

func TestCheckVisionSupport(t *testing.T) {
	vision := &models.DrChatLabModel{ID: "google/gemini-3.5-flash", SupportsImages: true}
	textOnly := &models.DrChatLabModel{ID: "z-ai/glm-5.2", SupportsImages: false}

	if msg := checkVisionSupport([]string{"image"}, textOnly); msg != "The selected model does not support image attachments" {
		t.Fatalf("image + text-only model should error, got %q", msg)
	}
	if msg := checkVisionSupport([]string{"file", "image"}, textOnly); msg == "" {
		t.Fatal("mixed attachments incl. image + text-only model should error")
	}
	if msg := checkVisionSupport([]string{"file"}, textOnly); msg != "" {
		t.Fatalf("non-image attachments on a text-only model are fine, got %q", msg)
	}
	if msg := checkVisionSupport([]string{"image"}, vision); msg != "" {
		t.Fatalf("image + vision model is fine, got %q", msg)
	}
	if msg := checkVisionSupport(nil, textOnly); msg != "" {
		t.Fatalf("no attachments is fine, got %q", msg)
	}
}

// ---- Feature B: feedback validation ------------------------------------------------

func TestValidateFeedbackRequest(t *testing.T) {
	longComment := strings.Repeat("x", drChatLabMaxFeedbackCommentBytes+1)
	cases := []struct {
		name       string
		rating     string
		categories []string
		comment    string
		wantOK     bool
	}{
		{"up with valid categories", "up", []string{"accurate", "concise"}, "", true},
		{"down with valid categories", "down", []string{"wrong_format", "ignored_instructions"}, "tabs please", true},
		{"empty categories fine", "up", nil, "", true},
		{"comment only", "down", nil, "just bad", true},
		{"unknown id", "up", []string{"amazing"}, "", false},
		{"wrong-rating id (down id on up)", "up", []string{"wrong_format"}, "", false},
		{"wrong-rating id (up id on down)", "down", []string{"accurate"}, "", false},
		{"too many categories", "down", []string{"inaccurate_hallucinated", "wrong_format", "ignored_instructions", "poor_ocr_transcription", "bad_structured_output", "too_verbose", "incomplete_cut_off"}, "", false},
		{"comment too long", "up", nil, longComment, false},
		{"bad rating", "sideways", nil, "", false},
		{"empty rating", "", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateFeedbackRequest(tc.rating, tc.categories, tc.comment)
			if (msg == "") != tc.wantOK {
				t.Fatalf("validateFeedbackRequest(%q, %v) = %q, wantOK=%v", tc.rating, tc.categories, msg, tc.wantOK)
			}
		})
	}
}

// ---- Feature B: memory prompt feedback section ---------------------------------------

func TestBuildMemoryPromptFeedbackSection(t *testing.T) {
	system, user := buildMemoryPrompt(memoryPromptInput{
		Name:     "OCR Pipeline",
		MaxChars: 4096,
		Feedback: []memoryFeedbackEntry{
			{Rating: "down", Model: "google/gemini-3.1-flash-lite", Categories: []string{"Ignored instructions", "Wrong format"}, Comment: "used commas instead of the tab delimiter I asked for"},
			{Rating: "up", Model: "anthropic/claude-opus-4.8", Categories: []string{"Great OCR/transcription"}},
		},
	})
	// The feedback-distillation guidance lives inside the Key learnings &
	// principles charter of the six-section instruction.
	if !strings.Contains(system, "durable guidance distilled from response feedback") {
		t.Fatal("system prompt must carry the feedback-distillation guidance")
	}
	if !strings.Contains(user, "## Response feedback") {
		t.Fatalf("feedback section missing:\n%s", user)
	}
	if !strings.Contains(user, `[feedback · 👎 · google/gemini-3.1-flash-lite] Ignored instructions; Wrong format — "used commas instead of the tab delimiter I asked for"`) {
		t.Fatalf("down feedback line wrong:\n%s", user)
	}
	if !strings.Contains(user, "[feedback · 👍 · anthropic/claude-opus-4.8] Great OCR/transcription\n") {
		t.Fatalf("up feedback line wrong (no comment → no quote):\n%s", user)
	}

	// No feedback → no section.
	_, user = buildMemoryPrompt(memoryPromptInput{Name: "Bare", MaxChars: 1000})
	if strings.Contains(user, "## Response feedback") {
		t.Fatal("empty feedback must omit the section")
	}
}

// ---- Feature C: cost estimation ------------------------------------------------------

func TestEstimateCostUSD(t *testing.T) {
	model := &models.DrChatLabModel{
		ID:      "google/gemini-3.5-flash",
		Pricing: models.DrChatLabModelPricing{PromptUsdPerMTok: 0.30, CompletionUsdPerMTok: 2.50},
	}

	// Provider cost present → passthrough, not estimated.
	cost, estimated := estimateCostUSD(&openrouter.Usage{PromptTokens: 100, CompletionTokens: 50, Cost: 0.0123}, model)
	if cost == nil || *cost != 0.0123 || estimated {
		t.Fatalf("passthrough failed: %v %v", cost, estimated)
	}

	// Absent cost → catalog estimate + flag.
	cost, estimated = estimateCostUSD(&openrouter.Usage{PromptTokens: 1_000_000, CompletionTokens: 2_000_000}, model)
	if cost == nil || !estimated {
		t.Fatalf("estimate failed: %v %v", cost, estimated)
	}
	if want := 0.30 + 2*2.50; *cost < want-1e-9 || *cost > want+1e-9 {
		t.Fatalf("estimate = %v, want %v", *cost, want)
	}

	// Unknown model → nil.
	cost, estimated = estimateCostUSD(&openrouter.Usage{PromptTokens: 100, CompletionTokens: 50}, nil)
	if cost != nil || estimated {
		t.Fatalf("unknown model should yield nil: %v %v", cost, estimated)
	}

	// No usage at all → nil.
	cost, estimated = estimateCostUSD(nil, model)
	if cost != nil || estimated {
		t.Fatalf("nil usage should yield nil: %v %v", cost, estimated)
	}
}

// ---- Feature C: balance semantics ------------------------------------------------------

func TestCreditBalanceSemantics(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	f := func(v float64) *float64 { return &v }

	// Empty ledger → no tracking, nothing credited.
	trackingSince, credited := ledgerTrackingAndCredited(nil, now)
	if trackingSince != nil || credited != 0 {
		t.Fatalf("empty ledger: %v %v", trackingSince, credited)
	}

	// Deposit yesterday + backdated top-up last week + FUTURE-dated entry.
	entries := []ledgerEntryLite{
		{AmountUSD: 25, EffectiveAt: now.Add(-1 * day)},
		{AmountUSD: 10, EffectiveAt: now.Add(-7 * day)}, // backdated top-up
		{AmountUSD: 99, EffectiveAt: now.Add(2 * day)},  // future → not yet credited
	}
	trackingSince, credited = ledgerTrackingAndCredited(entries, now)
	if trackingSince == nil || !trackingSince.Equal(now.Add(-7*day)) {
		t.Fatalf("trackingSince = %v, want -7d (the backdated entry shifts it)", trackingSince)
	}
	if credited != 35 {
		t.Fatalf("credited = %v, want 35 (future entry excluded)", credited)
	}

	// Spend: pre-tracking usage excluded; NULL costs contribute 0.
	events := []usageCostEvent{
		{OccurredAt: now.Add(-30 * day), CostUSD: f(4)}, // before trackingSince → excluded from balance
		{OccurredAt: now.Add(-2 * day), CostUSD: f(1.5)},
		{OccurredAt: now.Add(-1 * day), CostUSD: nil}, // unknown cost → under-counts (surfaced via unknownCostEvents)
		{OccurredAt: now.Add(-12 * time.Hour), CostUSD: f(0.5)},
	}
	spent := sumSpentUSD(events, *trackingSince, now)
	if spent != 2 {
		t.Fatalf("spent = %v, want 2", spent)
	}
	if current := credited - spent; current != 33 {
		t.Fatalf("current = %v, want 33", current)
	}
}

func TestValidateCreditEntry(t *testing.T) {
	ok := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		entryType string
		amount    float64
		at        time.Time
		wantOK    bool
	}{
		{"deposit positive", "deposit", 25, ok, true},
		{"deposit zero", "deposit", 0, ok, false},
		{"deposit negative", "deposit", -5, ok, false},
		{"adjustment negative fine", "adjustment", -3.5, ok, true},
		{"adjustment zero", "adjustment", 0, ok, false},
		{"too large", "deposit", 100001, ok, false},
		{"bad type", "refund", 5, ok, false},
		{"year too early", "deposit", 5, time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"year too late", "deposit", 5, time.Date(2101, 1, 1, 0, 0, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateCreditEntry(tc.entryType, tc.amount, tc.at)
			if (msg == "") != tc.wantOK {
				t.Fatalf("validateCreditEntry(%q, %v, %v) = %q, wantOK=%v", tc.entryType, tc.amount, tc.at, msg, tc.wantOK)
			}
		})
	}
}

// ---- Feature C: dimension/bucket allowlists ----------------------------------------------

func TestStatsAllowlists(t *testing.T) {
	for _, good := range []string{"model", "user", "project", "session", "kind"} {
		if _, ok := statsDimensions[good]; !ok {
			t.Fatalf("dimension %q should be allowed", good)
		}
	}
	for _, bad := range []string{"", "models", "user; DROP TABLE", "e.model", "occurred_at"} {
		if _, ok := statsDimensions[bad]; ok {
			t.Fatalf("dimension %q must be rejected", bad)
		}
	}
	for _, good := range []string{"none", "model", "kind"} {
		if _, ok := statsTimeseriesDimensions[good]; !ok {
			t.Fatalf("timeseries dimension %q should be allowed", good)
		}
	}
	if _, ok := statsTimeseriesDimensions["project"]; ok {
		t.Fatal("timeseries dimension project is not supported")
	}
	for _, good := range []string{"day", "week", "month"} {
		if !statsBuckets[good] {
			t.Fatalf("bucket %q should be allowed", good)
		}
	}
	for _, bad := range []string{"", "hour", "year", "day'; --"} {
		if statsBuckets[bad] {
			t.Fatalf("bucket %q must be rejected", bad)
		}
	}
}

// ---- Feature C: timeseries top-N+other rollup -----------------------------------------------

func TestRollupTimeseriesPoints(t *testing.T) {
	mk := func(bucket, key string, cost float64, tokens, events int64) models.DrChatStatsTimeseriesPoint {
		return models.DrChatStatsTimeseriesPoint{Bucket: bucket, Key: key, CostUsd: cost, TotalTokens: tokens, Events: events}
	}

	// 4 keys, topN=2 → keys c,d (highest total cost) survive; a,b roll into
	// per-bucket "other".
	points := []models.DrChatStatsTimeseriesPoint{
		mk("2026-07-01T00:00:00Z", "a", 1, 10, 1),
		mk("2026-07-01T00:00:00Z", "b", 2, 20, 1),
		mk("2026-07-01T00:00:00Z", "c", 10, 30, 1),
		mk("2026-07-02T00:00:00Z", "a", 1, 10, 1),
		mk("2026-07-02T00:00:00Z", "d", 20, 40, 2),
	}
	got := rollupTimeseriesPoints(points, 2)

	byBucketKey := map[string]models.DrChatStatsTimeseriesPoint{}
	for _, p := range got {
		byBucketKey[p.Bucket+"|"+p.Key] = p
	}
	if _, ok := byBucketKey["2026-07-01T00:00:00Z|c"]; !ok {
		t.Fatal("top key c missing")
	}
	if _, ok := byBucketKey["2026-07-02T00:00:00Z|d"]; !ok {
		t.Fatal("top key d missing")
	}
	o1 := byBucketKey["2026-07-01T00:00:00Z|other"]
	if o1.CostUsd != 3 || o1.TotalTokens != 30 || o1.Events != 2 {
		t.Fatalf("bucket1 other = %+v", o1)
	}
	o2 := byBucketKey["2026-07-02T00:00:00Z|other"]
	if o2.CostUsd != 1 || o2.TotalTokens != 10 || o2.Events != 1 {
		t.Fatalf("bucket2 other = %+v", o2)
	}
	for _, p := range got {
		if p.Key == "a" || p.Key == "b" {
			t.Fatalf("non-top key %q leaked through", p.Key)
		}
	}

	// ≤ topN distinct keys → untouched.
	few := []models.DrChatStatsTimeseriesPoint{mk("b1", "x", 1, 1, 1), mk("b1", "y", 2, 2, 1)}
	if got := rollupTimeseriesPoints(few, 8); len(got) != 2 {
		t.Fatalf("few keys should pass through, got %v", got)
	}

	// dimension=none (no keys) → untouched.
	none := []models.DrChatStatsTimeseriesPoint{mk("b1", "", 1, 1, 1), mk("b2", "", 2, 2, 1)}
	if got := rollupTimeseriesPoints(none, 2); len(got) != 2 || got[0].Key != "" {
		t.Fatalf("keyless points should pass through, got %v", got)
	}
}

// ---- Feature B: category catalog sanity -------------------------------------------------------

func TestFeedbackCategoryCatalog(t *testing.T) {
	cats := chatLabFeedbackCategories()
	if len(cats.Up) != 6 || len(cats.Down) != 7 {
		t.Fatalf("category counts = %d up / %d down", len(cats.Up), len(cats.Down))
	}
	seen := map[string]bool{}
	for _, c := range append(append([]models.DrChatLabFeedbackCategory{}, cats.Up...), cats.Down...) {
		if c.ID == "" || c.Label == "" {
			t.Fatalf("category with empty id/label: %+v", c)
		}
		if seen[c.ID] {
			t.Fatalf("duplicate category id %q", c.ID)
		}
		seen[c.ID] = true
		if drChatLabFeedbackCategoryLabels[c.ID] != c.Label {
			t.Fatalf("label map mismatch for %q", c.ID)
		}
	}
}
