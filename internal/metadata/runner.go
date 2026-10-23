package metadata

import (
	"bytes"
	"context"
	"os/exec"
)

// Runner executes an external tool. Tests supply a fake; production uses
// execRunner. Implementations return the context error when ctx is done.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

type execRunner struct{}

// Run mirrors services.runCommand: a context error wins over the exit error so
// callers can tell a timeout from a failure.
func (execRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}
