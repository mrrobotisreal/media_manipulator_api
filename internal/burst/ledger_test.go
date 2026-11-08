package burst

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

const usd = int64(1_000_000)

func newTestLedger(clk *fakeClock, st *fakeStore) *Ledger {
	return NewLedger(25, 250, st, clk.Now)
}

func TestLedger_DailyCapBoundary(t *testing.T) {
	clk, st := newClock(t0), newFakeStore()
	l := newTestLedger(clk, st)
	ctx := context.Background()
	if err := l.Reserve(ctx, "a", 25*usd); err != nil { // exactly the cap is allowed
		t.Fatalf("exact cap: %v", err)
	}
	if err := l.Reserve(ctx, "b", 1); !errors.Is(err, ErrCapDaily) {
		t.Fatalf("one micro over: %v", err)
	}
	l.Settle("a", 10*usd)
	if err := l.Reserve(ctx, "b", 15*usd); err != nil {
		t.Fatalf("after settle to 10: %v", err)
	}
	if err := l.Reserve(ctx, "c", 1); !errors.Is(err, ErrCapDaily) {
		t.Fatal("settled + reserved must count")
	}
}

func TestLedger_MonthlyCapBoundary(t *testing.T) {
	clk, st := newClock(t0), newFakeStore()
	// 245 USD already settled earlier this month, on previous days.
	st.runs["old"] = Run{ID: "old", OfferedAt: t0.AddDate(0, 0, -3), CostMicros: 245 * usd}
	l := newTestLedger(clk, st)
	ctx := context.Background()
	if err := l.Reserve(ctx, "a", 5*usd); err != nil {
		t.Fatalf("to exactly 250: %v", err)
	}
	if err := l.Reserve(ctx, "b", 1); !errors.Is(err, ErrCapMonthly) {
		t.Fatalf("want monthly: %v", err)
	}
}

func TestLedger_UTCDayRollover(t *testing.T) {
	clk, st := newClock(time.Date(2026, 11, 8, 23, 59, 0, 0, time.UTC)), newFakeStore()
	l := newTestLedger(clk, st)
	ctx := context.Background()
	if err := l.Reserve(ctx, "a", 25*usd); err != nil {
		t.Fatal(err)
	}
	l.Settle("a", 25*usd)
	st.runs["a"] = Run{ID: "a", OfferedAt: clk.Now(), CostMicros: 25 * usd}
	if err := l.Reserve(ctx, "b", usd); !errors.Is(err, ErrCapDaily) {
		t.Fatalf("same day: %v", err)
	}
	clk.Advance(2 * time.Minute) // 00:01 UTC next day
	if err := l.Reserve(ctx, "c", 25*usd); err != nil {
		t.Fatalf("new day must reset the daily sum: %v", err)
	}
}

func TestLedger_MonthRollover(t *testing.T) {
	clk, st := newClock(time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)), newFakeStore()
	st.runs["x"] = Run{ID: "x", OfferedAt: clk.Now().Add(-time.Hour), CostMicros: 250 * usd}
	l := newTestLedger(clk, st)
	ctx := context.Background()
	if err := l.Reserve(ctx, "a", usd); err == nil {
		t.Fatal("month is full")
	}
	clk.Advance(2 * time.Minute) // December 1st
	if err := l.Reserve(ctx, "b", 20*usd); err != nil {
		t.Fatalf("new month resets: %v", err)
	}
}

func TestLedger_ReleaseOnLostRace(t *testing.T) {
	clk, st := newClock(t0), newFakeStore()
	l := newTestLedger(clk, st)
	ctx := context.Background()
	if err := l.Reserve(ctx, "a", 20*usd); err != nil {
		t.Fatal(err)
	}
	if err := l.Reserve(ctx, "b", 10*usd); !errors.Is(err, ErrCapDaily) {
		t.Fatal("reservation must hold")
	}
	l.Release("a") // lost race before RunInstances: nothing billed
	if err := l.Reserve(ctx, "b", 10*usd); err != nil {
		t.Fatalf("released reservation must free the cap: %v", err)
	}
	if sp := l.Spent(ctx); sp.DayMicros != 10*usd {
		t.Fatalf("spent = %d", sp.DayMicros)
	}
}

func TestLedger_SettleIsIdempotent(t *testing.T) {
	clk, st := newClock(t0), newFakeStore()
	l := newTestLedger(clk, st)
	ctx := context.Background()
	_ = l.Reserve(ctx, "a", 5*usd)
	l.Settle("a", 2*usd)
	l.Settle("a", 2*usd)
	if sp := l.Spent(ctx); sp.DayMicros != 2*usd {
		t.Fatalf("spent = %d, want 2 USD", sp.DayMicros)
	}
}

