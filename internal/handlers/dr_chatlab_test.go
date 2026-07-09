package handlers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// Pure unit tests for the chat-lab helpers — no DB, no network, no S3.

// ---- filterModels -----------------------------------------------------------

func orModel(id string) openrouter.Model { return openrouter.Model{ID: id} }

func TestFilterModels(t *testing.T) {
	catalog := []openrouter.Model{
		orModel("anthropic/claude-opus-4.8"),
		orModel("anthropic/claude-haiku-4.5:free"),
		orModel("openai/gpt-5.2"),
		orModel("OpenAI/GPT-5.2-mini"), // odd casing upstream still matches
		orModel("z-ai/glm-5.2"),
		orModel("z-ai/glm-5.2:extended"),
		orModel("moonshotai/kimi-k2.6"),
		orModel("google/gemini-3-flash"),
		orModel("mistralai/mistral-large"),
	}
	rules := []string{"anthropic/", "openai/", "z-ai/glm-5.2", "moonshotai/kimi-k2.6"}

	got := filterModels(catalog, rules)
	ids := make([]string, 0, len(got))
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	want := []string{
		"anthropic/claude-opus-4.8",
		"openai/gpt-5.2",
		"OpenAI/GPT-5.2-mini",
		"z-ai/glm-5.2",
		"moonshotai/kimi-k2.6",
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("filterModels = %v, want %v", ids, want)
	}
}

func TestFilterModelsVariantExclusion(t *testing.T) {
	catalog := []openrouter.Model{
		orModel("anthropic/claude-haiku-4.5:free"),
		orModel("z-ai/glm-5.2:extended"),
	}
	// Prefix rules exclude variants; an EXACT rule naming the variant admits it.
	got := filterModels(catalog, []string{"anthropic/", "z-ai/glm-5.2:extended"})
	if len(got) != 1 || got[0].ID != "z-ai/glm-5.2:extended" {
		t.Fatalf("variant handling wrong: %v", got)
	}
}

func TestFilterModelsCaseInsensitive(t *testing.T) {
	got := filterModels([]openrouter.Model{orModel("Z-AI/GLM-5.2")}, []string{"z-ai/glm-5.2"})
	if len(got) != 1 {
		t.Fatalf("expected case-insensitive exact match, got %v", got)
	}
}

func TestFilterModelsEmptyRules(t *testing.T) {
	got := filterModels([]openrouter.Model{orModel("anthropic/claude-opus-4.8")}, nil)
	if len(got) != 0 {
		t.Fatalf("empty rules must admit nothing, got %v", got)
	}
}

// ---- catalog DTO mapping + sorting -------------------------------------------

func TestBuildChatLabModel(t *testing.T) {
	m := openrouter.Model{
		ID:                  "anthropic/claude-opus-4.8",
		Name:                "Claude Opus 4.8",
		Description:         "Frontier model. Supports xhigh reasoning effort.",
		ContextLength:       200000,
		Architecture:        openrouter.Architecture{InputModalities: []string{"text", "image"}},
		SupportedParameters: []string{"max_tokens", "reasoning", "temperature"},
		Pricing:             openrouter.Pricing{Prompt: "0.000003", Completion: "0.000015"},
		Created:             1750000000,
	}
	dto := buildChatLabModel(m)
	if dto.Provider != "anthropic" {
		t.Fatalf("provider = %q", dto.Provider)
	}
	if !dto.SupportsImages || !dto.SupportsReasoning {
		t.Fatalf("capability flags wrong: %+v", dto)
	}
	if strings.Join(dto.SupportedEfforts, ",") != "low,medium,high,xhigh" {
		t.Fatalf("efforts = %v", dto.SupportedEfforts)
	}
	if dto.Pricing.PromptUsdPerMTok != 3 || dto.Pricing.CompletionUsdPerMTok != 15 {
		t.Fatalf("pricing = %+v", dto.Pricing)
	}

	plain := buildChatLabModel(openrouter.Model{ID: "openai/gpt-5.2-mini", SupportedParameters: []string{"max_tokens"}})
	if plain.SupportsReasoning || len(plain.SupportedEfforts) != 0 {
		t.Fatalf("non-reasoning model should expose no efforts: %+v", plain)
	}
}

