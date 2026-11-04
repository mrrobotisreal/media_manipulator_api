package assistant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// The OpenRouter request: Anthropic-only routing, no fallbacks, data
// collection denied, a cache breakpoint after block 1 only, effort through
// reasoning, never temperature/top_p, images as image_url parts.
func TestOpenRouterChatRequest(t *testing.T) {
	p := &openrouterProvider{order: []string{"anthropic"}}
	req := p.chatRequest(Request{Model: "anthropic/claude-sonnet-5.5", MaxTokens: 8192, Effort: "medium",
		System:   []Block{{Text: "persona", Cache: true}, {Text: "live"}},
		Messages: []Message{{Role: "user", Text: "look", Images: []string{"data:image/jpeg;base64,AAA"}}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "c", Name: "search_help", Arguments: `{}`}}, ProviderState: json.RawMessage(`[{"type":"reasoning.encrypted"}]`)}, {Role: "tool", ToolCallID: "c", Text: "r"}},
		Tools:    []ToolDef{{Name: "search_help", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}}})
	b, _ := json.Marshal(req)
	s := string(b)
	for _, want := range []string{
		`"provider":{"order":["anthropic"],"allow_fallbacks":false,"data_collection":"deny"}`,
		`"reasoning":{"effort":"medium"}`,
		`{"role":"system","content":[{"type":"text","text":"persona","cache_control":{"type":"ephemeral"}},{"type":"text","text":"live"}]}`,
		`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AAA"}}`,
		`"reasoning_details":[{"type":"reasoning.encrypted"}]`,
		`{"role":"tool","content":"r","tool_call_id":"c"}`,
		`"max_tokens":8192`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s\n%s", want, s)
		}
	}
	for _, banned := range []string{"temperature", "top_p", `"usage"`} {
		if strings.Contains(s, banned) {
			t.Errorf("request must not send %s", banned)
		}
	}
}

type fakeChunks struct {
	chunks []*openrouter.StreamChunk
	err    error
}

func (f *fakeChunks) Next() (*openrouter.StreamChunk, error) {
	if len(f.chunks) == 0 {
		if f.err != nil {
			return nil, f.err
		}
		return nil, io.EOF
	}
	c := f.chunks[0]
	f.chunks = f.chunks[1:]
	return c, nil
}
func (f *fakeChunks) Close() error { return nil }

func chunk(js string) *openrouter.StreamChunk {
	var c openrouter.StreamChunk
	_ = json.Unmarshal([]byte(js), &c)
	return &c
}

// The adapter forwards content, accumulates tool-call fragments by index and
// raw reasoning_details, and ends with usage then done.
func TestORStreamAdapter(t *testing.T) {
	src := &fakeChunks{chunks: []*openrouter.StreamChunk{
		chunk(`{"choices":[{"delta":{"content":"Hel","reasoning_details":[{"type":"reasoning.text","text":"t"}]}}]}`),
		chunk(`{"choices":[{"delta":{"content":"lo","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"apply_edits","arguments":"{\"su"}}]}}]}`),
		chunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"mmary\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`),
		chunk(`{"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":40,"cost":0.0031,"prompt_tokens_details":{"cached_tokens":1000}}}`),
	}}
	s := newORStream(src)
	var evs []Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	if len(evs) != 4 || evs[0].Text != "Hel" || evs[1].Text != "lo" || evs[2].Kind != EventUsage || evs[3].Kind != EventDone {
		t.Fatalf("events = %+v", evs)
	}
	if u := evs[2].Usage; u.InputTokens != 1200 || u.CachedTokens != 1000 || u.CostUSD != 0.0031 || !u.Reported {
		t.Errorf("usage = %+v", u)
	}
	d := evs[3]
	if len(d.ToolCalls) != 1 || d.ToolCalls[0].ID != "c1" || d.ToolCalls[0].Arguments != `{"summary":"x"}` {
		t.Errorf("calls = %+v", d.ToolCalls)
	}
	if string(d.ProviderState) != `[{"type":"reasoning.text","text":"t"}]` {
		t.Errorf("state = %s", d.ProviderState)
	}
	// A mid-stream error surfaces as an error, not EOF.
	s = newORStream(&fakeChunks{err: errors.New("boom")})
	if _, err := s.Next(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
}

func jpeg(n int) string {
	b := make([]byte, n)
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0})
	return base64.StdEncoding.EncodeToString(b)
}

