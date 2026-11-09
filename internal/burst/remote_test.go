package burst

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/staging"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/worker"
	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
)

// syncBuf is a mutex-guarded buffer shared by the planner, remote and worker loggers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type fakeBinary struct{ hash, key string }

func (f fakeBinary) EnsureUploaded(context.Context) (string, string, error) {
	return f.hash, f.key, nil
}

type fixedResolver struct {
	mu    sync.Mutex
	calls int
	after int // not found until this many calls
}

func (f *fixedResolver) ResolveIP(_ context.Context, host string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.after {
		return "", ErrDeviceNotFound
	}
	return "100.64.0.9", nil
}

type e2e struct {
	rig       *rig
	remote    *Remote
	ub        *UserDataBuilder
	mem       *staging.MemStore
	run       *Run
	srv       *httptest.Server
	w         *worker.Worker
	local     string // API-side data root
	remoteDir string
	logb      *syncBuf
	shutdowns *int
}

const tsSecret = "tskey-auth-E2ESECRET"

func newE2E(t *testing.T, corrupt bool) *e2e {
	t.Helper()
	cfg := testCfg()
	r := newRig(cfg)
	logb := &syncBuf{}
	r.p.Log = slog.New(slog.NewTextHandler(logb, nil))
	r.logb = nil
	hash := strings.Repeat("ab", 32)
	ub := &UserDataBuilder{Cfg: UserDataConfig{Bucket: "bkt", Region: "us-west-2", Tag: "tag:mm-burst", Prefix: "burst/", Port: 7781, MaxMinutes: 120, OrphanMinutes: 10},
		Binary: fakeBinary{hash: hash, key: WorkerKey("burst/", hash)}}
	r.p.UserData = ub.Build
	r.min.key = tsSecret
	mem := staging.NewMemStore()
	r.p.Prefixes = &memPrefixes{mem: mem, prefix: "burst/"}
	run, err := r.p.Launch(context.Background(), Offer{JobID: "job-1", Kind: "remove_background", Tier: "premium"})
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := ub.Token(run.ID)
	if !ok {
		t.Fatal("no token")
	}
	local, remoteDir := t.TempDir(), t.TempDir()
	shut := 0
	w := worker.New(worker.Config{RunID: run.ID, Token: tok, Prefix: "burst/" + run.ID, Root: remoteDir, Version: "e2e", Store: mem,
		Log: slog.New(slog.NewTextHandler(logb, nil)), Shutdown: func() { shut++ }, Workers: 2,
		FreeGiB: func() float64 { return 100 }})
	srv := httptest.NewServer(w.Handler())
	t.Cleanup(srv.Close)
	if corrupt {
		mem.CorruptOnGet = func(key string, b []byte) []byte {
			if strings.Contains(key, "/out/") && !strings.HasSuffix(key, "manifest.json") && len(b) > 0 {
				b[0] ^= 0xff
			}
			return b
		}
	}
	rem := &Remote{Store: mem, Life: r.p, Tokens: ub, Resolver: &fixedResolver{after: 2}, Prefix: "burst/", Port: 7781, RemoteRoot: remoteDir,
		Validator: exec.Validator{Roots: []string{local}}, Sanitizer: cmdaudit.NewPathSanitizer(local, local, local),
		Log: slog.New(slog.NewTextHandler(logb, nil)), PollInterval: 5 * time.Millisecond, BaseURL: func(ip string) string {
			if ip != "100.64.0.9" {
				t.Errorf("unexpected ip %q", ip)
			}
			return srv.URL
		}, Concurrency: 2}
	return &e2e{rig: r, remote: rem, ub: ub, mem: mem, run: run, srv: srv, w: w, local: local, remoteDir: remoteDir, logb: logb, shutdowns: &shut}
}

type memPrefixes struct {
	mem    *staging.MemStore
	prefix string
}

func (m *memPrefixes) DeleteRun(ctx context.Context, id string) error {
	return m.mem.DeletePrefix(ctx, m.prefix+id+"/")
}
func (m *memPrefixes) ListRunIDs(context.Context) ([]string, error) { return nil, nil }

