package services

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// The burst planner (MM127 decisions 3, 5, 6, 7): the scheduler's BurstOfferer.
// It decides whether a parked waiter earns a cloud GPU, launches one
// asynchronously, hands the ready run to the waiter, and settles the hedged race
// (adopt or terminate the loser). Lock order: scheduler mu, then planner mu; the
// planner never calls the sink or the launcher's slow methods while holding mu.

// BurstLauncher is the slice of burst.Planner the offerer drives.
type BurstLauncher interface {
	Launch(ctx context.Context, offer burst.Offer) (*burst.Run, error)
	Advance(ctx context.Context, run *burst.Run, to burst.State) error
	Terminate(ctx context.Context, run *burst.Run, outcome burst.State) error
	Adopt(ctx context.Context, old *burst.Run, newJob burst.Offer) (*burst.Run, error)
	Live() []burst.Run
}

// BurstReadiness waits until a launched instance's worker is ready (burst.Remote).
type BurstReadiness interface {
	WaitReady(ctx context.Context, run *burst.Run) error
}

// Decline reasons (one per `burst: offer declined` line, RUNBOOK §8.6).
const (
	DeclineDisabled      = "disabled"
	DeclineNotConfigured = "not_configured"
	DeclineNoProfile     = "no_profile"
	DeclineNever         = "never_burst"
	DeclineWorkloadOff   = "workload_off"
	DeclineScope         = "scope"
	DeclineBackground    = "background"
	DeclineCardsNotFull  = "cards_not_full"
	DeclineHeadOfLine    = "head_of_line"
	DeclineDebounce      = "debounce"
	DeclineBelowMinWait  = "below_min_wait"
	DeclineMaxInstances  = "max_instances"
	DeclineNoAMI         = "no_ami"
	DeclineContested     = "slot_contested"
	DeclineCooldown      = "cooldown"
	DeclineCapRefused    = "cap_refused"
	DeclineLaunchFailed  = "launch_failed"
	DeclineBootFailed    = "boot_failed"
)

const (
	burstLogEvery        = 30 * time.Second // per waiter, per reason
	burstFailureCooldown = 60 * time.Second // after a failed launch/boot, per waiter
	burstCandidateStale  = 10 * time.Second // a contender not re-considered for this long is gone
)

type burstPhase int

const (
	phaseNone      burstPhase = iota
	phaseLaunching            // launching or booting; the offer goroutine owns the run
	phaseReady                // delivered to the waiter; Withdraw owns the outcome
	phaseDone
)

type burstWaiter struct {
	id       uint64
	view     WaiterView
	since    time.Time // when "cards full and not head of line" first held
	holding  bool
	ok       bool // every condition but debounce and capacity held at the last Consider
	lastSeen time.Time
	timerSet bool
	cooldown time.Time
	lastLog  map[string]time.Time

	phase     burstPhase
	run       *burst.Run
	cancel    context.CancelFunc
	withdrawn string
}

// BurstPlanner implements BurstOfferer on top of the burst core.
type BurstPlanner struct {
	Cfg      config.BurstConfig
	Launcher BurstLauncher
	Ready    BurstReadiness
	// Sink is the scheduler (set by Install).
	Sink BurstSink
	// Now is the planner clock (default time.Now); Logf defaults to log.Printf.
	Now  func() time.Time
	Logf func(format string, args ...any)
	// Ctx is the base context of every launch and boot wait (default Background).
	Ctx context.Context

	mu       sync.Mutex
	waiters  map[uint64]*burstWaiter
	inflight int // launches between Consider accepting and Launch returning
}

// NewBurstPlanner builds a planner and installs it on s.
func NewBurstPlanner(cfg config.BurstConfig, l BurstLauncher, r BurstReadiness, s *GPUScheduler) *BurstPlanner {
	p := &BurstPlanner{Cfg: cfg, Launcher: l, Ready: r, Sink: s}
	if s != nil {
		s.SetBurstOfferer(p)
	}
	return p
}

func (p *BurstPlanner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *BurstPlanner) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (p *BurstPlanner) baseCtx() context.Context {
	if p.Ctx != nil {
		return context.WithoutCancel(p.Ctx)
	}
	return context.Background()
}

// decline logs the reason once per waiter per burstLogEvery. Callers hold p.mu.
func (p *BurstPlanner) decline(st *burstWaiter, v WaiterView, reason string) bool {
	now := p.now()
	if st.lastLog == nil {
		st.lastLog = map[string]time.Time{}
	}
	if last, ok := st.lastLog[reason]; !ok || now.Sub(last) >= burstLogEvery {
		st.lastLog[reason] = now
		p.logf("burst: offer declined kind=%s reason=%s predicted=%s", v.Kind, reason, v.PredictedWait.Round(time.Second))
	}
	return false
}

