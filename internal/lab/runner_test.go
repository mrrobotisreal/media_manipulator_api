package lab

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// fakeClock is a manual clock with AfterFunc timers (no real sleeping).
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &fakeTimer{at: c.t.Add(d), f: f}
	c.timers = append(c.timers, tm)
	return func() { c.mu.Lock(); tm.stopped = true; c.mu.Unlock() }
}

// Advance moves the clock and fires due, unstopped timers.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	var due []*fakeTimer
	for _, tm := range c.timers {
		if !tm.stopped && !tm.fired && !tm.at.After(c.t) {
			tm.fired = true
			due = append(due, tm)
		}
	}
	c.mu.Unlock()
	for _, tm := range due {
		tm.f()
	}
}

// fakeBurst plays the planner and the remote executor.
type fakeBurst struct {
	mu          sync.Mutex
	launches    int
	terminated  []string
	extends     int
	sessions    []*time.Time
	runs        int
	extendErr   error
	launchErr   error
	runErr      error
	waitErr     error
	nextID      int
	outFor      func(spec exec.Spec) // writes the output the way the worker's staging-out would
	keepSeen    []bool
	lastProfile burst.Profile
	lastOffer   burst.Offer
}

func (f *fakeBurst) LaunchDirect(_ context.Context, prof burst.Profile, offer burst.Offer) (*burst.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	f.launches++
	f.nextID++
	f.lastProfile, f.lastOffer = prof, offer
	return &burst.Run{ID: "run-" + string(rune('a'+f.nextID)), Purpose: burst.PurposeLab, State: burst.StateBooting, InstanceID: "i-1"}, nil
}
func (f *fakeBurst) Terminate(_ context.Context, run *burst.Run, outcome burst.State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, run.ID)
	run.State = outcome
	return nil
}
func (f *fakeBurst) ExtendSession(context.Context, *burst.Run, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.extendErr != nil {
		return f.extendErr
	}
	f.extends++
	return nil
}
func (f *fakeBurst) SetSession(_ context.Context, _ *burst.Run, until *time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, until)
}
func (f *fakeBurst) WaitReady(context.Context, *burst.Run) error { return f.waitErr }
func (f *fakeBurst) Run(_ context.Context, run *burst.Run, spec exec.Spec) error {
	f.mu.Lock()
	f.runs++
	f.keepSeen = append(f.keepSeen, run.Keep)
	err := f.runErr
	f.mu.Unlock()
	if err != nil {
		run.State = burst.StateToolFailed // the real Remote.Run terminates on failure
		f.mu.Lock()
		f.terminated = append(f.terminated, run.ID)
		f.mu.Unlock()
		return err
	}
	if f.outFor != nil {
		f.outFor(spec)
	}
	return nil
}
func (f *fakeBurst) terminatedCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.terminated) }

type rig struct {
	r     *Runner
	fb    *fakeBurst
	clk   *fakeClock
	jm    *services.JobManager
	out   string
	local int // local steps run
}

func writeResult(spec exec.Spec) {
	for _, o := range spec.Outputs {
		dir := o.Local
		if !o.Dir {
			dir = filepath.Dir(o.Local)
		}
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "restored.png"), []byte("png"), 0o600)
	}
}

func newRig(t *testing.T, allowLocal, withBurst bool) *rig {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		UploadDir: filepath.Join(dir, "uploads"), OutputDir: filepath.Join(dir, "outputs"), TempDir: filepath.Join(dir, "temp"),
		AICUDAGPU: 1, AIFaceRestorePython: "/opt/py", AIFaceRestoreScript: "/opt/restore_image_faces.py",
		AIRestoreModelsDir: "/opt/m", AIRestoreReposDir: "/opt/r",
		Lab:   config.LabConfig{AllowLocal: allowLocal, SessionIdleSeconds: 120},
		Burst: config.BurstConfig{BootTimeoutSeconds: 600, PriceUSDPerHour: map[string]float64{"g6e.xlarge": 1.86}},
	}
	clk := newFakeClock()
	rg := &rig{fb: &fakeBurst{}, clk: clk, jm: services.NewJobManager(), out: cfg.OutputDir}
	rg.fb.outFor = writeResult
	r := &Runner{Cfg: cfg, Jobs: rg.jm, Now: clk.Now, AfterFunc: clk.AfterFunc,
		Validator: exec.Validator{Roots: exec.RootsFrom(cfg.UploadDir, cfg.OutputDir, cfg.TempDir, "")},
		Acquire: func(context.Context, string, int64, time.Duration) (int, func(), error) {
			return 1, func() {}, nil
		},
		Local: exec.StepRunnerFunc(func(_ context.Context, spec *exec.Spec, _ exec.Cmd) error {
			rg.local++
			writeResult(*spec)
			return nil
		}),
	}
	if withBurst {
		r.Launch, r.Remote = rg.fb, rg.fb
		cfg.Burst.AMIs = map[string]string{"lab-image:x86_64": "ami-test"}
	}
	rg.r = r
	return rg
}

