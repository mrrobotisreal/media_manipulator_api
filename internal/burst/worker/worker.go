// Package worker is the burst worker (MM125, design §10): an HTTP server that
// runs exactly one declared Spec on the instance it lives on, pulling inputs
// from and pushing outputs to the run's S3 prefix with sha256 verification. It
// authenticates every request with an HMAC over the per-run token and shuts the
// instance down when nobody talks to it. cmd/burst-worker wires it to the real
// world; tests wire fakes.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/staging"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/wire"
)

// Tunables.
const (
	DefaultOrphanMinutes = 10
	// FinishedShutdown is the belt-and-braces shutdown after the outputs are up.
	FinishedShutdown = 5 * time.Minute
	// DiskGuardFactor: refuse to start under inputBytes * 3 free (proposed).
	DiskGuardFactor = 3
	maxLines        = 20000
	stderrTailBytes = 4000
	maxBody         = 4 << 20
	// DefaultStepTimeout caps a step that declares none.
	DefaultStepTimeout = 2 * time.Hour
	// ReadyAfter: health.ready is true at the latest this long after start.
	ReadyAfter = 5 * time.Minute
)

// Config is everything the worker needs.
type Config struct {
	RunID     string
	Token     string // per-run HMAC key; never logged
	Prefix    string // "burst/<runId>"
	Root      string // /opt/dlami/nvme/burst
	Version   string
	Image     wire.ImageInfo
	OrphanMin int

	Store staging.ObjectStore
	Log   *slog.Logger
	Now   func() time.Time
	// Shutdown powers the instance off (`shutdown -h now`).
	Shutdown func()
	// FreeGiB reports free scratch space.
	FreeGiB func() float64
	// GPU reports nvidia-smi facts.
	GPU func() wire.GPUInfo
	// PrewarmDone reports the boot prewarm finished; nil = ready at once.
	PrewarmDone func() bool
	// Workers is the staging concurrency.
	Workers int
	// Session (MM138 Lab sessions) lets the worker accept another run once the
	// previous one has finished: the run directory is wiped and the finished-run
	// shutdown timer is disarmed. False is the product rule: one spec, then 409.
	Session bool
}

// Worker is the server state.
type Worker struct {
	cfg     Config
	started time.Time

	mu       sync.Mutex
	lastAuth time.Time
	deadline time.Time // finished-run shutdown deadline (zero = unarmed)
	fired    bool
	run      *runState
	accepted bool
}

type runState struct {
	mu        sync.Mutex
	req       wire.RunRequest
	state     string
	step      int
	lines     []string
	dropped   int
	phase     string
	err       string
	exitCode  int
	timedOut  bool
	stderr    tailBuf
	outBytes  int64
	outFiles  int
	cancel    context.CancelFunc
	cancelled bool
}

// New builds a Worker; the orphan clock starts now.
func New(cfg Config) *Worker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.OrphanMin <= 0 {
		cfg.OrphanMin = DefaultOrphanMinutes
	}
	if cfg.Root == "" {
		cfg.Root = "/opt/dlami/nvme/burst"
	}
	if cfg.Shutdown == nil {
		cfg.Shutdown = func() {}
	}
	w := &Worker{cfg: cfg, started: cfg.Now()}
	w.lastAuth = w.started
	return w
}

func (w *Worker) log() *slog.Logger { return w.cfg.Log }

// Handler is the HTTP surface.
func (w *Worker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", w.auth(w.health))
	mux.HandleFunc("POST /v1/runs", w.auth(w.postRun))
	mux.HandleFunc("GET /v1/runs/{id}", w.auth(w.getRun))
	mux.HandleFunc("POST /v1/runs/{id}/cancel", w.auth(w.cancelRun))
	return mux
}

type handlerFunc func(rw http.ResponseWriter, r *http.Request, body []byte)

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

func (w *Worker) auth(h handlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(body) > maxBody {
			writeJSON(rw, http.StatusRequestEntityTooLarge, wire.ErrorBody{Error: "body"})
			return
		}
		now := w.cfg.Now()
		if !wire.Verify(w.cfg.Token, r, body, now) {
			// An unauthenticated request does not reset the orphan clock.
			writeJSON(rw, http.StatusUnauthorized, wire.ErrorBody{Error: "unauthorized"})
			return
		}
		w.mu.Lock()
		w.lastAuth = now
		w.mu.Unlock()
		h(rw, r, body)
	}
}

