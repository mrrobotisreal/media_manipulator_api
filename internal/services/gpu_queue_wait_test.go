package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func queueTestScheduler() (*GPUScheduler, *fakeClock) {
	s := makeTestScheduler([]GPUSnapshot{{Index: 0, Name: "5080", TotalMiB: 16000, FreeMiB: 12000, UsedMiB: 4000}}, nil)
	clock := newFakeClock()
	s.now = clock.Now
	return s, clock
}

func waitForWaiting(t *testing.T, s *GPUScheduler, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.Snapshot().Waiting == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waiting never reached %d (got %+v)", want, s.Snapshot())
}

func TestGPUScheduler_SnapshotCounts(t *testing.T) {
	var nilSched *GPUScheduler
	if got := nilSched.Snapshot(); got != (GPUQueueSnapshot{}) {
		t.Fatalf("nil scheduler: %+v", got)
	}
	disabled := newGPUScheduler(func(context.Context) ([]GPUSnapshot, error) { return nil, nil })
	if got := disabled.Snapshot(); got != (GPUQueueSnapshot{}) {
		t.Fatalf("disabled scheduler: %+v", got)
	}

	s, _ := queueTestScheduler()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, release, err := s.Acquire(ctx, GPURequest{Kind: "a", VRAMRequiredMiB: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(); got != (GPUQueueSnapshot{Enabled: true, Running: 1}) {
		t.Fatalf("one running: %+v", got)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, rel, err := s.Acquire(ctx, GPURequest{Kind: "b", VRAMRequiredMiB: 5000})
		if err == nil {
			rel()
		}
	}()
	waitForWaiting(t, s, 1)
	if got := s.Snapshot(); got != (GPUQueueSnapshot{Enabled: true, Running: 1, Waiting: 1}) {
		t.Fatalf("one running one waiting: %+v", got)
	}
	release()
	<-done
	if got := s.Snapshot(); got != (GPUQueueSnapshot{Enabled: true}) {
		t.Fatalf("drained: %+v", got)
	}
}

func TestAcquireTimed_QueueWaitUsesSchedulerClock(t *testing.T) {
	s, clock := queueTestScheduler()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Immediate grant: not queued, zero wait.
	_, rel1, w1, err := s.AcquireTimed(ctx, GPURequest{Kind: "first", VRAMRequiredMiB: 5000})
	if err != nil || w1.Queued || w1.Wait != 0 {
		t.Fatalf("immediate acquire: %+v err=%v", w1, err)
	}

	type result struct {
		w   GPUWait
		err error
	}
	out := make(chan result, 1)
	go func() {
		_, rel, w, err := s.AcquireTimed(ctx, GPURequest{Kind: "second", VRAMRequiredMiB: 5000})
		if err == nil {
			rel()
		}
		out <- result{w, err}
	}()
	waitForWaiting(t, s, 1)
	clock.Advance(7 * time.Second)
	rel1()
	r := <-out
	if r.err != nil || !r.w.Queued || r.w.Wait != 7*time.Second {
		t.Fatalf("queued acquire: %+v err=%v", r.w, r.err)
	}
}

func TestAcquireGPUForJob_RecordsQueueWaitOnlyWhenQueued(t *testing.T) {
	jm := NewJobManager()
	immediate := jm.CreateJob(models.OriginalFileInfo{Name: "a"}, nil)
	queuedJob := jm.CreateJob(models.OriginalFileInfo{Name: "b"}, nil)
	s, clock := queueTestScheduler()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, rel1, err := acquireGPUForJobOn(ctx, s, jm, immediate.ID, GPURequest{Kind: "x", VRAMRequiredMiB: 5000})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := jm.GetJob(immediate.ID)
	if got.QueueWaitMs != nil {
		t.Fatalf("job that never queued must have nil QueueWaitMs, got %d", *got.QueueWaitMs)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "queueWaitMs") {
		t.Fatalf("queueWaitMs must be omitted: %s", raw)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, rel, err := acquireGPUForJobOn(ctx, s, jm, queuedJob.ID, GPURequest{Kind: "y", VRAMRequiredMiB: 5000})
		if err == nil {
			rel()
		}
	}()
	waitForWaiting(t, s, 1)
	clock.Advance(2500 * time.Millisecond)
	rel1()
	<-done

	got, _ = jm.GetJob(queuedJob.ID)
	if got.QueueWaitMs == nil || *got.QueueWaitMs != 2500 {
		t.Fatalf("queueWaitMs = %v, want 2500", got.QueueWaitMs)
	}
	raw, _ = json.Marshal(got)
	if !strings.Contains(string(raw), `"queueWaitMs":2500`) {
		t.Fatalf("JSON missing queueWaitMs: %s", raw)
	}

	// A second queued stage adds to the total.
	jm.AddQueueWait(queuedJob.ID, 500*time.Millisecond)
	got, _ = jm.GetJob(queuedJob.ID)
	if *got.QueueWaitMs != 3000 {
		t.Fatalf("stages must sum, got %d", *got.QueueWaitMs)
	}
	// Unknown job / nil manager are no-ops.
	jm.AddQueueWait("missing", time.Second)
	(*JobManager)(nil).AddQueueWait("x", time.Second)
}
