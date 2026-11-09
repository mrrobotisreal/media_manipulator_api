package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/staging"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/wire"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type clock struct{ t atomic.Int64 }

func newClock() *clock               { c := &clock{}; c.t.Store(t0.UnixNano()); return c }
func (c *clock) Now() time.Time      { return time.Unix(0, c.t.Load()).UTC() }
func (c *clock) Add(d time.Duration) { c.t.Add(int64(d)) }

const token = "run-token-SECRET"

type rig struct {
	w    *Worker
	srv  *httptest.Server
	clk  *clock
	mem  *staging.MemStore
	root string
	shut *atomic.Int32
	logb *bytes.Buffer
}

func newRig(t *testing.T, free float64) *rig {
	t.Helper()
	root := t.TempDir()
	clk, mem, shut, logb := newClock(), staging.NewMemStore(), &atomic.Int32{}, &bytes.Buffer{}
	w := New(Config{RunID: "run1", Token: token, Prefix: "burst/run1", Root: root, Version: "test", Store: mem, Now: clk.Now,
		Log: slog.New(slog.NewTextHandler(logb, nil)), Shutdown: func() { shut.Add(1) }, FreeGiB: func() float64 { return free },
		GPU: func() wire.GPUInfo { return wire.GPUInfo{Name: "L4", MemMiB: 24000} }, Workers: 2})
	srv := httptest.NewServer(w.Handler())
	t.Cleanup(srv.Close)
	return &rig{w, srv, clk, mem, root, shut, logb}
}

func (r *rig) do(t *testing.T, method, uri string, body any, mutate func(*http.Request)) (*http.Response, []byte) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, r.srv.URL+uri, bytes.NewReader(b))
	wire.SignRequest(req, token, b, r.clk.Now())
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return resp, out.Bytes()
}

func TestWorker_HMACWrongOrExpiredIs401(t *testing.T) {
	r := newRig(t, 100)
	if resp, _ := r.do(t, "GET", "/v1/health", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("good signature: %d", resp.StatusCode)
	}
	bad := map[string]func(*http.Request){
		"wrong sig":  func(q *http.Request) { q.Header.Set(wire.HeaderSig, strings.Repeat("0", 64)) },
		"no sig":     func(q *http.Request) { q.Header.Del(wire.HeaderSig) },
		"no ts":      func(q *http.Request) { q.Header.Del(wire.HeaderTs) },
		"wrong path": func(q *http.Request) { q.URL.Path = "/v1/runs/x"; q.RequestURI = "" },
		"old ts": func(q *http.Request) {
			old := r.clk.Now().Add(-2 * time.Minute)
			wire.SignRequest(q, token, nil, old)
		},
		"future ts":   func(q *http.Request) { wire.SignRequest(q, token, nil, r.clk.Now().Add(2*time.Minute)) },
		"other token": func(q *http.Request) { wire.SignRequest(q, "other", nil, r.clk.Now()) },
	}
	for name, m := range bad {
		if resp, _ := r.do(t, "GET", "/v1/health", nil, m); resp.StatusCode != 401 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	// Body tampering after signing.
	b := []byte(`{"runId":"run1"}`)
	req, _ := http.NewRequest("POST", r.srv.URL+"/v1/runs", bytes.NewReader([]byte(`{"runId":"evil"}`)))
	wire.SignRequest(req, token, b, r.clk.Now())
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("tampered body: %d", resp.StatusCode)
	}
	// Within skew passes.
	if resp, _ := r.do(t, "GET", "/v1/health", nil, func(q *http.Request) { wire.SignRequest(q, token, nil, r.clk.Now().Add(-59*time.Second)) }); resp.StatusCode != 200 {
		t.Errorf("59s skew: %d", resp.StatusCode)
	}
}

func (r *rig) req(steps ...wire.Cmd) wire.RunRequest {
	base := path.Join(r.root, "run1")
	return wire.RunRequest{RunID: "run1", Prefix: "burst/run1", Steps: steps,
		Inputs:  []wire.PathSpec{{N: 0, Remote: base + "/in/0/in.txt"}},
		Outputs: []wire.PathSpec{{N: 0, Remote: base + "/out/0/out.txt"}}, InputBytes: 5}
}