func (w *Worker) health(rw http.ResponseWriter, _ *http.Request, _ []byte) {
	h := wire.Health{WorkerVersion: w.cfg.Version, Protocol: wire.ProtocolVersion, Image: w.cfg.Image}
	if w.cfg.GPU != nil {
		h.GPU = w.cfg.GPU()
	}
	if w.cfg.FreeGiB != nil {
		h.Disk.ScratchFreeGiB = w.cfg.FreeGiB()
	}
	h.Ready = w.cfg.PrewarmDone == nil || w.cfg.PrewarmDone() || w.cfg.Now().Sub(w.started) >= ReadyAfter
	w.mu.Lock()
	rs := w.run
	w.mu.Unlock()
	h.Busy = rs != nil && !rs.finished()
	writeJSON(rw, http.StatusOK, h)
}

// ErrBadRequest is returned for a RunRequest the worker will not run.
var ErrBadRequest = errors.New("worker: bad run request")

func (w *Worker) validate(req wire.RunRequest) error {
	if req.RunID != w.cfg.RunID || req.Prefix != w.cfg.Prefix {
		return fmt.Errorf("%w: run id or prefix mismatch", ErrBadRequest)
	}
	if len(req.Steps) == 0 {
		return fmt.Errorf("%w: no steps", ErrBadRequest)
	}
	base := path.Join(w.cfg.Root, w.cfg.RunID) + "/"
	for _, group := range [][]wire.PathSpec{req.Inputs, req.Outputs, req.Scratch} {
		for _, p := range group {
			if !strings.HasPrefix(p.Remote, base) || path.Clean(p.Remote) != p.Remote {
				return fmt.Errorf("%w: path outside the run directory", ErrBadRequest)
			}
		}
	}
	return nil
}

func (w *Worker) postRun(rw http.ResponseWriter, _ *http.Request, body []byte) {
	var req wire.RunRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(rw, http.StatusBadRequest, wire.ErrorBody{Error: "unreadable body"})
		return
	}
	w.mu.Lock()
	if w.accepted && (!w.cfg.Session || w.run == nil || !w.run.finished()) {
		w.mu.Unlock()
		writeJSON(rw, http.StatusConflict, wire.ErrorBody{Error: "a run was already accepted on this instance"})
		return
	}
	if err := w.validate(req); err != nil {
		w.mu.Unlock()
		writeJSON(rw, http.StatusBadRequest, wire.ErrorBody{Error: err.Error()})
		return
	}
	if w.cfg.FreeGiB != nil {
		need := float64(req.InputBytes) * DiskGuardFactor / (1 << 30)
		if free := w.cfg.FreeGiB(); free < need {
			w.mu.Unlock()
			w.log().Warn("worker: disk guard refused the run", "run", req.RunID, "freeGiB", free, "needGiB", need)
			writeJSON(rw, http.StatusInsufficientStorage, wire.ErrorBody{Error: "disk_guard"})
			return
		}
	}
	if w.accepted { // a Lab session re-arm: start from an empty run directory, disarm the finished-run shutdown
		_ = os.RemoveAll(path.Join(w.cfg.Root, w.cfg.RunID))
		w.deadline = time.Time{}
	}
	w.accepted = true
	ctx, cancel := context.WithCancel(context.Background())
	rs := &runState{req: req, state: wire.StateStagingIn, cancel: cancel, stderr: tailBuf{max: stderrTailBytes}}
	w.run = rs
	w.mu.Unlock()
	w.log().Info("worker: run accepted", "run", req.RunID, "steps", len(req.Steps))
	go w.execute(ctx, rs)
	writeJSON(rw, http.StatusAccepted, map[string]string{"runId": req.RunID})
}

func (w *Worker) lookup(rw http.ResponseWriter, r *http.Request) *runState {
	w.mu.Lock()
	rs := w.run
	w.mu.Unlock()
	if rs == nil || rs.req.RunID != r.PathValue("id") {
		writeJSON(rw, http.StatusNotFound, wire.ErrorBody{Error: "no such run"})
		return nil
	}
	return rs
}

