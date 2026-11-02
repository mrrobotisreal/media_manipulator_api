package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func testOriginal() models.OriginalFileInfo {
	return models.OriginalFileInfo{Name: "in.png", Type: "image/png"}
}

// MM94 S2: the GPU slot helper, background priority and the Ollama card map.

func oneCardScheduler() *GPUScheduler {
	return makeTestScheduler([]GPUSnapshot{{Index: 1, Name: "5080", TotalMiB: 16000, FreeMiB: 12000, UsedMiB: 4000}}, nil)
}

// Premium > Standard > Background, whatever order they arrive in.
func TestGPUScheduler_BackgroundRunsLast(t *testing.T) {
	s := oneCardScheduler()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, releaseHolder, err := s.Acquire(ctx, GPURequest{Kind: "holder", VRAMRequiredMiB: 5000})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	spawn := func(name string, prio int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rel, err := s.Acquire(ctx, GPURequest{Kind: name, VRAMRequiredMiB: 5000, Priority: prio})
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			rel()
		}()
		time.Sleep(80 * time.Millisecond)
	}
	// Worst case for the queue: background first, Premium last.
	spawn("background", GPUPriorityBackground)
	spawn("standard", GPUPriorityStandard)
	spawn("premium", GPUPriorityPremium)
	releaseHolder()
	wg.Wait()
	want := []string{"premium", "standard", "background"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// A background waiter does not stand aside for a user job on a different card.
func TestGPUScheduler_BackgroundOnlyYieldsOnSharedCard(t *testing.T) {
	s := makeTestScheduler([]GPUSnapshot{
		{Index: 0, Name: "a", TotalMiB: 16000, FreeMiB: 12000},
		{Index: 1, Name: "b", TotalMiB: 16000, FreeMiB: 12000},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	zero, one := 0, 1
	_, rel0, err := s.Acquire(ctx, GPURequest{Kind: "holder0", VRAMRequiredMiB: 1000, ForceIndex: &zero})
	if err != nil {
		t.Fatal(err)
	}
	defer rel0()
	// A standard job parked waiting for card 0.
	go func() {
		_, rel, err := s.Acquire(ctx, GPURequest{Kind: "user-on-0", VRAMRequiredMiB: 1000, ForceIndex: &zero})
		if err == nil {
			rel()
		}
	}()
	waitForWaiting(t, s, 1)
	got, rel, err := s.Acquire(ctx, GPURequest{Kind: "bg-on-1", VRAMRequiredMiB: 1000, ForceIndex: &one, Priority: GPUPriorityBackground})
	if err != nil || got != 1 {
		t.Fatalf("background on a free, unrelated card must not wait: idx=%d err=%v", got, err)
	}
	rel()
}

// ForceIndex waits for its own card even when another card is free.
func TestWithGPU_ForceIndexWaitsForItsCard(t *testing.T) {
	s := makeTestScheduler([]GPUSnapshot{
		{Index: 0, Name: "free", TotalMiB: 16000, FreeMiB: 15000},
		{Index: 1, Name: "busy", TotalMiB: 16000, FreeMiB: 15000},
	}, nil)
	acquire := func(ctx context.Context, jm *JobManager, jobID string, req GPURequest) (int, func(), error) {
		return acquireGPUForJobOn(ctx, s, jm, jobID, req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	one := 1
	_, releaseHolder, err := s.Acquire(ctx, GPURequest{Kind: "holder", VRAMRequiredMiB: 1000, ForceIndex: &one})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan int, 1)
	go func() {
		_ = withGPUVia(ctx, acquire, nil, "", GPURequest{Kind: "tool", VRAMRequiredMiB: 1000, ForceIndex: &one}, func(idx int) error {
			ran <- idx
			return nil
		})
	}()
	select {
	case idx := <-ran:
		t.Fatalf("tool ran on card %d while its card was held", idx)
	case <-time.After(150 * time.Millisecond):
	}
	releaseHolder()
	select {
	case idx := <-ran:
		if idx != 1 {
			t.Fatalf("tool ran on card %d, want 1", idx)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tool never ran after its card was released")
	}
}

// A background request with MaxWait gives up with ErrGPUMaxWait instead of
// queueing forever, and the job manager records nothing.
func TestGPUScheduler_BackgroundMaxWaitDrop(t *testing.T) {
	s := oneCardScheduler()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, releaseHolder, err := s.Acquire(ctx, GPURequest{Kind: "holder", VRAMRequiredMiB: 5000})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHolder()
	ran := false
	start := time.Now()
	err = withGPUVia(ctx, func(ctx context.Context, jm *JobManager, jobID string, req GPURequest) (int, func(), error) {
		return acquireGPUForJobOn(ctx, s, jm, jobID, req)
	}, nil, "", GPURequest{Kind: "ollama[analysis]", VRAMRequiredMiB: 5000, Priority: GPUPriorityBackground, MaxWait: 150 * time.Millisecond}, func(int) error {
		ran = true
		return nil
	})
	if !errors.Is(err, ErrGPUMaxWait) {
		t.Fatalf("want ErrGPUMaxWait, got %v", err)
	}
	if ran {
		t.Fatal("fn must not run when the slot was never granted")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("max wait ignored: waited %s", waited)
	}
	if snap := s.Snapshot(); snap.Waiting != 0 || snap.Running != 1 {
		t.Fatalf("dropped waiter must leave the queue: %+v", snap)
	}
	// The caller's own cancellation is still reported as itself.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, _, err := s.Acquire(cctx, GPURequest{Kind: "x", VRAMRequiredMiB: 5000, MaxWait: time.Hour}); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancel must stay context.Canceled, got %v", err)
	}
}

// With the scheduler disabled, withGPU runs fn at once with idx -1 and never
// blocks or errors: exactly the pre-scheduler behaviour.
func TestWithGPU_DisabledSchedulerIsNoOp(t *testing.T) {
	disabled := newGPUScheduler(func(context.Context) ([]GPUSnapshot, error) {
		t.Fatal("a disabled scheduler must not probe")
		return nil, nil
	})
	got := -2
	err := withGPUVia(context.Background(), func(ctx context.Context, jm *JobManager, jobID string, req GPURequest) (int, func(), error) {
		return acquireGPUForJobOn(ctx, disabled, jm, jobID, req)
	}, NewJobManager(), "job", GPURequest{Kind: "tool", VRAMRequiredMiB: 1 << 40, Priority: GPUPriorityBackground, MaxWait: time.Nanosecond}, func(idx int) error {
		got = idx
		return nil
	})
	if err != nil || got != -1 {
		t.Fatalf("disabled: idx=%d err=%v", got, err)
	}
}

// withGPU releases the slot when fn returns, also on error, and records the
// queue wait on the job (MM33).
func TestWithGPU_ReleasesAndRecordsQueueWait(t *testing.T) {
	s := oneCardScheduler()
	jm := NewJobManager()
	job := jm.CreateJob(testOriginal(), map[string]interface{}{})
	acquire := func(ctx context.Context, jm *JobManager, jobID string, req GPURequest) (int, func(), error) {
		return acquireGPUForJobOn(ctx, s, jm, jobID, req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, releaseHolder, err := s.Acquire(ctx, GPURequest{Kind: "holder", VRAMRequiredMiB: 5000})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(120 * time.Millisecond)
		releaseHolder()
	}()
	boom := errors.New("boom")
	err = withGPUVia(ctx, acquire, jm, job.ID, GPURequest{Kind: "tool", VRAMRequiredMiB: 5000}, func(int) error {
		if s.Snapshot().Running != 1 {
			t.Error("slot must be held while fn runs")
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("fn error must pass through, got %v", err)
	}
	if s.Snapshot().Running != 0 {
		t.Fatal("slot must be released after fn returns")
	}
	got, _ := jm.GetJob(job.ID)
	if got.QueueWaitMs == nil || *got.QueueWaitMs <= 0 {
		t.Fatalf("queueWaitMs not recorded: %v", got.QueueWaitMs)
	}
}

// Memory freed without a release (an Ollama model unloading) wakes a parked
// waiter through the periodic re-probe.
func TestGPUScheduler_ReprobeSeesFreedMemory(t *testing.T) {
	var mu sync.Mutex
	free := int64(1000)
	s := newGPUScheduler(func(context.Context) ([]GPUSnapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		return []GPUSnapshot{{Index: 1, Name: "5080", TotalMiB: 16000, FreeMiB: free}}, nil
	})
	s.enabled = true
	s.reprobe = 30 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, rel, err := s.Acquire(ctx, GPURequest{Kind: "tool", VRAMRequiredMiB: 4000})
		if err == nil {
			rel()
		}
		done <- err
	}()
	waitForWaiting(t, s, 1)
	mu.Lock()
	free = 15000
	mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never re-probed after memory came back")
	}
}

func TestParseOllamaGPUMap(t *testing.T) {
	m := parseOllamaGPUMap(" http://localhost:11434=1, 127.0.0.1:11435 = 0 ,http://127.0.0.1:11436=none, bad, x=notanint")
	if idx := m["127.0.0.1:11434"]; idx == nil || *idx != 1 {
		t.Fatalf("11434: %v", idx)
	}
	if idx := m["127.0.0.1:11435"]; idx == nil || *idx != 0 {
		t.Fatalf("11435: %v", idx)
	}
	if idx, ok := m["127.0.0.1:11436"]; !ok || idx != nil {
		t.Fatalf("11436 must be explicitly unpinned: ok=%v idx=%v", ok, idx)
	}
	if len(m) != 3 {
		t.Fatalf("malformed pairs must be skipped: %v", m)
	}
}

func TestOllamaGPUIndexAndVRAMFromEnv(t *testing.T) {
	t.Setenv("OLLAMA_GPU_INDEX_BY_URL", "127.0.0.1:11434=1,127.0.0.1:11436=none")
	if idx := OllamaGPUIndexFor("http://localhost:11434/"); idx == nil || *idx != 1 {
		t.Fatalf("pinned daemon: %v", idx)
	}
	if idx := OllamaGPUIndexFor("http://127.0.0.1:11436"); idx != nil {
		t.Fatalf("unpinned daemon must have no card: %v", *idx)
	}
	if idx := OllamaGPUIndexFor("http://127.0.0.1:9999"); idx != nil {
		t.Fatalf("unlisted daemon must have no card: %v", *idx)
	}
	t.Setenv("OLLAMA_VRAM_MIB", "9000")
	t.Setenv("OLLAMA_VRAM_MIB_BY_MODEL", "qwen3-vl:8b-instruct-q8_0=14700, mm-captions=11000")
	if got := OllamaVRAMMiBFor("qwen3-vl:8b-instruct-q8_0"); got != 14700 {
		t.Fatalf("per-model VRAM: %d", got)
	}
	if got := OllamaVRAMMiBFor("other"); got != 9000 {
		t.Fatalf("default VRAM: %d", got)
	}
	slot := ollamaSlot{Caller: "analysis", BaseURL: "http://127.0.0.1:11434", Model: "qwen3-vl:8b-instruct-q8_0", Priority: GPUPriorityBackground, MaxWait: time.Minute}
	req := slot.request()
	if req.Kind != "ollama[analysis]" || req.VRAMRequiredMiB != 14700 || req.ForceIndex == nil || *req.ForceIndex != 1 ||
		req.Priority != GPUPriorityBackground || req.MaxWait != time.Minute {
		t.Fatalf("slot request: %+v", req)
	}
}
