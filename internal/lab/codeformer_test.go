package lab

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// The CodeFormer row reuses the product's restore_image_faces.py with
// --model codeformer, byte-for-byte the argv buildFaceRestoreCommand makes, and
// declares its input, output dir and card so the same Spec runs at home or on a
// burst instance.
func TestCodeFormerSpec(t *testing.T) {
	g := newRig(t, true, false)
	m, _ := Get("codeformer")
	sp, err := m.Command(CommandCtx{Cfg: g.r.Cfg, JobID: "j", Params: map[string]any{"fidelity": 0.25, "upscale": float64(4)},
		Inputs: map[string]string{"image": "/up/j/image_a.png"}, OutDir: "/out/j", Card: 1})
	if err != nil {
		t.Fatal(err)
	}
	c := sp.Steps[0]
	want := []string{"/opt/restore_image_faces.py", "--model", "codeformer", "--input", "/up/j/image_a.png", "--out-dir", "/out/j",
		"--upscale", "4", "--fidelity", "0.25", "--models-dir", "/opt/m", "--repos-dir", "/opt/r", "--gpu", "1"}
	if c.Exe != "/opt/py" || strings.Join(c.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("argv %q %q", c.Exe, c.Args)
	}
	if c.Card == nil || c.Card.Local != 1 || len(c.Card.Flags) != 1 || c.Card.Flags[0] != "--gpu" {
		t.Fatalf("card declaration %+v", c.Card)
	}
	if len(sp.Inputs) != 1 || sp.Inputs[0].Local != "/up/j/image_a.png" || len(sp.Outputs) != 1 || sp.Outputs[0].Local != "/out/j" || !sp.Outputs[0].Dir {
		t.Fatalf("paths: in=%v out=%v", sp.Inputs, sp.Outputs)
	}
	if sp.Workload != "lab_codeformer" || sp.Tool != "lab_codeformer" {
		t.Fatalf("%+v", sp)
	}
	if _, err := m.Command(CommandCtx{Cfg: g.r.Cfg, Params: map[string]any{"fidelity": 0.5, "upscale": float64(3)}, Inputs: map[string]string{"image": "/x"}}); err == nil {
		t.Fatal("upscale 3 must be refused by the command too")
	}
	// The row never reads the customer flag.
	if g.r.Cfg.ImageRestoreCodeFormerEnabled {
		t.Fatal("test config must keep IMAGE_RESTORE_CODEFORMER_ENABLED false")
	}
}

// End to end at home with the REAL local path (exec.Local over cmdaudit.Runner)
// and a fixture "python" that behaves like restore_image_faces.py: gate-less
// job -> local run -> declared result, with the customer flag off.
func TestCodeFormerLocalRunWithFixtureImage(t *testing.T) {
	g := newRig(t, true, false)
	dir := t.TempDir()
	fake := filepath.Join(dir, "fakepy.sh")
	// Parses --out-dir and --input like the real script, writes restored.png.
	script := `#!/bin/sh
out=""; inp=""; model=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out-dir) out="$2"; shift 2;;
    --input) inp="$2"; shift 2;;
    --model) model="$2"; shift 2;;
    *) shift;;
  esac
done
echo "fixture: model=$model faces=1"
cp "$inp" "$out/restored.png"
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	g.r.Cfg.AIFaceRestorePython = fake
	runner := cmdaudit.NewRunner(cmdaudit.NewPathSanitizer(g.r.Cfg.UploadDir, g.r.Cfg.OutputDir, g.r.Cfg.TempDir), cmdaudit.NopSink{})
	g.r.Local = exec.AuditRunner(runner)

	req := g.job(t, "fixture-job")
	if err := os.WriteFile(req.Inputs["image"], []byte("\x89PNG fixture face photo"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.r.Run(context.Background(), req)
	st, msg := g.status("fixture-job")
	if st != models.StatusCompleted {
		t.Fatalf("status %s %q", st, msg)
	}
	b, err := os.ReadFile(filepath.Join(g.out, "fixture-job", "restored.png"))
	if err != nil || !strings.Contains(string(b), "fixture face photo") {
		t.Fatalf("result %q %v", b, err)
	}
	j, _ := g.jm.GetJob("fixture-job")
	t.Logf("CodeFormer local run (fixture image): job %s status=%s progress=%d result=%s size=%d bytes; script ran: model=codeformer; IMAGE_RESTORE_CODEFORMER_ENABLED=%v",
		j.ID, j.Status, j.Progress, j.ResultURL, j.ResultSizeBytes, g.r.Cfg.ImageRestoreCodeFormerEnabled)
}

// A model that exits non-zero fails the job with a user-safe message (no path or
// stderr in it).
func TestCodeFormerLocalFailureIsUserSafe(t *testing.T) {
	g := newRig(t, true, false)
	dir := t.TempDir()
	fake := filepath.Join(dir, "failpy.sh")
	_ = os.WriteFile(fake, []byte("#!/bin/sh\necho 'secret /srv/uploads/x trace' 1>&2\nexit 3\n"), 0o755)
	g.r.Cfg.AIFaceRestorePython = fake
	g.r.Local = exec.AuditRunner(cmdaudit.NewRunner(nil, cmdaudit.NopSink{}))
	g.r.Run(context.Background(), g.job(t, "j"))
	st, msg := g.status("j")
	if st != models.StatusFailed || strings.Contains(msg, "secret") || strings.Contains(msg, "/srv") || msg == "" {
		t.Fatalf("%s %q", st, msg)
	}
}