func (g *rig) job(t *testing.T, id string) RunRequest {
	t.Helper()
	g.jm.CreateJobWithID(id, models.OriginalFileInfo{Name: "face.png", Type: "image/png"}, map[string]interface{}{"mode": "lab_codeformer"})
	in := filepath.Join(g.r.Cfg.UploadDir, id, "image_face.png")
	_ = os.MkdirAll(filepath.Dir(in), 0o755)
	_ = os.WriteFile(in, []byte("x"), 0o600)
	m, _ := Get("codeformer")
	params, err := ValidateParams(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	return RunRequest{JobID: id, Model: m, Params: params, Inputs: map[string]string{"image": in}}
}

func (g *rig) status(id string) (models.JobStatus, string) {
	j, _ := g.jm.GetJob(id)
	return j.Status, j.Error
}

// A FitsHome model runs on the home GPU through the local path with no burst
// configured at all, and finishes with its result file.
func TestRunner_FitsHomeRunsLocallyWithoutBurst(t *testing.T) {
	g := newRig(t, true, false)
	g.r.Run(context.Background(), g.job(t, "j1"))
	if st, msg := g.status("j1"); st != models.StatusCompleted {
		t.Fatalf("status %s %q", st, msg)
	}
	if g.local != 1 {
		t.Fatalf("local steps = %d, want 1", g.local)
	}
	if _, err := os.Stat(filepath.Join(g.out, "j1", "restored.png")); err != nil {
		t.Fatal(err)
	}
	j, _ := g.jm.GetJob("j1")
	if j.ResultURL != "/api/download/j1" || j.ResultFileName == "" {
		t.Fatalf("result not recorded: %+v", j)
	}
}

// The home request never asks for Premium priority and pins the face-restore card.
func TestHomeRequest_StandardPriorityNeverPremium(t *testing.T) {
	req := HomeRequest(&config.Config{AICUDAGPU: 1}, 6000, 0)
	if req.Priority != services.GPUPriorityStandard || req.Priority >= services.GPUPriorityPremium {
		t.Fatalf("priority %d", req.Priority)
	}
	if req.ForceIndex == nil || *req.ForceIndex != 1 || req.VRAMRequiredMiB != 6000 || req.Burst != nil {
		t.Fatalf("%+v", req)
	}
}

// With burst not configured a model that needs a remote GPU fails closed with a
// clear message (here by turning LAB_ALLOW_LOCAL off for a FitsHome model).
func TestRunner_BurstModelFailsClosedWithoutBurst(t *testing.T) {
	g := newRig(t, false, false)
	g.r.Run(context.Background(), g.job(t, "j1"))
	st, msg := g.status("j1")
	if st != models.StatusFailed || msg != "This model needs a burst GPU and burst is not configured" {
		t.Fatalf("status %s %q", st, msg)
	}
	if g.local != 0 {
		t.Fatal("a burst-only model must not run locally")
	}
}

// Burst is on (for the voice models, say) but there is no worker image for this
// home model's family: the job must wait for the home card as long as it takes,
// not give up after 3 s and fail on a launch that has no image.
func TestRunner_HomeModelWaitsForTheCardWhenItsFamilyHasNoImage(t *testing.T) {
	g := newRig(t, true, true)
	g.r.Cfg.Burst.AMIs = map[string]string{"lab-voice:x86_64": "ami-voice"}
	var waits []time.Duration
	g.r.Acquire = func(_ context.Context, _ string, _ int64, maxWait time.Duration) (int, func(), error) {
		waits = append(waits, maxWait)
		return 1, func() {}, nil
	}
	g.r.Run(context.Background(), g.job(t, "j1"))
	if st, msg := g.status("j1"); st != models.StatusCompleted {
		t.Fatalf("status %s %q", st, msg)
	}
	if len(waits) != 1 || waits[0] != 0 {
		t.Fatalf("home wait = %v, want one unlimited wait (0)", waits)
	}
	if g.local != 1 || g.fb.runs != 0 {
		t.Fatalf("local=%d burst runs=%d, want 1 and 0", g.local, g.fb.runs)
	}
	// With an image for its family the short wait (then burst) is back.
	g2 := newRig(t, true, true)
	waits = nil
	g2.r.Acquire = g.r.Acquire
	g2.r.Run(context.Background(), g2.job(t, "j2"))
	if len(waits) != 1 || waits[0] != 3*time.Second {
		t.Fatalf("home wait with an image = %v, want 3s", waits)
	}
}

// Home card busy past the wait: with burst configured the job bursts instead.
func TestRunner_HomeBusyFallsBackToBurst(t *testing.T) {
	g := newRig(t, true, true)
	g.r.Acquire = func(context.Context, string, int64, time.Duration) (int, func(), error) {
		return 0, nil, services.ErrGPUMaxWait
	}
	g.r.Run(context.Background(), g.job(t, "j1"))
	if st, msg := g.status("j1"); st != models.StatusCompleted {
		t.Fatalf("%s %q", st, msg)
	}
	if g.fb.launches != 1 || g.local != 0 {
		t.Fatalf("launches=%d local=%d", g.fb.launches, g.local)
	}
}

// Session reuse: the second job of the same model inside the idle window reuses
// the warm instance (no second launch, one extension), then the idle timer
// terminates it. Fake clock, no sleeping.
func TestRunner_SessionReuseAndIdleTermination(t *testing.T) {
	g := newRig(t, false, true)
	ctx := context.Background()

	g.r.Run(ctx, g.job(t, "j1"))
	if st, msg := g.status("j1"); st != models.StatusCompleted {
		t.Fatalf("j1 %s %q", st, msg)
	}
	if g.fb.launches != 1 || g.fb.terminatedCount() != 0 {
		t.Fatalf("after job 1: launches=%d terminated=%d (the instance must be parked, not terminated)", g.fb.launches, g.fb.terminatedCount())
	}
	if got := g.r.Sessions(); len(got) != 1 || got[0].Model != "codeformer" || !got[0].Until.Equal(g.clk.Now().Add(120*time.Second)) {
		t.Fatalf("sessions: %+v", got)
	}
	if !g.fb.keepSeen[0] {
		t.Fatal("the remote run must be told to keep the instance")
	}

	g.clk.Advance(90 * time.Second) // inside the 120 s window
	g.r.Run(ctx, g.job(t, "j2"))
	if st, msg := g.status("j2"); st != models.StatusCompleted {
		t.Fatalf("j2 %s %q", st, msg)
	}
	if g.fb.launches != 1 || g.fb.extends != 1 || g.fb.runs != 2 {
		t.Fatalf("reuse: launches=%d extends=%d runs=%d", g.fb.launches, g.fb.extends, g.fb.runs)
	}
	if g.fb.terminatedCount() != 0 {
		t.Fatal("the first idle timer was re-armed by the reuse and must not terminate")
	}

	// 119 s after job 2 the window (re-armed at job 2's end) is still open.
	g.clk.Advance(119 * time.Second)
	if g.fb.terminatedCount() != 0 || len(g.r.Sessions()) != 1 {
		t.Fatalf("still inside the window: terminated=%d", g.fb.terminatedCount())
	}
	g.clk.Advance(2 * time.Second) // past it
	if g.fb.terminatedCount() != 1 || len(g.r.Sessions()) != 0 {
		t.Fatalf("idle expiry must terminate: terminated=%d sessions=%v", g.fb.terminatedCount(), g.r.Sessions())
	}

	g.r.Run(ctx, g.job(t, "j3")) // a fresh launch after the session ended
	if g.fb.launches != 2 {
		t.Fatalf("launches = %d, want 2", g.fb.launches)
	}
}

// Without a second job, the instance terminates at the idle timeout; the lab
// flag, profile and tier reach the planner.
func TestRunner_OneJobTerminatesAtIdle(t *testing.T) {
	g := newRig(t, false, true)
	g.r.Run(context.Background(), g.job(t, "j1"))
	if !g.fb.lastOffer.Lab || !g.fb.lastProfile.LabOnly || g.fb.lastProfile.Key != "lab_codeformer" || g.fb.lastOffer.JobID != "j1" {
		t.Fatalf("offer %+v profile %+v", g.fb.lastOffer, g.fb.lastProfile)
	}
	g.clk.Advance(119 * time.Second)
	if g.fb.terminatedCount() != 0 {
		t.Fatal("not yet")
	}
	g.clk.Advance(2 * time.Second)
	if g.fb.terminatedCount() != 1 {
		t.Fatal("the idle timer must terminate the instance")
	}
}

// A queued second job (arriving while the first runs) reuses the instance
// without waiting for the idle window.
func TestRunner_QueuedJobReusesInstance(t *testing.T) {
	g := newRig(t, false, true)
	release := make(chan struct{})
	started := make(chan struct{})
	g.fb.outFor = func(spec exec.Spec) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		writeResult(spec)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"j1", "j2"} {
		req := g.job(t, id)
		wg.Add(1)
		go func() { defer wg.Done(); g.r.Run(context.Background(), req) }()
		if id == "j1" {
			<-started
		}
	}
	close(release)
	wg.Wait()
	if g.fb.launches != 1 || g.fb.runs != 2 {
		t.Fatalf("launches=%d runs=%d", g.fb.launches, g.fb.runs)
	}
}

