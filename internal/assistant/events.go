package assistant

import "encoding/json"

// Downstream SSE events of POST /api/assistant/chat (design §10), one JSON
// object per `data: {json}\n\n` record (the DR chat lab's wire format), with a
// `: ping` comment every 15 s. Order of one request:
//
//	meta → (delta | tool)* → [edits | action | ask]* → usage → done
//	meta → … → error                                  (any failure after meta)
//
// The shapes are the contract the web (MM107) and phone (MA54) clients parse.

// MetaEvent opens every stream. ConversationID is null for guests.
type MetaEvent struct {
	Type           string  `json:"type"` // "meta"
	ConversationID *string `json:"conversationId"`
	TurnID         string  `json:"turnId"`
	Model          string  `json:"model"`
	// PromptVersion is the build's content hash (MM113, PromptVersion()).
	PromptVersion string `json:"promptVersion"`
}

// DeltaEvent is visible answer text (markdown).
type DeltaEvent struct {
	Type string `json:"type"` // "delta"
	Text string `json:"text"`
}

// ToolEvent narrates a server tool: "running", then "ok" | "error".
type ToolEvent struct {
	Type   string `json:"type"` // "tool"
	Name   string `json:"name"`
	Status string `json:"status"`
}

// EditsEvent is a schema-valid apply_edits call: apply all ops or none as one
// undo step labelled "Medi: <summary>", then POST editResult with this callId.
type EditsEvent struct {
	Type    string          `json:"type"` // "edits"
	CallID  string          `json:"callId"`
	Summary string          `json:"summary"`
	Reason  string          `json:"reason,omitempty"`
	Ops     json.RawMessage `json:"ops"`
}

// ActionEvent is a button: kind navigate | open_tool | generate_captions |
// subject_mask | heal | voiceover | derive_audio | export | confirm_job. params: navigate
// {target}; open_tool {slug, route?, preset?, options?}; confirm_job (MM116b)
// {tool, toolName?, family, route?, options?, quota, queueEstimate} (the server computes
// toolName, quota and queueEstimate; nothing has run); suggest_action kinds pass the model's params through.
type ActionEvent struct {
	Type   string         `json:"type"` // "action"
	CallID string         `json:"callId"`
	Kind   string         `json:"kind"`
	Label  string         `json:"label"`
	Params map[string]any `json:"params"`
}

// AskEvent is a clarifying question with up to four choice chips; the user's
// next message (a chip or free text) is the answer.
type AskEvent struct {
	Type     string   `json:"type"` // "ask"
	CallID   string   `json:"callId"`
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
}

// UsageEvent totals every upstream call of this request.
type UsageEvent struct {
	Type         string  `json:"type"` // "usage"
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	CachedTokens int     `json:"cachedTokens"`
	CostUSD      float64 `json:"costUsd"`
}

// LimitEvent is reserved for an in-stream allowance notice. In MM104 an
// exhausted allowance is the pre-stream 429 `quota_exceeded` every gated
// route returns (metric assistant_turns), so this event is not emitted yet.
type LimitEvent struct {
	Type        string `json:"type"` // "limit"
	Metric      string `json:"metric"`
	Limit       int    `json:"limit"`
	UpgradePath string `json:"upgradePath"`
}

// DoneEvent ends a successful stream. Awaiting "edit_result" means an edits
// event was sent and the client must answer with editResult. MessageID is the
// stored assistant message (signed-in only; the feedback target). Outcome
// (MM107) marks the two answers that called no model: "static" (the spend-cap
// answer) and "out_of_scope" (the guide's scope gate); absent otherwise.
type DoneEvent struct {
	Type      string  `json:"type"` // "done"
	Awaiting  string  `json:"awaiting,omitempty"`
	MessageID *string `json:"messageId,omitempty"`
	Outcome   string  `json:"outcome,omitempty"`
}

// ErrorEvent ends a failed stream. Codes: upstream, assistant_paused,
// invalid_ops, tool_limit, internal.
type ErrorEvent struct {
	Type    string `json:"type"` // "error"
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes of ErrorEvent.
const (
	CodeUpstream = "upstream"
	CodePaused   = "assistant_paused"
	CodeInvalid  = "invalid_ops"
	CodeToolCap  = "tool_limit"
	CodeInternal = "internal"
)

// Emitter writes events downstream. Send returns an error when the client is
// gone; Ping writes a keepalive comment.
type Emitter interface {
	Send(ev any) error
	Ping() error
}
