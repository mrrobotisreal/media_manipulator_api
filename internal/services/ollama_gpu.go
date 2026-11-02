package services

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ollama GPU slots (MM94). Ollama places and keeps its own models, so the API
// takes a slot from the shared GPU scheduler around every Ollama call, pinned
// to the card the target daemon is pinned to, and asks Ollama to unload the
// model before the slot is released (keep_alive 0). That makes "slot released"
// mean "VRAM free", which is what the next consumer on the card needs.
//
// Config is read per call from the environment, like the rest of the Ollama
// paths (OLLAMA_URL, OLLAMA_TIMEOUT_SECONDS):
//
//   - OLLAMA_GPU_INDEX_BY_URL: comma-separated "<daemon>=<gpu index>" pairs,
//     e.g. "127.0.0.1:11434=1,127.0.0.1:11435=1". <daemon> is a URL or a
//     host:port; localhost and 127.0.0.1 are the same host. An index of
//     "none" (or a daemon that is not listed) means the daemon is not pinned
//     (the sharded summary daemon), so the slot carries no ForceIndex.
//   - OLLAMA_VRAM_MIB (default 10000) and OLLAMA_VRAM_MIB_BY_MODEL
//     ("model=MiB,..."): what one loaded model needs.

// ollamaKeepAliveWarm is what batched callers send between calls of one batch
// so the model stays loaded while they hold the slot; the last call sends
// ollamaKeepAliveUnload.
const (
	ollamaKeepAliveWarm   = "2m"
	ollamaKeepAliveUnload = "0s"
)

// defaultOllamaVRAMMiB is the request for a model with no measured entry. It
// matches document scan's long-standing Ollama-stage request. Not measured for
// the analysis/caption models yet (RUNBOOK section 7.2 VRAM table).
const defaultOllamaVRAMMiB = 10000

// ollamaDaemonKey reduces a daemon URL (or host:port) to "host:port", with
// localhost folded into 127.0.0.1 and the scheme's default port filled in.
func ollamaDaemonKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "::1" {
		host = "127.0.0.1"
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host + ":" + port
}

// parseOllamaGPUMap parses OLLAMA_GPU_INDEX_BY_URL. A nil value in the map is
// an explicitly unpinned daemon. Malformed pairs are skipped.
func parseOllamaGPUMap(raw string) map[string]*int {
	out := map[string]*int{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.LastIndex(pair, "=")
		if eq <= 0 {
			continue
		}
		key := ollamaDaemonKey(pair[:eq])
		val := strings.ToLower(strings.TrimSpace(pair[eq+1:]))
		if key == "" {
			continue
		}
		if val == "none" || val == "any" || val == "" {
			out[key] = nil
			continue
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			continue
		}
		idx := n
		out[key] = &idx
	}
	return out
}

var ollamaUnmappedWarned sync.Map

// OllamaGPUIndexFor is the card the daemon at baseURL is pinned to, from
// OLLAMA_GPU_INDEX_BY_URL; nil when it is unpinned or not configured.
func OllamaGPUIndexFor(baseURL string) *int {
	key := ollamaDaemonKey(baseURL)
	m := parseOllamaGPUMap(os.Getenv("OLLAMA_GPU_INDEX_BY_URL"))
	idx, ok := m[key]
	if !ok {
		if _, warned := ollamaUnmappedWarned.LoadOrStore(key, true); !warned && SharedGPUScheduler().Enabled() {
			log.Printf("gpu-scheduler: no card configured for ollama daemon %s (OLLAMA_GPU_INDEX_BY_URL); its slots are not pinned to a card", key)
		}
		return nil
	}
	if idx == nil {
		return nil
	}
	v := *idx
	return &v
}