func TestSortChatLabModels(t *testing.T) {
	raw := []openrouter.Model{
		{ID: "mistralai/mistral-large", Created: 300},
		{ID: "openai/gpt-5.2", Created: 100},
		{ID: "anthropic/claude-old", Created: 100},
		{ID: "anthropic/claude-new", Created: 200},
		{ID: "google/gemini-3", Created: 400},
	}
	dtos := make([]models.DrChatLabModel, 0, len(raw))
	for _, m := range raw {
		dtos = append(dtos, buildChatLabModel(m))
	}
	sortChatLabModels(dtos)

	ids := make([]string, 0, len(dtos))
	for _, d := range dtos {
		ids = append(ids, d.ID)
	}
	// Anthropic first (newest created first), then OpenAI, then others
	// alphabetically by provider.
	want := []string{
		"anthropic/claude-new",
		"anthropic/claude-old",
		"openai/gpt-5.2",
		"google/gemini-3",
		"mistralai/mistral-large",
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("sort order = %v, want %v", ids, want)
	}
}

// ---- chatLabAttachmentExt ------------------------------------------------------

func TestChatLabAttachmentExt(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		contentType string
		wantExt     string
		wantMax     int64
		wantOK      bool
	}{
		{"image png", "image", "image/png", "png", drMaxImageAssetBytes, true},
		{"image jpeg", "image", "image/jpeg", "jpg", drMaxImageAssetBytes, true},
		{"image webp", "image", "image/webp", "webp", drMaxImageAssetBytes, true},
		{"image gif", "image", "image/gif", "gif", drMaxImageAssetBytes, true},
		{"image mixed-case content type", "image", "IMAGE/PNG", "png", drMaxImageAssetBytes, true},
		{"file pdf", "file", "application/pdf", "pdf", drChatLabMaxPDFBytes, true},
		{"file txt", "file", "text/plain", "txt", drChatLabMaxInlineFileBytes, true},
		{"file csv", "file", "text/csv", "csv", drChatLabMaxInlineFileBytes, true},
		{"file md", "file", "text/markdown", "md", drChatLabMaxInlineFileBytes, true},
		{"file json", "file", "application/json", "json", drChatLabMaxInlineFileBytes, true},
		// Denied: things the feedback/doc allowlist accepts but chat-lab must not.
		{"no video kind", "video", "video/mp4", "", 0, false},
		{"no svg image", "image", "image/svg+xml", "", 0, false},
		{"no zip", "file", "application/zip", "", 0, false},
		{"no docx", "file", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "", 0, false},
		{"no image type as file", "file", "image/png", "", 0, false},
		{"no pdf as image", "image", "application/pdf", "", 0, false},
		{"unknown kind", "audio", "audio/mpeg", "", 0, false},
		{"empty", "", "", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext, maxBytes, ok := chatLabAttachmentExt(tc.kind, tc.contentType)
			if ext != tc.wantExt || maxBytes != tc.wantMax || ok != tc.wantOK {
				t.Fatalf("chatLabAttachmentExt(%q, %q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.kind, tc.contentType, ext, maxBytes, ok, tc.wantExt, tc.wantMax, tc.wantOK)
			}
		})
	}
}

// ---- Title derivation -------------------------------------------------------------

func TestDeriveChatTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "   ", "New Chat"},
		{"short passes through", "Convert this text to JSON", "Convert this text to JSON"},
		{"whitespace collapsed", "hello\n\n  world\t!", "hello world !"},
		{
			"long truncates on a word boundary with ellipsis",
			"Please transcribe the attached photo of my handwritten field notes and format everything neatly",
			"Please transcribe the attached photo of my handwritten…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveChatTitle(tc.in); got != tc.want {
				t.Fatalf("deriveChatTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Never longer than the cap + ellipsis, even with no spaces to break on.
	long := strings.Repeat("x", 500)
	got := deriveChatTitle(long)
	if len([]rune(got)) > drChatLabDerivedTitleChars+1 {
		t.Fatalf("derived title too long: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
}

func TestSanitizeGeneratedTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"strips wrapping quotes", `"OCR Model Comparison"`, "OCR Model Comparison"},
		{"strips newlines", "OCR\nModel\nComparison", "OCR Model Comparison"},
		{"strips backticks and smart quotes", "`“OCR Test”`", "OCR Test"},
		{"empty stays empty", "  \n ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeGeneratedTitle(tc.in); got != tc.want {
				t.Fatalf("sanitizeGeneratedTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	long := strings.Repeat("word ", 40) // 200 chars
	got := sanitizeGeneratedTitle(long)
	if n := len([]rune(got)); n > drChatLabGeneratedTitleChars {
		t.Fatalf("generated title too long: %d runes", n)
	}
}

// ---- buildOpenRouterMessages --------------------------------------------------------

func fakeFetch(objects map[string][]byte) func(string) ([]byte, error) {
	return func(key string) ([]byte, error) {
		if b, ok := objects[key]; ok {
			return b, nil
		}
		return nil, errors.New("missing object " + key)
	}
}

func TestBuildOpenRouterMessagesTextOnly(t *testing.T) {
	history := []storedChatMessage{
		{ID: "m1", Role: "user", Content: "What is 2+2?"},
		{ID: "m2", Role: "assistant", Content: "4"},
		{ID: "m3", Role: "user", Content: "And doubled?"},
	}
	got, err := buildOpenRouterMessages(history, nil, fakeFetch(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(got))
	}
	for i, m := range got {
		if _, ok := m.Content.(string); !ok {
			t.Fatalf("message %d content should be a plain string, got %T", i, m.Content)
		}
	}
	if got[1].Role != "assistant" || got[1].Content.(string) != "4" {
		t.Fatalf("assistant history wrong: %+v", got[1])
	}
}

// Assistant reasoning must NOT be echoed back upstream. storedChatMessage has
// no reasoning field by design — this test pins the assistant turn to exactly
// its plain text content.
func TestBuildOpenRouterMessagesAssistantReasoningNotEchoed(t *testing.T) {
	history := []storedChatMessage{
		{ID: "m1", Role: "user", Content: "Think hard about this."},
		{ID: "m2", Role: "assistant", Content: "Final answer only."},
	}
	got, err := buildOpenRouterMessages(history, nil, fakeFetch(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, err := json.Marshal(got[1])
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"role":"assistant","content":"Final answer only."}`; string(raw) != want {
		t.Fatalf("assistant upstream message = %s, want %s", raw, want)
	}
}

func TestBuildOpenRouterMessagesImageDataURL(t *testing.T) {
	imgBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a}
	history := []storedChatMessage{{ID: "m1", Role: "user", Content: "Transcribe this"}}
	atts := map[string][]storedChatAttachment{
		"m1": {{Kind: "image", FileName: "scan.png", ContentType: "image/png", S3Key: "chatlab/s/attachments/a.png"}},
	}
	got, err := buildOpenRouterMessages(history, atts, fakeFetch(map[string][]byte{"chatlab/s/attachments/a.png": imgBytes}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts, ok := got[0].Content.([]openrouter.ContentPart)
	if !ok {
		t.Fatalf("expected multimodal parts, got %T", got[0].Content)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "Transcribe this" {
		t.Fatalf("text part first, got %+v", parts)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imgBytes)
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != wantURL {
		t.Fatalf("image part = %+v, want data URL %q", parts[1], wantURL)
	}
}

func TestBuildOpenRouterMessagesPDFPart(t *testing.T) {
	pdfBytes := []byte("%PDF-1.7 fake")
	history := []storedChatMessage{{ID: "m1", Role: "user", Content: "Summarize"}}
	atts := map[string][]storedChatAttachment{
		"m1": {{Kind: "file", FileName: "report.pdf", ContentType: "application/pdf", S3Key: "k.pdf"}},
	}
	got, err := buildOpenRouterMessages(history, atts, fakeFetch(map[string][]byte{"k.pdf": pdfBytes}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts := got[0].Content.([]openrouter.ContentPart)
	if len(parts) != 2 || parts[1].Type != "file" || parts[1].File == nil {
		t.Fatalf("expected a file part, got %+v", parts)
	}
	if parts[1].File.Filename != "report.pdf" {
		t.Fatalf("file name = %q", parts[1].File.Filename)
	}
	wantData := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdfBytes)
	if parts[1].File.FileData != wantData {
		t.Fatalf("file data = %q, want %q", parts[1].File.FileData, wantData)
	}
	if !hasPDFAttachment(atts) {
		t.Fatal("hasPDFAttachment should be true")
	}
}

func TestBuildOpenRouterMessagesTextFileInlining(t *testing.T) {
	csv := "a,b\n1,2\n"
	history := []storedChatMessage{{ID: "m1", Role: "user", Content: "Convert to JSON"}}
	atts := map[string][]storedChatAttachment{
		"m1": {{Kind: "file", FileName: "data.csv", ContentType: "text/csv", S3Key: "k.csv"}},
	}
	got, err := buildOpenRouterMessages(history, atts, fakeFetch(map[string][]byte{"k.csv": []byte(csv)}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts := got[0].Content.([]openrouter.ContentPart)
	if len(parts) != 1 {
		t.Fatalf("inlined text file should produce only the text part, got %+v", parts)
	}
	want := fmt.Sprintf("Convert to JSON\n\n[Attached file: data.csv]\n```\n%s\n```", csv)
	if parts[0].Text != want {
		t.Fatalf("inlined text = %q, want %q", parts[0].Text, want)
	}
	if hasPDFAttachment(atts) {
		t.Fatal("hasPDFAttachment should be false")
	}
}

func TestBuildOpenRouterMessagesInlineBudget(t *testing.T) {
	big := make([]byte, 300<<10) // two of these blow the 512 KiB budget
	history := []storedChatMessage{
		{ID: "m1", Role: "user", Content: "file one"},
		{ID: "m2", Role: "user", Content: "file two"},
	}
	atts := map[string][]storedChatAttachment{
		"m1": {{Kind: "file", FileName: "one.txt", ContentType: "text/plain", S3Key: "one"}},
		"m2": {{Kind: "file", FileName: "two.txt", ContentType: "text/plain", S3Key: "two"}},
	}
	_, err := buildOpenRouterMessages(history, atts, fakeFetch(map[string][]byte{"one": big, "two": big}))
	if !errors.Is(err, errChatLabInlineBudget) {
		t.Fatalf("expected errChatLabInlineBudget, got %v", err)
	}
}

func TestBuildOpenRouterMessagesFetchError(t *testing.T) {
	history := []storedChatMessage{{ID: "m1", Role: "user", Content: "hi"}}
	atts := map[string][]storedChatAttachment{
		"m1": {{Kind: "image", FileName: "x.png", ContentType: "image/png", S3Key: "gone"}},
	}
	if _, err := buildOpenRouterMessages(history, atts, fakeFetch(nil)); err == nil {
		t.Fatal("expected a fetch error to propagate")
	}
}

// ---- Downstream SSE event wire shapes -------------------------------------------------

func TestChatLabEventMarshaling(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{
			"meta",
			chatLabMetaEvent{Type: "meta", UserMessageID: "u-1", AssistantMessageID: "a-1"},
			`{"type":"meta","userMessageId":"u-1","assistantMessageId":"a-1"}`,
		},
		{
			"reasoning",
			chatLabReasoningEvent{Type: "reasoning", Text: "thinking…"},
			`{"type":"reasoning","text":"thinking…"}`,
		},
		{
			"delta",
			chatLabDeltaEvent{Type: "delta", Text: "Hello"},
			`{"type":"delta","text":"Hello"}`,
		},
		{
			// durationMs/reasoningMs ride on the usage event so the
			// just-streamed message renders its timing without a refetch.
			"usage",
			chatLabUsageEvent{Type: "usage", PromptTokens: 10, CompletionTokens: 20, ReasoningTokens: 5, CostUsd: 0.0042, DurationMs: intPtr(231000), ReasoningMs: intPtr(194000)},
			`{"type":"usage","promptTokens":10,"completionTokens":20,"reasoningTokens":5,"costUsd":0.0042,"durationMs":231000,"reasoningMs":194000}`,
		},
		{
			// No reasoning → reasoningMs is null, never 0.
			"usage without reasoning",
			chatLabUsageEvent{Type: "usage", PromptTokens: 10, CompletionTokens: 20, CostUsd: 0.0042, DurationMs: intPtr(8600)},
			`{"type":"usage","promptTokens":10,"completionTokens":20,"reasoningTokens":0,"costUsd":0.0042,"durationMs":8600,"reasoningMs":null}`,
		},
		{
			"done",
			chatLabDoneEvent{Type: "done", Status: "complete"},
			`{"type":"done","status":"complete"}`,
		},
		{
			"error",
			chatLabErrorEvent{Type: "error", Message: chatLabSafeUpstreamError},
			`{"type":"error","message":"The AI provider request failed"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.want {
				t.Fatalf("marshal = %s, want %s", raw, tc.want)
			}
		})
	}
}

// ---- parsePerTokenUSD -----------------------------------------------------------------

func TestParsePerTokenUSD(t *testing.T) {
	if got := parsePerTokenUSD("0.000003"); got != 3 {
		t.Fatalf("parsePerTokenUSD = %v, want 3", got)
	}
	if got := parsePerTokenUSD(""); got != 0 {
		t.Fatalf("empty should be 0, got %v", got)
	}
	if got := parsePerTokenUSD("not-a-number"); got != 0 {
		t.Fatalf("garbage should be 0, got %v", got)
	}
}
