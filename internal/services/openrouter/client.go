package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// App-attribution headers (per the OpenRouter app-attribution docs):
// HTTP-Referer identifies the app's URL, X-OpenRouter-Title its display name.
const attributionTitle = "Double Raven Chat Lab"

// Client talks to the OpenRouter API. Two http.Clients on purpose: streaming
// completions must NOT carry a global timeout (a long generation is normal —
// cancellation comes from the caller's context), while the short non-streaming
// calls (models list, title generation) get a hard 30s cap.
type Client struct {
	baseURL    string
	apiKey     string
	referer    string
	streamHTTP *http.Client
	shortHTTP  *http.Client
}

// New builds a Client. baseURL is the API root (default
// https://openrouter.ai/api/v1); attributionURL is sent as the HTTP-Referer
// app-attribution header.
func New(baseURL, apiKey, attributionURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		referer:    attributionURL,
		streamHTTP: &http.Client{}, // no timeout: bounded by the request context
		shortHTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.referer != "" {
		req.Header.Set("HTTP-Referer", c.referer)
	}
	req.Header.Set("X-OpenRouter-Title", attributionTitle)
	return req, nil
}

// httpError drains (bounded) and closes the response body and returns a typed
// *HTTPError for a non-2xx response. The body is for server logs only.
func httpError(resp *http.Response) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return &HTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
}

// ListModels fetches GET /models and returns the raw catalog entries.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.shortHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	defer resp.Body.Close()
	var out modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	return out.Data, nil
}

// Complete runs a NON-streaming chat completion (used only for title
// generation). Stream is forced off.
func (c *Client) Complete(ctx context.Context, chatReq ChatRequest) (*ChatResponse, error) {
	chatReq.Stream = false
	payload, err := json.Marshal(chatReq)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	resp, err := c.shortHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	defer resp.Body.Close()
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode completion response: %w", err)
	}
	if out.Error != nil {
		return nil, out.Error
	}
	return &out, nil
}

// FirstText returns the first choice's message content, trimmed.
func (r *ChatResponse) FirstText() string {
	if r == nil || len(r.Choices) == 0 {
		return ""
	}
	return strings.TrimSpace(r.Choices[0].Message.Content)
}

// StreamChat starts a streaming chat completion. Stream is forced on. On a
// non-200 response the (bounded) body is captured into the returned error and
// no stream is opened. The caller must Close() the returned stream.
func (c *Client) StreamChat(ctx context.Context, chatReq ChatRequest) (*ChatStream, error) {
	chatReq.Stream = true
	payload, err := json.Marshal(chatReq)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	return newChatStream(resp), nil
}

// IsHTTPError reports whether err is a non-2xx OpenRouter response and returns
// its status code.
func IsHTTPError(err error) (int, bool) {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode, true
	}
	return 0, false
}
