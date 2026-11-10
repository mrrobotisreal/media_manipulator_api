package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

const offererSuiteEnv = "MM127_SUITE_WITH_OFFERER"

// panicOfferer is installed by makeTestScheduler when offererSuiteEnv is set.
type panicOfferer struct{}

func (panicOfferer) Consider(v WaiterView) bool {
	panic(fmt.Sprintf("offerer consulted for a request without a Burst descriptor: %+v", v))
}
func (panicOfferer) Withdraw(uint64, string) {}

// The whole MM94 scheduler suite, re-run in a child process with a non-nil
// offerer installed on every test scheduler and nil descriptors everywhere.
func TestMM94SchedulerSuite_WithOffererAndNilDescriptors(t *testing.T) {
	if os.Getenv(offererSuiteEnv) != "" {
		t.Skip("already the child run")
	}
	if testing.Short() {
		t.Skip("short")
	}
	cmd := exec.Command("go", "test", "-count=1", "-run",
		"TestGPUScheduler_|TestWithGPU_|TestAcquireTimed_|TestAcquireGPUForJob_|TestEstimateWhisperVRAMMiB|TestResolveWhisperGPURequest|TestGPUAgingFromEnv|TestOllama|TestParseOllama",
		"-v", ".")
	cmd.Env = append(os.Environ(), offererSuiteEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("MM94 suite failed with an offerer installed: %v\n%s", err, out)
	}
	if strings.Count(string(out), "--- PASS") < 20 {
		t.Fatalf("child ran too few tests:\n%s", out)
	}
}

// --- fakes -----------------------------------------------------------------

type fakeLauncher struct {
	mu         sync.Mutex
	launches   []burst.Offer
	live       map[string]*burst.Run
	terminated map[string]burst.State
	adopted    []string
	launchErr  error
	seq        int
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{live: map[string]*burst.Run{}, terminated: map[string]burst.State{}}
}

func (f *fakeLauncher) Launch(_ context.Context, o burst.Offer) (*burst.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, o)
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	f.seq++
	prof, _ := burst.Lookup(o.Kind)
	r := &burst.Run{ID: fmt.Sprintf("run-%d", f.seq), JobID: o.JobID, Kind: o.Kind, Workload: prof.Key, Family: prof.Family,
		State: burst.StateBooting, InstanceType: "g6.xlarge", InstanceID: fmt.Sprintf("i-%d", f.seq)}
	f.live[r.ID] = r
	return r, nil
}
func (f *fakeLauncher) Advance(_ context.Context, r *burst.Run, to burst.State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.State = to
	return nil
}
func (f *fakeLauncher) Terminate(_ context.Context, r *burst.Run, out burst.State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated[r.ID] = out
	delete(f.live, r.ID)
	return nil
}
func (f *fakeLauncher) Adopt(_ context.Context, old *burst.Run, o burst.Offer) (*burst.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	nr := *old
	nr.ID, nr.JobID, nr.Kind, nr.AdoptedFrom = fmt.Sprintf("run-%d", f.seq), o.JobID, o.Kind, old.ID
	f.terminated[old.ID] = burst.StateAdopted
	delete(f.live, old.ID)
	f.live[nr.ID] = &nr
	f.adopted = append(f.adopted, old.ID+">"+nr.ID)
	return &nr, nil
}
func (f *fakeLauncher) Live() []burst.Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]burst.Run, 0, len(f.live))
	for _, r := range f.live {
		out = append(out, *r)
	}
	return out
}
func (f *fakeLauncher) outcome(id string) (burst.State, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.terminated[id]
	return s, ok
}
func (f *fakeLauncher) launchCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.launches) }

// fakeReady blocks a fresh run until released; an adopted run (its instance is
// already up) is ready at once.
type fakeReady struct {
	gate chan struct{}
}

func newFakeReady() *fakeReady { return &fakeReady{gate: make(chan struct{})} }
func (f *fakeReady) open()     { close(f.gate) }
func (f *fakeReady) WaitReady(ctx context.Context, r *burst.Run) error {
	if r.AdoptedFrom != "" {
		return nil
	}
	select {
	case <-f.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type fakeSink struct {
	mu      sync.Mutex
	pending []uint64
	ready   map[uint64]*burst.Run
	failed  []uint64
}

func (f *fakeSink) BurstPending(id uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, id)
	return true
}
func (f *fakeSink) BurstReady(id uint64, r *burst.Run) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ready == nil {
		f.ready = map[uint64]*burst.Run{}
	}
	f.ready[id] = r
	return true
}
func (f *fakeSink) BurstFailed(id uint64) {
	f.mu.Lock()
	f.failed = append(f.failed, id)
	f.mu.Unlock()
}
func (f *fakeSink) WakeWaiters() {}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}
func (l *logSink) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
func (l *logSink) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func burstTestCfg() config.BurstConfig {
	return config.BurstConfig{
		Enabled: true, Scope: "all", Workloads: []string{"demucs", "remove_background", "restore-"},
		AccessKey: "ak", SecretKey: "sk", SubnetIDs: []string{"subnet-a"}, SecurityGroupID: "sg", InstanceProfile: "p",
		AMIs:              map[string]string{"audio:x86_64": "ami-a", "image:x86_64": "ami-i", "restore:x86_64": "ami-r"},
		TailscaleClientID: "c", TailscaleClientSecret: "s", TailscaleTailnet: "-",
		MaxInstances: 2, BootTimeoutSeconds: 600, MinPredictedWaitSeconds: 30, TriggerDebounceSeconds: 0,
		DailySpendCapUSD: 25, MonthlySpendCapUSD: 250,
	}
}

