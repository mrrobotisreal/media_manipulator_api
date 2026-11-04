package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureServer records the last request's headers and body and answers a
// minimal non-streaming completion. Loopback only (httptest), never the network.
func captureServer(t *testing.T) (*httptest.Server, *http.Header, *[]byte) {
	t.Helper()
	var hdr http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.0001,"prompt_tokens_details":{"cached_tokens":8,"cache_write_tokens":0}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hdr, &body
}

// The DR chat lab builds its client without options: its attribution title must
// stay "Double Raven Chat Lab" (MM104 acceptance).
func TestClientDefaultTitleIsChatLab(t *testing.T) {
	srv, hdr, _ := captureServer(t)
	c := New(srv.URL, "k", "https://media-manipulator.com")
	if _, err := c.Complete(context.Background(), ChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if got := hdr.Get("X-OpenRouter-Title"); got != "Double Raven Chat Lab" {
		t.Fatalf("title = %q", got)
	}
	if got := hdr.Get("HTTP-Referer"); got != "https://media-manipulator.com" {
		t.Fatalf("referer = %q", got)
	}
}

func TestClientWithTitle(t *testing.T) {
	srv, hdr, _ := captureServer(t)
	c := New(srv.URL, "k", "", WithTitle("Media Manipulator"))
	if _, err := c.Complete(context.Background(), ChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if got := hdr.Get("X-OpenRouter-Title"); got != "Media Manipulator" {
		t.Fatalf("title = %q", got)
	}
	// A blank title keeps the default rather than sending an empty header.
	c = New(srv.URL, "k", "", WithTitle("  "))
	_, _ = c.Complete(context.Background(), ChatRequest{Model: "m"})
	if got := hdr.Get("X-OpenRouter-Title"); got != "Double Raven Chat Lab" {
		t.Fatalf("blank WithTitle title = %q", got)
	}
}

// The MM104 request fields serialise with the names the OpenRouter docs use.
func TestChatRequestProviderAndCacheFields(t *testing.T) {
	no := false
	req := ChatRequest{
		Model: "anthropic/claude-sonnet-5.5",
		Messages: []Message{{Role: "system", Content: []ContentPart{
			{Type: "text", Text: "persona", CacheControl: EphemeralCache()},
			{Type: "text", Text: "volatile"},
		}}},
		Provider: &ProviderPrefs{Order: []string{"anthropic"}, AllowFallbacks: &no, DataCollection: "deny"},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"provider":{"order":["anthropic"],"allow_fallbacks":false,"data_collection":"deny"}`,
		`{"type":"text","text":"persona","cache_control":{"type":"ephemeral"}}`,
		`{"type":"text","text":"volatile"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("request JSON missing %s\n%s", want, s)
		}
	}
	if strings.Contains(s, `"usage"`) {
		t.Error("usage.include is deprecated and must not be sent")
	}
	// The DR chat lab's request (no provider) keeps its exact shape.
	b, _ = json.Marshal(ChatRequest{Model: "m"})
	if strings.Contains(string(b), "provider") {
		t.Errorf("nil Provider must be omitted: %s", b)
	}
}

func TestUsageCachedTokens(t *testing.T) {
	srv, _, _ := captureServer(t)
	resp, err := New(srv.URL, "k", "").Complete(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.CachedTokens() != 8 || resp.Usage.Cost != 0.0001 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	var nilUsage *Usage
	if nilUsage.CachedTokens() != 0 {
		t.Fatal("nil usage must report 0 cached tokens")
	}
}