// Decision 9: body limits.
func TestChatBodyValidate(t *testing.T) {
	ok := func(b ChatBody) ChatBody { return b }
	cases := []struct {
		name string
		body ChatBody
		code string
	}{
		{"guide ok", ok(guideMsg("hi")), ""},
		{"bad surface", ChatBody{Surface: "admin", Message: &MessageIn{Text: "x"}}, "invalid_surface"},
		{"neither", ChatBody{Surface: "guide"}, "invalid_body"},
		{"both", ChatBody{Surface: "photo", Message: &MessageIn{Text: "x"}, EditResult: &EditResult{CallID: "c"}}, "invalid_body"},
		{"empty", ChatBody{Surface: "guide", Message: &MessageIn{Text: "  "}}, "empty_message"},
		{"long", ChatBody{Surface: "guide", Message: &MessageIn{Text: strings.Repeat("я", MaxMessageChars+1)}}, "message_too_long"},
		{"4000 runes ok", ChatBody{Surface: "guide", Message: &MessageIn{Text: strings.Repeat("я", MaxMessageChars)}}, ""},
		{"digest", ChatBody{Surface: "photo", Message: &MessageIn{Text: "x"}, Context: &ContextIn{Digest: strings.Repeat("d", MaxDigestBytes+1)}}, "digest_too_large"},
		{"frame on guide", ChatBody{Surface: "guide", Message: &MessageIn{Text: "x"}, Frame: &FrameIn{JPEGBase64: jpeg(10)}}, "invalid_body"},
		{"frame ok", ChatBody{Surface: "photo", Message: &MessageIn{Text: "x"}, Frame: &FrameIn{JPEGBase64: jpeg(1000)}}, ""},
		{"frame big", ChatBody{Surface: "photo", Message: &MessageIn{Text: "x"}, Frame: &FrameIn{JPEGBase64: jpeg(307201)}}, "frame_too_large"},
		{"frame png", ChatBody{Surface: "photo", Message: &MessageIn{Text: "x"}, Frame: &FrameIn{JPEGBase64: base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"))}}, "invalid_frame"},
		{"editResult on guide", ChatBody{Surface: "guide", EditResult: &EditResult{CallID: "c"}}, "invalid_body"},
		{"editResult needs ids", ChatBody{Surface: "photo", EditResult: &EditResult{CallID: "c"}}, "invalid_body"},
		{"plan on guide", ChatBody{Surface: "guide", Plan: true, Message: &MessageIn{Text: "x"}}, "invalid_body"},
		{"bad conv id", ChatBody{Surface: "guide", ConversationID: "nope", Message: &MessageIn{Text: "x"}}, "invalid_conversation"},
		{"bad locale", ChatBody{Surface: "guide", Locale: "<script>", Message: &MessageIn{Text: "x"}}, "invalid_locale"},
		{"bad page", ChatBody{Surface: "guide", Page: PageRef{Path: "https://evil"}, Message: &MessageIn{Text: "x"}}, "invalid_page"},
	}
	for _, c := range cases {
		b := c.body
		_, _, err := b.Validate(307200)
		be, _ := AsBodyError(err)
		got := ""
		if be != nil {
			got = be.Code
		}
		if got != c.code {
			t.Errorf("%s: code = %q (%v), want %q", c.name, got, err, c.code)
		}
	}
	b := ChatBody{Surface: "photo", Message: &MessageIn{Text: "x"}, Frame: &FrameIn{JPEGBase64: jpeg(100)}}
	url, _, _ := b.Validate(307200)
	if !strings.HasPrefix(url, "data:image/jpeg;base64,/9j/") {
		t.Errorf("frame url = %.40s", url)
	}
}

// Decision 8: reservations hold worst-case spend; settle replaces it with the
// actual cost; the DB sum refreshes; the day rolls at 00:00 UTC.
func TestSpendCapReserveSettle(t *testing.T) {
	now := time.Date(2026, 11, 4, 23, 0, 0, 0, time.UTC)
	src := newFakeStore()
	src.spend = 9.0
	c := NewSpendCap(10, src, func() time.Time { return now })
	ctx := context.Background()
	if c.Reached(ctx) {
		t.Fatal("9 < 10")
	}
	r1, ok := c.Reserve(ctx, 0.6)
	if !ok {
		t.Fatal("9.6 fits")
	}
	if _, ok := c.Reserve(ctx, 0.6); ok {
		t.Fatal("10.2 does not fit while the first call is in flight")
	}
	r1.Settle(0.05)
	r1.Settle(5) // idempotent
	if got := c.Spent(ctx); got < 9.049 || got > 9.051 {
		t.Fatalf("spent = %v", got)
	}
	if _, ok := c.Reserve(ctx, 0.6); !ok {
		t.Fatal("settled to the actual cost, the next call fits")
	}
	// The usage row lands: the refresh replaces the local sum with the DB sum.
	src.spend = 10.0
	now = now.Add(time.Minute)
	if !c.Reached(ctx) {
		t.Fatal("DB says 10")
	}
	// Midnight UTC resets.
	src.spend = 0
	now = time.Date(2026, 11, 5, 0, 0, 1, 0, time.UTC)
	if c.Reached(ctx) {
		t.Fatal("new day")
	}
	var nilCap *SpendCap
	if nilCap.Reached(ctx) {
		t.Fatal("nil cap never reached")
	}
	if _, ok := NewSpendCap(0, nil, nil).Reserve(ctx, 100); !ok {
		t.Fatal("cap 0 = disabled")
	}
}

func TestCostEstimates(t *testing.T) {
	// 10k input (8k cached) + 1k output on Sonnet 5.5.
	got := EstimateUSD("anthropic/claude-sonnet-5.5", Usage{InputTokens: 10000, CachedTokens: 8000, OutputTokens: 1000})
	want := 2000*0.000002 + 8000*0.0000001 + 1000*0.00001
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("estimate = %v, want %v", got, want)
	}
	if WorstCaseUSD("unknown/model", 1000, 1000) < WorstCaseUSD("anthropic/claude-opus-5.5", 1000, 1000) {
		t.Error("an unknown model reserves at least the most expensive default")
	}
}

func TestStaticAnswerAndLocales(t *testing.T) {
	hits := []Hit{{Doc: Doc{Title: "A", URL: "/a"}}, {Doc: Doc{Title: "editor doc"}}, {Doc: Doc{Title: "B", URL: "/b"}}, {Doc: Doc{Title: "C", URL: "/c"}}, {Doc: Doc{Title: "D", URL: "/d"}}}
	got := StaticAnswer("ru-RU", hits)
	if strings.Count(got, "](/") != 3 || strings.Contains(got, "editor doc") || strings.Contains(got, "/d") {
		t.Errorf("static = %q", got)
	}
	if StaticAnswer("en", nil) == "" {
		t.Error("no hits still answers")
	}
	if LocaleName("pt-BR") != "Portuguese (Brazil)" || LocaleName("ru-RU") != "Russian" || LocaleName("xx") != "" {
		t.Error("locale names")
	}
}

func TestRoutes(t *testing.T) {
	r, err := ParseRoutes([]byte(`[{"route":"/pricing","kind":"page"},{"route":"/tools/a","kind":"tool","slug":"a"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasRoute("/pricing") || !r.HasRoute("/ru/pricing") || r.HasRoute("/admin") || r.HasRoute("/ru/admin") {
		t.Error("route checks")
	}
	if _, ok := r.Tool("a"); !ok {
		t.Error("tool a")
	}
	var none *Routes
	if !none.HasRoute("/anything") {
		t.Error("without routes.json only the shape is checked")
	}
	// MM105: routes.json is embedded (the generated export).
	if er := EmbeddedRoutes(); er == nil || !er.HasRoute("/pricing") || !er.HasRoute("/ru/pricing") {
		t.Error("embedded routes.json lists /pricing")
	} else if e, ok := er.Tool("compress-video"); !ok || e.Route != "/tools/compress-video" {
		t.Errorf("embedded tool route = %+v", e)
	}
}
