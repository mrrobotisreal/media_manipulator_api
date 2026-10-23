package metadata

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// fakeRunner serves canned tool output; no real binary is ever executed.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	// handler returns stdout, stderr, err for a call.
	handler func(ctx context.Context, name string, args []string) (string, string, error)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	return f.handler(ctx, name, args)
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

// fixtureRunner replays testdata/<name>/{exiftool.json,exiftool.n.json,ffprobe.json}.
// A missing file means the tool exits non-zero.
func fixtureRunner(name string) *fakeRunner {
	read := func(file string) (string, bool) {
		b, err := os.ReadFile(filepath.Join("testdata", name, file))
		return string(b), err == nil
	}
	return &fakeRunner{handler: func(_ context.Context, tool string, args []string) (string, string, error) {
		file := "exiftool.json"
		switch {
		case tool == "ffprobe":
			file = "ffprobe.json"
		case hasArg(args, "-n"):
			file = "exiftool.n.json"
		}
		if s, ok := read(file); ok {
			return s, "", nil
		}
		return "", "Error: no fixture " + file + "\nsecond line", &fakeExit{}
	}}
}

type fakeExit struct{}

func (*fakeExit) Error() string { return "exit status 1" }

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }
