// Package assistant is Medi, the Media Manipulator site guide and editing
// copilot (MM104, docs/design/MEDI_ASSISTANT.md, ADR-0018). Code identifier
// `assistant`, user-facing brand Medi (the Darkroom/`studio` split).
//
// Layout:
//
//	provider.go             Provider interface, request/events (the model seam)
//	openrouter_provider.go  the one Provider in this wave (hosted Claude via OpenRouter)
//	persona.go              persona + rules (design §12), byte-stable
//	prompt.go               system blocks: 1 cached (persona, rules, capsule), 2 volatile
//	tools.go                tool definitions, argument validation, server executors
//	ops.go                  JSON Schema validation of apply_edits (ops/schema/<surface>.json, MM108+)
//	retrieve.go             Knowledge/Retriever interface, routes.json, the MM104 stub
//	spend.go                the daily USD spend cap (reservations + settle)
//	store.go                the persistence slice the engine needs (*telemetry.Store)
//	request.go              the POST /api/assistant/chat body and its limits
//	engine.go               one request: prompt, upstream loop, client tools, persistence
//	static.go               the retrieval-only guide answer at the spend cap
//
// Nothing in this package touches a GPU or a local model daemon (a test greps for it).
package assistant

import (
	"context"
	"encoding/json"
)

// Block is one system-prompt block. Cache marks a prompt-cache breakpoint
// after the block (only the byte-stable block 1 sets it).
type Block struct {
	Text  string
	Cache bool
}

// ToolCall is a finalized tool invocation from the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded object
}

// Message is one provider-neutral conversation message.
//
//   - user:      Text, plus optional Images (data URLs; request-only, never stored)
//   - assistant: Text and/or ToolCalls; ProviderState is the provider's opaque
//     continuation data (OpenRouter reasoning_details), passed back verbatim
//   - tool:      ToolCallID + Text (the result)
type Message struct {
	Role          string
	Text          string
	Images        []string
	ToolCalls     []ToolCall
	ToolCallID    string
	ProviderState json.RawMessage
}

// ToolDef is one tool the model may call. Parameters is a JSON Schema object.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Request is one upstream call.
type Request struct {
	Model     string
	System    []Block
	Messages  []Message
	Tools     []ToolDef
	MaxTokens int
	// Effort is the reasoning effort ("low" | "medium" | "high"); "" = none.
	// temperature/top_p are never sent (decision 3).
	Effort string
}

// Usage is one upstream call's accounting. CostUSD is the provider-reported
// cost; Reported=false means the provider sent none (the engine estimates).
type Usage struct {
	InputTokens  int
	OutputTokens int
	CachedTokens int
	CostUSD      float64
	Reported     bool
}

// EventKind enumerates stream events.
type EventKind int

const (
	// EventDelta carries visible answer text.
	EventDelta EventKind = iota + 1
	// EventUsage carries the call's usage (once, near the end).
	EventUsage
	// EventDone ends the call: ToolCalls (possibly empty) and ProviderState.
	EventDone
)

// Event is one stream event.
type Event struct {
	Kind          EventKind
	Text          string
	Usage         *Usage
	ToolCalls     []ToolCall
	ProviderState json.RawMessage
}

// Stream yields events until EventDone, then io.EOF. A non-nil error other
// than io.EOF is an upstream or transport failure (or the context ending).
type Stream interface {
	Next() (Event, error)
	Close() error
}

// Provider starts one streamed completion. A pre-stream failure returns an
// error and no stream.
type Provider interface {
	Stream(ctx context.Context, req Request) (Stream, error)
}
