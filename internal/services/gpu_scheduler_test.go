package services

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEstimateWhisperVRAMMiB(t *testing.T) {
	cases := []struct {
		model       string
		computeType string
		wantMin     int64 // returned value should be at least this
		wantMax     int64 // and at most this
	}{
		{"tiny", "float16", 500, 1500},
		{"base", "float16", 1000, 1500},
		{"small", "float16", 2000, 2500},
		{"medium", "float16", 3000, 4000},
		{"large-v3", "float16", 5000, 6000},
		{"large-v2", "float16", 5000, 6000},
		{"unknown-model-name", "float16", 3000, 4000}, // defaults to ~medium
		{"large-v3", "float32", 10000, 12000},         // float32 doubles
		{"large-v3", "int8", 2500, 3000},              // int8 halves
		{"large-v3", "int8_float16", 3000, 3500},      // ~60%
	}
	for _, c := range cases {
		got := EstimateWhisperVRAMMiB(c.model, c.computeType)
		if got < c.wantMin || got > c.wantMax {
			t.Errorf("EstimateWhisperVRAMMiB(%q, %q) = %d, want [%d..%d]",
				c.model, c.computeType, got, c.wantMin, c.wantMax)
		}
	}
}

func TestResolveWhisperGPURequest_EnvParsing(t *testing.T) {
	t.Setenv("WHISPER_CT2_DEVICE_INDEX", "0,1,2")
	t.Setenv("WHISPER_CT2_FORCE_DEVICE_INDEX", "")
	req := ResolveWhisperGPURequest(WhisperCT2Config{Model: "medium", ComputeType: "float16"})
	if len(req.PreferredOrder) != 3 {
		t.Fatalf("expected 3 preferred indices, got %d: %v", len(req.PreferredOrder), req.PreferredOrder)
	}
	if req.PreferredOrder[0] != 0 || req.PreferredOrder[1] != 1 || req.PreferredOrder[2] != 2 {
		t.Errorf("preferred order should preserve input order, got %v", req.PreferredOrder)
	}
	if req.ForceIndex != nil {
		t.Errorf("ForceIndex should be nil when env unset, got %v", *req.ForceIndex)
	}
	if req.VRAMRequiredMiB <= 0 {
		t.Errorf("VRAMRequiredMiB should be positive, got %d", req.VRAMRequiredMiB)
	}
}

func TestResolveWhisperGPURequest_ForceIndex(t *testing.T) {
	t.Setenv("WHISPER_CT2_DEVICE_INDEX", "")
	t.Setenv("WHISPER_CT2_FORCE_DEVICE_INDEX", "1")
	req := ResolveWhisperGPURequest(WhisperCT2Config{Model: "medium", ComputeType: "float16"})
	if req.ForceIndex == nil {
		t.Fatalf("ForceIndex should be set")
	}
	if *req.ForceIndex != 1 {
		t.Errorf("ForceIndex = %d, want 1", *req.ForceIndex)
	}
}

func TestResolveWhisperGPURequest_SinglePreferredIndex(t *testing.T) {
	t.Setenv("WHISPER_CT2_DEVICE_INDEX", "0")
	t.Setenv("WHISPER_CT2_FORCE_DEVICE_INDEX", "")
	req := ResolveWhisperGPURequest(WhisperCT2Config{Model: "medium", ComputeType: "float16"})
	if len(req.PreferredOrder) != 1 || req.PreferredOrder[0] != 0 {
		t.Errorf("expected [0], got %v", req.PreferredOrder)
	}
}

// makeTestScheduler returns a scheduler wired to a constant probe so unit
// tests don't need nvidia-smi available.
func makeTestScheduler(snaps []GPUSnapshot, probeErr error) *GPUScheduler {
	probe := func(ctx context.Context) ([]GPUSnapshot, error) {
		if probeErr != nil {
			return nil, probeErr
		}
		out := make([]GPUSnapshot, len(snaps))
		copy(out, snaps)
		return out, nil
	}
	s := newGPUScheduler(probe)
	s.enabled = true
	return s
}