// better orders contenders: higher rank first, then earlier enqueue, then id.
func betterView(a, b WaiterView) bool {
	if a.Rank != b.Rank {
		return a.Rank > b.Rank
	}
	if !a.Enqueued.Equal(b.Enqueued) {
		return a.Enqueued.Before(b.Enqueued)
	}
	return a.ID < b.ID
}

// Consider implements BurstOfferer. It runs under the scheduler mutex: no I/O,
// no waiting. The launch is a goroutine.
func (p *BurstPlanner) Consider(v WaiterView) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiters == nil {
		p.waiters = map[uint64]*burstWaiter{}
	}
	now := p.now()
	st := p.waiters[v.ID]
	if st == nil {
		st = &burstWaiter{id: v.ID}
		p.waiters[v.ID] = st
	}
	st.view, st.lastSeen = v, now
	if st.phase != phaseNone {
		return false
	}

	// Static gates (design §4.1 items 1-2).
	if !p.Cfg.Enabled {
		return p.decline(st, v, DeclineDisabled)
	}
	if !p.Cfg.Configured() {
		return p.decline(st, v, DeclineNotConfigured)
	}
	prof, ok := burst.Lookup(v.Kind)
	if !ok {
		return p.decline(st, v, DeclineNoProfile)
	}
	if prof.Never {
		return p.decline(st, v, DeclineNever)
	}
	if !p.Cfg.WorkloadEnabled(prof.Key) {
		return p.decline(st, v, DeclineWorkloadOff)
	}
	if p.Cfg.Scope != "all" && !v.Burst.OwnerIsOperator {
		return p.decline(st, v, DeclineScope)
	}
	if v.Priority < GPUPriorityStandard {
		return p.decline(st, v, DeclineBackground)
	}

	// Dynamic conditions (items 3-4), then debounce (5).
	st.ok = false
	if !v.CardsFull {
		st.holding = false
		return p.decline(st, v, DeclineCardsNotFull)
	}
	if v.AheadOnCards < 1 {
		st.holding = false
		return p.decline(st, v, DeclineHeadOfLine)
	}
	if !st.holding {
		st.holding, st.since = true, now
	}
	if st.cooldown.After(now) {
		return p.decline(st, v, DeclineCooldown)
	}
	// Item 6 and the AMI check do not depend on time, so they register the
	// waiter as a contender before its debounce ends: a Premium job that is
	// still debouncing can still win a contested instance slot.
	if minWait := time.Duration(p.Cfg.MinPredictedWaitSeconds) * time.Second; v.PredictedWait < minWait {
		return p.decline(st, v, DeclineBelowMinWait)
	}
	cands := burst.CandidatesFor(prof, p.Cfg.InstanceTypes)
	if len(cands) == 0 || p.Cfg.AMIs[prof.Family+":"+burst.ArchOf(cands[0])] == "" {
		return p.decline(st, v, DeclineNoAMI)
	}
	st.ok = true

	debounce := time.Duration(p.Cfg.TriggerDebounceSeconds) * time.Second
	if rem := debounce - now.Sub(st.since); rem > 0 {
		if !st.timerSet && p.Sink != nil {
			st.timerSet = true
			sink := p.Sink
			time.AfterFunc(rem, func() {
				p.mu.Lock()
				st.timerSet = false
				p.mu.Unlock()
				sink.WakeWaiters()
			})
		}
		return p.decline(st, v, DeclineDebounce)
	}

	// Item 7: instances. Count what is live plus what is being launched.
	free := p.Cfg.MaxInstances - len(p.Launcher.Live()) - p.inflight
	if free < 1 {
		return p.decline(st, v, DeclineMaxInstances)
	}
	better := 0
	for id, o := range p.waiters {
		if id == v.ID || !o.ok || o.phase != phaseNone || now.Sub(o.lastSeen) > burstCandidateStale {
			continue
		}
		if betterView(o.view, v) {
			better++
		}
	}
	if better >= free {
		return p.decline(st, v, DeclineContested)
	}

	p.inflight++
	st.phase = phaseLaunching
	offer := burst.Offer{JobID: v.Burst.JobID, Kind: v.Kind, Tier: v.Burst.Tier, Priority: v.Priority,
		PredictedWaitMs: v.PredictedWait.Milliseconds()}
	p.logf("burst: offer accepted kind=%s job=%s predicted=%s ahead=%d rank=%d", v.Kind, offer.JobID, v.PredictedWait.Round(time.Second), v.AheadOnCards, v.Rank)
	go p.runOffer(st, offer)
	return true
}