func (w *Worker) getRun(rw http.ResponseWriter, r *http.Request, _ []byte) {
	rs := w.lookup(rw, r)
	if rs == nil {
		return
	}
	cursor := 0
	fmt.Sscanf(r.URL.Query().Get("cursor"), "%d", &cursor)
	rs.mu.Lock()
	defer rs.mu.Unlock()
	st := wire.Status{RunID: rs.req.RunID, State: rs.state, Step: rs.step, Phase: rs.phase, Error: rs.err,
		ExitCode: rs.exitCode, TimedOut: rs.timedOut, OutputBytes: rs.outBytes, OutputFiles: rs.outFiles}
	if cursor < rs.dropped {
		cursor = rs.dropped
	}
	idx := cursor - rs.dropped
	if idx < 0 || idx > len(rs.lines) {
		idx = len(rs.lines)
	}
	st.Lines = append([]string(nil), rs.lines[idx:]...)
	st.Cursor = rs.dropped + len(rs.lines)
	if st.State == wire.StateFailed {
		st.StderrTail = rs.stderr.String()
	}
	writeJSON(rw, http.StatusOK, st)
}

func (w *Worker) cancelRun(rw http.ResponseWriter, r *http.Request, _ []byte) {
	rs := w.lookup(rw, r)
	if rs == nil {
		return
	}
	rs.mu.Lock()
	rs.cancelled = true
	rs.mu.Unlock()
	rs.cancel()
	writeJSON(rw, http.StatusOK, map[string]string{"runId": rs.req.RunID})
}

// finished reports whether the run reached a terminal worker state.
func (rs *runState) finished() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.state == wire.StateDone || rs.state == wire.StateFailed || rs.state == wire.StateCancelled
}

func (rs *runState) set(state string) {
	rs.mu.Lock()
	rs.state = state
	rs.mu.Unlock()
}

func (rs *runState) fail(phase, msg string) {
	rs.mu.Lock()
	rs.phase, rs.err = phase, msg
	if rs.cancelled {
		rs.state = wire.StateCancelled
	} else {
		rs.state = wire.StateFailed
	}
	rs.mu.Unlock()
}

func (rs *runState) addLine(l string) {
	rs.mu.Lock()
	rs.lines = append(rs.lines, l)
	if len(rs.lines) > maxLines {
		cut := len(rs.lines) - maxLines
		rs.lines = rs.lines[cut:]
		rs.dropped += cut
	}
	rs.mu.Unlock()
}

// finish arms the post-run shutdown.
func (w *Worker) finish() {
	if w.cfg.Session {
		return // the API ends a session instance; the orphan timer is the backstop
	}
	w.mu.Lock()
	w.deadline = w.cfg.Now().Add(FinishedShutdown)
	w.mu.Unlock()
}

