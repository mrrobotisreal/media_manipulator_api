package burst

import (
	"context"
	"errors"
	"testing"
	"time"
)

// labProfile is a LabOnly profile like internal/lab generates from a registry row.
func labProfile() Profile {
	return Profile{Key: "lab_test", Family: "image", MinGPUMemGiB: 16, Candidates: []string{"g6e.xlarge"},
		RootGiB: 200, ScratchGiB: 100, SeedHoldSeconds: 60, RemoteTimeout: 1800, LabOnly: true}
}

func labRig(t *testing.T) *rig {
	t.Helper()
	cfg := testCfg() // BURST_WORKLOADS does not list lab_test: LaunchDirect must not need it
	r := newRig(cfg)
	r.p.LabLedger = NewLedger(10, 100, PurposeSource{Src: purposeStore{r.st}, Purpose: PurposeLab}, r.clk.Now)
	return r
}

// purposeStore adapts the fake store to PurposeSpendSource.
type purposeStore struct{ *fakeStore }

func (p purposeStore) SettledMicrosSincePurpose(_ context.Context, since time.Time, purpose string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int64
	for _, r := range p.runs {
		if !r.OfferedAt.Before(since) && r.Purpose == purpose {
			n += r.CostMicros
		}
	}
	return n, nil
}

