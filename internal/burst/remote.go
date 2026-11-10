package burst

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/staging"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/wire"
	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
)

// Typed failures; MM127 maps them to design §6.4's re-queue policy.
var (
	// ErrToolFailed: the tool exited non-zero (or timed out) remotely.
	ErrToolFailed = errors.New("burst: tool failed remotely")
	// ErrWorkerLost: the worker stopped answering or could not be reached.
	ErrWorkerLost = errors.New("burst: worker lost")
	// ErrStagingIn / ErrStagingOut: transfer or checksum failure.
	ErrStagingIn  = errors.New("burst: staging in failed")
	ErrStagingOut = errors.New("burst: staging out failed")
	// ErrCancelled: the caller's context ended.
	ErrCancelled = errors.New("burst: cancelled")
)

// ToolFailure carries the redacted stderr tail.
type ToolFailure struct {
	ExitCode   int
	TimedOut   bool
	StderrTail string
}

func (e *ToolFailure) Error() string {
	return fmt.Sprintf("burst: tool failed remotely (exit %d, timedOut %v)", e.ExitCode, e.TimedOut)
}
func (e *ToolFailure) Unwrap() error { return ErrToolFailed }

// Lifecycle is the slice of Planner the remote executor drives.
type Lifecycle interface {
	Advance(ctx context.Context, run *Run, to State) error
	Terminate(ctx context.Context, run *Run, outcome State) error
}

// TokenSource returns a run's HMAC token (UserDataBuilder).
type TokenSource interface {
	Token(runID string) (string, bool)
	Forget(runID string)
}

// Remote runs a Spec on a burst worker: Run(ctx, run, spec). It owns the
// run's states from ready to completed, deletes the staging prefix on every
// exit path, and terminates the instance through the Lifecycle.
type Remote struct {
	Store      staging.ObjectStore
	Life       Lifecycle
	Tokens     TokenSource
	Resolver   Resolver
	Validator  exec.Validator
	Prefix     string // "burst/"
	Port       int
	RemoteRoot string // /opt/dlami/nvme/burst
	Sanitizer  *cmdaudit.PathSanitizer
	HTTP       *http.Client
	Log        *slog.Logger
	Now        func() time.Time
	// BaseURL builds the worker URL from a tailnet IP (default http://ip:port).
	BaseURL func(ip string) string

	PollInterval    time.Duration // default 2s
	ResolveTimeout  time.Duration // default 5m (the boot deadline is enforced by MM127)
	MaxPollFailures int           // consecutive; default 5
	Concurrency     int
}

func (r *Remote) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Remote) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Remote) poll() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return 2 * time.Second
}

func (r *Remote) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (r *Remote) baseURL(ip string) string {
	if r.BaseURL != nil {
		return r.BaseURL(ip)
	}
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(r.Port))
}