func (e *e2e) spec(t *testing.T, out *bytes.Buffer) (exec.Spec, string) {
	in := filepath.Join(e.local, "in", "src.txt")
	os.MkdirAll(filepath.Dir(in), 0o755)
	os.WriteFile(in, []byte("remote gpu says hi"), 0o644)
	outDir := filepath.Join(e.local, "out", "res")
	var w io.Writer
	if out != nil {
		w = out
	}
	return exec.Spec{Workload: "remove_background", Tool: "t", JobID: "job-1", Stdout: w,
		Steps: []exec.Cmd{
			{Label: "copy", Exe: "sh", Args: []string{"-c", `mkdir -p "$2" && cp "$1" "$2/result.txt" && echo PROGRESS 1/2 && echo PROGRESS 2/2 && echo "$3" 1>&2`, "sh", in, outDir + "/", "x"},
				Env: []string{"CUDA_VISIBLE_DEVICES=3"}, Card: &exec.CardArgs{Local: 3, Env: []string{exec.CardEnvCUDA}}}},
		Inputs: []exec.Path{exec.P(in)}, Outputs: []exec.Path{exec.D(outDir)}}, outDir
}

func TestRemote_E2EFakeTransportRun(t *testing.T) {
	e := newE2E(t, false)
	var progress bytes.Buffer
	spec, outDir := e.spec(t, &progress)
	// "$2" is the dir with a trailing slash: the declared prefix still matches
	// at a path boundary, and the worker sees the remapped directory.
	if err := e.remote.Run(context.Background(), e.run, spec); err != nil {
		t.Fatalf("run: %v\n%s", err, e.logb.String())
	}
	b, err := os.ReadFile(filepath.Join(outDir, "result.txt"))
	if err != nil || string(b) != "remote gpu says hi" {
		t.Fatalf("output: %q %v", b, err)
	}
	if progress.String() != "PROGRESS 1/2\nPROGRESS 2/2\n" {
		t.Fatalf("progress %q", progress.String())
	}
	if e.run.State != StateCompleted || e.run.TerminatedAt == nil || e.run.BytesIn != int64(len("remote gpu says hi")) {
		t.Fatalf("run %+v", e.run)
	}
	if k := e.mem.Keys(); len(k) != 0 {
		t.Fatalf("staging prefix not deleted: %v", k)
	}
	if len(e.rig.prov.terminated) != 1 {
		t.Fatalf("instance not terminated: %v", e.rig.prov.terminated)
	}
	if _, ok := e.ub.Token(e.run.ID); ok {
		t.Fatal("token kept after the run")
	}
	if s := e.logb.String(); strings.Contains(s, tsSecret) || strings.Contains(s, "result.txt") || strings.Contains(s, "src.txt") {
		t.Fatalf("log leaks:\n%s", s)
	}
	t.Logf("progress lines streamed to Spec.Stdout: %q", progress.String())
	t.Logf("in-manifest before cleanup was uploaded to burst/%s/in/manifest.json; out-manifest fetched from burst/%s/out/manifest.json (verified sha256, 1 file, %d bytes)", e.run.ID, e.run.ID, len(b))
	t.Logf("cleanup: staging keys left = %v, instances terminated = %v, run state = %s", e.mem.Keys(), e.rig.prov.terminated, e.run.State)
	t.Logf("e2e log (planner, remote, worker):\n%s", e.logb.String())
}

func TestRemote_E2EOutputCorruptionIsStagingOutFailure(t *testing.T) {
	e := newE2E(t, true)
	spec, outDir := e.spec(t, nil)
	err := e.remote.Run(context.Background(), e.run, spec)
	if !errors.Is(err, ErrStagingOut) || !errors.Is(err, staging.ErrChecksum) && !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want staging_out checksum failure, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(outDir, "result.txt")); serr == nil {
		t.Fatal("a corrupt output was placed")
	}
	if e.run.State != StateToolFailed || e.run.ErrorType != "staging_out" {
		t.Fatalf("%s %s", e.run.State, e.run.ErrorType)
	}
	if k := e.mem.Keys(); len(k) != 0 {
		t.Fatalf("prefix not deleted on failure: %v", k)
	}
	if len(e.rig.prov.terminated) != 1 {
		t.Fatal("instance not terminated")
	}
}

