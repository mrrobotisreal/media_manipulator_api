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

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
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
	// MaxWait, when > 0, bounds how long the caller may wait for a slot; past
	// it Acquire gives up with ErrGPUMaxWait. Used by background work that may
	// be dropped rather than queue forever (MM94, ANALYSIS_GPU_MAX_WAIT_SECONDS).
	MaxWait time.Duration
	// Burst, when non-nil, makes this request eligible for a cloud GPU offer
	// (MM127, design §4). Nil = never burst; every call site that does not set
	// it behaves exactly as before. Only call sites that can run remotely set it.
	Burst *BurstEligibility
	// EnqueuedAt, when non-zero, is the waiter's original enqueue time on the
	// scheduler's clock. Internal: a fallback re-acquire after a failed burst
	// passes the first acquire's time so aging and order are preserved (§6.4).
	EnqueuedAt time.Time
}

// GPU queue priorities. Two levels is all the tier matrix sells, plus one for
// work no user is waiting on; a wider scale would only invite fine-grained
// tuning nobody can reason about.
const (
	// GPUPriorityBackground is internal work no user waits on (the upload
	// "product intelligence" analysis, MM94). It yields to every queued user
	// job whose cards overlap its own, never ages, and is normally given a
	// MaxWait so it is dropped rather than queued forever.
	GPUPriorityBackground = -10
	GPUPriorityStandard   = 0
	GPUPriorityPremium    = 10
)

// ErrGPUMaxWait is returned by Acquire when GPURequest.MaxWait elapsed before
// a slot was free.
var ErrGPUMaxWait = errors.New("gpu-scheduler: gave up waiting for a GPU slot (max wait reached)")

// gpuReprobeInterval is how often a parked waiter re-probes nvidia-smi even
// without a release. A slot is often blocked by memory another process is
// still freeing (an Ollama model unloading, a subprocess exiting), and no
// release wakes the queue when that memory comes back.
const gpuReprobeInterval = 2 * time.Second

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
	// reprobe is the parked waiters' re-probe interval (gpuReprobeInterval);
	// ticking is whether the re-probe goroutine is running. Guarded by mu.
	reprobe time.Duration
	ticking bool

	// MM127 burst hook. All nil/zero when burst is off: nothing below runs.
	burst   BurstOfferer
	pred    *holdPredictor
	running map[uint64]runningSlot // slot id (= waiter id) -> running slot
	nextID  uint64
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
	// parked is true while the waiter sleeps on the condition variable, i.e. it
	// is genuinely queued. Guarded by s.mu; read by Snapshot.
	parked bool
	// cards is the waiter's candidate set from its latest probe (nil before
	// the first probe). A background waiter only stands aside for a waiter
	// that wants one of the same cards.
	cards map[int]bool

	// MM127 burst bookkeeping (inert when no offerer is installed).
	id            uint64
	kind          string
	burst         *BurstEligibility
	vram          int64
	queued        bool         // has failed to get a slot at least once (stays true while re-probing)
	offerer       BurstOfferer // set once the offerer has seen this waiter (so it gets Withdraw)
	pendingRemote bool         // an offer was accepted; stop asking
	remote        *burst.Run   // set when the remote is ready
}

// hasBackgroundWaiter reports whether any background waiter is in flight.
// Callers hold s.mu.
func (s *GPUScheduler) hasBackgroundWaiter() bool {
	for w := range s.waiters {
		if w.background() {
			return true
		}
	}
	return false
}

// background reports whether w is background work (below the standard queue).
func (w *gpuWaiter) background() bool { return w.priority < GPUPriorityStandard }

