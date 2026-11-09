package exec

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
)

type call struct {
	label string
	env   []string
	name  string
	args  []string
}

func TestLocal_AIRunnerIdentity(t *testing.T) {
	var direct, viaLocal []call
	rec := func(dst *[]call) AIExecFunc {
		return func(_ context.Context, label string, env []string, name string, args ...string) error {
			*dst = append(*dst, call{label, env, name, args})
			return nil
		}
	}
	steps := []Cmd{
		{Label: "deepFilter", Exe: "/opt/media-manipulator-ai/bin/deepFilter", Args: []string{"--output-dir", "/srv/temp/j/o", "/srv/temp/j/in.wav"}, Env: []string{"CUDA_VISIBLE_DEVICES=3"}},
		{Label: "demucs", Exe: "demucs", Args: []string{"-n", "htdemucs", "-o", "/srv/outputs/j"}},
	}
	for _, c := range steps {
		_ = rec(&direct)(context.Background(), c.Label, c.Env, c.Exe, c.Args...)
	}
	l := Local{Runner: AIRunner(rec(&viaLocal)), Validator: &Validator{Roots: testRoots}}
	spec := Spec{Steps: steps, Inputs: []Path{P("/srv/temp/j/in.wav")}, Outputs: []Path{D("/srv/temp/j/o"), D("/srv/outputs/j")}}
	if err := l.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct, viaLocal) {
		t.Fatalf("not identical:\n%+v\n%+v", direct, viaLocal)
	}
	// A working directory cannot be honoured by that path: fail closed.
	err := Local{Runner: AIRunner(rec(&viaLocal))}.Run(context.Background(), Spec{Steps: []Cmd{{Exe: "x", Dir: "/tmp"}}})
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("dir accepted: %v", err)
	}
	// Local validates first, like the remote executor.
	err = l.Run(context.Background(), Spec{Steps: []Cmd{{Exe: "x", Args: []string{"/srv/uploads/undeclared"}}}})
	if !errors.Is(err, ErrUndeclaredPath) || len(viaLocal) != len(direct) {
		t.Fatalf("undeclared path ran: %v", err)
	}
}

type sinkRec struct {
	mu   sync.Mutex
	recs []cmdaudit.Record
}

func (s *sinkRec) Insert(_ context.Context, r cmdaudit.Record) {
	s.mu.Lock()
	s.recs = append(s.recs, r)
	s.mu.Unlock()
}
func (s *sinkRec) wait(n int) []cmdaudit.Record {
	for i := 0; i < 200; i++ {
		s.mu.Lock()
		if len(s.recs) >= n {
			out := append([]cmdaudit.Record(nil), s.recs...)
			s.mu.Unlock()
			return out
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

func TestLocal_AuditRunnerIdentity(t *testing.T) {
	san := cmdaudit.NewPathSanitizer("/srv/uploads", "/srv/outputs", "/srv/temp")
	var outDirect, outLocal bytes.Buffer
	sd, sl := &sinkRec{}, &sinkRec{}
	cs := cmdaudit.Spec{Tool: "t", Stage: "s", Executable: "sh", Args: []string{"-c", "echo PROGRESS 1/2; echo $K 1>&2"}, ExtraEnv: []string{"K=v"},
		WorkingDir: "/tmp", Stdout: &outDirect, Timeout: 10 * time.Second, RequestID: "r", JobID: "j"}
	if _, err := cmdaudit.NewRunner(san, sd).Run(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Tool: "t", RequestID: "r", JobID: "j", Stdout: &outLocal,
		Steps: []Cmd{{Label: "s", Exe: "sh", Args: []string{"-c", "echo PROGRESS 1/2; echo $K 1>&2"}, Env: []string{"K=v"}, Dir: "/tmp", Timeout: 10 * time.Second}}}
	if got := AuditSpec(&spec, spec.Steps[0]); !reflect.DeepEqual(got, withoutWriter(cs, got)) {
		t.Fatalf("audit spec differs:\n%+v\n%+v", got, cs)
	}
	if err := (Local{Runner: AuditRunner(cmdaudit.NewRunner(san, sl))}).Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if outDirect.String() != outLocal.String() || !strings.Contains(outLocal.String(), "PROGRESS 1/2") {
		t.Fatalf("stdout %q vs %q", outDirect.String(), outLocal.String())
	}
	a, b := sd.wait(1), sl.wait(1)
	if len(a) != 1 || len(b) != 1 {
		t.Fatal("no audit record")
	}
	x, y := a[0], b[0]
	x.AuditID, y.AuditID = "", ""
	x.StartedAt, y.StartedAt, x.CompletedAt, y.CompletedAt, x.DurationMS, y.DurationMS = time.Time{}, time.Time{}, time.Time{}, time.Time{}, 0, 0
	if !reflect.DeepEqual(x, y) {
		t.Fatalf("records differ:\n%+v\n%+v", x, y)
	}
}

// withoutWriter lets DeepEqual ignore the distinct Stdout buffers.
func withoutWriter(want, got cmdaudit.Spec) cmdaudit.Spec {
	want.Stdout = got.Stdout
	return want
}