func TestRemote_ToolFailureCarriesRedactedStderrAndDeletesPrefix(t *testing.T) {
	e := newE2E(t, false)
	spec, _ := e.spec(t, nil)
	remoteIn := filepath.Join(e.remoteDir, e.run.ID, "in", "0", "src.txt")
	spec.Steps[0].Args = []string{"-c", "echo failing on " + remoteIn + " token Bearer abcdefgh12345678 1>&2; exit 7"}
	err := e.remote.Run(context.Background(), e.run, spec)
	var tf *ToolFailure
	if !errors.As(err, &tf) || !errors.Is(err, ErrToolFailed) || tf.ExitCode != 7 {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(tf.StderrTail, e.remoteDir) || strings.Contains(tf.StderrTail, "abcdefgh12345678") || !strings.Contains(tf.StderrTail, "<UPLOAD_DIR>") {
		t.Fatalf("stderr tail not redacted: %q", tf.StderrTail)
	}
	if e.run.State != StateToolFailed || len(e.mem.Keys()) != 0 || len(e.rig.prov.terminated) != 1 {
		t.Fatalf("%s %v %v", e.run.State, e.mem.Keys(), e.rig.prov.terminated)
	}
}

func TestRemote_UndeclaredPathNeverReachesTheWorker(t *testing.T) {
	e := newE2E(t, false)
	spec, _ := e.spec(t, nil)
	spec.Steps[0].Args = append(spec.Steps[0].Args, filepath.Join(e.local, "other", "secret.txt"))
	err := e.remote.Run(context.Background(), e.run, spec)
	if !errors.Is(err, exec.ErrUndeclaredPath) || e.mem.PutCount != 0 {
		t.Fatalf("%v puts=%d", err, e.mem.PutCount)
	}
	if len(e.rig.prov.terminated) != 1 {
		t.Fatal("instance not terminated")
	}
}

func TestRemote_WorkerNeverJoinsIsWorkerLost(t *testing.T) {
	e := newE2E(t, false)
	e.remote.Resolver = &fixedResolver{after: 1 << 30}
	e.remote.ResolveTimeout = 50 * time.Millisecond
	spec, _ := e.spec(t, nil)
	err := e.remote.Run(context.Background(), e.run, spec)
	if !errors.Is(err, ErrWorkerLost) || e.run.State != StateWorkerLost {
		t.Fatalf("%v %s", err, e.run.State)
	}
}

func TestRemote_ContextCancelCancelsAndTerminates(t *testing.T) {
	e := newE2E(t, false)
	spec, _ := e.spec(t, nil)
	spec.Steps[0].Args = []string{"-c", "sleep 30"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(400 * time.Millisecond); cancel() }()
	start := time.Now()
	err := e.remote.Run(ctx, e.run, spec)
	if !errors.Is(err, ErrCancelled) || time.Since(start) > 10*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	if len(e.mem.Keys()) != 0 || len(e.rig.prov.terminated) != 1 {
		t.Fatal("cleanup after cancel")
	}
}

func TestRemote_ProgressLinesAreNotDuplicatedAcrossPolls(t *testing.T) {
	e := newE2E(t, false)
	spec, _ := e.spec(t, nil)
	var out bytes.Buffer
	spec.Stdout = &out
	spec.Steps[0].Args = []string{"-c", `for i in 1 2 3 4 5; do echo PROGRESS $i/5; sleep 0.03; done; mkdir -p "$1"; echo ok > "$1/result.txt"`, "sh", filepath.Join(e.local, "out", "res")}
	if err := e.remote.Run(context.Background(), e.run, spec); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "\n"); got != 5 {
		t.Fatalf("lines: %q", out.String())
	}
	_ = fmt.Sprint
}
