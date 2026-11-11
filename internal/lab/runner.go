package lab

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// Errors the handler maps to HTTP statuses.
var (
	// ErrUnavailable: the model needs a remote GPU and burst is not configured
	// (fail closed), or the Lab has no way to run it.
	ErrUnavailable = errors.New("lab: this model needs a burst GPU and burst is not configured")
	// ErrCap: a spend cap (Lab sub-cap or burst cap) would be crossed.
	ErrCap = errors.New("lab: a spend cap would be crossed")
)

// Launcher is the slice of burst.Planner the runner drives.
type Launcher interface {
	LaunchDirect(ctx context.Context, prof burst.Profile, offer burst.Offer) (*burst.Run, error)
	Terminate(ctx context.Context, run *burst.Run, outcome burst.State) error
	ExtendSession(ctx context.Context, run *burst.Run, hold time.Duration) error
	SetSession(ctx context.Context, run *burst.Run, until *time.Time)
}

// RemoteExec is the slice of burst.Remote the runner drives.
type RemoteExec interface {
	WaitReady(ctx context.Context, run *burst.Run) error
	Run(ctx context.Context, run *burst.Run, spec exec.Spec) error
}

// RunRequest is one validated Lab job.
type RunRequest struct {
	JobID     string
	RequestID string
	Model     LabModel
	Params    map[string]any
	// Inputs maps an input name to its local file.
	Inputs map[string]string
}

// SessionInfo is one warm Lab session, for GET /api/lab/spend.
type SessionInfo struct {
	Model string    `json:"model"`
	Until time.Time `json:"until"`
}

type session struct {
	run   *burst.Run
	busy  bool
	until time.Time
	gen   int
	stop  func()
}

type modelState struct {
	runMu   sync.Mutex // one burst job at a time per model: queued jobs wait here
	session *session
}

// Runner runs Lab jobs: at home through the GPU scheduler for FitsHome models,
// otherwise on a directly launched burst instance that stays warm between runs
// of the same model for LAB_SESSION_IDLE_SECONDS.
type Runner struct {
	Cfg  *config.Config
	Jobs *services.JobManager
	// Launch and Remote are nil unless burst is Configured(); a model that
	// needs them then fails closed with ErrUnavailable.
	Launch Launcher
	Remote RemoteExec
	// Forget drops a run's HMAC token (UserDataBuilder.Forget).
	Forget func(runID string)
	// Local runs a Spec on this host (exec.AuditRunner over the cmdaudit runner).
	Local     exec.StepRunner
	Validator exec.Validator
	// Acquire takes a home GPU card (default: the shared scheduler). maxWait > 0
	// bounds the wait: the caller bursts instead when it expires.
	Acquire func(ctx context.Context, jobID string, vramMiB int64, maxWait time.Duration) (card int, release func(), err error)
	// Now and AfterFunc are the clock; tests inject a fake.
	Now       func() time.Time
	AfterFunc func(d time.Duration, f func()) (stop func())
	Log       *slog.Logger

	mu     sync.Mutex
	models map[string]*modelState
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Runner) after(d time.Duration, f func()) func() {
	if r.AfterFunc != nil {
		return r.AfterFunc(d, f)
	}
	t := time.AfterFunc(d, f)
	return func() { t.Stop() }
}

func (r *Runner) state(model string) *modelState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.models == nil {
		r.models = map[string]*modelState{}
	}
	if r.models[model] == nil {
		r.models[model] = &modelState{}
	}
	return r.models[model]
}

func (r *Runner) idle() time.Duration {
	s := r.Cfg.Lab.SessionIdleSeconds
	if s <= 0 {
		s = 120
	}
	return time.Duration(s) * time.Second
}

// BurstAvailable reports whether the runner can launch burst instances.
func (r *Runner) BurstAvailable() bool { return r.Launch != nil && r.Remote != nil }

// Sessions lists the warm sessions, soonest expiry first.
func (r *Runner) Sessions() []SessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []SessionInfo
	for k, ms := range r.models {
		if ms.session != nil && !ms.session.busy {
			out = append(out, SessionInfo{Model: k, Until: ms.session.until})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until.Before(out[j].Until) })
	return out
}

// EndSession terminates a model's warm instance now. ended is false when there
// is no idle session (a run in progress is not interrupted).
func (r *Runner) EndSession(ctx context.Context, model string) (ended bool) {
	ms := r.state(model)
	r.mu.Lock()
	s := ms.session
	if s == nil || s.busy {
		r.mu.Unlock()
		return false
	}
	ms.session = nil
	if s.stop != nil {
		s.stop()
	}
	r.mu.Unlock()
	r.terminate(ctx, s.run, "ended")
	return true
}

func (r *Runner) terminate(ctx context.Context, run *burst.Run, why string) {
	if err := r.Launch.Terminate(context.WithoutCancel(ctx), run, burst.StateCompleted); err != nil {
		r.log().Warn("lab: session terminate failed; the reaper will retry", "run", run.ID, "why", why)
	}
	if r.Forget != nil {
		r.Forget(run.WorkerRunID())
	}
	r.log().Info("lab: session instance terminated", "run", run.ID, "why", why)
}