type clockT struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clockT) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clockT) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// unitPlanner builds a planner for direct Consider tests (no scheduler).
func unitPlanner(cfg config.BurstConfig) (*BurstPlanner, *fakeLauncher, *fakeReady, *logSink, *clockT) {
	l, r, ls := newFakeLauncher(), newFakeReady(), &logSink{}
	clk := &clockT{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	p := &BurstPlanner{Cfg: cfg, Launcher: l, Ready: r, Now: clk.Now, Logf: ls.logf}
	return p, l, r, ls, clk
}

func okView(id uint64) WaiterView {
	return WaiterView{ID: id, Kind: "demucs", Priority: GPUPriorityStandard, Rank: GPUPriorityStandard,
		Enqueued: time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Second),
		Cards:    []int{0}, Burst: BurstEligibility{Workload: "demucs", JobID: fmt.Sprintf("job-%d", id), Tier: "free"},
		CardsFull: true, AheadOnCards: 1, PredictedWait: 5 * time.Minute}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- predictor ---------------------------------------------------------------

func TestPredictor_EmptyQueueIsZero(t *testing.T) {
	p := newHoldPredictor()
	if got := p.predict(time.Now(), nil, nil, 2); got != 0 {
		t.Fatalf("got %s", got)
	}
}

func TestPredictor_OneTwentyMinuteRestoreRunning(t *testing.T) {
	p := newHoldPredictor()
	p.seed = func(string) time.Duration { return 20 * time.Minute }
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	got := p.predict(now, []runningSlot{{kind: "restore-swinir", card: 0, start: now}}, nil, 1)
	if got < 19*time.Minute || got > 21*time.Minute {
		t.Fatalf("got %s, want about 20m", got)
	}
	// Halfway through, about 10 minutes remain.
	got = p.predict(now.Add(10*time.Minute), []runningSlot{{kind: "restore-swinir", start: now}}, nil, 1)
	if got < 9*time.Minute || got > 11*time.Minute {
		t.Fatalf("halfway: got %s", got)
	}
	// An overrun slot keeps a 10% floor.
	got = p.predict(now.Add(time.Hour), []runningSlot{{kind: "restore-swinir", start: now}}, nil, 1)
	if got != 2*time.Minute {
		t.Fatalf("overrun: got %s, want 2m floor", got)
	}
}

func TestPredictor_TenRembgIsBelowMinWait(t *testing.T) {
	p := newHoldPredictor()
	p.seed = func(string) time.Duration { return 3 * time.Second }
	kinds := make([]string, 10)
	for i := range kinds {
		kinds[i] = "remove_background"
	}
	got := p.predict(time.Now(), nil, kinds, 1)
	if got != 30*time.Second {
		t.Fatalf("got %s, want 30s", got)
	}
	bp, l, _, ls, _ := unitPlanner(burstTestCfg())
	bp.Cfg.MinPredictedWaitSeconds = 120
	v := okView(2)
	v.Kind, v.PredictedWait = "remove_background", got
	if bp.Consider(v) || l.launchCount() != 0 || !ls.has("reason=below_min_wait predicted=30s") {
		t.Fatalf("a 30s queue must not burst: lines=%v", ls.lines)
	}
}

func TestPredictor_EWMAUpdatesOnRelease(t *testing.T) {
	p := newHoldPredictor()
	p.seed = func(string) time.Duration { return 100 * time.Second }
	p.observe("demucs", 200*time.Second)
	if got := p.estimate("demucs"); got != 130*time.Second { // 0.3*200 + 0.7*100
		t.Fatalf("got %s", got)
	}
	if got := newHoldPredictor().estimate("demucs"); got != 60*time.Second {
		t.Fatalf("profile seed: got %s", got)
	}
}

func TestPredictor_DividedByCards(t *testing.T) {
	p := newHoldPredictor()
	p.seed = func(string) time.Duration { return 60 * time.Second }
	now := time.Now()
	got := p.predict(now, []runningSlot{{kind: "a", start: now}, {kind: "a", start: now}}, []string{"a"}, 2)
	if got != 90*time.Second { // (60+60+60)/2
		t.Fatalf("got %s", got)
	}
}

// --- views: CardsFull and AheadOnCards ------------------------------------------

type recOfferer struct {
	mu    sync.Mutex
	views []WaiterView
	taken bool
	wd    map[uint64]string
}

func (r *recOfferer) Consider(v WaiterView) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.views = append(r.views, v)
	return r.taken
}
func (r *recOfferer) Withdraw(id uint64, reason string) {
	r.mu.Lock()
	if r.wd == nil {
		r.wd = map[uint64]string{}
	}
	r.wd[id] = reason
	r.mu.Unlock()
}
func (r *recOfferer) last(kind string) (WaiterView, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.views) - 1; i >= 0; i-- {
		if r.views[i].Kind == kind {
			return r.views[i], true
		}
	}
	return WaiterView{}, false
}

func twoCardScheduler() *GPUScheduler {
	return makeTestScheduler([]GPUSnapshot{
		{Index: 0, Name: "a", TotalMiB: 16000, FreeMiB: 12000},
		{Index: 1, Name: "b", TotalMiB: 16000, FreeMiB: 12000},
	}, nil)
}

func park(ctx context.Context, s *GPUScheduler, req GPURequest) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, rel, _, err := s.AcquireTarget(ctx, req)
		if err == nil {
			rel()
		}
	}()
	return done
}

