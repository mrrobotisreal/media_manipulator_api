package cmdaudit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultStdoutTail = 4 * 1024
	defaultStderrTail = 4 * 1024
)

// AuditSink persists a single command audit record. Implementations must be
// safe for concurrent use; the runner spawns a background goroutine to call
// Insert so command execution latency is not affected by DB hiccups.
type AuditSink interface {
	Insert(ctx context.Context, rec Record)
}

// NopSink discards records. Used when telemetry DB is disabled or while
// running tests.
type NopSink struct{}

// Insert satisfies AuditSink.
func (NopSink) Insert(context.Context, Record) {}

// Record is the unit of audit. Fields mirror mm_command_audit_logs.
type Record struct {
	AuditID       string
	RequestID     string
	JobID         string
	Tool          string
	Stage         string
	Executable    string
	ArgsRedacted  []string
	EnvRedacted   map[string]string
	WorkingDirRed string
	StartedAt     time.Time
	CompletedAt   time.Time
	DurationMS    int
	ExitCode      int
	TimedOut      bool
	Success       bool
	StdoutTail    string
	StderrTail    string
	ErrorMessage  string
}

// Spec describes one command to execute. Caller may provide its own Stdin /
// Stdout / Stderr pipes; in that case the runner mirrors output into a
// limited-size ring buffer for the audit tail.
type Spec struct {
	Tool        string
	Stage       string
	Executable  string
	Args        []string
	Env         []string // when non-nil, REPLACES inherited env in audit, not in exec
	ExtraEnv    []string // appended to inherited env at exec time
	WorkingDir  string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	Timeout     time.Duration
	StdoutLimit int
	StderrLimit int
	RequestID   string
	JobID       string
}

// Result mirrors the audit record returned to the caller plus collected
// stdout/stderr for downstream parsing (e.g. ffmpeg progress).
type Result struct {
	AuditID    string
	Stdout     string
	Stderr     string
	ExitCode   int
	DurationMS int
	TimedOut   bool
	Success    bool
	Err        error
}

// Runner wraps exec.CommandContext with redaction + audit.
type Runner struct {
	Sanitizer *PathSanitizer
	Sink      AuditSink
}

// NewRunner builds a Runner. A nil sink is treated as NopSink.
func NewRunner(s *PathSanitizer, sink AuditSink) *Runner {
	if sink == nil {
		sink = NopSink{}
	}
	if s == nil {
		s = NewPathSanitizer("", "", "")
	}
	return &Runner{Sanitizer: s, Sink: sink}
}

// Run executes the spec, captures stdout/stderr, and persists an audit
// record. Returns ExecResult so callers can branch on TimedOut/ExitCode
// without parsing err strings.
//
// Both stdout and stderr are still streamed to the caller-provided writers
// (when set) so existing progress parsers continue to work.
func (r *Runner) Run(ctx context.Context, spec Spec) (Result, error) {
	stdoutLimit := spec.StdoutLimit
	if stdoutLimit <= 0 {
		stdoutLimit = defaultStdoutTail
	}
	stderrLimit := spec.StderrLimit
	if stderrLimit <= 0 {
		stderrLimit = defaultStderrTail
	}

	cmdCtx := ctx
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(cmdCtx, spec.Executable, spec.Args...)
	cmd.Dir = spec.WorkingDir
	cmd.Stdin = spec.Stdin
	if len(spec.ExtraEnv) > 0 {
		cmd.Env = append(cmd.Environ(), spec.ExtraEnv...)
	}

	outBuf := newRing(stdoutLimit)
	errBuf := newRing(stderrLimit)
	if spec.Stdout != nil {
		cmd.Stdout = io.MultiWriter(outBuf, spec.Stdout)
	} else {
		cmd.Stdout = outBuf
	}
	if spec.Stderr != nil {
		cmd.Stderr = io.MultiWriter(errBuf, spec.Stderr)
	} else {
		cmd.Stderr = errBuf
	}

	started := time.Now()
	err := cmd.Run()
	completed := time.Now()

	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	timedOut := false
	if cmdCtx.Err() == context.DeadlineExceeded {
		timedOut = true
	}

	rec := Record{
		AuditID:       uuid.NewString(),
		RequestID:     spec.RequestID,
		JobID:         spec.JobID,
		Tool:          spec.Tool,
		Stage:         spec.Stage,
		Executable:    safeExecutableLabel(spec.Executable),
		ArgsRedacted:  r.Sanitizer.RedactArgs(spec.Args),
		EnvRedacted:   r.Sanitizer.RedactEnv(spec.ExtraEnv),
		WorkingDirRed: r.Sanitizer.RedactWorkingDir(spec.WorkingDir),
		StartedAt:     started,
		CompletedAt:   completed,
		DurationMS:    int(completed.Sub(started) / time.Millisecond),
		ExitCode:      exitCode,
		TimedOut:      timedOut,
		Success:       err == nil,
		StdoutTail:    r.Sanitizer.RedactArg(outBuf.String()),
		StderrTail:    r.Sanitizer.RedactArg(errBuf.String()),
	}
	if err != nil {
		rec.ErrorMessage = r.Sanitizer.RedactErrorMessage(err.Error())
	}
	go r.Sink.Insert(context.Background(), rec)

	return Result{
		AuditID:    rec.AuditID,
		Stdout:     outBuf.String(),
		Stderr:     errBuf.String(),
		ExitCode:   exitCode,
		DurationMS: rec.DurationMS,
		TimedOut:   timedOut,
		Success:    err == nil,
		Err:        err,
	}, err
}

// safeExecutableLabel keeps only the basename so an audit row never leaks
// an install-specific absolute path like /opt/foo/venvs/bar/bin/python.
func safeExecutableLabel(exe string) string {
	exe = strings.TrimSpace(exe)
	if exe == "" {
		return ""
	}
	return filepath.Base(exe)
}

// --- bounded ring buffer for stdout/stderr -----------------------------------

type ringBuffer struct {
	buf bytes.Buffer
	max int
}

func newRing(max int) *ringBuffer {
	if max <= 0 {
		max = defaultStdoutTail
	}
	return &ringBuffer{max: max}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	n, err := r.buf.Write(p)
	if r.buf.Len() > r.max {
		excess := r.buf.Len() - r.max
		_ = r.buf.Next(excess)
	}
	return n, err
}

func (r *ringBuffer) String() string {
	return r.buf.String()
}
