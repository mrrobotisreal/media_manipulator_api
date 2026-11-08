package burst

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

type fakeProvider struct {
	mu         sync.Mutex
	calls      int
	launches   []LaunchRequest
	terminated [][]string
	tagged     []Instance
	launchErr  error
	listErr    error
}

func (f *fakeProvider) Launch(_ context.Context, req LaunchRequest) (Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.launches = append(f.launches, req)
	if f.launchErr != nil {
		return Instance{}, f.launchErr
	}
	return Instance{ID: "i-" + req.RunID[:4], Type: req.Candidates[0], AZ: "us-west-2a", AMI: req.AMIFor(req.Candidates[0]), State: "pending"}, nil
}
func (f *fakeProvider) Describe(context.Context, []string) ([]Instance, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return nil, nil
}
func (f *fakeProvider) ListTagged(context.Context) ([]Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.tagged, f.listErr
}
func (f *fakeProvider) Terminate(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.terminated = append(f.terminated, ids)
	return nil
}

type fakeMinter struct {
	key   string
	calls int
	err   error
}

func (m *fakeMinter) Mint(context.Context) (string, error) { m.calls++; return m.key, m.err }

type fakePrefixes struct {
	mu      sync.Mutex
	ids     []string
	deleted []string
}

func (f *fakePrefixes) DeleteRun(_ context.Context, id string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, id)
	f.mu.Unlock()
	return nil
}
func (f *fakePrefixes) ListRunIDs(context.Context) ([]string, error) { return f.ids, nil }

func testCfg() config.BurstConfig {
	return config.BurstConfig{
		Enabled: true, Workloads: []string{"remove_background", "demucs"}, Region: "us-west-2",
		SubnetIDs: []string{"subnet-a"}, SecurityGroupID: "sg-1", InstanceProfile: "prof",
		AMIs:             map[string]string{"image:x86_64": "ami-image", "audio:x86_64": "ami-audio"},
		PriceUSDPerHour:  map[string]float64{"g6.xlarge": 0.8, "g7.2xlarge": 2.52, "g6e.xlarge": 1.86},
		DailySpendCapUSD: 25, MonthlySpendCapUSD: 250, MaxInstances: 2, MaxInstanceMinutes: 120,
		BootTimeoutSeconds: 600, ReaperIntervalSecs: 60, EBSUSDPerGBMonth: 0.08,
	}
}

type rig struct {
	p    *Planner
	prov *fakeProvider
	st   *fakeStore
	clk  *fakeClock
	min  *fakeMinter
	pre  *fakePrefixes
	logb *bytes.Buffer
}

func newRig(cfg config.BurstConfig) *rig {
	clk, st := newClock(t0), newFakeStore()
	logb := &bytes.Buffer{}
	min := &fakeMinter{key: "tskey-auth-PLANNERSECRET"}
	pre := &fakePrefixes{}
	prov := &fakeProvider{}
	p := &Planner{
		Cfg: cfg, Provider: prov, Store: st, Ledger: NewLedger(cfg.DailySpendCapUSD, cfg.MonthlySpendCapUSD, st, clk.Now),
		Minter: min, Prefixes: pre, Now: clk.Now, Log: slog.New(slog.NewTextHandler(logb, nil)),
		UserData: func(r Run, pr Profile, key string) (string, error) { return "#!/bin/sh\n# " + key, nil },
	}
	return &rig{p, prov, st, clk, min, pre, logb}
}