func TestGPUScheduler_CardsFullIsPerCard(t *testing.T) {
	s := twoCardScheduler()
	rec := &recOfferer{}
	s.SetBurstOfferer(rec)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	one := 1
	_, rel, err := s.Acquire(ctx, GPURequest{Kind: "holder", VRAMRequiredMiB: 1000, ForceIndex: &one})
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	// GPU 1 is full, GPU 0 is idle. Pinned to 1: full. Unpinned: not full (card 0 is free,
	// so it simply takes it and is never even considered).
	b := &BurstEligibility{Workload: "demucs", JobID: "j"}
	d1 := park(ctx, s, GPURequest{Kind: "pinned", VRAMRequiredMiB: 1000, ForceIndex: &one, Burst: b})
	waitForWaiting(t, s, 1)
	v, ok := rec.last("pinned")
	if !ok || !v.CardsFull || len(v.Cards) != 1 || v.Cards[0] != 1 {
		t.Fatalf("pinned view = %+v ok=%v", v, ok)
	}
	// An unpinned request on a two-card box where card 1 is full but card 0 is free is
	// served by card 0 at once.
	idx, rel0, err := s.Acquire(ctx, GPURequest{Kind: "unpinned", VRAMRequiredMiB: 1000, Burst: nil})
	if err != nil || idx != 0 {
		t.Fatalf("unpinned should get idle card 0: idx=%d err=%v", idx, err)
	}
	// With card 0 now full too, an unpinned burst request sees every card full.
	d2 := park(ctx, s, GPURequest{Kind: "unpinned2", VRAMRequiredMiB: 1000, Burst: b})
	waitForWaiting(t, s, 2)
	v, _ = rec.last("unpinned2")
	if !v.CardsFull || len(v.Cards) != 2 {
		t.Fatalf("unpinned2 view = %+v", v)
	}
	// Free memory also counts: a request that cannot fit the free VRAM on an idle card is full.
	rel0()
	d3 := park(ctx, s, GPURequest{Kind: "big", VRAMRequiredMiB: 11800, ForceIndex: new(int), Burst: b})
	// card 0 has 12000 free but required is 11800+512 safety > 12000 -> cannot fit, though inUse is 0
	waitUntil(t, "big considered", func() bool { _, ok := rec.last("big"); return ok })
	v, _ = rec.last("big")
	if !v.CardsFull {
		t.Fatalf("a card without enough free VRAM must count as full: %+v", v)
	}
	cancel()
	<-d1
	<-d2
	<-d3
}

func TestGPUScheduler_AheadOnCardsCountsSharingHigherOrEqualOnly(t *testing.T) {
	s := twoCardScheduler()
	rec := &recOfferer{}
	s.SetBurstOfferer(rec)
	clk := newFakeClock()
	s.now = clk.Now
	s.agingAfter = 0
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	zero, one := 0, 1
	_, r0, _ := s.Acquire(ctx, GPURequest{Kind: "h0", VRAMRequiredMiB: 1000, ForceIndex: &zero})
	_, r1, _ := s.Acquire(ctx, GPURequest{Kind: "h1", VRAMRequiredMiB: 1000, ForceIndex: &one})
	defer r0()
	defer r1()
	b := &BurstEligibility{Workload: "demucs", JobID: "j"}
	var dones []chan struct{}
	spawn := func(kind string, prio int, idx *int, burst *BurstEligibility, want int) {
		dones = append(dones, park(ctx, s, GPURequest{Kind: kind, VRAMRequiredMiB: 1000, ForceIndex: idx, Priority: prio, Burst: burst}))
		waitForWaiting(t, s, want)
		clk.Advance(time.Second)
	}
	spawn("first0", 0, &zero, nil, 1) // card 0, standard
	spawn("on1", 0, &one, nil, 2)     // card 1: shares nothing with card 0 waiters
	spawn("prem0", 10, &zero, nil, 3) // card 0, premium: outranks later standard
	spawn("late0", 0, &zero, b, 4)    // card 0 standard, last in line
	// late0 shares card 0 with first0 (same rank, earlier) and prem0 (higher): 2 ahead; on1 is on card 1.
	waitUntil(t, "late0 considered", func() bool { v, ok := rec.last("late0"); return ok && v.AheadOnCards == 2 })
	// A premium waiter has nobody ahead of it on card 0 except aged/higher: none.
	spawn("prem0b", 10, &zero, b, 5)
	waitUntil(t, "prem0b considered", func() bool { v, ok := rec.last("prem0b"); return ok && v.AheadOnCards == 1 })
	// ...and an unpinned waiter that shares both cards counts both card-0 and card-1 waiters
	// ranking at or above it (first0, on1, prem0, prem0b, late0 are all standard-or-higher & earlier or higher).
	spawn("any", 0, nil, b, 6)
	waitUntil(t, "any considered", func() bool { v, ok := rec.last("any"); return ok && v.AheadOnCards == 5 })
	cancel()
	for _, d := range dones {
		<-d
	}
}

func TestGPUScheduler_HeadOfLineHasNoOneAhead(t *testing.T) {
	s := twoCardScheduler()
	rec := &recOfferer{}
	s.SetBurstOfferer(rec)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	zero := 0
	_, r0, _ := s.Acquire(ctx, GPURequest{Kind: "h0", VRAMRequiredMiB: 1000, ForceIndex: &zero})
	defer r0()
	d := park(ctx, s, GPURequest{Kind: "head", VRAMRequiredMiB: 1000, ForceIndex: &zero, Burst: &BurstEligibility{Workload: "demucs", JobID: "j"}})
	waitForWaiting(t, s, 1)
	v, ok := rec.last("head")
	if !ok || v.AheadOnCards != 0 || !v.CardsFull {
		t.Fatalf("head of line view = %+v", v)
	}
	cancel()
	<-d
}

