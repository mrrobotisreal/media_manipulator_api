package services

import (
	"log"
	"sort"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
)

// GPU burst hook (MM127, design §4, ADR-0020). With no BurstOfferer installed
// none of this runs: the scheduler's parking loop is the pre-MM127 code.

// BurstEligibility is the request descriptor that makes a GPURequest eligible
// for a cloud GPU offer. nil on GPURequest.Burst means never. Only call sites
// that can run remotely set it (wave 1, MM127b).
type BurstEligibility struct {
	// Workload is the burst profile key (BURST_WORKLOADS value).
	Workload string
	JobID    string
	Tier     string
	// OwnerIsOperator is true when the job's owner is an operator-tier account
	// (BURST_SCOPE=operator admits only these).
	OwnerIsOperator bool
}

// WaiterView is a value copy of one parked waiter, handed to the offerer.
type WaiterView struct {
	ID       uint64
	Kind     string
	Priority int
	// Rank is the effective priority (an aged waiter ranks above Premium).
	Rank     int
	Aged     bool
	Enqueued time.Time
	// Cards are the waiter's candidate cards, ascending.
	Cards []int
	// VRAMRequiredMiB includes the safety margin.
	VRAMRequiredMiB int64
	Burst           BurstEligibility
	// CardsFull: every candidate card has inUse >= maxPerGPU or FreeMiB < required
	// at the last probe.
	CardsFull bool
	// AheadOnCards counts queued, non-impossible waiters that share a card with
	// this one and order before it (rank, then aged arrival, then enqueue time).
	AheadOnCards int
	// PredictedWait is the design §4.2 estimate of the local wait.
	PredictedWait time.Duration
}

// BurstOfferer is the scheduler's hook. Both methods are called with the
// scheduler mutex held: they must not block, must not call anything slow and
// must not call back into the scheduler synchronously.
type BurstOfferer interface {
	// Consider is called for a parked waiter whose request carries a Burst
	// descriptor, each time it fails to get a local slot. It reports whether it
	// accepted (launched, asynchronously) an offer for the waiter.
	Consider(v WaiterView) (accepted bool)
	// Withdraw is called once per waiter that was Consider'ed, when it leaves the
	// queue: reason is one of the Withdraw* constants.
	Withdraw(waiterID uint64, reason string)
}

// Withdraw reasons.
const (
	WithdrawLocalSlot   = "local_slot"
	WithdrawGaveUp      = "gave_up"
	WithdrawRemoteTaken = "remote"
)

// Target is where an acquired GPU step runs. Remote == nil is a local card
// (Card is the index, -1 when the scheduler is disabled). A remote target has
// Card 0 (the instance's one GPU).
type Target struct {
	Card   int
	Remote *burst.Run
	// EnqueuedAt is the waiter's original enqueue time on the scheduler's clock,
	// kept so a fallback re-acquire preserves its age (GPURequest.EnqueuedAt).
	EnqueuedAt time.Time
}

// BurstSink is how a planner reports back to the scheduler from its own
// goroutines. Each call takes the scheduler mutex.
type BurstSink interface {
	// BurstPending marks a waiter as having a remote on the way (adoption). It
	// reports false when the waiter is gone or already has one.
	BurstPending(waiterID uint64) bool
	// BurstReady hands a ready run to the waiter and wakes it. It reports false
	// when the waiter is gone (the caller then owns the run again).
	BurstReady(waiterID uint64, run *burst.Run) bool
	// BurstFailed clears the waiter's pending remote (it stays parked, invisibly).
	BurstFailed(waiterID uint64)
	// WakeWaiters re-runs every parked waiter's loop (debounce timers).
	WakeWaiters()
}

// SetBurstOfferer installs the offerer (nil removes it). With nil the
// scheduler's code path is identical to before MM127.
func (s *GPUScheduler) SetBurstOfferer(o BurstOfferer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.burst = o
	if o != nil && s.pred == nil {
		s.pred = newHoldPredictor()
		s.running = map[uint64]runningSlot{}
	}
}

// observeReleaseLocked feeds a finished slot to the predictor. Callers hold s.mu.
func (s *GPUScheduler) observeReleaseLocked(slotID uint64) {
	if s.pred == nil {
		return
	}
	if r, ok := s.running[slotID]; ok {
		delete(s.running, slotID)
		s.pred.observe(r.kind, s.now().Sub(r.start))
	}
}

// waiterAhead reports whether other orders before w: higher rank, then (both
// aged or equal rank) the earlier arrival, ties broken by id so exactly one
// waiter is the head of the line.
func waiterAhead(other, w *gpuWaiter) bool {
	if other.rank() != w.rank() {
		return other.rank() > w.rank()
	}
	if !other.enqueued.Equal(w.enqueued) {
		return other.enqueued.Before(w.enqueued)
	}
	return other.id < w.id
}

// considerBurstLocked builds w's view and asks the offerer. Callers hold s.mu
// and have just found no local slot for w.
func (s *GPUScheduler) considerBurstLocked(w *gpuWaiter, req GPURequest, cands []int, byIdx map[int]GPUSnapshot, required int64, maxPerGPU int) {
	full := len(cands) > 0
	for _, idx := range cands {
		if s.inUse[idx] < maxPerGPU && byIdx[idx].FreeMiB >= required {
			full = false
			break
		}
	}
	var aheadKinds []string
	for other := range s.waiters {
		if other == w || other.impossible || !other.queued || !w.sharesCard(other) {
			continue
		}
		if waiterAhead(other, w) {
			aheadKinds = append(aheadKinds, other.kind)
		}
	}
	var running []runningSlot
	for _, r := range s.running {
		if w.cards[r.card] {
			running = append(running, r)
		}
	}
	cardList := append([]int(nil), cands...)
	sort.Ints(cardList)
	v := WaiterView{
		ID: w.id, Kind: w.kind, Priority: w.priority, Rank: w.rank(), Aged: w.aged, Enqueued: w.enqueued,
		Cards: cardList, VRAMRequiredMiB: required, Burst: *req.Burst,
		CardsFull: full, AheadOnCards: len(aheadKinds),
		PredictedWait: s.pred.predict(s.now(), running, aheadKinds, len(cands)),
	}
	w.offerer = s.burst
	if s.burst.Consider(v) {
		w.pendingRemote = true
		log.Printf("gpu-scheduler: %s offered a burst GPU (ahead=%d predicted=%s)", w.kind, v.AheadOnCards, v.PredictedWait.Round(time.Second))
	}
}

func (s *GPUScheduler) waiterByID(id uint64) *gpuWaiter {
	for w := range s.waiters {
		if w.id == id {
			return w
		}
	}
	return nil
}

// BurstPending implements BurstSink.
func (s *GPUScheduler) BurstPending(id uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waiterByID(id)
	if w == nil || w.pendingRemote || w.remote != nil {
		return false
	}
	w.pendingRemote = true
	return true
}

// BurstReady implements BurstSink.
func (s *GPUScheduler) BurstReady(id uint64, run *burst.Run) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waiterByID(id)
	if w == nil {
		return false
	}
	w.remote = run
	w.pendingRemote = false
	s.cond.Broadcast()
	return true
}

// BurstFailed implements BurstSink.
func (s *GPUScheduler) BurstFailed(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.waiterByID(id); w != nil {
		w.pendingRemote = false
		w.remote = nil
		s.cond.Broadcast()
	}
}

// WakeWaiters implements BurstSink.
func (s *GPUScheduler) WakeWaiters() {
	s.mu.Lock()
	s.cond.Broadcast()
	s.mu.Unlock()
}