// call sends one signed request and decodes a JSON reply.
func (r *Remote) call(ctx context.Context, base, token, method, uri string, in, out any) (int, error) {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+uri, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	wire.SignRequest(req, token, body, r.now())
	resp, err := r.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// redactTail makes a worker stderr tail safe to log: remote run paths and local
// roots become placeholders, then the standard cmdaudit rewrites apply.
func (r *Remote) redactTail(runID, s string) string {
	base := strings.TrimRight(coalesce(r.RemoteRoot, exec.DefaultRemoteRoot), "/") + "/" + runID
	san := r.Sanitizer
	if san == nil {
		san = cmdaudit.NewPathSanitizer("", "", "")
	}
	remote := cmdaudit.NewPathSanitizer(base+"/in", base+"/out", base+"/scratch")
	return san.RedactArg(remote.RedactArg(cmdaudit.TailString(s, 4000)))
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// WaitReady blocks until the run's worker has joined the tailnet and reports
// ready and idle (the first half of Run), without staging or running anything.
// The scheduler's burst planner (MM127) uses it to learn that a booting
// instance can take the job. It does not change the run's state.
func (r *Remote) WaitReady(ctx context.Context, run *Run) error {
	_, _, err := r.waitForWorker(ctx, run)
	return err
}

func (r *Remote) waitForWorker(ctx context.Context, run *Run) (base, token string, err error) {
	token, ok := r.Tokens.Token(run.WorkerRunID())
	if !ok {
		return "", "", fmt.Errorf("%w: no run token", ErrWorkerLost)
	}
	limit := r.ResolveTimeout
	if limit <= 0 {
		limit = 5 * time.Minute
	}
	rctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	host := WorkerHostname(run.WorkerRunID())
	var ip string
	for ip == "" {
		ip, err = r.Resolver.ResolveIP(rctx, host)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrDeviceNotFound) {
			r.log().Warn("burst: device lookup failed", "run", run.ID, "error", err.Error())
		}
		ip = ""
		if werr := sleepCtx(rctx, r.poll()); werr != nil {
			return "", "", fmt.Errorf("%w: worker never joined the tailnet", ErrWorkerLost)
		}
	}
	base = r.baseURL(ip)
	for {
		var h wire.Health
		code, herr := r.call(rctx, base, token, http.MethodGet, "/v1/health", nil, &h)
		if herr == nil && code == http.StatusOK && h.Ready && !h.Busy {
			if h.Protocol != wire.ProtocolVersion {
				return "", "", fmt.Errorf("%w: protocol %d", ErrWorkerLost, h.Protocol)
			}
			return base, token, nil
		}
		if werr := sleepCtx(rctx, r.poll()); werr != nil {
			return "", "", fmt.Errorf("%w: worker never became ready", ErrWorkerLost)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func toWire(m []exec.Mapping, optional func(exec.Mapping) bool) []wire.PathSpec {
	out := make([]wire.PathSpec, 0, len(m))
	for _, mp := range m {
		out = append(out, wire.PathSpec{N: mp.N, Remote: mp.Remote, Dir: mp.Path.Dir, Optional: mp.Path.Optional})
	}
	return out
}

// Run executes spec on the run's worker. On return the declared outputs are at
// their original local paths, verified, the staging prefix is gone and the run
// is terminated (completed or the failure's outcome).
func (r *Remote) Run(ctx context.Context, run *Run, spec exec.Spec) (err error) {
	outcome := StateToolFailed
	defer func() {
		bg := context.WithoutCancel(ctx)
		if derr := r.Store.DeletePrefix(bg, RunPrefix(r.Prefix, run.WorkerRunID())+"/"); derr != nil {
			r.log().Warn("burst: staging prefix delete failed; the reaper will retry", "run", run.ID, "error", derr.Error())
		}
		if err == nil {
			outcome = StateCompleted
		}
		r.log().Info("burst: staging prefix deleted, terminating", "run", run.ID, "outcome", string(outcome))
		if terr := r.Life.Terminate(bg, run, outcome); terr != nil {
			r.log().Warn("burst: terminate after run", "run", run.ID, "outcome", string(outcome), "error", terr.Error())
		}
		r.Tokens.Forget(run.WorkerRunID())
	}()
	fail := func(state State, errType string, e error) error {
		outcome, run.ErrorType = state, errType
		return e
	}

	if verr := r.Validator.Validate(spec); verr != nil {
		return fail(StateToolFailed, "invalid_spec", verr)
	}
	mp, merr := exec.NewMapper(r.RemoteRoot, run.WorkerRunID(), spec)
	if merr != nil {
		return fail(StateToolFailed, "invalid_spec", merr)
	}
	steps, serr := mp.RewriteSteps(r.Validator, spec.Steps)
	if serr != nil {
		return fail(StateToolFailed, "invalid_spec", serr)
	}

	base, token, werr := r.waitForWorker(ctx, run)
	if werr != nil {
		if ctx.Err() != nil {
			return fail(StateToolFailed, "cancelled", fmt.Errorf("%w: %v", ErrCancelled, ctx.Err()))
		}
		return fail(StateWorkerLost, "worker_unreachable", werr)
	}
	if run.State == StateBooting {
		if aerr := r.Life.Advance(ctx, run, StateReady); aerr != nil {
			return fail(StateToolFailed, "transition", aerr)
		}
	}

	// staging_in
	if aerr := r.Life.Advance(ctx, run, StateStagingIn); aerr != nil {
		return fail(StateToolFailed, "transition", aerr)
	}
	prefix := RunPrefix(r.Prefix, run.WorkerRunID())
	var srcs []staging.Source
	for _, m := range mp.Maps {
		if m.Group == exec.GroupIn {
			srcs = append(srcs, staging.Source{N: m.N, Local: m.Local, Dir: m.Path.Dir})
		}
	}
	inM, uerr := staging.Upload(ctx, r.Store, prefix, "in", srcs, r.Concurrency)
	if uerr != nil {
		if ctx.Err() != nil {
			return fail(StateToolFailed, "cancelled", fmt.Errorf("%w: %v", ErrCancelled, ctx.Err()))
		}
		r.log().Error("burst: staging in failed", "run", run.ID, "error", uerr.Error())
		return fail(StateToolFailed, "staging_in", fmt.Errorf("%w: %v", ErrStagingIn, uerr))
	}
	run.BytesIn = inM.TotalBytes()
	r.log().Info("burst: inputs staged", "run", run.ID, "files", len(inM.Files), "bytes", inM.TotalBytes())

	req := wire.RunRequest{RunID: run.WorkerRunID(), Prefix: prefix, InputBytes: inM.TotalBytes()}
	for _, c := range steps {
		req.Steps = append(req.Steps, wire.Cmd{Label: c.Label, Exe: c.Exe, Args: c.Args, Env: c.Env, Dir: c.Dir, TimeoutSec: int(c.Timeout / time.Second)})
	}
	var in, out, scratch []exec.Mapping
	for _, m := range mp.Maps {
		switch m.Group {
		case exec.GroupIn:
			in = append(in, m)
		case exec.GroupOut:
			out = append(out, m)
		default:
			scratch = append(scratch, m)
		}
	}
	req.Inputs, req.Outputs, req.Scratch = toWire(in, nil), toWire(out, nil), toWire(scratch, nil)
	code, perr := r.call(ctx, base, token, http.MethodPost, "/v1/runs", req, nil)
	if perr != nil || code != http.StatusAccepted {
		if ctx.Err() != nil {
			return fail(StateToolFailed, "cancelled", fmt.Errorf("%w: %v", ErrCancelled, ctx.Err()))
		}
		r.log().Error("burst: worker refused the run", "run", run.ID, "status", code)
		return fail(StateWorkerLost, "run_refused", fmt.Errorf("%w: POST /v1/runs status %d", ErrWorkerLost, code))
	}
	if aerr := r.Life.Advance(ctx, run, StateRunning); aerr != nil {
		return fail(StateToolFailed, "transition", aerr)
	}

	st, polErr := r.pollUntilDone(ctx, run, base, token, spec.Stdout)
	if polErr != nil {
		if errors.Is(polErr, ErrCancelled) {
			_, _ = r.call(context.WithoutCancel(ctx), base, token, http.MethodPost, "/v1/runs/"+run.WorkerRunID()+"/cancel", nil, nil)
			return fail(StateToolFailed, "cancelled", polErr)
		}
		return fail(StateWorkerLost, "worker_lost", polErr)
	}
	switch st.State {
	case wire.StateCancelled:
		return fail(StateToolFailed, "cancelled", ErrCancelled)
	case wire.StateFailed:
		if st.Phase == wire.PhaseRunning {
			return fail(StateToolFailed, "tool_failed", &ToolFailure{ExitCode: st.ExitCode, TimedOut: st.TimedOut, StderrTail: r.redactTail(run.WorkerRunID(), st.StderrTail)})
		}
		kind := ErrStagingOut
		if st.Phase == wire.PhaseStagingIn {
			kind = ErrStagingIn
		}
		return fail(StateToolFailed, st.Phase, fmt.Errorf("%w: worker reported %s", kind, st.Error))
	}

	// staging_out
	if run.State == StateRunning {
		if aerr := r.Life.Advance(ctx, run, StateStagingOut); aerr != nil {
			return fail(StateToolFailed, "transition", aerr)
		}
	}
	if derr := r.fetchOutputs(ctx, prefix, mp, spec); derr != nil {
		if ctx.Err() != nil {
			return fail(StateToolFailed, "cancelled", fmt.Errorf("%w: %v", ErrCancelled, ctx.Err()))
		}
		r.log().Error("burst: staging out failed", "run", run.ID, "error", derr.Error())
		return fail(StateToolFailed, "staging_out", derr)
	}
	r.log().Info("burst: outputs verified and placed", "run", run.ID)
	return nil
}

func (r *Remote) fetchOutputs(ctx context.Context, prefix string, mp *exec.Mapper, spec exec.Spec) error {
	m, err := staging.FetchManifest(ctx, r.Store, prefix, "out")
	if err != nil {
		return fmt.Errorf("%w: manifest: %v", ErrStagingOut, err)
	}
	roots := map[int]string{}
	have := map[int]bool{}
	for _, e := range m.Files {
		have[e.N] = true
	}
	for _, mm := range mp.Maps {
		if mm.Group != exec.GroupOut {
			continue
		}
		roots[mm.N] = mm.Local
		if !have[mm.N] && !mm.Path.Dir && !mm.Path.Optional {
			return fmt.Errorf("%w: a required output was not produced", ErrStagingOut)
		}
	}
	if err := staging.Download(ctx, r.Store, prefix, "out", m, roots, r.Concurrency); err != nil {
		return fmt.Errorf("%w: %v", ErrStagingOut, err)
	}
	return nil
}

func (r *Remote) pollUntilDone(ctx context.Context, run *Run, base, token string, stdout io.Writer) (wire.Status, error) {
	max := r.MaxPollFailures
	if max <= 0 {
		max = 5
	}
	cursor, fails := 0, 0
	for {
		var st wire.Status
		code, err := r.call(ctx, base, token, http.MethodGet, "/v1/runs/"+run.WorkerRunID()+"?cursor="+strconv.Itoa(cursor), nil, &st)
		switch {
		case ctx.Err() != nil:
			return wire.Status{}, fmt.Errorf("%w: %v", ErrCancelled, ctx.Err())
		case err != nil || code != http.StatusOK:
			fails++
			if fails >= max {
				return wire.Status{}, fmt.Errorf("%w: %d failed polls", ErrWorkerLost, fails)
			}
		default:
			fails = 0
			for _, l := range st.Lines {
				if stdout != nil {
					_, _ = io.WriteString(stdout, l+"\n")
				}
			}
			cursor = st.Cursor
			if st.Terminal() {
				return st, nil
			}
		}
		if werr := sleepCtx(ctx, r.poll()); werr != nil {
			return wire.Status{}, fmt.Errorf("%w: %v", ErrCancelled, werr)
		}
	}
}
