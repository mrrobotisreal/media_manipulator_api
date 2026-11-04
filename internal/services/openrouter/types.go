// Package openrouter is a small, self-contained OpenRouter client for the DR
// AI Chat Test Lab: the models catalog (GET /models) plus streaming and
// non-streaming chat completions (POST /chat/completions). Plain net/http +
// bufio — deliberately no third-party OpenAI SDK, consistent with how the repo
// avoids heavyweight deps. Shapes follow the OpenRouter API reference
// (https://openrouter.ai/docs — streaming, reasoning-tokens, image/PDF inputs,
// usage accounting) as of 2026-07.
package openrouter

import (
	"encoding/json"
	"fmt"
)

// ---- Chat completion request ------------------------------------------------

// Message is one chat turn. Content is either a plain string (text-only) or a
// []ContentPart (multimodal: text + image_url + input_audio + file parts).
//
// For an assistant turn that requested tools, ToolCalls carries the finalized
// calls and ReasoningDetails carries the model's reasoning_details VERBATIM —
// per the reasoning docs the block sequence must be passed back unmodified for
// reasoning models to continue after tool execution. For a tool-result turn
// (Role "tool"), ToolCallID links the result to its call.
type Message struct {
	Role             string            `json:"role"`
	Content          any               `json:"content"`
	ToolCalls        []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
	ReasoningDetails []json.RawMessage `json:"reasoning_details,omitempty"`
}

// ContentPart is one multimodal content element of a user message.
type ContentPart struct {
	Type       string      `json:"type"` // "text" | "image_url" | "input_audio" | "file"
	Text       string      `json:"text,omitempty"`
	ImageURL   *ImageURL   `json:"image_url,omitempty"`
	InputAudio *InputAudio `json:"input_audio,omitempty"`
	File       *FilePart   `json:"file,omitempty"`
	// CacheControl marks an explicit prompt-cache breakpoint after this part
	// (MM104; Anthropic models, at most four per request, per
	// https://openrouter.ai/docs/guides/best-practices/prompt-caching).
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl is the per-part cache breakpoint: Type "ephemeral" (5-minute
// TTL by default; TTL "1h" for the longer write).
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// EphemeralCache is the default 5-minute breakpoint.
func EphemeralCache() *CacheControl { return &CacheControl{Type: "ephemeral"} }

// ProviderPrefs is the request's provider-routing object (MM104), per
// https://openrouter.ai/docs/guides/routing/provider-selection: Order is the
// provider slugs tried in order, AllowFallbacks=false forbids any other
// provider, DataCollection "deny" limits routing to providers that do not
// collect user data. AllowFallbacks is a pointer because the API default is
// true and false must be sent explicitly.
type ProviderPrefs struct {
	Order          []string `json:"order,omitempty"`
	AllowFallbacks *bool    `json:"allow_fallbacks,omitempty"`
	DataCollection string   `json:"data_collection,omitempty"` // "allow" | "deny"
}

// InputAudio carries audio as RAW base64 (per the audio docs: base64 only,
// direct URLs are not supported for audio) plus its format ("mp3", "wav", …).
type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// ImageURL carries an image as a URL — for the chat lab always a base64 data
// URL (data:{contentType};base64,...) because the S3 bucket is private and
// presigned URLs must not be relied on being fetchable by OpenRouter.
type ImageURL struct {
	URL string `json:"url"`
}

// FilePart carries a document (PDF) as a base64 data URL, parsed upstream by
// the file-parser plugin (see Plugin).
type FilePart struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

// Plugin activates an OpenRouter plugin for the request. The chat lab uses only
// the file parser for PDF attachments; PDF.Engine is left empty so OpenRouter
// picks its documented default engine (mistral-ocr, falling back to native).
type Plugin struct {
	ID  string     `json:"id"`
	PDF *PDFPlugin `json:"pdf,omitempty"`
}

type PDFPlugin struct {
	Engine string `json:"engine,omitempty"`
}

// Reasoning is OpenRouter's unified reasoning control. The chat lab only sets
// Effort ('minimal'|'low'|'medium'|'high'|'xhigh'); the key is omitted entirely
// when reasoning is off or unsupported by the model.
type Reasoning struct {
	Effort string `json:"effort,omitempty"`
}

// ChatRequest is the POST /chat/completions body. Usage accounting needs no
// opt-in: per the current usage-accounting docs, `usage: {include: true}` is
// deprecated and full usage details are always included (in the final SSE chunk
// when streaming). Re-confirmed 2026-11-04 (MM104), so Medi does not send it.
type ChatRequest struct {
	Model     string     `json:"model"`
	Messages  []Message  `json:"messages"`
	Stream    bool       `json:"stream,omitempty"`
	MaxTokens int        `json:"max_tokens,omitempty"`
	Reasoning *Reasoning `json:"reasoning,omitempty"`
	Plugins   []Plugin   `json:"plugins,omitempty"`
	Tools     []Tool     `json:"tools,omitempty"`
	// Provider is the routing object (MM104); nil = OpenRouter's defaults,
	// which is what the DR chat lab sends.
	Provider *ProviderPrefs `json:"provider,omitempty"`
}

// Tool is one entry of the request's tools array (OpenAI function format).
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes the function; Parameters is a raw JSON Schema object.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is a finalized tool invocation on an assistant message.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded argument object
}

// ToolCallDelta is one streamed tool-call fragment (choices[].delta.tool_calls).
// The FIRST chunk for an index carries id/type/function.name; subsequent chunks
// carry only function.arguments fragments to concatenate — accumulate with
// toolCallAccumulator-style logic keyed by Index.
type ToolCallDelta struct {
	Index    int                   `json:"index"`
	ID       string                `json:"id"`
	Type     string                `json:"type"`
	Function ToolCallFunctionDelta `json:"function"`
}

type ToolCallFunctionDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ---- Models catalog -----------------------------------------------------------

// Model is one entry of GET /models `data[]`, keeping only the fields the chat
// lab consumes.
type Model struct {
	ID                  string       `json:"id"`
	Name                string       `json:"name"`
	Description         string       `json:"description"`
	ContextLength       int64        `json:"context_length"`
	Architecture        Architecture `json:"architecture"`
	SupportedParameters []string     `json:"supported_parameters"`
	Pricing             Pricing      `json:"pricing"`
	Created             int64        `json:"created"`
}

type Architecture struct {
	InputModalities []string `json:"input_modalities"`
}

// Pricing values are decimal strings in USD **per token** (per the models API
// reference), e.g. "0.000003" = $3/MTok.
type Pricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

type modelsResponse struct {
	Data []Model `json:"data"`
}

// ---- Streaming chunks ---------------------------------------------------------

// StreamChunk is one decoded SSE record of a streamed completion. Usage arrives
// once, on the final chunk. A mid-stream upstream failure arrives as a
// top-level Error alongside a choice with finish_reason "error" (HTTP status
// stays 200).
type StreamChunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage"`
	Error   *APIError      `json:"error"`
}

