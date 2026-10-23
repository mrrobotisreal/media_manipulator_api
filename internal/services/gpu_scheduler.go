package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GPUSnapshot is one row from `nvidia-smi --query-gpu=...,memory.free`. We use
// the live `memory.free` rather than tracking our own reservations because it
// accounts for ALL processes on the card — ours, Ollama, anything Python
// someone left running in another shell — at the moment of the probe.
type GPUSnapshot struct {
	Index    int
	Name     string
	TotalMiB int64
	FreeMiB  int64
	UsedMiB  int64
}

// GPURequest describes one GPU-needing job for the scheduler.
//
// PreferredOrder is the operator's hint ("try 5060 Ti first, then the 5080");
// after exhausting it the scheduler falls back to remaining GPUs sorted by
// ascending TotalMiB so small jobs don't displace big ones.
//
// ForceIndex pins to a specific GPU and disables fallback. Useful for
// debugging a single card or when you want to dedicate a GPU to a workload.
type GPURequest struct {
	Kind            string // free-form label for logs ("whisper-ct2", etc.)
	VRAMRequiredMiB int64
	SafetyMarginMiB int64 // 0 → use GPU_SCHED_SAFETY_MARGIN_MIB env (default 512)
	PreferredOrder  []int
	ForceIndex      *int
	// Priority lets a Premium job jump the queue. Higher wins; use the
	// GPUPriority* constants. The default zero value is the standard queue, so
	// every existing call site keeps its current behaviour.
	Priority int
}

// GPU queue priorities. Two levels is all the tier matrix sells, and a wider
// scale would only invite fine-grained tuning nobody can reason about.
const (
	GPUPriorityStandard = 0
	GPUPriorityPremium  = 10
)

// GPUScheduler is a process-wide, GPU-aware queue. Multiple goroutines can
// call Acquire concurrently; whoever can fit on a free card gets it,
// everyone else parks on the cond until a release wakes them.
type GPUScheduler struct {
	mu      sync.Mutex
	cond    *sync.Cond
	inUse   map[int]int // gpu index → number of jobs we've reserved on it
	enabled bool

	// waiters is every in-flight Acquire, including the one currently probing.
	// It exists so a standard-priority caller can tell whether a Premium caller
	// is queued behind it and stand aside.
	waiters map[*gpuWaiter]struct{}

	// probe is injectable for tests; production callers use ProbeGPUSnapshots.
	probe func(context.Context) ([]GPUSnapshot, error)

	// now is the scheduler's clock, injectable so aging tests need no sleeps.
	now func() time.Time
	// agingAfter is how long a waiter may wait before it is promoted past
	// every un-aged waiter (GPU_SCHED_AGING_MINUTES). Zero disables aging.
	agingAfter time.Duration
}

// gpuWaiter is one in-flight Acquire.
type gpuWaiter struct {
	priority int
	// blocking is whether lower-priority callers must stand aside for this
	// one. It starts true for a prioritised waiter and is cleared if the probe
	// shows no card in the box could ever satisfy it — otherwise an
	// impossible Premium request would wedge the whole scheduler rather than
	// simply failing to find a home.
	blocking bool

	// enqueued is when the waiter registered, on the scheduler's clock. Age is
	// measured from here and is monotonic: once aged, always aged.
	enqueued time.Time
	aged     bool
	// impossible is set when the probe shows no card could ever hold the
	// request; such a waiter never blocks anyone, aged or not.
	impossible bool
}

// blocksOthers reports whether lower-ranked callers must stand aside for w.
func (w *gpuWaiter) blocksOthers() bool { return (w.blocking || w.aged) && !w.impossible }

// rank is the waiter's effective priority. An aged waiter ranks one level above
// Premium, so it outranks every un-aged waiter regardless of tier.
func (w *gpuWaiter) rank() int {
	if w.aged {
		return GPUPriorityPremium + 1
	}
	return w.priority
}

var (
	gpuSchedulerOnce     sync.Once
	gpuSchedulerInstance *GPUScheduler
)

// SharedGPUScheduler returns the lazily-initialized process-wide scheduler.
// First call probes nvidia-smi; subsequent calls return the same instance.
func SharedGPUScheduler() *GPUScheduler {
	gpuSchedulerOnce.Do(func() {
		s := newGPUScheduler(ProbeGPUSnapshots)
		// One-time enablement probe. If nvidia-smi is missing we mark the
		// scheduler disabled and Acquire returns (-1, no-op, nil) so callers
		// degrade gracefully on CPU-only hosts.
		if snaps, err := s.probe(context.Background()); err == nil && len(snaps) > 0 {
			s.enabled = true
			var b strings.Builder
			for i, snap := range snaps {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%d=%s(%dMiB total)", snap.Index, snap.Name, snap.TotalMiB)
			}
			log.Printf("gpu-scheduler: enabled with %d GPUs [%s]", len(snaps), b.String())
		} else {
			log.Printf("gpu-scheduler: disabled — nvidia-smi unavailable or no GPUs (err=%v)", err)
		}
		gpuSchedulerInstance = s
	})
	return gpuSchedulerInstance
}

