package exec

import (
	"context"
	"fmt"
	"io"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
)

// Executor runs a Spec to completion.
type Executor interface {
	Run(ctx context.Context, spec Spec) error
}

// StepRunner runs one command. Adapters wrap today's execution paths.
type StepRunner interface {
	RunStep(ctx context.Context, spec *Spec, c Cmd) error
}

// StepRunnerFunc adapts a function.
type StepRunnerFunc func(ctx context.Context, spec *Spec, c Cmd) error

func (f StepRunnerFunc) RunStep(ctx context.Context, spec *Spec, c Cmd) error { return f(ctx, spec, c) }

// Local is today's behaviour: every step goes, unchanged, to the injected
// runner. Validator (optional) runs first, so a tool that forgets a
// declaration fails the same way locally and remotely.
type Local struct {
	Runner    StepRunner
	Validator *Validator
}

// Run executes the steps in order and stops at the first error.
func (l Local) Run(ctx context.Context, spec Spec) error {
	if l.Validator != nil {
		if err := l.Validator.Validate(spec); err != nil {
			return err
		}
	}
	for _, c := range spec.Steps {
		if err := l.Runner.RunStep(ctx, &spec, c); err != nil {
			return err
		}
	}
	return nil
}

// AIExecFunc is the shape of AIService.exec.
type AIExecFunc func(ctx context.Context, label string, env []string, name string, args ...string) error

// AIRunner adapts AIService.exec (and its injectable run func). It cannot
// honour Dir or Stdout, so a Cmd that sets Dir is refused (fail closed); a
// per-step Timeout becomes a context deadline.
func AIRunner(fn AIExecFunc) StepRunner {
	return StepRunnerFunc(func(ctx context.Context, _ *Spec, c Cmd) error {
		if c.Dir != "" {
			return fmt.Errorf("%w: the AI exec path has no working directory", ErrInvalidSpec)
		}
		if c.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.Timeout)
			defer cancel()
		}
		return fn(ctx, c.Label, c.Env, c.Exe, c.Args...)
	})
}

// AuditSpec is the exact cmdaudit.Spec a Cmd maps to.
func AuditSpec(spec *Spec, c Cmd) cmdaudit.Spec {
	var out io.Writer
	if spec.Stdout != nil {
		out = spec.Stdout
	}
	return cmdaudit.Spec{
		Tool: spec.Tool, Stage: c.Label, Executable: c.Exe, Args: c.Args, ExtraEnv: c.Env,
		WorkingDir: c.Dir, Stdout: out, Timeout: c.Timeout, RequestID: spec.RequestID, JobID: spec.JobID,
	}
}

// AuditRunner adapts cmdaudit.Runner.Run.
func AuditRunner(r *cmdaudit.Runner) StepRunner {
	return StepRunnerFunc(func(ctx context.Context, spec *Spec, c Cmd) error {
		_, err := r.Run(ctx, AuditSpec(spec, c))
		return err
	})
}