// Run executes one job to a terminal job state. It never returns an error to
// the caller: failures are recorded on the job with a user-safe message.
func (r *Runner) Run(ctx context.Context, req RunRequest) {
	m := req.Model
	jobID := req.JobID
	_ = r.Jobs.UpdateJobStatus(jobID, models.StatusProcessing)
	_ = r.Jobs.UpdateJobProgress(jobID, 5)
	if err := os.MkdirAll(filepath.Join(r.Cfg.OutputDir, jobID), 0o755); err != nil {
		_ = r.Jobs.UpdateJobError(jobID, "Could not prepare the output folder")
		return
	}
	var err error
	if PlacementOf(m, r.Cfg.Lab) == PlaceHome {
		err = r.runHome(ctx, req)
		if errors.Is(err, services.ErrGPUMaxWait) {
			// No home card was free in time: burst instead, when it is configured.
			err = r.runBurst(ctx, req)
		}
	} else {
		err = r.runBurst(ctx, req)
	}
	if err != nil {
		r.log().Warn("lab: job failed", "job", jobID, "model", m.Key, "error", err.Error())
		_ = r.Jobs.UpdateJobError(jobID, userMessage(err))
		return
	}
	out := filepath.Join(r.Cfg.OutputDir, jobID, m.ResultFile)
	st, serr := os.Stat(out)
	if serr != nil || st.IsDir() {
		_ = r.Jobs.UpdateJobError(jobID, "The model produced no output")
		return
	}
	_ = r.Jobs.SetResultMetadata(jobID, "", m.Key+"_"+m.ResultFile, time.Time{})
	_ = r.Jobs.SetResultSize(jobID, st.Size())
	_ = r.Jobs.UpdateJobResult(jobID, "/api/download/"+jobID)
	_ = r.Jobs.UpdateJobStatus(jobID, models.StatusCompleted)
}

func userMessage(err error) string {
	switch {
	case errors.Is(err, ErrUnavailable):
		return "This model needs a burst GPU and burst is not configured"
	case errors.Is(err, ErrCap):
		return "A Lab or burst spend cap would be crossed"
	case errors.Is(err, burst.ErrToolFailed):
		return "The model failed while processing the input"
	case errors.Is(err, burst.ErrWorkerLost), errors.Is(err, burst.ErrStagingIn), errors.Is(err, burst.ErrStagingOut):
		return "The GPU instance was lost or a transfer failed"
	case errors.Is(err, context.Canceled):
		return "Cancelled"
	}
	return "The Lab job failed"
}

func (r *Runner) spec(req RunRequest, card int) (exec.Spec, error) {
	sp, err := req.Model.Command(CommandCtx{
		Cfg: r.Cfg, JobID: req.JobID, RequestID: req.RequestID, Params: req.Params, Inputs: req.Inputs,
		OutDir: filepath.Join(r.Cfg.OutputDir, req.JobID), Card: card,
	})
	if err != nil {
		return sp, err
	}
	for i := range sp.Steps {
		if sp.Steps[i].Timeout <= 0 {
			sp.Steps[i].Timeout = req.Model.Profile().RemoteTimeoutDuration()
		}
	}
	return sp, nil
}

func (r *Runner) acquire(ctx context.Context, jobID string, vram int64, maxWait time.Duration) (int, func(), error) {
	if r.Acquire != nil {
		return r.Acquire(ctx, jobID, vram, maxWait)
	}
	s := services.SharedGPUScheduler()
	card, release, waited, err := s.AcquireTimed(ctx, HomeRequest(r.Cfg, vram, maxWait))
	if err == nil && waited.Queued {
		r.Jobs.AddQueueWait(jobID, waited.Wait)
	}
	return card, release, err
}

// HomeRequest is the scheduler request of a home Lab run: the face-restore
// venv's card, at the standard priority and never Premium, so the Lab always
// yields to customers.
func HomeRequest(cfg *config.Config, vramMiB int64, maxWait time.Duration) services.GPURequest {
	force := cfg.AICUDAGPU
	return services.GPURequest{Kind: "lab_home", VRAMRequiredMiB: vramMiB, MaxWait: maxWait,
		ForceIndex: &force, Priority: services.GPUPriorityStandard}
}

// canBurst reports whether a model could run on a burst instance right now:
// burst is configured and a worker image exists for the model's family.
func (r *Runner) canBurst(m LabModel) bool {
	return r.BurstAvailable() && r.Cfg.Burst.AMIs[m.Family+":x86_64"] != ""
}

// runHome runs a FitsHome model on a free home card (LAB_ALLOW_LOCAL). When
// the model could burst instead it waits at most a few seconds for a card,
// then bursts; otherwise (burst off, or no worker image for its family yet)
// it waits as long as it takes. Without the image check, switching burst on
// for the voice models would have made CodeFormer fail whenever the home card
// was busy for more than 3 s, because no lab-image AMI exists.
func (r *Runner) runHome(ctx context.Context, req RunRequest) error {
	var maxWait time.Duration
	if r.canBurst(req.Model) {
		maxWait = 3 * time.Second
	}
	card, release, err := r.acquire(ctx, req.JobID, req.Model.HomeVRAMMiB, maxWait)
	if err != nil {
		return err
	}
	defer release()
	if card < 0 {
		card = r.Cfg.AICUDAGPU
	}
	sp, err := r.spec(req, card)
	if err != nil {
		return err
	}
	_ = r.Jobs.UpdateJobProgress(req.JobID, 20)
	return exec.Local{Runner: r.Local, Validator: &r.Validator}.Run(ctx, sp)
}