// sharesCard reports whether w and other want at least one common card. A
// waiter that has not probed yet is assumed to overlap.
func (w *gpuWaiter) sharesCard(other *gpuWaiter) bool {
	if w.cards == nil || other.cards == nil {
		return true
	}
	for idx := range w.cards {
		if other.cards[idx] {
			return true
		}
	}
	return false
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
		reprobe:    gpuReprobeInterval,
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
		// Background work never ages: it must never jump a user's job.
		if w.background() {
			continue
		}
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
//
// Background contract (MM94): a background waiter additionally stands aside
// for every higher-ranked waiter that wants one of its cards and could ever be
// served there, so internal work never delays a queued user job.
func (s *GPUScheduler) blockedByHigherPriority(w *gpuWaiter) bool {
	for other := range s.waiters {
		if other == w {
			continue
		}
		if w.background() && !other.impossible && other.rank() > w.rank() && w.sharesCard(other) {
			return true
		}
		if !other.blocksOthers() {
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

// GPUQueueSnapshot is a point-in-time view of the shared GPU queue.
type GPUQueueSnapshot struct {
	Enabled bool
	Running int
	Waiting int
}

// Snapshot returns the running (slots held) and waiting (parked callers)
// counts. It takes only the scheduler mutex, never probes nvidia-smi, and is
// safe on a nil or disabled scheduler (all zero).
func (s *GPUScheduler) Snapshot() GPUQueueSnapshot {
	if s == nil || !s.enabled {
		return GPUQueueSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := GPUQueueSnapshot{Enabled: true}
	for _, n := range s.inUse {
		snap.Running += n
	}
	for w := range s.waiters {
		if w.parked {
			snap.Waiting++
		}
	}
	return snap
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
	idx, release, _, err := s.AcquireTimed(ctx, req)
	return idx, release, err
}

// GPUWait reports how an Acquire spent its time. Queued is true only when the
// caller actually had to park (no slot, or standing aside for a higher-priority
// job); Wait is then enqueue to acquire on the scheduler's clock. A disabled
// scheduler, or an immediate grant, is the zero value.
type GPUWait struct {
	Queued bool
	Wait   time.Duration
}

// AcquireTimed is Acquire plus the time spent waiting for the slot (MM33
// queueWaitMs). All Acquire semantics apply.
//
// It is local-only: a Burst descriptor on req is ignored (MM127), so every
// existing call site keeps its semantics. Use AcquireTarget to allow a remote.
func (s *GPUScheduler) AcquireTimed(ctx context.Context, req GPURequest) (int, func(), GPUWait, error) {
	req.Burst = nil
	t, release, waited, err := s.AcquireTarget(ctx, req)
	return t.Card, release, waited, err
}

// AcquireTarget is AcquireTimed that may also return a remote burst target
// (MM127): when req.Burst is set, an offerer is installed and it accepts the
// waiter, a cloud GPU races the local queue and whichever is ready first wins.
// For a remote target Card is 0 (the instance has one GPU), the release func
// is a no-op for local accounting, and GPUWait.Wait is enqueue -> remote ready.
func (s *GPUScheduler) AcquireTarget(ctx context.Context, req GPURequest) (Target, func(), GPUWait, error) {
	if !s.enabled {
		return Target{Card: -1}, func() {}, GPUWait{}, nil
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

	// MaxWait: a derived deadline, reported as ErrGPUMaxWait rather than as the
	// caller's own cancellation.
	parentCtx := ctx
	if req.MaxWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.MaxWait)
		defer cancel()
	}
	ctxErr := func() error {
		if req.MaxWait > 0 && parentCtx.Err() == nil && ctx.Err() != nil {
			log.Printf("gpu-scheduler: %s dropped after waiting %s (max wait)", req.Kind, req.MaxWait)
			return ErrGPUMaxWait
		}
		return ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Register before the first probe so a Premium caller starts deterring
	// standard callers immediately, not only once it has already lost a race.
	enqueuedAt := s.now()
	if !req.EnqueuedAt.IsZero() && req.EnqueuedAt.Before(enqueuedAt) {
		// A burst fallback re-acquire keeps the first acquire's age (MM127, §6.4).
		enqueuedAt = req.EnqueuedAt
	}
	s.nextID++
	w := &gpuWaiter{priority: req.Priority, blocking: req.Priority > GPUPriorityStandard, enqueued: enqueuedAt,
		id: s.nextID, kind: req.Kind, burst: req.Burst, vram: required}
	s.waiters[w] = struct{}{}
	exit := WithdrawGaveUp
	defer func() {
		// Tell the offerer this waiter is gone (MM127). Non-blocking by contract.
		if w.offerer != nil {
			w.offerer.Withdraw(w.id, exit)
		}
	}()
	defer func() {
		delete(s.waiters, w)
		// Whoever was standing aside for this caller can stop now: callers
		// below a Premium/aged waiter, and background waiters below anyone.
		if w.blocksOthers() || s.hasBackgroundWaiter() {
			s.cond.Broadcast()
		}
	}()

	var waitStarted time.Time
	queued := false

	for {
		// Probe under no lock — nvidia-smi can take a few hundred ms and we
		// don't want to block other Acquire/Release goroutines waiting on us.
		s.mu.Unlock()
		snaps, err := s.probe(ctx)
		s.mu.Lock()

		if ctx.Err() != nil {
			return Target{Card: -1}, nil, GPUWait{}, ctxErr()
		}
		if err != nil {
			return Target{Card: -1}, nil, GPUWait{}, fmt.Errorf("nvidia-smi probe: %w", err)
		}

		gpuByIdx := make(map[int]GPUSnapshot, len(snaps))
		for _, g := range snaps {
			gpuByIdx[g.Index] = g
		}

		cands := s.buildCandidateOrder(req, snaps, gpuByIdx)
		w.cards = make(map[int]bool, len(cands))
		for _, idx := range cands {
			w.cards[idx] = true
		}

		s.refreshAging()

		// A request that no card could ever hold — not "is busy", but "is too
		// small even when idle" — stops deterring everyone else (Premium and
		// aged waiters directly, any waiter for the background queue).
		if !w.impossible && !fitsAnyCard(required, cands, gpuByIdx) {
			w.blocking = false
			w.impossible = true
			s.cond.Broadcast()
			log.Printf("gpu-scheduler: %s requires %dMiB, which exceeds every card; dropping its queue priority",
				req.Kind, required)
		}

		// Stand aside for a Premium job. Checked per iteration rather than
		// once, because the prioritised caller may arrive while we are parked.
		yielding := s.blockedByHigherPriority(w)
		if yielding {
			queued = true
		}
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
			var waited GPUWait
			if queued {
				waited = GPUWait{Queued: true, Wait: s.now().Sub(w.enqueued)}
			}
			if s.pred != nil {
				s.running[w.id] = runningSlot{kind: req.Kind, card: chosen, start: s.now()}
			}
			slotID := w.id
			var releaseOnce sync.Once
			release := func() {
				releaseOnce.Do(func() {
					s.mu.Lock()
					if s.inUse[chosen] > 0 {
						s.inUse[chosen]--
					}
					s.observeReleaseLocked(slotID)
					s.cond.Broadcast()
					s.mu.Unlock()
					log.Printf("gpu-scheduler: %s released GPU %d", req.Kind, chosen)
				})
			}
			exit = WithdrawLocalSlot
			return Target{Card: chosen, EnqueuedAt: enqueuedAt}, release, waited, nil
		}

		// MM127: a cloud GPU that became ready first wins the race; a local
		// slot found above always wins a tie. With no offerer both branches
		// are skipped (w.remote is only ever set by an offerer's launch), so
		// the loop is the pre-MM127 code.
		if w.remote != nil {
			exit = WithdrawRemoteTaken
			waited := GPUWait{Queued: true, Wait: s.now().Sub(enqueuedAt)}
			log.Printf("gpu-scheduler: %s took remote burst run %s after %s in queue", req.Kind, w.remote.ID, waited.Wait.Round(time.Second))
			return Target{Card: 0, Remote: w.remote, EnqueuedAt: enqueuedAt}, func() {}, waited, nil
		}
		if s.burst != nil && req.Burst != nil && !w.pendingRemote {
			s.considerBurstLocked(w, req, cands, gpuByIdx, required, maxPerGPU)
		}

		// Nothing fits — queue.
		queued = true
		if waitStarted.IsZero() {
			waitStarted = time.Now()
			log.Printf("gpu-scheduler: %s queued (required=%dMiB+%dMiB safety); waiting for GPU", req.Kind, req.VRAMRequiredMiB, safety)
		}
		w.parked = true
		w.queued = true
		s.startReprobeTicker()
		err = s.waitOrCtx(ctx, s.agingWake(w))
		w.parked = false
		if err != nil {
			return Target{Card: -1}, nil, GPUWait{}, ctxErr()
		}
	}
}

// acquireGPUForJob is SharedGPUScheduler().Acquire that also records, on the
// job, how long the caller waited in the queue (MM33 queueWaitMs). jm or jobID
// may be empty, in which case it is a plain Acquire.
func acquireGPUForJob(ctx context.Context, jm *JobManager, jobID string, req GPURequest) (int, func(), error) {
	return acquireGPUForJobOn(ctx, SharedGPUScheduler(), jm, jobID, req)
}

func acquireGPUForJobOn(ctx context.Context, s *GPUScheduler, jm *JobManager, jobID string, req GPURequest) (int, func(), error) {
	idx, release, waited, err := s.AcquireTimed(ctx, req)
	if err == nil && waited.Queued {
		jm.AddQueueWait(jobID, waited.Wait)
	}
	return idx, release, err
}

// acquireTargetForJobOn is AcquireTarget that also records the queue wait on the
// job (MM33 queueWaitMs), for a local or a remote target alike (MM127). The
// wave-1 helper (runGPUStep, MM127b) uses it.
func acquireTargetForJobOn(ctx context.Context, s *GPUScheduler, jm *JobManager, jobID string, req GPURequest) (Target, func(), error) {
	t, release, waited, err := s.AcquireTarget(ctx, req)
	if err == nil && waited.Queued {
		jm.AddQueueWait(jobID, waited.Wait)
	}
	return t, release, err
}

// gpuAcquireFunc is acquireGPUForJob's shape, so services can inject a fake
// scheduler in tests (VoicesService, AIService, VideoSummaryService).
type gpuAcquireFunc func(ctx context.Context, jm *JobManager, jobID string, req GPURequest) (int, func(), error)

// withGPU acquires a slot from the shared scheduler, runs fn with the chosen
// card, and releases the slot when fn returns (MM94). fn gets -1 when the
// scheduler is disabled (no nvidia-smi, CPU-only host, tests) and must then
// use its configured card exactly as before the scheduler existed.
func withGPU(ctx context.Context, jm *JobManager, jobID string, req GPURequest, fn func(idx int) error) error {
	return withGPUVia(ctx, acquireGPUForJob, jm, jobID, req, fn)
}

// withGPUVia is withGPU with an injectable acquire (nil = the shared scheduler).
// It logs one line per slot with the kind, card and time waited.
func withGPUVia(ctx context.Context, acquire gpuAcquireFunc, jm *JobManager, jobID string, req GPURequest, fn func(idx int) error) error {
	if acquire == nil {
		acquire = acquireGPUForJob
	}
	start := time.Now()
	idx, release, err := acquire(ctx, jm, jobID, req)
	if err != nil {
		return fmt.Errorf("waiting for a GPU (%s): %w", req.Kind, err)
	}
	defer release()
	if idx >= 0 {
		log.Printf("gpu-scheduler: slot kind=%s card=%d waited=%s job=%s",
			req.Kind, idx, time.Since(start).Round(time.Millisecond), jobID)
	}
	return fn(idx)
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

// startReprobeTicker makes sure one goroutine broadcasts every s.reprobe while
// any waiter is parked, so parked waiters re-probe free memory that came back
// without a release (an Ollama model unloading after keep_alive 0, a crashed
// subprocess). It stops itself when the queue is empty. Callers hold s.mu.
func (s *GPUScheduler) startReprobeTicker() {
	if s.reprobe <= 0 || s.ticking {
		return
	}
	s.ticking = true
	go func() {
		for {
			time.Sleep(s.reprobe)
			s.mu.Lock()
			parked := false
			for w := range s.waiters {
				if w.parked {
					parked = true
					break
				}
			}
			if !parked {
				s.ticking = false
				s.mu.Unlock()
				return
			}
			s.cond.Broadcast()
			s.mu.Unlock()
		}
	}()
}

// agingWake is how long until w ages, or 0 when there is nothing to schedule
// (aging disabled or already aged). Callers must hold s.mu.
func (s *GPUScheduler) agingWake(w *gpuWaiter) time.Duration {
	if s.agingAfter <= 0 || w.aged || w.background() {
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