// A failed remote run ends the instance (Remote.Run terminates it) and drops
// the session, so the next job launches fresh.
func TestRunner_FailedRunDropsSession(t *testing.T) {
	g := newRig(t, false, true)
	g.fb.runErr = burst.ErrToolFailed
	g.r.Run(context.Background(), g.job(t, "j1"))
	if st, msg := g.status("j1"); st != models.StatusFailed || msg != "The model failed while processing the input" {
		t.Fatalf("%s %q", st, msg)
	}
	if len(g.r.Sessions()) != 0 || g.fb.terminatedCount() != 1 {
		t.Fatalf("sessions=%v terminated=%d", g.r.Sessions(), g.fb.terminatedCount())
	}
	g.fb.runErr = nil
	g.r.Run(context.Background(), g.job(t, "j2"))
	if g.fb.launches != 2 {
		t.Fatalf("launches=%d", g.fb.launches)
	}
}

// Cap refusals: at launch (cap error from LaunchDirect) and on a reuse (cap
// error from ExtendSession, which also ends the warm instance).
func TestRunner_CapRefusals(t *testing.T) {
	g := newRig(t, false, true)
	g.fb.launchErr = burst.ErrCapDaily
	g.r.Run(context.Background(), g.job(t, "j1"))
	if st, msg := g.status("j1"); st != models.StatusFailed || msg != "A Lab or burst spend cap would be crossed" {
		t.Fatalf("%s %q", st, msg)
	}

	g = newRig(t, false, true)
	g.r.Run(context.Background(), g.job(t, "a"))
	g.fb.extendErr = burst.ErrCapMonthly
	g.r.Run(context.Background(), g.job(t, "b"))
	if st, msg := g.status("b"); st != models.StatusFailed || !errors.Is(burst.ErrCapMonthly, burst.ErrCapMonthly) || msg != "A Lab or burst spend cap would be crossed" {
		t.Fatalf("%s %q", st, msg)
	}
	if g.fb.terminatedCount() != 1 || len(g.r.Sessions()) != 0 {
		t.Fatalf("a refused extension ends the warm instance: terminated=%d", g.fb.terminatedCount())
	}
}