type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta carries the incremental content. Reasoning may arrive as the legacy
// plaintext `reasoning` field and/or structured `reasoning_details` entries —
// ReasoningText() below merges the two, preferring the plaintext field.
// ReasoningDetails is kept RAW so callers can pass the blocks back verbatim on
// the assistant tool-call message (the reasoning docs forbid rearranging or
// modifying the sequence). ToolCalls are streamed fragments (see ToolCallDelta).
type Delta struct {
	Content          string            `json:"content"`
	Reasoning        string            `json:"reasoning"`
	ReasoningDetails []json.RawMessage `json:"reasoning_details"`
	ToolCalls        []ToolCallDelta   `json:"tool_calls"`
}

// ReasoningDetail is the typed view of one structured reasoning entry
// ("reasoning.text" / "reasoning.summary" / "reasoning.encrypted" — encrypted
// payloads carry no readable text and are skipped for display).
type ReasoningDetail struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Summary string `json:"summary"`
}

// ReasoningText returns the human-readable reasoning delta of this chunk:
// the plaintext `delta.reasoning` when present, otherwise the concatenated
// readable reasoning_details entries.
func (d Delta) ReasoningText() string {
	if d.Reasoning != "" {
		return d.Reasoning
	}
	var out string
	for _, raw := range d.ReasoningDetails {
		var rd ReasoningDetail
		if err := json.Unmarshal(raw, &rd); err != nil {
			continue
		}
		switch rd.Type {
		case "reasoning.text":
			out += rd.Text
		case "reasoning.summary":
			out += rd.Summary
		}
	}
	return out
}

// Usage is OpenRouter's usage accounting object (always included; final SSE
// chunk when streaming). Cost is in USD credits.
type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	Cost                    float64                  `json:"cost"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details"`
	// PromptTokensDetails carries the prompt-cache accounting (MM104):
	// cached_tokens read from the cache, cache_write_tokens written to it.
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details"`
}

// PromptTokensDetails is usage.prompt_tokens_details (usage-accounting docs).
type PromptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// CachedTokens is a nil-safe accessor for prompt tokens read from the cache.
func (u *Usage) CachedTokens() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ReasoningTokens is a nil-safe accessor for the nested reasoning token count.
func (u *Usage) ReasoningTokens() int {
	if u == nil || u.CompletionTokensDetails == nil {
		return 0
	}
	return u.CompletionTokensDetails.ReasoningTokens
}

// ---- Non-streaming response ----------------------------------------------------

// ChatResponse is the non-streaming POST /chat/completions body (used only for
// title generation).
type ChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *Usage    `json:"usage"`
	Error *APIError `json:"error"`
}

// ---- Errors --------------------------------------------------------------------

// APIError is OpenRouter's error payload (top-level `error` object, both
// pre-stream JSON responses and mid-stream chunks). Code can be a number or a
// string upstream, so it is decoded loosely.
type APIError struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openrouter api error (code %v): %s", e.Code, e.Message)
}

// HTTPError is a non-2xx HTTP response from OpenRouter (pre-stream). Body is
// captured (truncated) for server-side logging only — callers must NEVER echo
// it to clients.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("openrouter http %d: %s", e.StatusCode, e.Body)
}
