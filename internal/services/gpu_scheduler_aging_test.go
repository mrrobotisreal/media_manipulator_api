package services

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for the scheduler's `now` seam.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type agingHarness struct {
	s     *GPUScheduler
	clock *fakeClock
	mu    sync.Mutex
	order []string
	wg    sync.WaitGroup
	ctx   context.Context
}

func newAgingHarness(t *testing.T, agingMinutes int) *agingHarness {
	t.Helper()
	s := makeTestScheduler([]GPUSnapshot{{Index: 0, Name: "5080", TotalMiB: 16000, FreeMiB: 12000, UsedMiB: 4000}}, nil)
	clock := newFakeClock()
	s.now = clock.Now
	s.agingAfter = time.Duration(agingMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return &agingHarness{s: s, clock: clock, ctx: ctx}
}

// spawn parks a waiter that records its name, holds the GPU until `hold` is
// closed, then releases. The sleep only lets the goroutine reach the queue.
func (h *agingHarness) spawn(t *testing.T, name string, priority int, hold <-chan struct{}) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		_, release, err := h.s.Acquire(h.ctx, GPURequest{Kind: name, VRAMRequiredMiB: 5000, Priority: priority})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		h.mu.Lock()
		h.order = append(h.order, name)
		h.mu.Unlock()
		<-hold
		release()
	}()
	time.Sleep(80 * time.Millisecond)
}

func (h *agingHarness) served() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.order...)
}

// A standard waiter yields to Premium until the aging threshold, then is served
// next even though another Premium job is queued ahead of it by priority.
func TestGPUScheduler_AgingStandardBeatsPremiumAfterThreshold(t *testing.T) {
	h := newAgingHarness(t, 10)
	_, releaseHolder, err := h.s.Acquire(h.ctx, GPURequest{Kind: "holder", VRAMRequiredMiB: 5000})
	if err != nil {
		t.Fatal(err)
	}
	holdP1, holdS, holdP2 := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h.spawn(t, "standard", GPUPriorityStandard, holdS)
	h.clock.Advance(1 * time.Minute)
	h.spawn(t, "premium1", GPUPriorityPremium, holdP1)

	// Under the threshold: premium wins.
	h.clock.Advance(4 * time.Minute) // standard is 5 min old
	releaseHolder()
	time.Sleep(150 * time.Millisecond)
	if got := h.served(); len(got) != 1 || got[0] != "premium1" {
		t.Fatalf("before the threshold premium must win, got %v", got)
	}

	// A fresh Premium arrives while premium1 runs; the standard job has now
	// waited past 10 minutes, so it must go next.
	h.spawn(t, "premium2", GPUPriorityPremium, holdP2)
	h.clock.Advance(6 * time.Minute) // standard is 11 min old, premium2 is 6
	close(holdP1)
	time.Sleep(150 * time.Millisecond)
	if got := h.served(); len(got) != 2 || got[1] != "standard" {
		t.Fatalf("an aged standard waiter must be served next, got %v", got)
	}
	close(holdS)
	close(holdP2)
	h.wg.Wait()
	if got := h.served(); got[2] != "premium2" {
		t.Fatalf("premium2 should finish last, got %v", got)
	}
}

// With aging disabled (GPU_SCHED_AGING_MINUTES=0) strict precedence is
// unchanged no matter how long a standard waiter has waited.
func TestGPUScheduler_AgingDisabledKeepsStrictPrecedence(t *testing.T) {
	h := newAgingHarness(t, 0)
	_, releaseHolder, err := h.s.Acquire(h.ctx, GPURequest{Kind: "holder", VRAMRequiredMiB: 5000})
	if err != nil {
		t.Fatal(err)
	}
	holdS, holdP := make(chan struct{}), make(chan struct{})
	h.spawn(t, "standard", GPUPriorityStandard, holdS)
	h.spawn(t, "premium", GPUPriorityPremium, holdP)
	h.clock.Advance(24 * time.Hour)
	releaseHolder()
	time.Sleep(150 * time.Millisecond)
	if got := h.served(); len(got) != 1 || got[0] != "premium" {
		t.Fatalf("aging off: premium must still win, got %v", got)
	}
	close(holdP)
	close(holdS)
	h.wg.Wait()
}

// The age counter is monotonic and per waiter: it starts at the waiter's own
// arrival, so a young standard waiter does not inherit an old one's age.
func TestGPUScheduler_AgingIsPerWaiter(t *testing.T) {
	h := newAgingHarness(t, 10)
	_, releaseHolder, err := h.s.Acquire(h.ctx, GPURequest{Kind: "holder", VRAMRequiredMiB: 5000})
	if err != nil {
		t.Fatal(err)
	}
	holdOld, holdYoung, holdP := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h.spawn(t, "old-standard", GPUPriorityStandard, holdOld)
	h.clock.Advance(11 * time.Minute)
	h.spawn(t, "young-standard", GPUPriorityStandard, holdYoung)
	h.spawn(t, "premium", GPUPriorityPremium, holdP)
	releaseHolder()
	time.Sleep(150 * time.Millisecond)
	if got := h.served(); len(got) != 1 || got[0] != "old-standard" {
		t.Fatalf("only the aged waiter jumps the queue, got %v", got)
	}
	close(holdOld)
	time.Sleep(150 * time.Millisecond)
	if got := h.served(); len(got) != 2 || got[1] != "premium" {
		t.Fatalf("young standard must still yield to premium, got %v", got)
	}
	close(holdP)
	close(holdYoung)
	h.wg.Wait()
}

func TestGPUAgingFromEnv(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
	}{{"", 10 * time.Minute}, {"0", 0}, {"3", 3 * time.Minute}, {"-1", 10 * time.Minute}, {"abc", 10 * time.Minute}} {
		t.Setenv("GPU_SCHED_AGING_MINUTES", c.env)
		if got := gpuAgingFromEnv(); got != c.want {
			t.Errorf("env=%q: got %v want %v", c.env, got, c.want)
		}
	}
}
