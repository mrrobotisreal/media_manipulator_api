package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MM94 S4: every Ollama caller holds a GPU slot for exactly its requests,
// sends keep_alive so the model is gone when the slot is released, and holds
// one slot (not one per call) across a batch.

// slotOllama is a fake Ollama that records, per request, the payload and
// whether a slot was held while it was served.
type slotOllama struct {
	srv   *httptest.Server
	held  *atomic.Int32
	mu    sync.Mutex
	calls []slotCall
	reply func(payload map[string]any) string
}

type slotCall struct {
	path    string
	payload map[string]any
	held    int32
}

func newSlotOllama(t *testing.T, held *atomic.Int32, reply func(map[string]any) string) *slotOllama {
	t.Helper()
	f := &slotOllama{held: held, reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		f.mu.Lock()
		f.calls = append(f.calls, slotCall{path: r.URL.Path, payload: p, held: held.Load()})
		f.mu.Unlock()
		content := ""
		if f.reply != nil {
			content = f.reply(p)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "done": true})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *slotOllama) chats() []slotCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []slotCall
	for _, c := range f.calls {
		if c.path == "/api/chat" {
			out = append(out, c)
		}
	}
	return out
}

func (f *slotOllama) unloads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.path == "/api/generate" && c.payload["keep_alive"] == float64(0) {
			n++
		}
	}
	return n
}

// slotRecorder is a fake scheduler: it counts acquires and holds.
type slotRecorder struct {
	held  atomic.Int32
	mu    sync.Mutex
	reqs  []GPURequest
	peak  int32
	fails error
}

func (r *slotRecorder) acquire(_ context.Context, _ *JobManager, _ string, req GPURequest) (int, func(), error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	fail := r.fails
	r.mu.Unlock()
	if fail != nil {
		return -1, nil, fail
	}
	n := r.held.Add(1)
	r.mu.Lock()
	if n > r.peak {
		r.peak = n
	}
	r.mu.Unlock()
	idx := 0
	if req.ForceIndex != nil {
		idx = *req.ForceIndex
	}
	var once sync.Once
	return idx, func() { once.Do(func() { r.held.Add(-1) }) }, nil
}

func (r *slotRecorder) requests() []GPURequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]GPURequest(nil), r.reqs...)
}

func assertAllHeld(t *testing.T, calls []slotCall) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("no Ollama calls recorded")
	}
	for i, c := range calls {
		if c.held != 1 {
			t.Fatalf("call %d served with %d slots held, want exactly 1", i, c.held)
		}
	}
}

func TestAnalysisImage_SlotAndKeepAlive(t *testing.T) {
	rec := &slotRecorder{}
	f := newSlotOllama(t, &rec.held, func(map[string]any) string { return `{"scene":"x"}` })
	t.Setenv("OLLAMA_URL", f.srv.URL)
	t.Setenv("OLLAMA_GPU_INDEX_BY_URL", strings.TrimPrefix(f.srv.URL, "http://")+"=1")
	t.Setenv("OLLAMA_VLM_MODEL", "vlm-test")
	t.Setenv("OLLAMA_VRAM_MIB_BY_MODEL", "vlm-test=14700")
	img := filepath.Join(t.TempDir(), "a.jpg")
	_ = os.WriteFile(img, []byte("jpeg"), 0o644)
	q := &AnalysisQueue{cfg: &config.Config{AnalysisGPUMaxWait: 90 * time.Second}, acquire: rec.acquire}
	if _, err := q.analyzeImages(context.Background(), []string{img}, "P"); err != nil {
		t.Fatal(err)
	}
	calls := f.chats()
	assertAllHeld(t, calls)
	if calls[0].payload["keep_alive"] != "0s" {
		t.Fatalf("keep_alive = %v, want 0s", calls[0].payload["keep_alive"])
	}
	reqs := rec.requests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 slot, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Kind != "ollama[analysis-image]" || r.Priority != GPUPriorityBackground || r.MaxWait != 90*time.Second ||
		r.ForceIndex == nil || *r.ForceIndex != 1 || r.VRAMRequiredMiB != 14700 {
		t.Fatalf("slot request: %+v", r)
	}
	if rec.held.Load() != 0 {
		t.Fatal("slot not released")
	}
	if f.unloads() != 0 {
		t.Fatal("a keep_alive 0 call needs no extra unload")
	}
}

func TestAnalysisTranscript_SlotAndKeepAlive(t *testing.T) {
	rec := &slotRecorder{}
	f := newSlotOllama(t, &rec.held, func(map[string]any) string { return `{"summary":"s"}` })
	t.Setenv("OLLAMA_URL", f.srv.URL)
	q := &AnalysisQueue{acquire: rec.acquire}
	if _, err := q.analyzeTranscript(context.Background(), "hello", "en"); err != nil {
		t.Fatal(err)
	}
	calls := f.chats()
	assertAllHeld(t, calls)
	if calls[0].payload["keep_alive"] != "0s" {
		t.Fatalf("keep_alive = %v", calls[0].payload["keep_alive"])
	}
	if r := rec.requests()[0]; r.Kind != "ollama[analysis-transcript]" || r.Priority != GPUPriorityBackground || r.MaxWait != 120*time.Second {
		t.Fatalf("slot request: %+v", r)
	}
}