func (w *Worker) execute(ctx context.Context, rs *runState) {
	defer w.finish()
	req := rs.req
	// staging in
	roots := map[int]string{}
	for _, p := range req.Inputs {
		roots[p.N] = p.Remote
	}
	if len(req.Inputs) > 0 {
		m, err := staging.FetchManifest(ctx, w.cfg.Store, req.Prefix, "in")
		if err == nil {
			err = staging.Download(ctx, w.cfg.Store, req.Prefix, "in", m, roots, w.cfg.Workers)
		}
		if err != nil {
			rs.fail(wire.PhaseStagingIn, errKind(err))
			w.log().Error("worker: staging in failed", "run", req.RunID, "error", errKind(err))
			return
		}
		for _, p := range req.Inputs {
			if p.Dir {
				_ = os.MkdirAll(p.Remote, 0o755)
			}
		}
	}
	for _, p := range req.Scratch {
		if err := os.MkdirAll(p.Remote, 0o755); err != nil {
			rs.fail(wire.PhaseRunning, "scratch")
			return
		}
	}
	for _, p := range req.Outputs {
		dir := p.Remote
		if !p.Dir {
			dir = filepath.Dir(p.Remote)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			rs.fail(wire.PhaseRunning, "output dir")
			return
		}
	}
	// running
	rs.set(wire.StateRunning)
	for i, c := range req.Steps {
		rs.mu.Lock()
		rs.step = i
		rs.mu.Unlock()
		if err := w.runStep(ctx, rs, c); err != nil {
			w.log().Error("worker: step failed", "run", req.RunID, "step", i)
			return
		}
	}
	if ctx.Err() != nil {
		rs.fail(wire.PhaseRunning, "cancelled")
		return
	}
	// staging out
	rs.set(wire.StateStagingOut)
	var sources []staging.Source
	for _, p := range req.Outputs {
		if _, err := os.Stat(p.Remote); err != nil {
			if p.Optional {
				continue
			}
			rs.fail(wire.PhaseStagingOut, "missing_output")
			w.log().Error("worker: declared output missing", "run", req.RunID, "n", p.N)
			return
		}
		sources = append(sources, staging.Source{N: p.N, Local: p.Remote, Dir: p.Dir})
	}
	m, err := staging.Upload(ctx, w.cfg.Store, req.Prefix, "out", sources, w.cfg.Workers)
	if err != nil {
		rs.fail(wire.PhaseStagingOut, errKind(err))
		w.log().Error("worker: staging out failed", "run", req.RunID, "error", errKind(err))
		return
	}
	rs.mu.Lock()
	rs.outBytes, rs.outFiles = m.TotalBytes(), len(m.Files)
	rs.state = wire.StateDone
	rs.mu.Unlock()
	w.log().Info("worker: run done", "run", req.RunID, "outputFiles", len(m.Files), "outputBytes", m.TotalBytes())
}

// errKind keeps error text free of file names.
func errKind(err error) string {
	switch {
	case errors.Is(err, staging.ErrChecksum):
		return "checksum"
	case errors.Is(err, staging.ErrNotFound):
		return "not_found"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "staging"
}

func (w *Worker) runStep(ctx context.Context, rs *runState, c wire.Cmd) error {
	timeout := time.Duration(c.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(sctx, c.Exe, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	lw := &lineWriter{emit: rs.addLine}
	cmd.Stdout = lw
	cmd.Stderr = &rs.stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	lw.flush()
	if err == nil {
		return nil
	}
	rs.mu.Lock()
	if sctx.Err() == context.DeadlineExceeded {
		rs.timedOut = true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		rs.exitCode = ee.ExitCode()
	} else {
		rs.exitCode = -1
	}
	rs.mu.Unlock()
	rs.fail(wire.PhaseRunning, "step_failed")
	return err
}

// lineWriter splits stdout into lines.
type lineWriter struct {
	buf  bytes.Buffer
	emit func(string)
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		b := l.buf.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if l.buf.Len() > 64<<10 {
				l.flush()
			}
			return len(p), nil
		}
		l.emit(strings.TrimRight(string(b[:i]), "\r"))
		l.buf.Next(i + 1)
	}
}

func (l *lineWriter) flush() {
	if l.buf.Len() > 0 {
		l.emit(strings.TrimRight(l.buf.String(), "\r\n"))
		l.buf.Reset()
	}
}

// tailBuf keeps the last max bytes.
type tailBuf struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// CheckOrphan powers the instance off when no authenticated request has
// arrived for OrphanMin minutes, or when the finished-run deadline passed. It
// fires at most once and reports whether it did.
func (w *Worker) CheckOrphan(now time.Time) bool {
	w.mu.Lock()
	if w.fired {
		w.mu.Unlock()
		return false
	}
	orphan := now.Sub(w.lastAuth) >= time.Duration(w.cfg.OrphanMin)*time.Minute
	finished := !w.deadline.IsZero() && !now.Before(w.deadline)
	if !orphan && !finished {
		w.mu.Unlock()
		return false
	}
	w.fired = true
	w.mu.Unlock()
	reason := "orphan"
	if finished {
		reason = "finished"
	}
	w.log().Warn("worker: shutting the instance down", "reason", reason)
	w.cfg.Shutdown()
	return true
}

// WatchOrphan polls CheckOrphan until ctx ends or it fires.
func (w *Worker) WatchOrphan(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w.CheckOrphan(w.cfg.Now()) {
				return
			}
		}
	}
}
