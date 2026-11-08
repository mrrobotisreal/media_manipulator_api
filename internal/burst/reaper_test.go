package burst

import (
	"context"
	"testing"
	"time"
)

func reaperRig(live func() []Run) (*Reaper, *fakeProvider, *fakeStore, *fakePrefixes, *fakeClock) {
	cfg := testCfg()
	clk, st := newClock(t0), newFakeStore()
	prov, pre := &fakeProvider{}, &fakePrefixes{}
	return &Reaper{Cfg: cfg, Provider: prov, Store: st, Prefixes: pre, Live: live, Now: clk.Now,
		Ledger: NewLedger(25, 250, st, clk.Now)}, prov, st, pre, clk
}

func TestReaper_OrphanOldAndPrefixes(t *testing.T) {
	liveRun := Run{ID: "live-run", InstanceID: "i-live", State: StateRunning}
	rp, prov, st, pre, clk := reaperRig(func() []Run { return []Run{liveRun} })
	prov.tagged = []Instance{
		{ID: "i-live", RunID: "live-run", LaunchTime: clk.Now().Add(-10 * time.Minute)},
		{ID: "i-orphan", RunID: "dead-run", LaunchTime: clk.Now().Add(-20 * time.Minute), Type: "g6.xlarge"},
	}
	lt := clk.Now().Add(-20 * time.Minute)
	st.runs["dead-run"] = Run{ID: "dead-run", InstanceID: "i-orphan", InstanceType: "g6.xlarge", Kind: "demucs", State: StateRunning, OfferedAt: lt, LaunchedAt: &lt}
	pre.ids = []string{"live-run", "dead-run", "unknown-run"}

	rep := rp.Pass(context.Background(), false, false)
	if len(rep.Terminated) != 1 || rep.Terminated[0] != "i-orphan" {
		t.Fatalf("only the orphan is terminated: %+v", rep)
	}
	if got := st.get("dead-run"); got.State != StateOrphaned || got.TerminatedAt == nil || got.CostMicros <= 0 {
		t.Fatalf("orphan row: %+v", got)
	}
	if len(pre.deleted) != 2 || pre.deleted[0] != "dead-run" || pre.deleted[1] != "unknown-run" {
		t.Fatalf("prefixes deleted = %v (live run's must stay)", pre.deleted)
	}
}

func TestReaper_OldInstanceTerminatedEvenIfLive(t *testing.T) {
	liveRun := Run{ID: "r", InstanceID: "i-old", State: StateRunning}
	rp, prov, _, _, clk := reaperRig(func() []Run { return []Run{liveRun} })
	prov.tagged = []Instance{{ID: "i-old", RunID: "r", LaunchTime: clk.Now().Add(-136 * time.Minute)}} // > 120 + 15
	rep := rp.Pass(context.Background(), false, false)
	if len(rep.Terminated) != 1 || rep.Reasons["i-old"] != "older than max lifetime" {
		t.Fatalf("%+v", rep)
	}
	prov.tagged = []Instance{{ID: "i-ok", RunID: "r", LaunchTime: clk.Now().Add(-130 * time.Minute)}}
	prov.terminated = nil
	liveRun.InstanceID = "i-ok"
	if rep := rp.Pass(context.Background(), false, false); len(rep.Terminated) != 0 {
		t.Fatalf("130 min is inside 135: %+v", rep)
	}
}

func TestReaper_BootSettlesPreviousProcessRows(t *testing.T) {
	rp, prov, st, pre, clk := reaperRig(nil)
	lt := clk.Now().Add(-30 * time.Minute)
	st.runs["prev"] = Run{ID: "prev", InstanceID: "i-prev", InstanceType: "g7.2xlarge", Kind: "demucs", State: StateRunning, OfferedAt: lt, LaunchedAt: &lt}
	st.runs["prev-offered"] = Run{ID: "prev-offered", State: StateOffered, OfferedAt: lt}
	prov.tagged = []Instance{{ID: "i-prev", RunID: "prev", Type: "g7.2xlarge", LaunchTime: lt}}
	pre.ids = []string{"prev"}
	rep := rp.Pass(context.Background(), true, false)
	for _, id := range []string{"prev", "prev-offered"} {
		got := st.get(id)
		if got.State != StateOrphaned || got.TerminatedAt == nil {
			t.Fatalf("%s: %+v", id, got)
		}
	}
	// 30 min of g7.2xlarge at 2.52/h = 1.26 USD plus EBS.
	if c := st.get("prev").CostMicros; c < 1_260_000 || c > 1_400_000 {
		t.Fatalf("best-known cost = %d", c)
	}
	if st.get("prev-offered").CostMicros != 0 {
		t.Fatal("a never-launched row costs nothing")
	}
	if len(rep.Terminated) != 1 || len(pre.deleted) != 1 {
		t.Fatalf("%+v %v", rep, pre.deleted)
	}
}

func TestReaper_DryRunChangesNothing(t *testing.T) {
	rp, prov, st, pre, clk := reaperRig(nil)
	lt := clk.Now().Add(-20 * time.Minute)
	st.runs["x"] = Run{ID: "x", InstanceID: "i-x", State: StateRunning, OfferedAt: lt, LaunchedAt: &lt}
	prov.tagged = []Instance{{ID: "i-x", RunID: "x", LaunchTime: lt}}
	pre.ids = []string{"x"}
	rep := rp.Pass(context.Background(), false, true)
	if len(rep.Terminated) != 1 || len(prov.terminated) != 0 || len(pre.deleted) != 0 || st.get("x").State != StateRunning {
		t.Fatalf("dry run must report only: %+v", rep)
	}
}

func TestReaper_LaunchTestGraceWindow(t *testing.T) {
	rp, prov, st, _, clk := reaperRig(nil)
	st.runs["lt"] = Run{ID: "lt", InstanceID: "i-lt", Kind: LaunchTestKind, State: StateBooting, OfferedAt: clk.Now().Add(-2 * time.Minute)}
	prov.tagged = []Instance{{ID: "i-lt", RunID: "lt", LaunchTime: clk.Now().Add(-2 * time.Minute)}}
	if rep := rp.Pass(context.Background(), false, false); len(rep.Terminated) != 0 {
		t.Fatalf("a fresh launch-test from another process is spared: %+v", rep)
	}
	clk.Advance(20 * time.Minute)
	if rep := rp.Pass(context.Background(), false, false); len(rep.Terminated) != 1 {
		t.Fatal("past the window it is reaped")
	}
}

func TestReaper_ProviderErrorIsReportedNotFatal(t *testing.T) {
	rp, prov, _, _, _ := reaperRig(nil)
	prov.listErr = ErrDenied
	if rep := rp.Pass(context.Background(), false, false); len(rep.Errors) == 0 {
		t.Fatal("error expected in report")
	}
}