func newGPUScheduler(probe func(context.Context) ([]GPUSnapshot, error)) *GPUScheduler {
	s := &GPUScheduler{
		inUse:      make(map[int]int),
		waiters:    map[*gpuWaiter]struct{}{},
		probe:      probe,
		now:        time.Now,
		agingAfter: gpuAgingFromEnv(),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// gpuAgingFromEnv reads GPU_SCHED_AGING_MINUTES: default 10, 0 disables aging,
// negative or unparsable values fall back to the default.
func gpuAgingFromEnv() time.Duration {
	const defaultMinutes = 10
	raw := strings.TrimSpace(os.Getenv("GPU_SCHED_AGING_MINUTES"))
	if raw == "" {
		return defaultMinutes * time.Minute
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultMinutes * time.Minute
	}
	return time.Duration(n) * time.Minute
}

// refreshAging promotes every waiter that has waited past agingAfter. Callers
// must hold s.mu. Promotion is logged once per waiter and never reverted.
func (s *GPUScheduler) refreshAging() {
	if s.agingAfter <= 0 {
		return
	}
	now := s.now()
	for w := range s.waiters {
		if !w.aged && now.Sub(w.enqueued) >= s.agingAfter {
			w.aged = true
			log.Printf("gpu-scheduler: waiter (priority %d) aged after %s; promoted ahead of un-aged jobs",
				w.priority, now.Sub(w.enqueued).Round(time.Second))
		}
	}
}

// blockedByHigherPriority reports whether a caller must stand aside because a
// more important one is queued. Callers must hold s.mu.
//
// Aging contract: priority is strict (Premium before standard) until a waiter
// has waited GPU_SCHED_AGING_MINUTES (default 10; 0 disables aging). From then
// on it is "aged" and ranks one level above Premium, so a continuous stream of
// Premium jobs cannot starve the standard queue. Aged waiters order among
// themselves by arrival (oldest first). Aging is per waiter and monotonic, and
// an aged waiter that no card could ever hold still blocks nobody.
func (s *GPUScheduler) blockedByHigherPriority(w *gpuWaiter) bool {
	for other := range s.waiters {
		if other == w || !other.blocksOthers() {
			continue
		}
		if other.rank() > w.rank() {
			return true
		}
		if other.aged && w.aged && other.enqueued.Before(w.enqueued) {
			return true
		}
	}
	return false
}

// Enabled reports whether the scheduler has GPU info to work with.
func (s *GPUScheduler) Enabled() bool { return s.enabled }

// Acquire blocks until a GPU is free and fits the request. Returns the
// chosen GPU index plus a release function the caller MUST defer. Returns
// (-1, no-op, nil) when the scheduler is disabled so callers can keep their
// previous code path on CPU-only hosts.
//
// Cancellation: if ctx is cancelled while we're queued, returns ctx.Err()
// and the broadcast safety path wakes any other parked waiters too.
func (s *GPUScheduler) Acquire(ctx context.Context, req GPURequest) (int, func(), error) {
	if !s.enabled {
		return -1, func() {}, nil
	}
	safety := req.SafetyMarginMiB
	if safety <= 0 {
		safety = int64(envInt("GPU_SCHED_SAFETY_MARGIN_MIB", 512))
	}
	maxPerGPU := envInt("GPU_SCHED_MAX_JOBS_PER_GPU", 1)
	if maxPerGPU < 1 {
		maxPerGPU = 1
	}
	required := req.VRAMRequiredMiB + safety

	s.mu.Lock()
	defer s.mu.Unlock()

	// Register before the first probe so a Premium caller starts deterring
	// standard callers immediately, not only once it has already lost a race.
	w := &gpuWaiter{priority: req.Priority, blocking: req.Priority > GPUPriorityStandard, enqueued: s.now()}
	s.waiters[w] = struct{}{}
	defer func() {
		delete(s.waiters, w)
		// Whoever was standing aside for this caller can stop now.
		if w.blocksOthers() {
			s.cond.Broadcast()
		}
	}()

	var waitStarted time.Time

	for {
		// Probe under no lock — nvidia-smi can take a few hundred ms and we
		// don't want to block other Acquire/Release goroutines waiting on us.
		s.mu.Unlock()
		snaps, err := s.probe(ctx)
		s.mu.Lock()

		if ctx.Err() != nil {
			return -1, nil, ctx.Err()
		}
		if err != nil {
			return -1, nil, fmt.Errorf("nvidia-smi probe: %w", err)
		}

		gpuByIdx := make(map[int]GPUSnapshot, len(snaps))
		for _, g := range snaps {
			gpuByIdx[g.Index] = g
		}

		cands := s.buildCandidateOrder(req, snaps, gpuByIdx)

		s.refreshAging()

		// A prioritised (or aged) request that no card could ever hold — not
		// "is busy", but "is too small even when idle" — stops deterring
		// everyone else.
		if !w.impossible && (w.blocking || w.aged) && !fitsAnyCard(required, cands, gpuByIdx) {
			w.blocking = false
			w.impossible = true
			s.cond.Broadcast()
			log.Printf("gpu-scheduler: %s requires %dMiB, which exceeds every card; dropping its queue priority",
				req.Kind, required)
		}

		// Stand aside for a Premium job. Checked per iteration rather than
		// once, because the prioritised caller may arrive while we are parked.
		yielding := s.blockedByHigherPriority(w)
		if yielding && waitStarted.IsZero() {
			waitStarted = time.Now()
			log.Printf("gpu-scheduler: %s standing aside for a higher-priority job", req.Kind)
		}

		for _, idx := range cands {
			if yielding {
				break
			}
			g := gpuByIdx[idx]
			if s.inUse[idx] >= maxPerGPU {
				continue
			}
			if g.FreeMiB < required {
				continue
			}
			// Take it.
			s.inUse[idx]++
			chosen := idx
			log.Printf("gpu-scheduler: %s acquired GPU %d (%s) — free=%dMiB used=%dMiB total=%dMiB required=%dMiB+safety=%dMiB",
				req.Kind, idx, g.Name, g.FreeMiB, g.UsedMiB, g.TotalMiB, req.VRAMRequiredMiB, safety)
			if !waitStarted.IsZero() {
				log.Printf("gpu-scheduler: %s waited %s in queue", req.Kind, time.Since(waitStarted).Round(time.Second))
			}
			var releaseOnce sync.Once
			release := func() {
				releaseOnce.Do(func() {
					s.mu.Lock()
					if s.inUse[chosen] > 0 {
						s.inUse[chosen]--
					}
					s.cond.Broadcast()
					s.mu.Unlock()
					log.Printf("gpu-scheduler: %s released GPU %d", req.Kind, chosen)
				})
			}
			return chosen, release, nil
		}

		// Nothing fits — queue.
		if waitStarted.IsZero() {
			waitStarted = time.Now()
			log.Printf("gpu-scheduler: %s queued (required=%dMiB+%dMiB safety); waiting for GPU", req.Kind, req.VRAMRequiredMiB, safety)
		}
		if err := s.waitOrCtx(ctx, s.agingWake(w)); err != nil {
			return -1, nil, err
		}
	}
}

// fitsAnyCard reports whether any candidate card has enough TOTAL memory for
// the request — i.e. whether waiting could ever succeed, ignoring what is
// currently in use.
func fitsAnyCard(required int64, cands []int, byIdx map[int]GPUSnapshot) bool {
	for _, idx := range cands {
		if byIdx[idx].TotalMiB >= required {
			return true
		}
	}
	return false
}

// buildCandidateOrder enumerates GPUs in the order we should try them: forced
// index (if any) standalone, then PreferredOrder (de-duplicated), then any
// remaining GPUs sorted by ascending TotalMiB so small models still prefer
// the smaller card even when no preference is given.
func (s *GPUScheduler) buildCandidateOrder(req GPURequest, snaps []GPUSnapshot, byIdx map[int]GPUSnapshot) []int {
	if req.ForceIndex != nil {
		if _, ok := byIdx[*req.ForceIndex]; ok {
			return []int{*req.ForceIndex}
		}
		return nil
	}
	seen := make(map[int]bool)
	cands := make([]int, 0, len(snaps))
	for _, i := range req.PreferredOrder {
		if _, ok := byIdx[i]; ok && !seen[i] {
			cands = append(cands, i)
			seen[i] = true
		}
	}
	rest := make([]GPUSnapshot, 0, len(snaps))
	for _, g := range snaps {
		if !seen[g.Index] {
			rest = append(rest, g)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].TotalMiB < rest[j].TotalMiB })
	for _, g := range rest {
		cands = append(cands, g.Index)
	}
	return cands
}

// agingWake is how long until w ages, or 0 when there is nothing to schedule
// (aging disabled or already aged). Callers must hold s.mu.
func (s *GPUScheduler) agingWake(w *gpuWaiter) time.Duration {
	if s.agingAfter <= 0 || w.aged {
		return 0
	}
	remaining := s.agingAfter - s.now().Sub(w.enqueued)
	if remaining <= 0 {
		// Aged by the clock but not yet marked; the next loop marks it.
		return time.Millisecond
	}
	return remaining
}

// waitOrCtx parks on the condition variable until ctx cancels, another
// goroutine broadcasts, or (when wakeAfter > 0) the waiter is due to age. The
// timer and the ctx watcher are both torn down on return, so nothing leaks.
// Releases s.mu while waiting per sync.Cond contract.
func (s *GPUScheduler) waitOrCtx(ctx context.Context, wakeAfter time.Duration) error {
	done := make(chan struct{})
	// Wake the cond when ctx fires.
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-done:
		}
	}()
	if wakeAfter > 0 {
		timer := time.AfterFunc(wakeAfter, func() {
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		})
		defer timer.Stop()
	}
	s.cond.Wait()
	close(done)
	return ctx.Err()
}