// MM138: LaunchDirect scope. It skips the trigger and BURST_WORKLOADS but keeps
// max instances, the profile check and the lab flag in both directions.
func TestLaunchDirect_Scope(t *testing.T) {
	ctx := context.Background()
	r := labRig(t)

	run, err := r.p.LaunchDirect(ctx, labProfile(), Offer{JobID: "lab1", Kind: "lab_test", Lab: true})
	if err != nil {
		t.Fatalf("lab launch must not need BURST_WORKLOADS: %v", err)
	}
	if run.Purpose != PurposeLab || run.State != StateBooting || r.st.get(run.ID).Purpose != PurposeLab {
		t.Fatalf("lab run row: %+v", run)
	}

	// A LabOnly profile without the lab flag is refused (planner half of test (e)).
	if _, err := r.p.LaunchDirect(ctx, labProfile(), Offer{JobID: "x", Kind: "lab_test"}); !errors.Is(err, ErrLabOnly) {
		t.Fatalf("lab profile without lab flag: %v", err)
	}
	// A product profile with the lab flag is refused too.
	prod, _ := Lookup("remove_background")
	if _, err := r.p.LaunchDirect(ctx, prod, Offer{JobID: "x", Kind: "remove_background", Lab: true}); !errors.Is(err, ErrLabOnly) {
		t.Fatalf("product profile with lab flag: %v", err)
	}
	// The trigger path (Launch) never launches a lab offer or a lab profile.
	if _, err := r.p.Launch(ctx, Offer{JobID: "x", Kind: "remove_background", Lab: true}); !errors.Is(err, ErrLabOnly) {
		t.Fatalf("Launch with lab flag: %v", err)
	}
	// Never rows stay never.
	if _, err := r.p.LaunchDirect(ctx, Profile{Key: "k", Never: true}, Offer{}); !errors.Is(err, ErrNotBurstable) {
		t.Fatalf("never row: %v", err)
	}
	// Max instances still applies.
	if _, err := r.p.LaunchDirect(ctx, labProfile(), Offer{JobID: "lab2", Kind: "lab_test", Lab: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.LaunchDirect(ctx, labProfile(), Offer{JobID: "lab3", Kind: "lab_test", Lab: true}); !errors.Is(err, ErrMaxInstances) {
		t.Fatalf("third instance: %v", err)
	}
	// A lab instance is never adopted.
	if _, err := r.p.Adopt(ctx, run, Offer{JobID: "other", Kind: "remove_background"}); !errors.Is(err, ErrLabOnly) {
		t.Fatalf("adopt lab run: %v", err)
	}
}

// MM138: the Lab sub-cap is enforced on top of the burst caps, and a refusal
// leaves no reservation in either ledger.
func TestLab_SubCapOnTopOfBurstCaps(t *testing.T) {
	ctx := context.Background()
	r := labRig(t)
	// g6e.xlarge $1.86/h, boot p50 300 s + hold 60 + 2 min + 2 min, x1.25 + EBS: well under $10.
	run, err := r.p.LaunchDirect(ctx, labProfile(), Offer{JobID: "a", Kind: "lab_test", Lab: true})
	if err != nil {
		t.Fatal(err)
	}
	labSpent := r.p.LabLedger.Spent(ctx)
	if labSpent.DayReservedMicros != run.ReservedMicros || r.p.Ledger.Spent(ctx).DayReservedMicros != run.ReservedMicros {
		t.Fatalf("a lab run reserves in both ledgers: lab=%+v burst=%+v", labSpent, r.p.Ledger.Spent(ctx))
	}

	// Tighten the Lab daily sub-cap below one more run: refused, burst ledger untouched.
	r.p.LabLedger = NewLedger(0.30, 100, PurposeSource{Src: purposeStore{r.st}, Purpose: PurposeLab}, r.clk.Now)
	before := r.p.Ledger.Spent(ctx).DayReservedMicros
	_, err = r.p.LaunchDirect(ctx, labProfile(), Offer{JobID: "b", Kind: "lab_test", Lab: true})
	if !errors.Is(err, ErrCapDaily) {
		t.Fatalf("lab sub-cap: %v", err)
	}
	if got := r.p.Ledger.Spent(ctx).DayReservedMicros; got != before {
		t.Fatalf("a refused lab run must not hold a burst reservation: %d -> %d", before, got)
	}
	if n := len(r.prov.launches); n != 1 {
		t.Fatalf("no instance for the refused run: %d launches", n)
	}

	// The burst cap applies to lab runs too (lab cap generous, burst cap tiny).
	r2 := labRig(t)
	r2.p.Ledger = NewLedger(0.30, 250, r2.st, r2.clk.Now)
	if _, err := r2.p.LaunchDirect(ctx, labProfile(), Offer{JobID: "c", Kind: "lab_test", Lab: true}); !errors.Is(err, ErrCapDaily) {
		t.Fatalf("burst cap on a lab run: %v", err)
	}
	if got := r2.p.LabLedger.Spent(ctx).DayReservedMicros; got != 0 {
		t.Fatalf("burst refusal must release the lab reservation: %d", got)
	}
}

// Lab spend sums only purpose='lab' rows; product bursts do not eat the Lab cap.
func TestLab_SubCapCountsOnlyLabRows(t *testing.T) {
	ctx := context.Background()
	r := labRig(t)
	t0n := r.clk.Now()
	r.st.runs["burst-row"] = Run{ID: "burst-row", Purpose: PurposeBurst, CostMicros: 9_000_000, OfferedAt: t0n, State: StateCompleted}
	r.st.runs["lab-row"] = Run{ID: "lab-row", Purpose: PurposeLab, CostMicros: 2_000_000, OfferedAt: t0n, State: StateCompleted}
	if got := r.p.LabLedger.Spent(ctx).DayMicros; got != 2_000_000 {
		t.Fatalf("lab spend = %d, want 2000000", got)
	}
	if got := r.p.Ledger.Spent(ctx).DayMicros; got != 11_000_000 {
		t.Fatalf("burst spend counts every row: %d", got)
	}
}

// MM138: ExtendSession reserves one more job on a warm instance; a refusal
// leaves the session alone, and Terminate releases the extras.
func TestExtendSession_ReservesAndReleases(t *testing.T) {
	ctx := context.Background()
	r := labRig(t)
	run, err := r.p.LaunchDirect(ctx, labProfile(), Offer{JobID: "a", Kind: "lab_test", Lab: true})
	if err != nil {
		t.Fatal(err)
	}
	base := r.p.LabLedger.Spent(ctx).DayReservedMicros
	if err := r.p.ExtendSession(ctx, run, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := r.p.LabLedger.Spent(ctx).DayReservedMicros; got <= base {
		t.Fatalf("extension must reserve: %d -> %d", base, got)
	}
	r.p.LabLedger = NewLedger(0.01, 100, PurposeSource{Src: purposeStore{r.st}, Purpose: PurposeLab}, r.clk.Now)
	if err := r.p.ExtendSession(ctx, run, 90*time.Second); !errors.Is(err, ErrCapDaily) {
		t.Fatalf("extension past the cap: %v", err)
	}
	for _, s := range []State{StateReady, StateStagingIn, StateRunning, StateStagingOut} {
		_ = r.p.Advance(ctx, run, s)
	}
	r.clk.Advance(10 * time.Minute)
	if err := r.p.Terminate(ctx, run, StateCompleted); err != nil {
		t.Fatal(err)
	}
	if got := r.p.Ledger.Spent(ctx).DayReservedMicros; got != 0 {
		t.Fatalf("terminate releases the extras: %d", got)
	}
	if got := r.st.get(run.ID); got.CostMicros <= 0 || got.SessionUntil != nil {
		t.Fatalf("settled row: %+v", got)
	}
}

// MM138 reaper session awareness: a parked session instance is live only while
// session_until is in the future. The max lifetime still applies.
func TestReaper_SessionAware(t *testing.T) {
	ctx := context.Background()
	until := func(clk *fakeClock, d time.Duration) *time.Time { t := clk.Now().Add(d); return &t }
	var live []Run
	rp, prov, _, _, clk := reaperRig(func() []Run { return live })
	prov.tagged = []Instance{{ID: "i-lab", RunID: "lab-run", LaunchTime: clk.Now().Add(-10 * time.Minute)}}

	// Parked, window still open: spared.
	live = []Run{{ID: "lab-run", InstanceID: "i-lab", State: StateStagingOut, Purpose: PurposeLab, SessionUntil: until(clk, 90*time.Second)}}
	if rep := rp.Pass(ctx, false, false); len(rep.Terminated) != 0 {
		t.Fatalf("an open session window is live: %+v", rep)
	}
	// Window passed (the runner's timer was lost): reaped as an orphan.
	clk.Advance(3 * time.Minute)
	rep := rp.Pass(ctx, false, false)
	if len(rep.Terminated) != 1 || rep.Reasons["i-lab"] != "orphan: no live run" {
		t.Fatalf("an expired session window must be reaped: %+v", rep)
	}
	// Busy again (session_until cleared): live.
	prov.terminated = nil
	live = []Run{{ID: "lab-run", InstanceID: "i-lab", State: StateRunning, Purpose: PurposeLab}}
	if rep := rp.Pass(ctx, false, false); len(rep.Terminated) != 0 {
		t.Fatalf("a busy session is live: %+v", rep)
	}
	// Even a busy or parked session dies at the hard max lifetime.
	prov.tagged = []Instance{{ID: "i-lab", RunID: "lab-run", LaunchTime: clk.Now().Add(-136 * time.Minute)}}
	live = []Run{{ID: "lab-run", InstanceID: "i-lab", State: StateStagingOut, Purpose: PurposeLab, SessionUntil: until(clk, time.Hour)}}
	if rep := rp.Pass(ctx, false, false); len(rep.Terminated) != 1 || rep.Reasons["i-lab"] != "older than max lifetime" {
		t.Fatalf("max lifetime still applies: %+v", rep)
	}
}

// A lab run priced by the reaper uses its lab profile's disk (LookupAny).
func TestLookupAny_LabKindOnlyThroughHook(t *testing.T) {
	if _, ok := Lookup("lab_test"); ok {
		t.Fatal("the product table must never resolve a lab kind")
	}
	old := LabProfileLookup
	defer func() { LabProfileLookup = old }()
	LabProfileLookup = func(k string) (Profile, bool) { return labProfile(), k == "lab_test" }
	if p, ok := LookupAny("lab_test"); !ok || !p.LabOnly {
		t.Fatal("LookupAny must resolve lab kinds through the hook")
	}
	if Burstable("lab_test") {
		t.Fatal("a lab kind is never Burstable for a product path")
	}
}
