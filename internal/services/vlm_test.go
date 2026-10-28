package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeOllama is an httptest /api/chat that records every request body and
// answers from a scripted list of assistant contents (the last one repeats).
type fakeOllama struct {
	mu       sync.Mutex
	requests []map[string]any
	replies  []string
	status   int
	srv      *httptest.Server
}

func newFakeOllama(t *testing.T, replies ...string) *fakeOllama {
	t.Helper()
	f := &fakeOllama{replies: replies, status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		f.mu.Lock()
		f.requests = append(f.requests, payload)
		idx := len(f.requests) - 1
		if idx >= len(f.replies) {
			idx = len(f.replies) - 1
		}
		reply := ""
		if idx >= 0 {
			reply = f.replies[idx]
		}
		status := f.status
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   payload["model"],
			"message": map[string]any{"role": "assistant", "content": reply},
			"done":    true,
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOllama) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeOllama) request(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func TestParseShowinfoTimes(t *testing.T) {
	stderr := strings.Join([]string{
		"Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'in.mp4':",
		"[Parsed_showinfo_1 @ 0x600001] config in time_base: 1/90000, frame_rate: 30/1",
		"[Parsed_showinfo_1 @ 0x600001] n:   0 pts:      0 pts_time:0       duration:3000 fmt:yuv420p",
		"[Parsed_showinfo_1 @ 0x600001] n:   1 pts: 451200 pts_time:5.01333 duration:3000 fmt:yuv420p",
		"[Parsed_scale_2 @ 0x600002] pts_time:9.9 (not showinfo, ignored)",
		"[Parsed_showinfo_1 @ 0x600001] n:   2 pts:1123200 pts_time:12.48   duration:3000",
		"frame=    3 fps=0.0 q=-0.0 Lsize=N/A time=00:00:12.48",
	}, "\n")
	got := parseShowinfoTimes(stderr)
	want := []float64{0, 5.01333, 12.48}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := parseShowinfoTimes(""); len(got) != 0 {
		t.Fatalf("empty log: got %v", got)
	}
}

func TestFrameFileNameSortsChronologically(t *testing.T) {
	if got := frameFileName(12.48); got != "frame_0012.480.jpg" {
		t.Fatalf("got %q", got)
	}
	if !(frameFileName(9.5) < frameFileName(10) && frameFileName(10) < frameFileName(125.25)) {
		t.Fatal("names must sort in time order")
	}
}

func TestStripCodeFence(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:                  `{"a":1}`,
		"```json\n{\"a\":1}\n```":  `{"a":1}`,
		"```\n{\"a\":1}```":        `{"a":1}`,
		"  \n{\"a\":1}\n  ":        `{"a":1}`,
		"```json\n{\"a\":1}\n``` ": `{"a":1}`,
	} {
		if got := stripCodeFence(in); got != want {
			t.Errorf("stripCodeFence(%q) = %q want %q", in, got, want)
		}
	}
}