// ProbeGPUSnapshots queries nvidia-smi for the per-device state. Returns an
// error when nvidia-smi is not installed or fails.
func ProbeGPUSnapshots(ctx context.Context) ([]GPUSnapshot, error) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil, errors.New("nvidia-smi not found in PATH")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "nvidia-smi",
		"--query-gpu=index,memory.total,memory.free,memory.used,name",
		"--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi probe: %w", err)
	}
	var snaps []GPUSnapshot
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 5 {
			continue
		}
		idx, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		total, e2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		free, e3 := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		used, e4 := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue
		}
		snaps = append(snaps, GPUSnapshot{
			Index:    idx,
			Name:     strings.TrimSpace(parts[4]),
			TotalMiB: total,
			FreeMiB:  free,
			UsedMiB:  used,
		})
	}
	return snaps, nil
}

// EstimateWhisperVRAMMiB returns a conservative VRAM estimate for the given
// faster-whisper model + compute type. Values are deliberately on the high
// side — better to bounce to a bigger card than OOM-crash mid-decode.
//
// Reference numbers (faster-whisper / ctranslate2, float16, batch=1):
//   - tiny  ~ 0.5–0.8 GB
//   - base  ~ 0.8–1.2 GB
//   - small ~ 1.5–2.2 GB
//   - medium ~ 2.5–3.5 GB
//   - large-v3 ~ 4.5–5.5 GB
//
// Batched inference and very long audio can push these higher, hence the
// padding. int8 quantization roughly halves the requirement.
func EstimateWhisperVRAMMiB(model, computeType string) int64 {
	model = strings.ToLower(strings.TrimSpace(model))
	base := int64(3500)
	switch {
	case strings.HasPrefix(model, "large"):
		base = 5500
	case strings.HasPrefix(model, "medium"):
		base = 3500
	case strings.HasPrefix(model, "small"):
		base = 2200
	case strings.HasPrefix(model, "base"):
		base = 1200
	case strings.HasPrefix(model, "tiny"):
		base = 900
	}
	switch strings.ToLower(strings.TrimSpace(computeType)) {
	case "float32", "fp32":
		return base * 2
	case "int8":
		return base / 2
	case "int8_float16", "int8_float32":
		return base * 6 / 10
	}
	return base
}

// ResolveWhisperGPURequest derives a GPURequest from cfg + the operator env.
//
// WHISPER_CT2_DEVICE_INDEX may be a single index ("0") or a comma-separated
// preference list ("0,1") — the scheduler tries them in order and falls
// through to the remaining GPUs if none fit.
//
// WHISPER_CT2_FORCE_DEVICE_INDEX (single int) hard-pins to a GPU; if it
// can't accommodate the job we wait for it rather than falling back.
func ResolveWhisperGPURequest(cfg WhisperCT2Config) GPURequest {
	req := GPURequest{
		Kind:            "whisper-ct2",
		VRAMRequiredMiB: EstimateWhisperVRAMMiB(cfg.Model, cfg.ComputeType),
	}
	if v := strings.TrimSpace(os.Getenv("WHISPER_CT2_FORCE_DEVICE_INDEX")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			req.ForceIndex = &n
		}
	}
	if pref := strings.TrimSpace(os.Getenv("WHISPER_CT2_DEVICE_INDEX")); pref != "" {
		for _, p := range strings.Split(pref, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if n, err := strconv.Atoi(p); err == nil {
				req.PreferredOrder = append(req.PreferredOrder, n)
			}
		}
	}
	return req
}