// --- planner decisions ----------------------------------------------------------------

func TestPlanner_HeadOfLineIsNeverOffered(t *testing.T) {
	p, l, _, ls, _ := unitPlanner(burstTestCfg())
	v := okView(1)
	v.AheadOnCards = 0
	if p.Consider(v) || l.launchCount() != 0 || !ls.has("burst: offer declined kind=demucs reason=head_of_line") {
		t.Fatalf("head of line must wait locally; lines=%v", ls.lines)
	}
}

func TestPlanner_BackgroundIsNeverOffered(t *testing.T) {
	p, l, _, ls, _ := unitPlanner(burstTestCfg())
	v := okView(1)
	v.Priority, v.Rank = GPUPriorityBackground, GPUPriorityBackground
	if p.Consider(v) || l.launchCount() != 0 || !ls.has("reason=background") {
		t.Fatalf("background offered: %v", ls.lines)
	}
}

func TestPlanner_StaticGates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.BurstConfig, *WaiterView)
		reason string
	}{
		{"disabled", func(c *config.BurstConfig, _ *WaiterView) { c.Enabled = false }, "disabled"},
		{"not configured", func(c *config.BurstConfig, _ *WaiterView) { c.AMIs = nil }, "not_configured"},
		{"no profile", func(_ *config.BurstConfig, v *WaiterView) { v.Kind = "mystery" }, "no_profile"},
		{"never", func(_ *config.BurstConfig, v *WaiterView) { v.Kind = "face_privacy" }, "never_burst"},
		{"workload off", func(c *config.BurstConfig, _ *WaiterView) { c.Workloads = []string{"remove_background"} }, "workload_off"},
		{"scope operator", func(c *config.BurstConfig, _ *WaiterView) { c.Scope = "operator" }, "scope"},
		{"cards not full", func(_ *config.BurstConfig, v *WaiterView) { v.CardsFull = false }, "cards_not_full"},
		{"no ami", func(c *config.BurstConfig, _ *WaiterView) {
			delete(c.AMIs, "audio:x86_64")
			c.AMIs["image:x86_64"] = "x"
		}, "no_ami"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := burstTestCfg()
			v := okView(1)
			tc.mutate(&cfg, &v)
			p, l, _, ls, _ := unitPlanner(cfg)
			if p.Consider(v) || l.launchCount() != 0 || !ls.has("reason="+tc.reason) {
				t.Fatalf("want decline %s: %v", tc.reason, ls.lines)
			}
		})
	}
	// Operator scope admits an operator-owned job.
	cfg := burstTestCfg()
	cfg.Scope = "operator"
	p, _, r, _, _ := unitPlanner(cfg)
	defer r.open()
	v := okView(1)
	v.Burst.OwnerIsOperator = true
	if !p.Consider(v) {
		t.Fatal("operator job under operator scope must be offered")
	}
}

func TestPlanner_Debounce(t *testing.T) {
	cfg := burstTestCfg()
	cfg.TriggerDebounceSeconds = 3
	p, l, r, ls, clk := unitPlanner(cfg)
	defer r.open()
	v := okView(1)
	if p.Consider(v) || !ls.has("reason=debounce") {
		t.Fatalf("first sighting must debounce: %v", ls.lines)
	}
	clk.Add(2 * time.Second)
	if p.Consider(v) || l.launchCount() != 0 {
		t.Fatal("still inside the window")
	}
	// The condition breaks (the waiter became head of the line): the window restarts.
	head := v
	head.AheadOnCards = 0
	p.Consider(head)
	clk.Add(2 * time.Second)
	if p.Consider(v) || l.launchCount() != 0 {
		t.Fatal("a broken condition must restart the debounce window")
	}
	clk.Add(3 * time.Second)
	if !p.Consider(v) {
		t.Fatal("held for the full window: must be offered")
	}
	waitUntil(t, "launch", func() bool { return l.launchCount() == 1 })
}

func TestPlanner_BelowMinWaitDecline(t *testing.T) {
	cfg := burstTestCfg()
	cfg.MinPredictedWaitSeconds = 120
	p, l, _, ls, _ := unitPlanner(cfg)
	v := okView(1)
	v.PredictedWait = 37 * time.Second
	if p.Consider(v) || l.launchCount() != 0 {
		t.Fatal("must decline")
	}
	if !ls.has("burst: offer declined kind=demucs reason=below_min_wait predicted=37s") {
		t.Fatalf("log line: %v", ls.lines)
	}
	// MIN=0 is the literal rule.
	cfg.MinPredictedWaitSeconds = 0
	p2, _, r2, _, _ := unitPlanner(cfg)
	defer r2.open()
	if !p2.Consider(v) {
		t.Fatal("min wait 0 must offer the second job")
	}
}