func TestChatJSONValidFirstTry(t *testing.T) {
	f := newFakeOllama(t, `{"ok":true}`)
	res, err := ChatJSON(context.Background(), JSONChatRequest{BaseURL: f.srv.URL, Model: "m", System: "sys", User: "hi", Images: []string{"aW1n"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 1 || string(res.Content) != `{"ok":true}` || f.calls() != 1 {
		t.Fatalf("attempts=%d content=%s calls=%d", res.Attempts, res.Content, f.calls())
	}
	req := f.request(0)
	if req["format"] != "json" || req["stream"] != false || req["model"] != "m" {
		t.Fatalf("payload: %v", req)
	}
	msgs := req["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages: %v", msgs)
	}
	if imgs := msgs[1].(map[string]any)["images"].([]any); len(imgs) != 1 {
		t.Fatalf("images not attached to the user turn: %v", msgs[1])
	}
}

func TestChatJSONRepairRetryOnce(t *testing.T) {
	f := newFakeOllama(t, `not json at all`, `{"ok":true}`)
	res, err := ChatJSON(context.Background(), JSONChatRequest{BaseURL: f.srv.URL, Model: "m", User: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 2 || f.calls() != 2 {
		t.Fatalf("attempts=%d calls=%d", res.Attempts, f.calls())
	}
	msgs := f.request(1)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("repair call must carry user, bad assistant reply and the repair prompt: %v", msgs)
	}
	if msgs[1].(map[string]any)["content"] != "not json at all" {
		t.Fatalf("bad reply not echoed: %v", msgs[1])
	}
	if !strings.Contains(msgs[2].(map[string]any)["content"].(string), "not valid for the required JSON schema") {
		t.Fatalf("repair prompt missing: %v", msgs[2])
	}
}

func TestChatJSONFailsAfterSecondInvalidReply(t *testing.T) {
	f := newFakeOllama(t, `{"bad":1}`, `{"bad":2}`, `{"ok":true}`)
	validate := func(raw []byte) error {
		if !strings.Contains(string(raw), "ok") {
			return errFieldMissing("ok")
		}
		return nil
	}
	_, err := ChatJSON(context.Background(), JSONChatRequest{BaseURL: f.srv.URL, Model: "m", User: "hi"}, validate)
	if err == nil || !strings.Contains(err.Error(), "after one repair retry") || !strings.Contains(err.Error(), "ok is required") {
		t.Fatalf("want a clear repair failure, got %v", err)
	}
	if f.calls() != 2 {
		t.Fatalf("exactly one repair retry: calls=%d", f.calls())
	}
}

func TestChatJSONTransportErrorIsNotRepaired(t *testing.T) {
	f := newFakeOllama(t, `{"ok":true}`)
	f.status = http.StatusInternalServerError
	_, err := ChatJSON(context.Background(), JSONChatRequest{BaseURL: f.srv.URL, Model: "m", User: "hi"}, nil)
	if err == nil || f.calls() != 1 {
		t.Fatalf("err=%v calls=%d", err, f.calls())
	}
}

type errFieldMissing string

func (e errFieldMissing) Error() string { return string(e) + " is required" }

// The S1 refactor must not change what the internal analysis sends to Ollama:
// same model, same prompt, images on the single user message, format json, no
// system turn, and the raw response object returned as-is.
func TestAnalysisQueueAnalyzeImagesPayloadUnchanged(t *testing.T) {
	f := newFakeOllama(t, `{"scene":"x"}`)
	t.Setenv("OLLAMA_URL", f.srv.URL)
	t.Setenv("OLLAMA_VLM_MODEL", "vlm-test")
	dir := t.TempDir()
	img := filepath.Join(dir, "frame_00001.jpg")
	if err := os.WriteFile(img, []byte("jpeg-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	q := &AnalysisQueue{}
	out, err := q.analyzeImages(context.Background(), []string{img}, "PROMPT")
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := out.(map[string]any)
	if !ok || resp["message"] == nil || resp["done"] != true {
		t.Fatalf("raw response must be returned unchanged: %#v", out)
	}
	req := f.request(0)
	want := map[string]any{
		"model":  "vlm-test",
		"stream": false,
		"format": "json",
		"messages": []any{map[string]any{
			"role":    "user",
			"content": "PROMPT",
			"images":  []any{base64.StdEncoding.EncodeToString([]byte("jpeg-bytes"))},
		}},
	}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("payload changed:\n got %#v\nwant %#v", req, want)
	}
}

func TestAnalysisQueueAnalyzeTranscriptParsesContent(t *testing.T) {
	f := newFakeOllama(t, `{"summary":"s"}`)
	t.Setenv("OLLAMA_URL", f.srv.URL)
	t.Setenv("OLLAMA_TEXT_MODEL", "text-test")
	q := &AnalysisQueue{}
	out, err := q.analyzeTranscript(context.Background(), "hello world", "en")
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := out.(map[string]any); !ok || m["summary"] != "s" {
		t.Fatalf("parsed content expected, got %#v", out)
	}
	req := f.request(0)
	if req["model"] != "text-test" || req["format"] != "json" || len(req["messages"].([]any)) != 1 {
		t.Fatalf("payload changed: %#v", req)
	}
	// Non-JSON content still comes back as {"raw": ...}.
	f2 := newFakeOllama(t, `plain words`)
	t.Setenv("OLLAMA_URL", f2.srv.URL)
	out, err = q.analyzeTranscript(context.Background(), "hello", "en")
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := out.(map[string]any); !ok || m["raw"] != "plain words" {
		t.Fatalf("raw fallback changed: %#v", out)
	}
}