// runOffer launches, waits for the worker, and delivers the run (decision 5).
func (p *BurstPlanner) runOffer(st *burstWaiter, offer burst.Offer) {
	run, err := p.Launcher.Launch(p.baseCtx(), offer)
	p.mu.Lock()
	p.inflight--
	p.mu.Unlock()
	if err != nil {
		reason := DeclineLaunchFailed
		if errors.Is(err, burst.ErrCapDaily) || errors.Is(err, burst.ErrCapMonthly) {
			reason = DeclineCapRefused
		} else if errors.Is(err, burst.ErrMaxInstances) {
			reason = DeclineMaxInstances
		}
		p.failOffer(st, reason, err)
		return
	}
	p.mu.Lock()
	st.run = run
	p.mu.Unlock()
	p.awaitReady(st, run)
}

// failOffer puts the waiter back, invisibly: pendingRemote cleared, no
// user-visible effect (design §6.4, decision 6). Nothing is left running.
func (p *BurstPlanner) failOffer(st *burstWaiter, reason string, err error) {
	p.logf("burst: offer failed kind=%s job=%s reason=%s error=%v", st.view.Kind, st.view.Burst.JobID, reason, err)
	p.mu.Lock()
	st.phase, st.run, st.cancel = phaseNone, nil, nil
	st.cooldown = p.now().Add(burstFailureCooldown)
	gone := p.waiters[st.id] != st
	p.mu.Unlock()
	if !gone && p.Sink != nil {
		p.Sink.BurstFailed(st.id)
	}
}

