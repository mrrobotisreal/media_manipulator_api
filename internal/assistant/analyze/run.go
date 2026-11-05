package analyze

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// ErrNoFFmpeg is returned when ffmpeg or ffprobe is not installed.
var ErrNoFFmpeg = errors.New("analyze: ffmpeg is not installed")

// Timeouts per subprocess.
const (
	probeTimeout   = 15 * time.Second
	analyzeTimeout = 90 * time.Second
)

// HasFFmpeg reports whether both binaries are on PATH.
func HasFFmpeg() bool {
	_, e1 := exec.LookPath("ffmpeg")
	_, e2 := exec.LookPath("ffprobe")
	return e1 == nil && e2 == nil
}

// run executes a binary with a timeout and returns stdout and stderr (ffmpeg
// prints its filter reports on stderr).
func run(ctx context.Context, timeout time.Duration, bin string, args ...string) (stdout, stderr []byte, err error) {
	path, lerr := exec.LookPath(bin)
	if lerr != nil {
		return nil, nil, ErrNoFFmpeg
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("analyze: %s timed out", bin)
		}
		return out.Bytes(), errb.Bytes(), fmt.Errorf("analyze: %s failed: %w", bin, err)
	}
	return out.Bytes(), errb.Bytes(), nil
}