func TestPlanner_DeclineLogIsRateLimitedPerWaiter(t *testing.T) {
	p, _, _, ls, clk := unitPlanner(burstTestCfg())
	v := okView(1)
	v.CardsFull = false
	for i := 0; i < 10; i++ {
		p.Consider(v)
	}
	if n := ls.count("reason=cards_not_full"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	clk.Add(31 * time.Second)
	p.Consider(v)
	if n := ls.count("reason=cards_not_full"); n != 2 {
		t.Fatalf("after the window: %d", n)
	}
	other := okView(2)
	other.CardsFull = false
	p.Consider(other)
	if n := ls.count("reason=cards_not_full"); n != 3 {
		t.Fatalf("a second waiter logs its own line: %d", n)
	}
}

func TestPlanner_CapRefusedDecline(t *testing.T) {
	p, l, _, ls, _ := unitPlanner(burstTestCfg())
	l.launchErr = burst.ErrCapDaily
	sink := &fakeSink{}
	p.Sink = sink
	if !p.Consider(okView(1)) {
		t.Fatal("the cap is checked by Reserve inside Launch, after the offer")
	}
	waitUntil(t, "failed", func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return len(sink.failed) == 1 })
	if !ls.has("reason=cap_refused") {
		t.Fatalf("log: %v", ls.lines)
	}
	if len(l.Live()) != 0 {
		t.Fatal("nothing may be left running")
	}
	// The waiter is put back and is not re-offered every pass.
	if p.Consider(okView(1)) || !ls.has("reason=cooldown") {
		t.Fatalf("expected cooldown: %v", ls.lines)
	}
}

func TestPlanner_MaxInstances(t *testing.T) {
	cfg := burstTestCfg()
	cfg.MaxInstances = 1
	p, l, r, ls, _ := unitPlanner(cfg)
	defer r.open()
	if !p.Consider(okView(1)) {
		t.Fatal("first must launch")
	}
	if p.Consider(okView(2)) || !ls.has("reason=max_instances") {
		t.Fatalf("second must be declined: %v", ls.lines)
	}
	waitUntil(t, "one launch", func() bool { return l.launchCount() == 1 })
	if l.launchCount() != 1 {
		t.Fatal("cap on instances exceeded")
	}
}

func TestPlanner_PremiumWinsAContestedSlot(t *testing.T) {
	cfg := burstTestCfg()
	cfg.MaxInstances = 1
	cfg.TriggerDebounceSeconds = 3
	p, l, r, ls, clk := unitPlanner(cfg)
	defer r.open()
	free, prem := okView(1), okView(2)
	prem.Priority, prem.Rank = GPUPriorityPremium, GPUPriorityPremium
	prem.Enqueued = free.Enqueued.Add(time.Hour) // later, but outranks it
	p.Consider(free)
	p.Consider(prem)
	clk.Add(4 * time.Second)
	// The free job's debounce ended first, but a better contender holds the only slot.
	if p.Consider(free) || !ls.has("reason=slot_contested") {
		t.Fatalf("free job must wait for the premium one: %v", ls.lines)
	}
	if !p.Consider(prem) {
		t.Fatal("premium must win the contested slot")
	}
	waitUntil(t, "launch", func() bool { return l.launchCount() == 1 })
	l.mu.Lock()
	got := l.launches[0]
	l.mu.Unlock()
	if got.JobID != "job-2" || got.Priority != GPUPriorityPremium {
		t.Fatalf("launched for %+v", got)
	}
}

func TestPlanner_ConsiderDoesNotBlock(t *testing.T) {
	p, l, r, _, _ := unitPlanner(burstTestCfg())
	defer r.open()
	l.launchErr = nil
	slow := make(chan struct{})
	p.Launcher = blockingLauncher{fakeLauncher: l, block: slow}
	defer close(slow)
	done := make(chan bool, 1)
	go func() { done <- p.Consider(okView(1)) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("expected accept")
		}
	case <-time.After(time.Second):
		t.Fatal("Consider blocked on a slow launch: it runs under the scheduler mutex")
	}
}

type blockingLauncher struct {
	*fakeLauncher
	block chan struct{}
}

func (b blockingLauncher) Launch(ctx context.Context, o burst.Offer) (*burst.Run, error) {
	<-b.block
	return b.fakeLauncher.Launch(ctx, o)
}

// --- hedged race through the real scheduler ---------------------------------------------

type raceRig struct {
	s     *GPUScheduler
	p     *BurstPlanner
	l     *fakeLauncher
	ready *fakeReady
	ls    *logSink
	ctx   context.Context
	stop  context.CancelFunc
}

func newRaceRig(t *testing.T, cards []GPUSnapshot, mutate func(*config.BurstConfig)) *raceRig {
	t.Helper()
	s := makeTestScheduler(cards, nil)
	s.reprobe = 50 * time.Millisecond
	cfg := burstTestCfg()
	if mutate != nil {
		mutate(&cfg)
	}
	l, r, ls := newFakeLauncher(), newFakeReady(), &logSink{}
	p := NewBurstPlanner(cfg, l, r, s)
	p.Logf = ls.logf
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return &raceRig{s: s, p: p, l: l, ready: r, ls: ls, ctx: ctx, stop: cancel}
}

func oneCard() []GPUSnapshot {
	return []GPUSnapshot{{Index: 0, Name: "5080", TotalMiB: 16000, FreeMiB: 12000}}
}

func (r *raceRig) b(job string) *BurstEligibility {
	return &BurstEligibility{Workload: "demucs", JobID: job, Tier: "free"}
}

type acquired struct {
	t    Target
	rel  func()
	wait GPUWait
	err  error
}

func (r *raceRig) acquireAsync(req GPURequest) chan acquired {
	ch := make(chan acquired, 1)
	go func() {
		t, rel, w, err := r.s.AcquireTarget(r.ctx, req)
		ch <- acquired{t, rel, w, err}
	}()
	return ch
}