func TestLedger_RefreshEvery30s(t *testing.T) {
	clk, st := newClock(t0), newFakeStore()
	l := newTestLedger(clk, st)
	ctx := context.Background()
	l.Spent(ctx)
	r1 := st.reads
	l.Spent(ctx)
	if st.reads != r1 {
		t.Fatal("must not re-read within 30 s")
	}
	clk.Advance(31 * time.Second)
	l.Spent(ctx)
	if st.reads == r1 {
		t.Fatal("must re-read after 30 s")
	}
	// A failed read keeps the in-memory numbers.
	_ = l.Reserve(ctx, "a", usd)
	st.fail = true
	clk.Advance(31 * time.Second)
	if sp := l.Spent(ctx); sp.DayMicros != usd {
		t.Fatalf("failed read must keep local state: %d", sp.DayMicros)
	}
}

// Concurrent reserves never overshoot the cap (-race).
func TestLedger_ConcurrentReserves(t *testing.T) {
	clk, st := newClock(t0), newFakeStore()
	l := newTestLedger(clk, st)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := l.Reserve(context.Background(), string(rune('A'+i%26))+string(rune('a'+i/26)), usd); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if granted != 25 {
		t.Fatalf("granted %d reservations of 1 USD under a 25 USD cap", granted)
	}
}

func TestCostFormula(t *testing.T) {
	r := Rates{PriceUSDPerHour: 3.6, EBSUSDPerGBMonth: 0, EgressUSDPerGB: 0.09, VolumeInitUSD: 0.5}
	// 10 s bills as 60 s: 60/3600*3.6 = 0.06
	if got := CostMicros(r, t0, t0.Add(10*time.Second), 0, 0, false); got != 60_000 {
		t.Fatalf("60 s minimum: %d", got)
	}
	// 1 h + 1 GiB egress + volume init.
	if got := CostMicros(r, t0, t0.Add(time.Hour), 0, 1<<30, true); got != 3_600_000+90_000+500_000 {
		t.Fatalf("full formula: %d", got)
	}
	// EBS: 100 GiB for a 30-day month at 0.08 = 8 USD.
	r2 := Rates{EBSUSDPerGBMonth: 0.08}
	if got := CostMicros(r2, t0, t0.Add(30*24*time.Hour), 100, 0, false); got != 8_000_000 {
		t.Fatalf("ebs: %d", got)
	}
	if ReserveMicros(Rates{PriceUSDPerHour: 3.6}, 3*time.Minute, 5*time.Minute, 2*time.Minute, 0) != USDToMicros(12*60.0/3600*3.6*1.25) {
		t.Fatal("reservation estimate")
	}
}

func TestStateTransitions(t *testing.T) {
	happy := []State{StateOffered, StateLaunching, StateBooting, StateReady, StateStagingIn, StateRunning, StateStagingOut, StateCompleted}
	for i := 0; i+1 < len(happy); i++ {
		if !CanTransition(happy[i], happy[i+1]) {
			t.Errorf("%s -> %s must be legal", happy[i], happy[i+1])
		}
	}
	if !CanTransition(StateOffered, StateCapRefused) || !CanTransition(StateBooting, StateBootTimeout) ||
		!CanTransition(StateReady, StateLostRace) || !CanTransition(StateRunning, StateWorkerLost) ||
		!CanTransition(StateRunning, StateToolFailed) || !CanTransition(StateBooting, StateAdopted) ||
		!CanTransition(StateLaunching, StateLaunchFailed) || !CanTransition(StateRunning, StateOrphaned) {
		t.Error("a side exit is missing")
	}
	// Illegal: skipping states, going backwards, leaving a terminal state.
	for _, bad := range [][2]State{{StateOffered, StateRunning}, {StateRunning, StateReady}, {StateCompleted, StateRunning},
		{StateOrphaned, StateCompleted}, {StateReady, StateCompleted}, {StateOffered, StateCompleted}} {
		if CanTransition(bad[0], bad[1]) {
			t.Errorf("%s -> %s must be illegal", bad[0], bad[1])
		}
	}
	r := &Run{ID: "r", State: StateOffered}
	err := r.Transition(StateRunning, t0, nil)
	if !errors.Is(err, ErrIllegalTransition) || r.State != StateOffered {
		t.Fatalf("illegal transition must error and not move: %v %s", err, r.State)
	}
	if err := r.Transition(StateLaunching, t0, nil); err != nil || r.LaunchedAt == nil {
		t.Fatal("launching must stamp launched_at")
	}
}