func (r *rig) stageInput(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "in.txt")
	os.WriteFile(p, []byte(body), 0o644)
	if _, err := staging.Upload(context.Background(), r.mem, "burst/run1", "in", []staging.Source{{N: 0, Local: p}}, 1); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) waitTerminal(t *testing.T) wire.Status {
	t.Helper()
	for i := 0; i < 400; i++ {
		_, b := r.do(t, "GET", "/v1/runs/run1", nil, nil)
		var st wire.Status
		json.Unmarshal(b, &st)
		if st.Terminal() {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run never finished")
	return wire.Status{}
}

func TestWorker_RunThenSecondRunIs409(t *testing.T) {
	r := newRig(t, 100)
	r.stageInput(t, "hello")
	req := r.req(wire.Cmd{Exe: "sh", Args: []string{"-c", "cp " + r.root + "/run1/in/0/in.txt " + r.root + "/run1/out/0/out.txt; echo PROGRESS 1/1"}})
	if resp, b := r.do(t, "POST", "/v1/runs", req, nil); resp.StatusCode != 202 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if resp, _ := r.do(t, "POST", "/v1/runs", req, nil); resp.StatusCode != 409 {
		t.Fatalf("second run while running: %d", resp.StatusCode)
	}
	st := r.waitTerminal(t)
	if st.State != wire.StateDone || len(st.Lines) != 1 || st.Lines[0] != "PROGRESS 1/1" {
		t.Fatalf("%+v", st)
	}
	// Still 409 after it finished: one spec per instance.
	if resp, _ := r.do(t, "POST", "/v1/runs", req, nil); resp.StatusCode != 409 {
		t.Fatalf("second run after done: %d", resp.StatusCode)
	}
	m, err := staging.FetchManifest(context.Background(), r.mem, "burst/run1", "out")
	if err != nil || len(m.Files) != 1 || m.Files[0].Size != 5 {
		t.Fatalf("%+v %v", m, err)
	}
	// Cursor returns only the new lines.
	_, b := r.do(t, "GET", "/v1/runs/run1?cursor=1", nil, nil)
	var st2 wire.Status
	json.Unmarshal(b, &st2)
	if len(st2.Lines) != 0 || st2.Cursor != 1 {
		t.Fatalf("%+v", st2)
	}
}

func TestWorker_FailedStepReportsStderrAndExit(t *testing.T) {
	r := newRig(t, 100)
	r.stageInput(t, "hello")
	req := r.req(wire.Cmd{Exe: "sh", Args: []string{"-c", "echo boom 1>&2; exit 3"}})
	r.do(t, "POST", "/v1/runs", req, nil)
	st := r.waitTerminal(t)
	if st.State != wire.StateFailed || st.ExitCode != 3 || st.Phase != wire.PhaseRunning || !strings.Contains(st.StderrTail, "boom") {
		t.Fatalf("%+v", st)
	}
}

func TestWorker_RejectsPathsOutsideRunDir(t *testing.T) {
	r := newRig(t, 100)
	req := r.req(wire.Cmd{Exe: "true"})
	req.Outputs[0].Remote = "/etc/passwd"
	if resp, _ := r.do(t, "POST", "/v1/runs", req, nil); resp.StatusCode != 400 {
		t.Fatalf("%d", resp.StatusCode)
	}
	req = r.req(wire.Cmd{Exe: "true"})
	req.RunID = "other"
	if resp, _ := r.do(t, "POST", "/v1/runs", req, nil); resp.StatusCode != 400 {
		t.Fatalf("%d", resp.StatusCode)
	}
	// A refused request does not consume the single run slot.
	r.stageInput(t, "hello")
	good := r.req(wire.Cmd{Exe: "sh", Args: []string{"-c", "cp " + r.root + "/run1/in/0/in.txt " + r.root + "/run1/out/0/out.txt"}})
	if resp, _ := r.do(t, "POST", "/v1/runs", good, nil); resp.StatusCode != 202 {
		t.Fatalf("%d", resp.StatusCode)
	}
	r.waitTerminal(t)
}

func TestWorker_DiskGuard(t *testing.T) {
	r := newRig(t, 2) // 2 GiB free
	req := r.req(wire.Cmd{Exe: "true"})
	req.InputBytes = 1 << 30 // needs 3 GiB
	if resp, _ := r.do(t, "POST", "/v1/runs", req, nil); resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("%d", resp.StatusCode)
	}
	req.InputBytes = 600 << 20 // needs ~1.76 GiB
	r.stageInput(t, "x")
	req.Steps = []wire.Cmd{{Exe: "sh", Args: []string{"-c", "echo a > " + r.root + "/run1/out/0/out.txt"}}}
	if resp, b := r.do(t, "POST", "/v1/runs", req, nil); resp.StatusCode != 202 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	r.waitTerminal(t)
}

func TestWorker_OrphanTimerFires(t *testing.T) {
	r := newRig(t, 100)
	if r.w.CheckOrphan(r.clk.Now()) {
		t.Fatal("fired at once")
	}
	r.clk.Add(9 * time.Minute)
	// An authenticated request resets the clock; an unauthenticated one does not.
	r.do(t, "GET", "/v1/health", nil, nil)
	r.clk.Add(9 * time.Minute)
	if r.w.CheckOrphan(r.clk.Now()) {
		t.Fatal("fired despite a recent authenticated request")
	}
	r.do(t, "GET", "/v1/health", nil, func(q *http.Request) { q.Header.Set(wire.HeaderSig, "bad") })
	r.clk.Add(2 * time.Minute)
	if !r.w.CheckOrphan(r.clk.Now()) || r.shut.Load() != 1 {
		t.Fatal("orphan timer did not fire")
	}
	if r.w.CheckOrphan(r.clk.Now().Add(time.Hour)) || r.shut.Load() != 1 {
		t.Fatal("fired twice")
	}
}

func TestWorker_FinishedRunArmsFiveMinuteShutdown(t *testing.T) {
	r := newRig(t, 100)
	r.stageInput(t, "hello")
	r.do(t, "POST", "/v1/runs", r.req(wire.Cmd{Exe: "sh", Args: []string{"-c", "cp " + r.root + "/run1/in/0/in.txt " + r.root + "/run1/out/0/out.txt"}}), nil)
	r.waitTerminal(t)
	time.Sleep(50 * time.Millisecond) // finish() runs after the state flips
	r.clk.Add(4 * time.Minute)
	r.do(t, "GET", "/v1/runs/run1", nil, nil) // keeps the orphan clock fresh
	if r.w.CheckOrphan(r.clk.Now()) {
		t.Fatal("fired before 5 minutes")
	}
	r.clk.Add(61 * time.Second)
	r.do(t, "GET", "/v1/runs/run1", nil, nil)
	if !r.w.CheckOrphan(r.clk.Now()) || r.shut.Load() != 1 {
		t.Fatal("finished-run shutdown did not fire")
	}
}

func TestWorker_LogsNeverCarryFilenamesOrToken(t *testing.T) {
	r := newRig(t, 100)
	r.stageInput(t, "hello")
	r.do(t, "POST", "/v1/runs", r.req(wire.Cmd{Exe: "sh", Args: []string{"-c", "echo secretfilename.mp4 1>&2; exit 1"}}), nil)
	r.waitTerminal(t)
	if s := r.logb.String(); strings.Contains(s, token) || strings.Contains(s, "secretfilename") || strings.Contains(s, "in.txt") || strings.Contains(s, "out.txt") {
		t.Fatalf("log leaks: %s", s)
	}
}

func TestWorker_Health(t *testing.T) {
	r := newRig(t, 12.5)
	_, b := r.do(t, "GET", "/v1/health", nil, nil)
	var h wire.Health
	json.Unmarshal(b, &h)
	if !h.Ready || h.GPU.Name != "L4" || h.Disk.ScratchFreeGiB != 12.5 || h.WorkerVersion != "test" || h.Protocol != wire.ProtocolVersion {
		t.Fatalf("%+v", h)
	}
	r.w.cfg.PrewarmDone = func() bool { return false }
	_, b = r.do(t, "GET", "/v1/health", nil, nil)
	json.Unmarshal(b, &h)
	if h.Ready {
		t.Fatal("ready before prewarm")
	}
	r.clk.Add(ReadyAfter)
	_, b = r.do(t, "GET", "/v1/health", nil, nil)
	json.Unmarshal(b, &h)
	if !h.Ready {
		t.Fatal("not ready after 5 minutes")
	}
}