func TestRace_LocalFirstLoserRunIsTerminated(t *testing.T) {
	// Deterministic version: A is cancelled so B is alone when the card frees.
	r := newRaceRig(t, oneCard(), nil)
	_, hold, _ := r.s.Acquire(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000})
	ctxA, cancelA := context.WithCancel(r.ctx)
	aDone := make(chan error, 1)
	go func() {
		_, _, _, err := r.s.AcquireTarget(ctxA, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000})
		aDone <- err
	}()
	waitForWaiting(t, r.s, 1)
	b := r.acquireAsync(GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, Burst: r.b("job-b")})
	waitUntil(t, "launch", func() bool { return r.l.launchCount() == 1 })
	cancelA()
	<-aDone
	hold()
	rb := <-b
	if rb.err != nil || rb.t.Remote != nil {
		t.Fatalf("B must take the local slot: %+v", rb)
	}
	defer rb.rel()
	waitUntil(t, "lost_race", func() bool { st, ok := r.l.outcome("run-1"); return ok && st == burst.StateLostRace })
	if !r.ls.has("burst: lost race run=run-1") {
		t.Fatalf("log: %v", r.ls.lines)
	}
	if len(r.l.Live()) != 0 {
		t.Fatal("instance left running")
	}
}

func TestRace_RemoteFirstReturnsTargetAndReleasesQueue(t *testing.T) {
	r := newRaceRig(t, oneCard(), nil)
	clk := newFakeClock()
	r.s.now = clk.Now
	jm := NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "b"}, nil)
	_, hold, _ := r.s.Acquire(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000})
	defer hold()
	ctxA, cancelA := context.WithCancel(r.ctx)
	defer cancelA()
	aDone := make(chan struct{})
	go func() { defer close(aDone); r.s.AcquireTarget(ctxA, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000}) }()
	waitForWaiting(t, r.s, 1)
	type res struct {
		t   Target
		rel func()
		err error
	}
	ch := make(chan res, 1)
	go func() {
		tg, rel, err := acquireTargetForJobOn(r.ctx, r.s, jm, job.ID, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, Burst: r.b(job.ID)})
		ch <- res{tg, rel, err}
	}()
	waitForWaiting(t, r.s, 2)
	waitUntil(t, "launch", func() bool { return r.l.launchCount() == 1 })
	clk.Advance(95 * time.Second)
	r.ready.open()
	rr := <-ch
	if rr.err != nil || rr.t.Remote == nil || rr.t.Remote.ID != "run-1" || rr.t.Card != 0 {
		t.Fatalf("want a remote target: %+v", rr)
	}
	rr.rel() // no-op for local accounting
	if snap := r.s.Snapshot(); snap.Waiting != 1 || snap.Running != 1 {
		t.Fatalf("remote waiter must leave the queue and take no local slot: %+v", snap)
	}
	got, _ := jm.GetJob(job.ID)
	if got.QueueWaitMs == nil || *got.QueueWaitMs < 95000 {
		t.Fatalf("queueWaitMs = %v, want enqueue -> remote ready (>= 95000)", got.QueueWaitMs)
	}
	if st, done := r.l.outcome("run-1"); done {
		t.Fatalf("the executor owns the run after hand-over, the planner must not terminate it (got %s)", st)
	}
	if !r.ls.has("burst: remote ready run=run-1") {
		t.Fatalf("log: %v", r.ls.lines)
	}
	cancelA()
	<-aDone
}

func TestRace_CtxCancelTerminatesPendingRun(t *testing.T) {
	r := newRaceRig(t, oneCard(), nil)
	_, hold, _ := r.s.Acquire(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000})
	defer hold()
	go func() {
		if _, rel, _, err := r.s.AcquireTarget(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000}); err == nil {
			rel()
		}
	}()
	waitForWaiting(t, r.s, 1)
	ctxB, cancelB := context.WithCancel(r.ctx)
	errc := make(chan error, 1)
	go func() {
		_, _, _, err := r.s.AcquireTarget(ctxB, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, Burst: r.b("job-b")})
		errc <- err
	}()
	waitUntil(t, "launch", func() bool { return r.l.launchCount() == 1 })
	cancelB()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	waitUntil(t, "terminated", func() bool { _, ok := r.l.outcome("run-1"); return ok })
	if len(r.l.Live()) != 0 {
		t.Fatal("pending run must be terminated when the caller gives up")
	}
}

func TestRace_MaxWaitTerminatesPendingRun(t *testing.T) {
	r := newRaceRig(t, oneCard(), nil)
	_, hold, _ := r.s.Acquire(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000})
	defer hold()
	go func() {
		if _, rel, _, err := r.s.AcquireTarget(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000}); err == nil {
			rel()
		}
	}()
	waitForWaiting(t, r.s, 1)
	errc := make(chan error, 1)
	go func() {
		_, _, _, err := r.s.AcquireTarget(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, Burst: r.b("job-b"), MaxWait: 500 * time.Millisecond})
		errc <- err
	}()
	if err := <-errc; !errors.Is(err, ErrGPUMaxWait) {
		t.Fatalf("err = %v", err)
	}
	waitUntil(t, "terminated", func() bool { st, ok := r.l.outcome("run-1"); return ok && st == burst.StateLostRace })
}