// Frame batches: ONE slot for the whole video, warm in between, unload last.
func TestAnalysisFrameBatches_OneSlotWarmThenUnload(t *testing.T) {
	rec := &slotRecorder{}
	f := newSlotOllama(t, &rec.held, func(map[string]any) string { return `{"scene":"x"}` })
	t.Setenv("OLLAMA_URL", f.srv.URL)
	t.Setenv("OLLAMA_VLM_BATCH_SIZE", "2")
	dir := t.TempDir()
	var frames []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "f"+string(rune('a'+i))+".jpg")
		_ = os.WriteFile(p, []byte("jpeg"), 0o644)
		frames = append(frames, p)
	}
	q := &AnalysisQueue{acquire: rec.acquire}
	out, err := q.analyzeFrameBatches(context.Background(), frames, "")
	if err != nil || len(out) != 3 {
		t.Fatalf("batches: %d err=%v", len(out), err)
	}
	if n := len(rec.requests()); n != 1 {
		t.Fatalf("want one slot for the batch, got %d", n)
	}
	calls := f.chats()
	assertAllHeld(t, calls)
	want := []string{"2m", "2m", "0s"}
	for i, c := range calls {
		if c.payload["keep_alive"] != want[i] {
			t.Fatalf("call %d keep_alive = %v, want %s", i, c.payload["keep_alive"], want[i])
		}
	}
	if f.unloads() != 0 {
		t.Fatal("no explicit unload needed when the last call sent 0s")
	}
}

// A batch that stops after a warm call unloads the model before the slot goes.
func TestAnalysisFrameBatches_EarlyErrorUnloads(t *testing.T) {
	rec := &slotRecorder{}
	f := newSlotOllama(t, &rec.held, nil)
	t.Setenv("OLLAMA_URL", f.srv.URL)
	t.Setenv("OLLAMA_VLM_BATCH_SIZE", "1")
	dir := t.TempDir()
	good := filepath.Join(dir, "a.jpg")
	_ = os.WriteFile(good, []byte("jpeg"), 0o644)
	frames := []string{good, filepath.Join(dir, "missing.jpg"), good}
	q := &AnalysisQueue{acquire: rec.acquire}
	if _, err := q.analyzeFrameBatches(context.Background(), frames, ""); err == nil {
		t.Fatal("want an error for the unreadable frame")
	}
	if f.unloads() != 1 {
		t.Fatalf("want one explicit unload after the warm call, got %d", f.unloads())
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if last.path != "/api/generate" || last.held != 1 {
		t.Fatalf("the unload must happen while the slot is still held: %+v", last)
	}
}

// A dropped background slot means no Ollama call at all.
func TestAnalysisImage_DroppedSlotSkipsOllama(t *testing.T) {
	rec := &slotRecorder{fails: ErrGPUMaxWait}
	f := newSlotOllama(t, &rec.held, nil)
	t.Setenv("OLLAMA_URL", f.srv.URL)
	img := filepath.Join(t.TempDir(), "a.jpg")
	_ = os.WriteFile(img, []byte("jpeg"), 0o644)
	q := &AnalysisQueue{acquire: rec.acquire}
	if _, err := q.analyzeImages(context.Background(), []string{img}, "P"); err == nil || !strings.Contains(err.Error(), "max wait") {
		t.Fatalf("want the max-wait error, got %v", err)
	}
	if len(f.chats()) != 0 {
		t.Fatal("Ollama must not be called without a slot")
	}
}

// Caption translation: one slot for every batch of the file, warm calls,
// unload before release, the job's priority and queue wait.
func TestCaptionTranslator_OneSlotWarmThenUnload(t *testing.T) {
	rec := &slotRecorder{}
	f := newSlotOllama(t, &rec.held, func(p map[string]any) string {
		msgs := p["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].(string)
		body := content[strings.Index(content, "Caption content:\n")+len("Caption content:\n"):]
		return body // echo the segments back
	})
	t.Setenv("OLLAMA_URL", f.srv.URL)
	t.Setenv("OLLAMA_GPU_INDEX_BY_URL", strings.TrimPrefix(f.srv.URL, "http://")+"=1")
	t.Setenv("OLLAMA_CAPTIONS_MODEL", "cap-test")
	dir := t.TempDir()
	in := filepath.Join(dir, "in.srt")
	srt := "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:04,000\nWorld\n\n3\n00:00:05,000 --> 00:00:06,000\nAgain\n"
	_ = os.WriteFile(in, []byte(srt), 0o644)
	jm := NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "in.srt"}, map[string]interface{}{})
	jm.SetJobGPUPriority(job.ID, GPUPriorityPremium)
	svc := NewCaptionTranslatorService(&config.Config{}, jm)
	svc.acquire = rec.acquire
	err := svc.Translate(context.Background(), CaptionTranslatorInput{
		JobID: job.ID, InputPath: in, OutputPath: filepath.Join(dir, "out.srt"),
		InputFormat: "srt", OutputFormat: "srt", TargetLanguage: "es", BatchSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs := rec.requests()
	if len(reqs) != 1 {
		t.Fatalf("want one slot for all batches, got %d", len(reqs))
	}
	if r := reqs[0]; r.Kind != "ollama[caption-translate]" || r.Priority != GPUPriorityPremium || r.ForceIndex == nil || *r.ForceIndex != 1 {
		t.Fatalf("slot request: %+v", r)
	}
	calls := f.chats()
	if len(calls) != 3 {
		t.Fatalf("want 3 batch calls, got %d", len(calls))
	}
	assertAllHeld(t, calls)
	for i, c := range calls {
		if c.payload["keep_alive"] != ollamaKeepAliveWarm {
			t.Fatalf("call %d keep_alive = %v", i, c.payload["keep_alive"])
		}
	}
	if f.unloads() != 1 {
		t.Fatalf("want exactly one unload at the end, got %d", f.unloads())
	}
	if rec.held.Load() != 0 {
		t.Fatal("slot not released")
	}
}