func TestGPUScheduler_DisabledIsNoOp(t *testing.T) {
	s := newGPUScheduler(func(ctx context.Context) ([]GPUSnapshot, error) { return nil, nil })
	// enabled defaults to false
	idx, release, err := s.Acquire(context.Background(), GPURequest{VRAMRequiredMiB: 1000})
	if err != nil {
		t.Fatalf("unexpected error from disabled scheduler: %v", err)
	}
	if idx != -1 {
		t.Errorf("expected idx=-1 when disabled, got %d", idx)
	}
	if release == nil {
		t.Fatal("release fn must be non-nil even when disabled")
	}
	release() // must not panic
}

func TestGPUScheduler_PreferredOrderWins(t *testing.T) {
	// Both GPUs fit, but PreferredOrder=[1] should pick 1 even though 0 is smaller.
	s := makeTestScheduler([]GPUSnapshot{
		{Index: 0, Name: "5060Ti", TotalMiB: 8000, FreeMiB: 7500, UsedMiB: 500},
		{Index: 1, Name: "5080", TotalMiB: 16000, FreeMiB: 15500, UsedMiB: 500},
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	idx, release, err := s.Acquire(ctx, GPURequest{
		Kind:            "test",
		VRAMRequiredMiB: 3000,
		PreferredOrder:  []int{1, 0},
	})
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	defer release()
	if idx != 1 {
		t.Errorf("expected idx=1 from PreferredOrder, got %d", idx)
	}
}

func TestGPUScheduler_FallsBackWhenPreferredDoesntFit(t *testing.T) {
	// 5060 Ti has only 3GB free (some other process is using ~5GB). large-v3
	// needs ~5.5GB → must fall back to the 5080.
	s := makeTestScheduler([]GPUSnapshot{
		{Index: 0, Name: "5060Ti", TotalMiB: 8000, FreeMiB: 3000, UsedMiB: 5000},
		{Index: 1, Name: "5080", TotalMiB: 16000, FreeMiB: 15500, UsedMiB: 500},
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	idx, release, err := s.Acquire(ctx, GPURequest{
		Kind:            "test",
		VRAMRequiredMiB: 5500,
		PreferredOrder:  []int{0, 1},
	})
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	defer release()
	if idx != 1 {
		t.Errorf("expected fallback to idx=1, got %d", idx)
	}
}

func TestGPUScheduler_ForceIndexNeverFallsBack(t *testing.T) {
	// 5060 Ti is too small. ForceIndex=0 should make us wait, not fall back.
	// We expect ctx.Deadline to fire because no release ever happens.
	s := makeTestScheduler([]GPUSnapshot{
		{Index: 0, Name: "5060Ti", TotalMiB: 8000, FreeMiB: 3000, UsedMiB: 5000},
		{Index: 1, Name: "5080", TotalMiB: 16000, FreeMiB: 15500, UsedMiB: 500},
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	force0 := 0
	_, _, err := s.Acquire(ctx, GPURequest{
		Kind:            "test",
		VRAMRequiredMiB: 5500,
		ForceIndex:      &force0,
	})
	if err == nil {
		t.Fatal("expected ctx.Err() since ForceIndex=0 doesn't fit and shouldn't fall back")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected deadline error, got %v", err)
	}
}

func TestGPUScheduler_QueueingWhenNoGPUFits(t *testing.T) {
	// Both GPUs are saturated. First job takes the 5080, second job parks on
	// the cond until first releases.
	s := makeTestScheduler([]GPUSnapshot{
		{Index: 0, Name: "5060Ti", TotalMiB: 8000, FreeMiB: 2000, UsedMiB: 6000},
		{Index: 1, Name: "5080", TotalMiB: 16000, FreeMiB: 12000, UsedMiB: 4000},
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	idx1, release1, err := s.Acquire(ctx, GPURequest{Kind: "first", VRAMRequiredMiB: 5500})
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	if idx1 != 1 {
		t.Errorf("first should land on idx=1, got %d", idx1)
	}

	// Second job tries to take any GPU — 5080 is now in-use by us, 5060 Ti
	// doesn't fit. It must park.
	var idx2 int
	var release2 func()
	var err2 error
	done := make(chan struct{})
	startedAt := time.Now()
	go func() {
		idx2, release2, err2 = s.Acquire(ctx, GPURequest{Kind: "second", VRAMRequiredMiB: 5500})
		close(done)
	}()

	// Give the second goroutine time to park.
	time.Sleep(50 * time.Millisecond)

	// Release the first → second should wake.
	release1()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("second Acquire did not unblock after first released")
	}

	if err2 != nil {
		t.Fatalf("second Acquire returned error: %v", err2)
	}
	if release2 != nil {
		release2()
	}
	if idx2 != 1 {
		t.Errorf("second should have grabbed idx=1 after release, got %d", idx2)
	}
	if elapsed := time.Since(startedAt); elapsed < 50*time.Millisecond {
		t.Errorf("second job should have waited; elapsed=%v", elapsed)
	}
}

func TestGPUScheduler_ConcurrentAcquiresSerializedPerGPU(t *testing.T) {
	// 1 GPU, default max 1 job per GPU. Two acquires → must be serialized.
	s := makeTestScheduler([]GPUSnapshot{
		{Index: 0, Name: "5060Ti", TotalMiB: 8000, FreeMiB: 7500, UsedMiB: 500},
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var inflight atomic.Int32
	var sawConcurrent atomic.Bool

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := s.Acquire(ctx, GPURequest{Kind: "concurrent", VRAMRequiredMiB: 3000})
			if err != nil {
				t.Errorf("Acquire failed: %v", err)
				return
			}
			defer release()
			if inflight.Add(1) > 1 {
				sawConcurrent.Store(true)
			}
			time.Sleep(20 * time.Millisecond)
			inflight.Add(-1)
		}()
	}
	wg.Wait()
	if sawConcurrent.Load() {
		t.Errorf("two acquires ran concurrently on the same GPU; scheduler did not serialize")
	}
}

func TestGPUScheduler_CancelCtxWhileWaiting(t *testing.T) {
	// Single GPU. First Acquire takes it. Second Acquire is parked and we
	// cancel its context — it should return ctx.Err() and free the goroutine.
	s := makeTestScheduler([]GPUSnapshot{
		{Index: 0, Name: "5060Ti", TotalMiB: 8000, FreeMiB: 7500, UsedMiB: 500},
	}, nil)

	idx1, release1, err := s.Acquire(context.Background(), GPURequest{Kind: "first", VRAMRequiredMiB: 3000})
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	if idx1 != 0 {
		t.Fatalf("first should grab idx=0, got %d", idx1)
	}
	defer release1()

	cancelCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Acquire(cancelCtx, GPURequest{Kind: "second", VRAMRequiredMiB: 3000})
		done <- err
	}()

	time.Sleep(50 * time.Millisecond) // let it park
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Acquire did not return after ctx cancel")
	}
}

func TestGPUScheduler_ProbeErrorPropagates(t *testing.T) {
	probeErr := errors.New("nvidia-smi died")
	s := makeTestScheduler(nil, probeErr)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, _, err := s.Acquire(ctx, GPURequest{Kind: "test", VRAMRequiredMiB: 3000})
	if err == nil || !contains(err.Error(), "nvidia-smi died") {
		t.Errorf("expected probe error to propagate, got %v", err)
	}
}

// contains is a tiny stand-in for strings.Contains so we don't import strings.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// Helper to keep the env clean between tests if anyone runs them with their
// own WHISPER_CT2_* already set.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("WHISPER_CT2_DEVICE_INDEX")
	_ = os.Unsetenv("WHISPER_CT2_FORCE_DEVICE_INDEX")
	_ = os.Unsetenv("GPU_SCHED_MAX_JOBS_PER_GPU")
	_ = os.Unsetenv("GPU_SCHED_SAFETY_MARGIN_MIB")
	os.Exit(m.Run())
}