func TestRace_LocalFirstAdoptedByAnotherEligibleWaiter(t *testing.T) {
	two := []GPUSnapshot{
		{Index: 0, Name: "a", TotalMiB: 16000, FreeMiB: 12000},
		{Index: 1, Name: "b", TotalMiB: 16000, FreeMiB: 12000},
	}
	r := newRaceRig(t, two, func(c *config.BurstConfig) { c.MaxInstances = 1 })
	zero, one := 0, 1
	_, h0, _ := r.s.Acquire(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, ForceIndex: &zero})
	_, h1, _ := r.s.Acquire(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, ForceIndex: &one})
	defer h1()
	ctxA0, cancelA0 := context.WithCancel(r.ctx)
	a0 := make(chan struct{})
	go func() {
		defer close(a0)
		r.s.AcquireTarget(ctxA0, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, ForceIndex: &zero})
	}()
	go r.s.AcquireTarget(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, ForceIndex: &one})
	waitForWaiting(t, r.s, 2)
	b := r.acquireAsync(GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, ForceIndex: &zero, Burst: r.b("job-b")})
	waitUntil(t, "B launch", func() bool { return r.l.launchCount() == 1 })
	c := r.acquireAsync(GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, ForceIndex: &one, Burst: r.b("job-c")})
	waitUntil(t, "C declined for the instance cap", func() bool { return r.ls.has("kind=demucs reason=max_instances") })
	cancelA0()
	<-a0
	h0() // card 0 frees: B (alone on it) takes the local slot
	rb := <-b
	if rb.err != nil || rb.t.Remote != nil {
		t.Fatalf("B local: %+v", rb)
	}
	defer rb.rel()
	rc := <-c
	if rc.err != nil || rc.t.Remote == nil {
		t.Fatalf("C must receive the adopted instance: %+v", rc)
	}
	if rc.t.Remote.AdoptedFrom != "run-1" || rc.t.Remote.JobID != "job-c" {
		t.Fatalf("adopted run: %+v", rc.t.Remote)
	}
	if st, _ := r.l.outcome("run-1"); st != burst.StateAdopted {
		t.Fatalf("old run state = %q", st)
	}
	if r.l.launchCount() != 1 {
		t.Fatal("adoption must not launch a second instance")
	}
	r.ready.open()
}

func TestPlanner_AdoptRequiresSameFamilyAndFittingGPU(t *testing.T) {
	cfg := burstTestCfg()
	cfg.MaxInstances = 1
	p, l, r, _, _ := unitPlanner(cfg)
	defer r.open()
	sink := &fakeSink{}
	p.Sink = sink
	if !p.Consider(okView(1)) { // demucs, audio family
		t.Fatal("B must launch")
	}
	waitUntil(t, "launch", func() bool { return l.launchCount() == 1 })
	img := okView(2)
	img.Kind = "remove_background" // image family: cannot adopt an audio instance
	p.Consider(img)
	rs := okView(3)
	rs.Kind = "restore-swinir" // restore family needs 16 GB and a different image
	p.Consider(rs)
	p.Withdraw(1, WithdrawLocalSlot)
	waitUntil(t, "lost race", func() bool { st, ok := l.outcome("run-1"); return ok && st == burst.StateLostRace })
	if len(sink.pending) != 0 {
		t.Fatalf("no other-family waiter may adopt: %v", sink.pending)
	}
}

func TestRace_NoSecondBurstOnFallbackReacquire(t *testing.T) {
	r := newRaceRig(t, oneCard(), nil)
	rec := &recOfferer{}
	r.s.SetBurstOfferer(rec) // replaces the planner: counts every Consider
	_, hold, _ := r.s.Acquire(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000})
	enq := time.Now().Add(-time.Minute)
	orig := GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, Burst: r.b("job-x")}
	fb := PlanBurstFallback(orig, Target{Card: 0, Remote: &burst.Run{ID: "r"}, EnqueuedAt: enq}, burst.ErrWorkerLost, false)
	if !fb.Retry || fb.Request.Burst != nil || !fb.Request.EnqueuedAt.Equal(enq) {
		t.Fatalf("fallback request: %+v", fb)
	}
	if orig.Burst == nil {
		t.Fatal("the original request must not be mutated")
	}
	ch := r.acquireAsync(fb.Request)
	waitForWaiting(t, r.s, 1)
	time.Sleep(150 * time.Millisecond) // several re-probe passes
	hold()
	got := <-ch
	if got.err != nil || got.t.Remote != nil {
		t.Fatalf("fallback must be local: %+v", got)
	}
	defer got.rel()
	rec.mu.Lock()
	n := len(rec.views)
	rec.mu.Unlock()
	if n != 0 || r.l.launchCount() != 0 {
		t.Fatalf("a re-acquire with Burst cleared must never be offered again (considered %d, launched %d)", n, r.l.launchCount())
	}
	if !got.t.EnqueuedAt.Equal(enq) {
		t.Fatalf("EnqueuedAt not preserved: %v vs %v", got.t.EnqueuedAt, enq)
	}
}

func TestRace_FallbackKeepsAgeInTheQueue(t *testing.T) {
	// A re-acquire with EnqueuedAt 15 minutes old is aged at once (10 minute threshold)
	// and so outranks a Premium waiter, instead of going to the back of the line.
	s := makeTestScheduler(oneCard(), nil)
	clk := newFakeClock()
	s.now = clk.Now
	s.agingAfter = 10 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, hold, _ := s.Acquire(ctx, GPURequest{Kind: "x", VRAMRequiredMiB: 5000})
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	run := func(name string, req GPURequest) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rel, _, err := s.AcquireTarget(ctx, req)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			rel()
		}()
	}
	run("premium", GPURequest{Kind: "p", VRAMRequiredMiB: 5000, Priority: GPUPriorityPremium})
	waitForWaiting(t, s, 1)
	clk.Advance(15 * time.Minute)
	run("fallback", GPURequest{Kind: "f", VRAMRequiredMiB: 5000, EnqueuedAt: clk.Now().Add(-16 * time.Minute)})
	waitForWaiting(t, s, 2)
	hold()
	wg.Wait()
	if len(order) != 2 || order[0] != "fallback" {
		t.Fatalf("order = %v, the re-acquire must keep its age", order)
	}
}

