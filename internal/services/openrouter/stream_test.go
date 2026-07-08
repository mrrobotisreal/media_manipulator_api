package openrouter

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Fixture payloads shaped per the current OpenRouter streaming docs: chunks
// carry choices[].delta (content and/or reasoning/reasoning_details), the final
// chunk carries usage, mid-stream failures carry a top-level error object, and
// the stream terminates with "[DONE]".

func TestParseSSERecordContentDelta(t *testing.T) {
	chunk, err := parseSSERecord([]byte(`{"id":"gen-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk == nil || len(chunk.Choices) != 1 {
		t.Fatalf("expected one choice, got %+v", chunk)
	}
	if got := chunk.Choices[0].Delta.Content; got != "Hello" {
		t.Fatalf("content delta = %q, want %q", got, "Hello")
	}
	if chunk.Choices[0].FinishReason != nil {
		t.Fatalf("finish_reason should be nil, got %v", *chunk.Choices[0].FinishReason)
	}
}

func TestParseSSERecordReasoningDelta(t *testing.T) {
	// Legacy plaintext field.
	chunk, err := parseSSERecord([]byte(`{"choices":[{"delta":{"reasoning":"Let me think"}}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := chunk.Choices[0].Delta.ReasoningText(); got != "Let me think" {
		t.Fatalf("reasoning delta = %q, want %q", got, "Let me think")
	}

	// Structured reasoning_details entries.
	chunk, err = parseSSERecord([]byte(`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"step one"},{"type":"reasoning.summary","summary":" summarized"},{"type":"reasoning.encrypted","data":"opaque"}]}}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := chunk.Choices[0].Delta.ReasoningText(); got != "step one summarized" {
		t.Fatalf("reasoning details text = %q, want %q", got, "step one summarized")
	}

	// Plaintext wins over details when both are present.
	chunk, err = parseSSERecord([]byte(`{"choices":[{"delta":{"reasoning":"plain","reasoning_details":[{"type":"reasoning.text","text":"detail"}]}}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := chunk.Choices[0].Delta.ReasoningText(); got != "plain" {
		t.Fatalf("reasoning text preference = %q, want %q", got, "plain")
	}
}

func TestParseSSERecordFinalUsageChunk(t *testing.T) {
	chunk, err := parseSSERecord([]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":194,"completion_tokens":42,"total_tokens":236,"cost":0.0042,"completion_tokens_details":{"reasoning_tokens":7}}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u := chunk.Usage
	if u == nil {
		t.Fatal("expected usage on the final chunk")
	}
	if u.PromptTokens != 194 || u.CompletionTokens != 42 || u.Cost != 0.0042 {
		t.Fatalf("usage = %+v", u)
	}
	if u.ReasoningTokens() != 7 {
		t.Fatalf("reasoning tokens = %d, want 7", u.ReasoningTokens())
	}
	if fr := chunk.Choices[0].FinishReason; fr == nil || *fr != "stop" {
		t.Fatalf("finish_reason = %v, want stop", fr)
	}
}

func TestParseSSERecordDone(t *testing.T) {
	chunk, err := parseSSERecord([]byte(`[DONE]`))
	if !errors.Is(err, errStreamDone) {
		t.Fatalf("expected errStreamDone, got %v (chunk %v)", err, chunk)
	}
}

func TestParseSSERecordEmpty(t *testing.T) {
	chunk, err := parseSSERecord([]byte("   \n "))
	if chunk != nil || err != nil {
		t.Fatalf("empty record should be a no-op, got chunk=%v err=%v", chunk, err)
	}
}

func TestParseSSERecordMalformedTolerated(t *testing.T) {
	chunk, err := parseSSERecord([]byte(`{"choices": [ this is not json`))
	if !errors.Is(err, errMalformedRecord) {
		t.Fatalf("expected errMalformedRecord, got %v (chunk %v)", err, chunk)
	}
}

func TestParseSSERecordUpstreamError(t *testing.T) {
	chunk, err := parseSSERecord([]byte(`{"error":{"code":"server_error","message":"provider exploded"},"choices":[{"delta":{},"finish_reason":"error"}]}`))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %v", err)
	}
	if apiErr.Message != "provider exploded" {
		t.Fatalf("api error message = %q", apiErr.Message)
	}
	// The partial chunk is still surfaced alongside the error.
	if chunk == nil || len(chunk.Choices) != 1 {
		t.Fatalf("expected chunk alongside the error, got %+v", chunk)
	}
}

// TestChatStreamNext runs a full wire-format fixture through the reader:
// comment/keepalive lines ignored, multiple records parsed in order, malformed
// records skipped, [DONE] → io.EOF.
func TestChatStreamNext(t *testing.T) {
	wire := strings.Join([]string{
		": OPENROUTER PROCESSING",
		"",
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		"",
		": keepalive comment mid-stream",
		"",
		"data: {not valid json",
		"",
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"cost":0.001}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	stream := newChatStream(&http.Response{Body: io.NopCloser(strings.NewReader(wire))})
	defer stream.Close()

	var contents []string
	var sawUsage bool
	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected stream error: %v", err)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				contents = append(contents, choice.Delta.Content)
			}
		}
		if chunk.Usage != nil {
			sawUsage = true
		}
	}
	if got := strings.Join(contents, ""); got != "Hello" {
		t.Fatalf("streamed content = %q, want %q", got, "Hello")
	}
	if !sawUsage {
		t.Fatal("expected the final usage chunk")
	}
}

// TestChatStreamMidStreamError: an upstream error record surfaces as a typed
// *APIError from Next().
func TestChatStreamMidStreamError(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
		"",
		`data: {"error":{"code":502,"message":"upstream body — must never reach clients"},"choices":[{"delta":{},"finish_reason":"error"}]}`,
		"",
	}, "\n")
	stream := newChatStream(&http.Response{Body: io.NopCloser(strings.NewReader(wire))})
	defer stream.Close()

	chunk, err := stream.Next()
	if err != nil || chunk.Choices[0].Delta.Content != "partial" {
		t.Fatalf("first chunk = %+v, err %v", chunk, err)
	}
	_, err = stream.Next()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %v", err)
	}
}