// End session: terminates the idle instance now; nothing to end the second time.
func TestRunner_EndSession(t *testing.T) {
	g := newRig(t, false, true)
	g.r.Run(context.Background(), g.job(t, "j1"))
	if !g.r.EndSession(context.Background(), "codeformer") {
		t.Fatal("must end the session")
	}
	if g.fb.terminatedCount() != 1 || len(g.r.Sessions()) != 0 {
		t.Fatal("terminated now")
	}
	if g.r.EndSession(context.Background(), "codeformer") {
		t.Fatal("nothing left to end")
	}
	g.clk.Advance(10 * time.Minute) // the cancelled idle timer must not terminate again
	if g.fb.terminatedCount() != 1 {
		t.Fatal("double terminate")
	}
}

// A boot that never becomes ready terminates the instance and fails the job.
func TestRunner_BootFailureTerminates(t *testing.T) {
	g := newRig(t, false, true)
	g.fb.waitErr = errors.New("never ready")
	g.r.Run(context.Background(), g.job(t, "j1"))
	if st, _ := g.status("j1"); st != models.StatusFailed {
		t.Fatalf("%s", st)
	}
	if g.fb.terminatedCount() != 1 || len(g.r.Sessions()) != 0 {
		t.Fatal("must terminate the instance that never became ready")
	}
}