// runBurst runs the job on a Lab session instance: reuse the model's warm one
// when it is still inside its idle window, else launch directly. Fails closed
// when burst is not configured.
func (r *Runner) runBurst(ctx context.Context, req RunRequest) error {
	if r.Launch == nil || r.Remote == nil {
		return ErrUnavailable
	}
	m := req.Model
	ms := r.state(m.Key)
	ms.runMu.Lock()
	defer ms.runMu.Unlock()

	hold := time.Duration(m.EstSeconds) * time.Second
	var run *burst.Run
	r.mu.Lock()
	if s := ms.session; s != nil && !s.busy && r.now().Before(s.until) {
		s.busy = true
		s.gen++
		if s.stop != nil {
			s.stop()
			s.stop = nil
		}
		run = s.run
	} else if s != nil && !s.busy {
		// Past its window and the timer has not fired yet: end it, launch fresh.
		ms.session = nil
		r.mu.Unlock()
		r.terminate(ctx, s.run, "idle")
		r.mu.Lock()
	}
	r.mu.Unlock()

	if run != nil { // warm reuse
		if err := r.Launch.ExtendSession(ctx, run, hold); err != nil {
			r.dropSession(ms)
			r.terminate(ctx, run, "cap")
			return fmt.Errorf("%w: %v", ErrCap, err)
		}
		r.Launch.SetSession(ctx, run, nil)
		r.log().Info("lab: reusing the warm session", "model", m.Key, "run", run.ID)
	} else {
		var err error
		run, err = r.Launch.LaunchDirect(ctx, m.Profile(), burst.Offer{JobID: req.JobID, Kind: m.Tool(), Tier: "operator", Lab: true})
		if err != nil {
			if errors.Is(err, burst.ErrCapDaily) || errors.Is(err, burst.ErrCapMonthly) {
				return fmt.Errorf("%w: %v", ErrCap, err)
			}
			return err
		}
		timeout := time.Duration(r.Cfg.Burst.BootTimeoutSeconds) * time.Second
		wctx, cancel := context.WithTimeout(ctx, timeout)
		werr := r.Remote.WaitReady(wctx, run)
		cancel()
		if werr != nil {
			outcome := burst.StateWorkerLost
			if errors.Is(wctx.Err(), context.DeadlineExceeded) {
				outcome = burst.StateBootTimeout
			}
			_ = r.Launch.Terminate(context.WithoutCancel(ctx), run, outcome)
			if r.Forget != nil {
				r.Forget(run.WorkerRunID())
			}
			return burst.ErrWorkerLost
		}
		r.mu.Lock()
		ms.session = &session{run: run, busy: true}
		r.mu.Unlock()
	}

	sp, err := r.spec(req, 0)
	if err != nil {
		r.parkOrEnd(ctx, ms, run, false)
		return err
	}
	_ = r.Jobs.UpdateJobProgress(req.JobID, 30)
	run.Keep = true
	rerr := r.Remote.Run(ctx, run, sp)
	run.Keep = false
	r.parkOrEnd(ctx, ms, run, rerr == nil)
	return rerr
}

func (r *Runner) dropSession(ms *modelState) {
	r.mu.Lock()
	ms.session = nil
	r.mu.Unlock()
}

// parkOrEnd settles the session after a run. A success parks the instance for
// the idle window (session_until, plus a timer that terminates it); a failure
// means Remote.Run already terminated the instance, so the session is gone.
func (r *Runner) parkOrEnd(ctx context.Context, ms *modelState, run *burst.Run, ok bool) {
	if !ok {
		r.dropSession(ms)
		if run.State.Terminal() {
			if r.Forget != nil {
				r.Forget(run.WorkerRunID())
			}
			return
		}
		r.terminate(ctx, run, "failed")
		return
	}
	until := r.now().Add(r.idle())
	r.mu.Lock()
	s := ms.session
	if s == nil {
		s = &session{run: run}
		ms.session = s
	}
	s.busy, s.until = false, until
	s.gen++
	gen := s.gen
	r.mu.Unlock()
	r.Launch.SetSession(ctx, run, &until)
	stop := r.after(r.idle(), func() { r.idleExpired(ms, gen) })
	r.mu.Lock()
	s.stop = stop
	r.mu.Unlock()
}

// idleExpired terminates the session when nothing re-armed it meanwhile.
func (r *Runner) idleExpired(ms *modelState, gen int) {
	r.mu.Lock()
	s := ms.session
	if s == nil || s.busy || s.gen != gen {
		r.mu.Unlock()
		return
	}
	ms.session = nil
	r.mu.Unlock()
	r.terminate(context.Background(), s.run, "idle")
}
