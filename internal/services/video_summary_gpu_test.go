package services

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MM94 S4: the summary's Ollama stages take a scheduler slot each: vision holds
// ONE slot across all its batches, synthesis one for its call (and repair),
// the job never holds two slots at once (Whisper → vision → synthesis, in
// that order, so no deadlock), and the slot is held for every Ollama request.

type summarySlots struct {
	mu      sync.Mutex
	held    int
	maxHeld int
	reqs    []GPURequest
}

func (s *summarySlots) acquire(_ context.Context, _ *JobManager, _ string, req GPURequest) (int, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	s.held++
	if s.held > s.maxHeld {
		s.maxHeld = s.held
	}
	var once sync.Once
	return 1, func() {
		once.Do(func() {
			s.mu.Lock()
			s.held--
			s.mu.Unlock()
		})
	}, nil
}

func (s *summarySlots) heldNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

func TestVideoSummary_OllamaStagesHoldOneSlotEach(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	t.Setenv("OLLAMA_GPU_INDEX_BY_URL", strings.TrimPrefix(h.ollama.srv.URL, "http://")+"=1")
	slots := &summarySlots{}
	h.svc.deps.acquire = slots.acquire
	// Every request must be served while exactly one slot is held.
	var mu sync.Mutex
	var heldAtCall []int
	inner := h.ollama.srv.Config.Handler
	h.ollama.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		heldAtCall = append(heldAtCall, slots.heldNow())
		mu.Unlock()
		inner.ServeHTTP(w, r)
	})
	job := h.run(24, "auto")
	if job.Status != models.StatusCompleted {
		t.Fatalf("job: %s %q", job.Status, job.Error)
	}
	if len(slots.reqs) != 2 {
		t.Fatalf("want 2 slots (vision, synthesis), got %+v", slots.reqs)
	}
	if slots.reqs[0].Kind != "ollama[summary-vision]" || slots.reqs[1].Kind != "ollama[summary-synthesis]" {
		t.Fatalf("slot kinds: %s, %s", slots.reqs[0].Kind, slots.reqs[1].Kind)
	}
	for _, r := range slots.reqs {
		if r.ForceIndex == nil || *r.ForceIndex != 1 {
			t.Fatalf("pinned daemon must give ForceIndex 1: %+v", r)
		}
	}
	if slots.maxHeld != 1 {
		t.Fatalf("the summary must never hold two slots at once, peak %d", slots.maxHeld)
	}
	if slots.heldNow() != 0 {
		t.Fatal("slot leaked")
	}
	if len(heldAtCall) < 2 {
		t.Fatalf("too few Ollama calls: %d", len(heldAtCall))
	}
	for i, n := range heldAtCall {
		if n != 1 {
			t.Fatalf("Ollama call %d served with %d slots held", i, n)
		}
	}
	// keep_alive: warm between vision batches, 0s on the last and on synthesis.
	vlm := h.ollama.vlmCalls
	if len(vlm) < 2 {
		t.Fatalf("want several vision batches, got %d", len(vlm))
	}
	for i, c := range vlm {
		want := ollamaKeepAliveWarm
		if i == len(vlm)-1 {
			want = ollamaKeepAliveUnload
		}
		if c["keep_alive"] != want {
			t.Fatalf("vision call %d keep_alive %v, want %s", i, c["keep_alive"], want)
		}
	}
	for i, c := range h.ollama.textCalls {
		if c["keep_alive"] != ollamaKeepAliveUnload {
			t.Fatalf("synthesis call %d keep_alive %v", i, c["keep_alive"])
		}
	}
}

// The unpinned deep daemon gets a slot without a card.
func TestVideoSummary_DeepDaemonSlotIsUnpinned(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	deep := newSummaryOllama(t, "vlm-test")
	t.Setenv("OLLAMA_GPU_INDEX_BY_URL",
		strings.TrimPrefix(h.ollama.srv.URL, "http://")+"=1,"+strings.TrimPrefix(deep.srv.URL, "http://")+"=none")
	h.svc.settings.DeepModel, h.svc.settings.DeepURL = "big-27b", deep.srv.URL
	slots := &summarySlots{}
	h.svc.deps.acquire = slots.acquire
	h.svc.process(context.Background(), VideoSummaryRequest{JobID: h.jobID, InputPath: "/x", OutputDir: h.outDir, Depth: models.VideoSummaryDepthDeep, MaxFrames: 24}, nil)
	if len(slots.reqs) != 2 || slots.reqs[1].ForceIndex != nil {
		t.Fatalf("deep synthesis slot must be unpinned: %+v", slots.reqs)
	}
}

// A slot that cannot be had fails the stage with a user-safe message.
func TestVideoSummary_SlotFailureFailsClearly(t *testing.T) {
	h := newSummaryHarness(t, videoProbe(180))
	h.svc.deps.acquire = func(context.Context, *JobManager, string, GPURequest) (int, func(), error) {
		return -1, nil, context.DeadlineExceeded
	}
	job := h.run(24, "auto")
	if job.Status != models.StatusFailed || job.Error != "Timed out waiting for a free GPU" {
		t.Fatalf("job: %s %q", job.Status, job.Error)
	}
	if len(h.ollama.vlmCalls)+len(h.ollama.textCalls) != 0 {
		t.Fatal("no Ollama call without a slot")
	}
}