func TestPlanBurstFallback_Table(t *testing.T) {
	enq := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	orig := GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, Priority: GPUPriorityPremium, Burst: &BurstEligibility{JobID: "j"}}
	tg := Target{Remote: &burst.Run{ID: "r"}, EnqueuedAt: enq}
	cases := []struct {
		name    string
		err     error
		already bool
		retry   bool
		reason  string
	}{
		{"success", nil, false, false, "ok"},
		{"tool failed once", &burst.ToolFailure{ExitCode: 2}, false, true, "tool_failed"},
		{"tool failed after fallback", &burst.ToolFailure{ExitCode: 2}, true, false, "already_fell_back"},
		{"worker lost", fmt.Errorf("%w: gone", burst.ErrWorkerLost), false, true, "worker_lost"},
		{"staging in", fmt.Errorf("%w: x", burst.ErrStagingIn), false, true, "staging"},
		{"staging out", fmt.Errorf("%w: x", burst.ErrStagingOut), false, true, "staging"},
		{"invalid spec", errors.New("exec: spec refused"), false, true, "remote_error"},
		{"cancelled", burst.ErrCancelled, false, false, "cancelled"},
		{"ctx cancelled", context.Canceled, false, false, "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := PlanBurstFallback(orig, tg, tc.err, tc.already)
			if fb.Retry != tc.retry || fb.Reason != tc.reason {
				t.Fatalf("got retry=%v reason=%s", fb.Retry, fb.Reason)
			}
			if fb.Request.Burst != nil {
				t.Fatal("re-acquire must clear Burst: it cannot burst twice")
			}
			if !fb.Request.EnqueuedAt.Equal(enq) {
				t.Fatalf("EnqueuedAt not kept: %v", fb.Request.EnqueuedAt)
			}
			if fb.Request.Kind != "demucs" || fb.Request.Priority != GPUPriorityPremium || fb.Request.VRAMRequiredMiB != 5000 {
				t.Fatalf("request fields lost: %+v", fb.Request)
			}
		})
	}
}

func TestRace_InfraFailureBeforeRunningPutsWaiterBackInvisibly(t *testing.T) {
	r := newRaceRig(t, oneCard(), func(c *config.BurstConfig) { c.BootTimeoutSeconds = 600 })
	_, hold, _ := r.s.Acquire(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000})
	go func() {
		if _, rel, _, err := r.s.AcquireTarget(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000}); err == nil {
			rel()
		}
	}()
	waitForWaiting(t, r.s, 1)
	failing := &failingReady{}
	r.p.Ready = failing
	b := r.acquireAsync(GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, Burst: r.b("job-b")})
	waitUntil(t, "terminated worker_lost", func() bool { st, ok := r.l.outcome("run-1"); return ok && st == burst.StateWorkerLost })
	waitUntil(t, "put back", func() bool { return r.ls.has("burst: offer failed") })
	// The waiter is still queued, has no pending remote, and no instance is left.
	if r.s.Snapshot().Waiting != 2 || len(r.l.Live()) != 0 {
		t.Fatalf("snapshot %+v live %d", r.s.Snapshot(), len(r.l.Live()))
	}
	hold()
	got := <-b
	if got.err != nil {
		t.Fatal(got.err)
	}
	got.rel()
}

type failingReady struct{}

func (failingReady) WaitReady(context.Context, *burst.Run) error {
	return errors.New("worker never joined")
}

func TestRace_TwoWaitersLaunchAtMostOneInstanceEach(t *testing.T) {
	// Repeated passes (the 50 ms re-probe) never launch twice for one waiter.
	r := newRaceRig(t, oneCard(), nil)
	_, hold, _ := r.s.Acquire(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000})
	go func() {
		if _, rel, _, err := r.s.AcquireTarget(r.ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000}); err == nil {
			rel()
		}
	}()
	waitForWaiting(t, r.s, 1)
	b := r.acquireAsync(GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, Burst: r.b("job-b")})
	waitUntil(t, "launch", func() bool { return r.l.launchCount() == 1 })
	time.Sleep(300 * time.Millisecond)
	if r.l.launchCount() != 1 {
		t.Fatalf("launched %d instances for one waiter", r.l.launchCount())
	}
	r.ready.open()
	got := <-b
	if got.t.Remote == nil {
		t.Fatalf("expected remote: %+v", got)
	}
	hold()
}

func TestAcquireTimed_IgnoresBurstDescriptor(t *testing.T) {
	s := makeTestScheduler(oneCard(), nil)
	rec := &recOfferer{}
	s.SetBurstOfferer(rec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, hold, _ := s.Acquire(ctx, GPURequest{Kind: "x", VRAMRequiredMiB: 5000})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, rel, _, err := s.AcquireTimed(ctx, GPURequest{Kind: "demucs", VRAMRequiredMiB: 5000, Burst: &BurstEligibility{JobID: "j"}})
		if err == nil {
			rel()
		}
	}()
	waitForWaiting(t, s, 1)
	time.Sleep(100 * time.Millisecond)
	hold()
	<-done
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.views) != 0 {
		t.Fatal("AcquireTimed is local-only: the offerer must not be consulted")
	}
}

func TestAcquireTarget_DisabledSchedulerIsLocalNoOp(t *testing.T) {
	s := newGPUScheduler(func(context.Context) ([]GPUSnapshot, error) { return nil, nil })
	tg, rel, w, err := s.AcquireTarget(context.Background(), GPURequest{Kind: "x", Burst: &BurstEligibility{}})
	if err != nil || tg.Card != -1 || tg.Remote != nil || w.Queued {
		t.Fatalf("%+v %+v %v", tg, w, err)
	}
	rel()
}