func TestPlanner_LaunchThenTerminateCompletes(t *testing.T) {
	r := newRig(testCfg())
	ctx := context.Background()
	run, err := r.p.Launch(ctx, Offer{JobID: "j1", Kind: "remove_background", Tier: "premium", Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != StateBooting || run.InstanceID == "" || len(r.p.Live()) != 1 {
		t.Fatalf("after launch: %+v", run)
	}
	row := r.st.get(run.ID)
	if row.State != StateBooting || row.ReservedMicros == 0 || row.TerminatedAt != nil {
		t.Fatalf("row: %+v", row)
	}
	// The key reached user-data only.
	if !strings.Contains(r.prov.launches[0].UserData, "PLANNERSECRET") {
		t.Fatal("user data should carry the key")
	}
	if strings.Contains(r.logb.String(), "PLANNERSECRET") {
		t.Fatal("key leaked to log")
	}
	for _, s := range []State{StateReady, StateStagingIn, StateRunning, StateStagingOut} {
		if err := r.p.Advance(ctx, run, s); err != nil {
			t.Fatal(err)
		}
	}
	r.clk.Advance(10 * time.Minute)
	if err := r.p.Terminate(ctx, run, StateCompleted); err != nil {
		t.Fatal(err)
	}
	row = r.st.get(run.ID)
	if row.State != StateCompleted || row.TerminatedAt == nil || row.CostMicros <= 0 {
		t.Fatalf("settled row: %+v", row)
	}
	if len(r.prov.terminated) != 1 || len(r.pre.deleted) != 1 || len(r.p.Live()) != 0 {
		t.Fatal("terminate + prefix delete + untrack expected")
	}
	// Cost ~ 10 min at 0.8/h = 0.1333 + ebs; reservation is gone.
	if row.CostMicros < 130_000 || row.CostMicros > 200_000 {
		t.Fatalf("cost = %d", row.CostMicros)
	}
	if sp := r.p.Ledger.Spent(ctx); sp.DayMicros != row.CostMicros {
		t.Fatalf("ledger spent %d != cost %d", sp.DayMicros, row.CostMicros)
	}
}

func TestPlanner_Refusals(t *testing.T) {
	r := newRig(testCfg())
	ctx := context.Background()
	if _, err := r.p.Launch(ctx, Offer{Kind: "rembg[photo]"}); !errors.Is(err, ErrNotBurstable) {
		t.Fatalf("never kind: %v", err)
	}
	if _, err := r.p.Launch(ctx, Offer{Kind: "ai_upscale"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("workload not enabled: %v", err)
	}
	r.p.Launch(ctx, Offer{JobID: "a", Kind: "demucs"})
	r.p.Launch(ctx, Offer{JobID: "b", Kind: "demucs"})
	if _, err := r.p.Launch(ctx, Offer{JobID: "c", Kind: "demucs"}); !errors.Is(err, ErrMaxInstances) {
		t.Fatalf("max instances: %v", err)
	}
	if r.min.calls != 2 || r.prov.calls != 2 {
		t.Fatal("refusals must not mint or launch")
	}
}

func TestPlanner_CapRefusedIsLoggedAndNeverLaunched(t *testing.T) {
	cfg := testCfg()
	cfg.DailySpendCapUSD = 0.01
	r := newRig(cfg)
	_, err := r.p.Launch(context.Background(), Offer{JobID: "j", Kind: "remove_background"})
	if !errors.Is(err, ErrCapDaily) {
		t.Fatalf("err = %v", err)
	}
	if r.prov.calls != 0 || r.min.calls != 0 {
		t.Fatal("cap refusal must not mint a key or touch the provider")
	}
	var found bool
	for _, row := range r.st.runs {
		found = row.State == StateCapRefused && row.TerminatedAt != nil
	}
	if !found {
		t.Fatal("a cap_refused row with terminated_at expected")
	}
}

func TestPlanner_LaunchFailureReleasesReservation(t *testing.T) {
	r := newRig(testCfg())
	r.prov.launchErr = ErrNoCapacity
	ctx := context.Background()
	if _, err := r.p.Launch(ctx, Offer{JobID: "j", Kind: "remove_background"}); !errors.Is(err, ErrNoCapacity) {
		t.Fatal(err)
	}
	if sp := r.p.Ledger.Spent(ctx); sp.DayMicros != 0 {
		t.Fatalf("reservation must be released: %d", sp.DayMicros)
	}
	for _, row := range r.st.runs {
		if row.State != StateLaunchFailed || row.TerminatedAt == nil || row.ErrorType != "no_capacity" {
			t.Fatalf("row: %+v", row)
		}
	}
	if len(r.p.Live()) != 0 {
		t.Fatal("failed launch must not stay live")
	}
}

func TestPlanner_IllegalTransitionErrorsAndLogs(t *testing.T) {
	r := newRig(testCfg())
	ctx := context.Background()
	run, _ := r.p.Launch(ctx, Offer{JobID: "j", Kind: "remove_background"})
	if err := r.p.Advance(ctx, run, StateRunning); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("booting -> running must be illegal: %v", err)
	}
	if !strings.Contains(r.logb.String(), "illegal state transition") {
		t.Fatal("illegal transition must be logged")
	}
	// completed straight from booting is illegal too, and the instance is left alone.
	if err := r.p.Terminate(ctx, run, StateCompleted); !errors.Is(err, ErrIllegalTransition) {
		t.Fatal(err)
	}
	if len(r.prov.terminated) != 0 {
		t.Fatal("illegal outcome must not terminate")
	}
}

func TestPlanner_LostRaceTerminatesAndSettlesMinimum(t *testing.T) {
	r := newRig(testCfg())
	ctx := context.Background()
	run, _ := r.p.Launch(ctx, Offer{JobID: "j", Kind: "remove_background"})
	r.clk.Advance(5 * time.Second)
	if err := r.p.Terminate(ctx, run, StateLostRace); err != nil {
		t.Fatal(err)
	}
	row := r.st.get(run.ID)
	if row.State != StateLostRace || row.CostMicros < 13_000 { // 60 s minimum at 0.8/h = 13,333
		t.Fatalf("60 s minimum expected: %+v", row)
	}
}

func TestPlanner_AdoptHandsOverTheInstance(t *testing.T) {
	r := newRig(testCfg())
	ctx := context.Background()
	old, _ := r.p.Launch(ctx, Offer{JobID: "j1", Kind: "remove_background"})
	if _, err := r.p.Adopt(ctx, old, Offer{JobID: "j2", Kind: "demucs"}); err == nil {
		t.Fatal("a different family must not adopt")
	}
	r.clk.Advance(30 * time.Second)
	nr, err := r.p.Adopt(ctx, old, Offer{JobID: "j2", Kind: "ai_upscale"})
	if err != nil {
		t.Fatal(err)
	}
	if nr.AdoptedFrom != old.ID || nr.InstanceID != old.InstanceID || nr.JobID != "j2" || nr.State != StateBooting {
		t.Fatalf("new run: %+v", nr)
	}
	if o := r.st.get(old.ID); o.State != StateAdopted || o.TerminatedAt == nil {
		t.Fatalf("old row: %+v", o)
	}
	if len(r.prov.terminated) != 0 {
		t.Fatal("adoption must not terminate the instance")
	}
	live := r.p.Live()
	if len(live) != 1 || live[0].ID != nr.ID {
		t.Fatalf("live = %+v", live)
	}
}

// With burst unconfigured and no AWS creds: nothing is called.
func TestBurstDark_NoProviderOrMinterCalls(t *testing.T) {
	cfg := config.BurstConfig{}
	if cfg.Configured() || cfg.AWSConfigured() {
		t.Fatal("empty config must be dark")
	}
	prov, min := &fakeProvider{}, &fakeMinter{}
	// The wiring rule (cmd/api): no creds -> no reaper, no planner. Model it:
	if Start(context.Background(), cfg, prov, nil, nil, nil, nil, nil) != nil {
		t.Fatal("Start must return no reaper when creds are absent")
	}
	if prov.calls != 0 || min.calls != 0 {
		t.Fatal("zero provider or minter calls expected")
	}
}