// OllamaVRAMMiBFor is the VRAM a slot for model asks for.
func OllamaVRAMMiBFor(model string) int64 {
	model = strings.TrimSpace(model)
	for _, pair := range strings.Split(os.Getenv("OLLAMA_VRAM_MIB_BY_MODEL"), ",") {
		eq := strings.LastIndex(pair, "=")
		if eq <= 0 {
			continue
		}
		if strings.TrimSpace(pair[:eq]) != model {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(pair[eq+1:]), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return int64(envInt("OLLAMA_VRAM_MIB", defaultOllamaVRAMMiB))
}

// LogOllamaGPUMap logs, once at startup, which card each configured daemon's
// slots are pinned to, so an operator can check it against systemd.
func LogOllamaGPUMap(urls ...string) {
	seen := map[string]bool{}
	m := parseOllamaGPUMap(os.Getenv("OLLAMA_GPU_INDEX_BY_URL"))
	for _, u := range urls {
		key := ollamaDaemonKey(u)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		idx, ok := m[key]
		switch {
		case !ok:
			log.Printf("gpu-scheduler: ollama daemon %s → not configured (slots unpinned; set OLLAMA_GPU_INDEX_BY_URL)", key)
		case idx == nil:
			log.Printf("gpu-scheduler: ollama daemon %s → unpinned", key)
		default:
			log.Printf("gpu-scheduler: ollama daemon %s → GPU %d", key, *idx)
		}
	}
}

// ollamaSlot is one Ollama caller's GPU slot request.
type ollamaSlot struct {
	// Caller names the consumer in logs: Kind becomes "ollama[<caller>]".
	Caller  string
	BaseURL string
	Model   string
	JobID   string
	JM      *JobManager
	// Priority is a GPUPriority* constant; MaxWait bounds the wait (0 = none).
	Priority int
	MaxWait  time.Duration
	// Acquire overrides the shared scheduler (tests). nil = acquireGPUForJob.
	Acquire gpuAcquireFunc
}

func (o ollamaSlot) request() GPURequest {
	return GPURequest{
		Kind:            "ollama[" + o.Caller + "]",
		VRAMRequiredMiB: OllamaVRAMMiBFor(o.Model),
		ForceIndex:      OllamaGPUIndexFor(o.BaseURL),
		Priority:        o.Priority,
		MaxWait:         o.MaxWait,
	}
}

// withOllamaSlot holds a GPU slot on the daemon's card while fn talks to
// Ollama. fn must send keep_alive 0 on its last call; if fn returns early
// after a warm call (see ollamaBatch), the batch unloads the model before the
// slot is released.
func withOllamaSlot(ctx context.Context, o ollamaSlot, fn func() error) error {
	return withGPUVia(ctx, o.Acquire, o.JM, o.JobID, o.request(), func(int) error { return fn() })
}

// ollamaBatch tracks whether a model may still be loaded on its daemon, so a
// batch that stops early (an error after a warm call) can unload it before
// its slot is released.
type ollamaBatch struct {
	client  *http.Client
	baseURL string
	model   string
	warm    bool
}

// keepAlive is the keep_alive for call i of n: warm in between, unload last.
func (b *ollamaBatch) keepAlive(i, n int) string {
	if i < n-1 {
		b.warm = true
		return ollamaKeepAliveWarm
	}
	b.warm = false
	return ollamaKeepAliveUnload
}

// finish unloads the model if the last call it saw was a warm one.
func (b *ollamaBatch) finish() {
	if !b.warm {
		return
	}
	b.warm = false
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ollamaUnload(ctx, b.client, b.baseURL, b.model); err != nil {
		log.Printf("gpu-scheduler: unload of %s on %s failed: %v", b.model, ollamaDaemonKey(b.baseURL), err)
	}
}

// ollamaUnload asks the daemon to drop model now: /api/generate with no prompt
// and keep_alive 0 is Ollama's documented unload request.
func ollamaUnload(ctx context.Context, client *http.Client, baseURL, model string) error {
	if client == nil {
		client = http.DefaultClient
	}
	body, _ := json.Marshal(map[string]any{"model": model, "keep_alive": 0})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
