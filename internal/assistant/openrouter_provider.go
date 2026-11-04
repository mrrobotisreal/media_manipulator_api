package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// openrouterProvider is the hosted-Claude Provider (decision 1). Every request
// pins provider.order (default anthropic) with fallbacks disallowed and data
// collection denied (design §6 "Upstream privacy"), puts a cache_control
// breakpoint after each cached system block, and never sends temperature/top_p.
// Field names were verified against openrouter.ai docs on 2026-11-04 (ticket log).
type openrouterProvider struct {
	client streamer
	order  []string
}

// streamer is the slice of *openrouter.Client the provider uses.
type streamer interface {
	StreamChat(ctx context.Context, req openrouter.ChatRequest) (*openrouter.ChatStream, error)
}

// AttributionTitle is the X-OpenRouter-Title of every Medi request.
const AttributionTitle = "Media Manipulator"

// NewOpenRouterProvider builds the provider over Medi's own key. order is
// provider.order (empty = ["anthropic"]).
func NewOpenRouterProvider(baseURL, apiKey, attributionURL string, order []string) Provider {
	if len(order) == 0 {
		order = []string{"anthropic"}
	}
	return &openrouterProvider{
		client: openrouter.New(baseURL, apiKey, attributionURL, openrouter.WithTitle(AttributionTitle)),
		order:  append([]string(nil), order...),
	}
}

// chatRequest converts a provider-neutral Request (pure; unit-tested).
func (p *openrouterProvider) chatRequest(req Request) openrouter.ChatRequest {
	no := false
	out := openrouter.ChatRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Provider:  &openrouter.ProviderPrefs{Order: p.order, AllowFallbacks: &no, DataCollection: "deny"},
	}
	if req.Effort != "" {
		out.Reasoning = &openrouter.Reasoning{Effort: req.Effort}
	}
	if len(req.System) > 0 {
		parts := make([]openrouter.ContentPart, 0, len(req.System))
		for _, b := range req.System {
			if b.Text == "" {
				continue
			}
			part := openrouter.ContentPart{Type: "text", Text: b.Text}
			if b.Cache {
				part.CacheControl = openrouter.EphemeralCache()
			}
			parts = append(parts, part)
		}
		out.Messages = append(out.Messages, openrouter.Message{Role: "system", Content: parts})
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, toORMessage(m))
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, openrouter.Tool{Type: "function", Function: openrouter.ToolFunction{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		}})
	}
	return out
}

func toORMessage(m Message) openrouter.Message {
	switch m.Role {
	case "assistant":
		om := openrouter.Message{Role: "assistant"}
		if m.Text != "" {
			om.Content = m.Text
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, openrouter.ToolCall{ID: tc.ID, Type: "function",
				Function: openrouter.ToolCallFunction{Name: tc.Name, Arguments: tc.Arguments}})
		}
		if len(m.ProviderState) > 0 {
			var details []json.RawMessage
			if json.Unmarshal(m.ProviderState, &details) == nil {
				om.ReasoningDetails = details
			}
		}
		return om
	case "tool":
		return openrouter.Message{Role: "tool", ToolCallID: m.ToolCallID, Content: m.Text}
	default:
		if len(m.Images) == 0 {
			return openrouter.Message{Role: m.Role, Content: m.Text}
		}
		parts := []openrouter.ContentPart{}
		if m.Text != "" {
			parts = append(parts, openrouter.ContentPart{Type: "text", Text: m.Text})
		}
		for _, img := range m.Images {
			parts = append(parts, openrouter.ContentPart{Type: "image_url", ImageURL: &openrouter.ImageURL{URL: img}})
		}
		return openrouter.Message{Role: m.Role, Content: parts}
	}
}

func (p *openrouterProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	s, err := p.client.StreamChat(ctx, p.chatRequest(req))
	if err != nil {
		return nil, err
	}
	return newORStream(s), nil
}

// chunkSource is the slice of *openrouter.ChatStream the adapter reads.
type chunkSource interface {
	Next() (*openrouter.StreamChunk, error)
	Close() error
}

// orStream adapts OpenRouter chunks to Events: content deltas pass through,
// tool-call fragments accumulate by index, reasoning_details accumulate raw
// (never shown, passed back verbatim on the next round), usage is held until
// the end so Usage precedes Done.
type orStream struct {
	src     chunkSource
	pending []Event
	calls   map[int]*ToolCall
	order   []int
	details []json.RawMessage
	usage   *Usage
	done    bool
}

func newORStream(src chunkSource) *orStream {
	return &orStream{src: src, calls: map[int]*ToolCall{}}
}

func (s *orStream) Next() (Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.done {
			return Event{}, io.EOF
		}
		chunk, err := s.src.Next()
		if chunk != nil {
			s.absorb(chunk)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return Event{}, err
			}
			s.finish()
			continue
		}
	}
}

func (s *orStream) absorb(chunk *openrouter.StreamChunk) {
	for _, ch := range chunk.Choices {
		if ch.Delta.Content != "" {
			s.pending = append(s.pending, Event{Kind: EventDelta, Text: ch.Delta.Content})
		}
		s.details = append(s.details, ch.Delta.ReasoningDetails...)
		for _, d := range ch.Delta.ToolCalls {
			tc, ok := s.calls[d.Index]
			if !ok {
				tc = &ToolCall{}
				s.calls[d.Index] = tc
				s.order = append(s.order, d.Index)
			}
			if d.ID != "" {
				tc.ID = d.ID
			}
			if d.Function.Name != "" {
				tc.Name = d.Function.Name
			}
			tc.Arguments += d.Function.Arguments
		}
	}
	if u := chunk.Usage; u != nil {
		s.usage = &Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, CachedTokens: u.CachedTokens(),
			CostUSD: u.Cost, Reported: u.Cost != 0}
	}
}

func (s *orStream) finish() {
	s.done = true
	if s.usage != nil {
		s.pending = append(s.pending, Event{Kind: EventUsage, Usage: s.usage})
	}
	sort.Ints(s.order)
	var calls []ToolCall
	for _, i := range s.order {
		tc := *s.calls[i]
		tc.Name = strings.TrimSpace(tc.Name)
		if tc.Arguments == "" {
			tc.Arguments = "{}"
		}
		calls = append(calls, tc)
	}
	done := Event{Kind: EventDone, ToolCalls: calls}
	if len(s.details) > 0 {
		if b, err := json.Marshal(s.details); err == nil {
			done.ProviderState = b
		}
	}
	s.pending = append(s.pending, done)
}

func (s *orStream) Close() error { return s.src.Close() }