// awaitReady waits for the worker of run (booting or ready), moves the run to
// ready and hands it to the waiter. A withdrawal at any point ends in release.
func (p *BurstPlanner) awaitReady(st *burstWaiter, run *burst.Run) {
	timeout := time.Duration(p.Cfg.BootTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(p.baseCtx(), timeout)
	defer cancel()
	p.mu.Lock()
	if st.withdrawn != "" {
		reason := st.withdrawn
		st.phase = phaseDone
		p.mu.Unlock()
		p.release(run, reason)
		return
	}
	st.phase, st.run, st.cancel = phaseLaunching, run, cancel
	p.mu.Unlock()

	werr := p.Ready.WaitReady(ctx, run)
	if werr == nil && run.State == burst.StateBooting {
		werr = p.Launcher.Advance(p.baseCtx(), run, burst.StateReady)
	}

	p.mu.Lock()
	st.cancel = nil
	if st.withdrawn != "" { // the race was lost (or the caller left) while we booted
		reason := st.withdrawn
		st.phase = phaseDone
		p.mu.Unlock()
		p.release(run, reason)
		return
	}
	if werr != nil {
		p.mu.Unlock()
		outcome, reason := burst.StateWorkerLost, "worker_lost"
		if errors.Is(werr, context.DeadlineExceeded) {
			outcome, reason = burst.StateBootTimeout, "boot_timeout"
		}
		run.ErrorType = reason
		if terr := p.Launcher.Terminate(p.baseCtx(), run, outcome); terr != nil {
			p.logf("burst: terminate after boot failure: %v", terr)
		}
		p.failOffer(st, DeclineBootFailed, werr)
		return
	}
	st.phase = phaseReady
	p.mu.Unlock()
	p.logf("burst: remote ready run=%s kind=%s job=%s instance=%s", run.ID, run.Kind, run.JobID, run.InstanceType)
	if p.Sink != nil && p.Sink.BurstReady(st.id, run) {
		return
	}
	// The waiter was already gone: Withdraw has taken (or will take) the run.
	p.mu.Lock()
	mine := st.phase == phaseReady
	if mine {
		st.phase = phaseDone
	}
	p.mu.Unlock()
	if mine {
		p.release(run, WithdrawGaveUp)
	}
}

// Withdraw implements BurstOfferer. Called under the scheduler mutex: it only
// flips state and spawns goroutines.
func (p *BurstPlanner) Withdraw(id uint64, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.waiters[id]
	if st == nil {
		return
	}
	delete(p.waiters, id)
	if reason == WithdrawRemoteTaken {
		st.phase = phaseDone // the executor path owns the run now
		return
	}
	switch st.phase {
	case phaseLaunching:
		st.withdrawn = reason
		if st.cancel != nil {
			st.cancel()
		}
	case phaseReady:
		st.phase = phaseDone
		go p.release(st.run, reason)
	}
}

// release settles a run that nobody is going to use: hand it to the best other
// eligible waiter (adopted) when the reason is a local slot, else terminate it
// (lost_race). A cancelled or abandoned wait is terminated.
func (p *BurstPlanner) release(run *burst.Run, reason string) {
	if run == nil {
		return
	}
	if reason == WithdrawLocalSlot && p.tryAdopt(run) {
		return
	}
	run.ErrorType = reason
	p.logf("burst: lost race run=%s kind=%s job=%s reason=%s: terminating", run.ID, run.Kind, run.JobID, reason)
	if err := p.Launcher.Terminate(p.baseCtx(), run, burst.StateLostRace); err != nil {
		p.logf("burst: terminate lost run %s: %v", run.ID, err)
	}
}

// adoptable lists contenders that could use run's instance, best first.
func (p *BurstPlanner) adoptable(run *burst.Run) []*burstWaiter {
	now := p.now()
	mem := burst.GPUMemGiB(run.InstanceType)
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*burstWaiter
	for _, o := range p.waiters {
		if !o.ok || o.phase != phaseNone || o.run != nil || now.Sub(o.lastSeen) > burstCandidateStale {
			continue
		}
		prof, ok := burst.Lookup(o.view.Kind)
		if !ok || prof.Never || prof.Family != run.Family || mem == 0 || prof.MinGPUMemGiB > mem {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return betterView(out[i].view, out[j].view) })
	return out
}

func (p *BurstPlanner) tryAdopt(run *burst.Run) bool {
	if p.Sink == nil || (run.State != burst.StateBooting && run.State != burst.StateReady) {
		return false
	}
	for _, c := range p.adoptable(run) {
		if !p.Sink.BurstPending(c.id) {
			continue
		}
		offer := burst.Offer{JobID: c.view.Burst.JobID, Kind: c.view.Kind, Tier: c.view.Burst.Tier, Priority: c.view.Priority,
			PredictedWaitMs: c.view.PredictedWait.Milliseconds()}
		nr, err := p.Launcher.Adopt(p.baseCtx(), run, offer)
		if err != nil {
			p.logf("burst: adopt refused run=%s job=%s: %v", run.ID, offer.JobID, err)
			p.Sink.BurstFailed(c.id)
			return false
		}
		p.mu.Lock()
		c.phase, c.run = phaseLaunching, nr
		p.mu.Unlock()
		p.logf("burst: adopted run=%s by job=%s kind=%s (new run %s)", run.ID, offer.JobID, offer.Kind, nr.ID)
		go p.awaitReady(c, nr)
		return true
	}
	return false
}

// --- failure policy (decision 6, design §6.4) ---

// BurstFallback is what the 127b helper does after a remote attempt failed.
type BurstFallback struct {
	// Retry is whether to re-acquire a LOCAL target and re-run the step once.
	Retry bool
	// Reason is a short label for the log ("worker_lost", "tool_failed", ...).
	Reason string
	// Request is the re-acquire request: Burst cleared (it cannot burst twice)
	// and EnqueuedAt preserved (the job keeps its age and place in line).
	Request GPURequest
}

// PlanBurstFallback applies the §6.4 table to a failed remote run. req is the
// original request, target the remote target it acquired and runErr what the
// remote executor returned (nil = success, no fallback). already is true when
// the step already fell back once: a failure then fails the job as today.
//
//   - cancellation: no retry (the caller left);
//   - infra failure before running, after running, staging, an invalid spec,
//     and a tool exit != 0 remotely: one local re-acquire at the original age;
//   - cap refused / launch failure: never reach here (the waiter was put back
//     in the queue by the planner and no remote target exists).
func PlanBurstFallback(req GPURequest, target Target, runErr error, already bool) BurstFallback {
	fb := BurstFallback{Request: req}
	fb.Request.Burst = nil
	if !target.EnqueuedAt.IsZero() {
		fb.Request.EnqueuedAt = target.EnqueuedAt
	}
	switch {
	case runErr == nil:
		fb.Reason = "ok"
	case errors.Is(runErr, burst.ErrCancelled), errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		fb.Reason = "cancelled"
	case already:
		fb.Reason = "already_fell_back"
	case errors.Is(runErr, burst.ErrToolFailed):
		fb.Retry, fb.Reason = true, "tool_failed"
	case errors.Is(runErr, burst.ErrWorkerLost):
		fb.Retry, fb.Reason = true, "worker_lost"
	case errors.Is(runErr, burst.ErrStagingIn), errors.Is(runErr, burst.ErrStagingOut):
		fb.Retry, fb.Reason = true, "staging"
	default:
		fb.Retry, fb.Reason = true, "remote_error"
	}
	return fb
}
