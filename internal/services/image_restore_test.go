package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func ids(in ...string) []models.ImageRestoreModelID {
	out := make([]models.ImageRestoreModelID, len(in))
	for i, s := range in {
		out[i] = models.ImageRestoreModelID(s)
	}
	return out
}

func resultIDs(units []imageRestoreUnit) []string {
	out := make([]string, len(units))
	for i, u := range units {
		out[i] = u.ResultID
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNormalizeImageRestoreModels(t *testing.T) {
	t.Run("splits by kind, dedupes, normalizes case", func(t *testing.T) {
		g, f, err := NormalizeImageRestoreModels([]string{" RealESRGAN ", "HAT", "hat"}, []string{"GFPGAN"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !eqStrings(idStrings(g), []string{"realesrgan", "hat"}) {
			t.Fatalf("general = %v", g)
		}
		if !eqStrings(idStrings(f), []string{"gfpgan"}) {
			t.Fatalf("face = %v", f)
		}
	})
	t.Run("rejects general id in face list", func(t *testing.T) {
		if _, _, err := NormalizeImageRestoreModels(nil, []string{"realesrgan"}); err == nil {
			t.Fatal("expected cross-kind rejection")
		}
	})
	t.Run("rejects preclean id in general list", func(t *testing.T) {
		if _, _, err := NormalizeImageRestoreModels([]string{"fbcnn"}, nil); err == nil {
			t.Fatal("expected cross-kind rejection")
		}
	})
	t.Run("rejects unknown / injection", func(t *testing.T) {
		if _, _, err := NormalizeImageRestoreModels([]string{"realesrgan; rm -rf /"}, nil); err == nil {
			t.Fatal("expected rejection")
		}
	})
}

func TestNormalizeImageRestorePreclean(t *testing.T) {
	// Reversed selection order must normalize to fbcnn → scunet → nafnet.
	p, err := NormalizeImageRestorePreclean([]string{"nafnet", "scunet", "fbcnn"})
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(idStrings(p), []string{"fbcnn", "scunet", "nafnet"}) {
		t.Fatalf("expected fixed run order, got %v", p)
	}
	if _, err := NormalizeImageRestorePreclean([]string{"gfpgan"}); err == nil {
		t.Fatal("expected cross-kind rejection of a face id in preclean")
	}
}

func TestValidateImageRestoreSelection(t *testing.T) {
	if err := ValidateImageRestoreSelection(nil, nil, nil); err == nil {
		t.Fatal("expected error when nothing selected")
	}
	if err := ValidateImageRestoreSelection(ids("fbcnn"), nil, nil); err != nil {
		t.Fatalf("preclean-only should be valid: %v", err)
	}
}

func TestValidateImageRestoreChain(t *testing.T) {
	if err := ValidateImageRestoreChain(true, ids("realesrgan"), nil); err == nil {
		t.Fatal("chain with no face models must fail")
	}
	if err := ValidateImageRestoreChain(true, nil, ids("gfpgan")); err == nil {
		t.Fatal("chain with no general models must fail")
	}
	if err := ValidateImageRestoreChain(true, ids("realesrgan"), ids("gfpgan")); err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}
	if err := ValidateImageRestoreChain(false, nil, nil); err != nil {
		t.Fatalf("chain off should always validate: %v", err)
	}
}

func TestCountImageRestoreOutputs(t *testing.T) {
	// §2.1 worked example: 2 general, 2 face, chain → 2+2+2*2 = 8.
	if got := CountImageRestoreOutputs(0, 2, 2, true); got != 8 {
		t.Fatalf("expected 8, got %d", got)
	}
	if got := CountImageRestoreOutputs(0, 2, 2, false); got != 4 {
		t.Fatalf("chain off expected 4, got %d", got)
	}
	if got := CountImageRestoreOutputs(2, 1, 0, true); got != 3 {
		t.Fatalf("preclean+general expected 3, got %d", got)
	}
}

func TestValidateImageRestoreOutputBudget(t *testing.T) {
	if err := ValidateImageRestoreOutputBudget(12, 12); err != nil {
		t.Fatalf("boundary should pass: %v", err)
	}
	if err := ValidateImageRestoreOutputBudget(13, 12); err == nil {
		t.Fatal("over budget should fail")
	}
}

func TestValidateFBCNNQualityFactor(t *testing.T) {
	for _, ok := range []int{0, 1, 50, 100} {
		if err := ValidateFBCNNQualityFactor(ok); err != nil {
			t.Fatalf("qf %d should pass: %v", ok, err)
		}
	}
	for _, bad := range []int{-1, 101, 200} {
		if err := ValidateFBCNNQualityFactor(bad); err == nil {
			t.Fatalf("qf %d should fail", bad)
		}
	}
}

func TestValidateImageRestoreCrop(t *testing.T) {
	if err := ValidateImageRestoreCrop(nil); err != nil {
		t.Fatalf("nil crop should pass: %v", err)
	}
	if err := ValidateImageRestoreCrop(&models.NormalizedRect{X: 0.1, Y: 0.1, Width: 0.5, Height: 0.5}); err != nil {
		t.Fatalf("valid crop should pass: %v", err)
	}
	bad := []models.NormalizedRect{
		{X: -0.1, Y: 0, Width: 0.5, Height: 0.5},
		{X: 0, Y: 0, Width: 0, Height: 0.5},
		{X: 0.6, Y: 0, Width: 0.5, Height: 0.5}, // x+w > 1
	}
	for _, r := range bad {
		rr := r
		if err := ValidateImageRestoreCrop(&rr); err == nil {
			t.Fatalf("expected rejection for %+v", rr)
		}
	}
}

func TestResolveImageRestoreScale(t *testing.T) {
	cases := []struct {
		scale, h, want int
		wantErr        bool
	}{
		{0, 400, 4, false},
		{0, 540, 4, false},
		{0, 541, 2, false},
		{0, 4000, 2, false},
		{2, 4000, 2, false},
		{4, 4000, 4, false}, // no 1080 ceiling for stills
		{3, 400, 0, true},
	}
	for _, c := range cases {
		got, err := ResolveImageRestoreScale(c.scale, c.h)
		if c.wantErr {
			if err == nil {
				t.Fatalf("scale %d h %d: expected error", c.scale, c.h)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("scale %d h %d: got %d (%v), want %d", c.scale, c.h, got, err, c.want)
		}
	}
}

func TestValidateImageRestoreOutputPixels(t *testing.T) {
	const max = int64(67108864) // 64MP
	if err := ValidateImageRestoreOutputPixels(2000, 2000, 2, max); err != nil {
		t.Fatalf("16MP output should pass: %v", err)
	}
	if err := ValidateImageRestoreOutputPixels(3000, 3000, 4, max); err == nil {
		t.Fatal("144MP output should fail")
	}
}

func TestClampCodeFormerFidelity(t *testing.T) {
	if got := ClampCodeFormerFidelity(0); got != 0.7 {
		t.Fatalf("0 should default to 0.7, got %v", got)
	}
	if got := ClampCodeFormerFidelity(1.5); got != 1 {
		t.Fatalf("clamp high, got %v", got)
	}
	if got := ClampCodeFormerFidelity(-1); got != 0 {
		t.Fatalf("clamp low, got %v", got)
	}
	if got := ClampCodeFormerFidelity(0.4); got != 0.4 {
		t.Fatalf("passthrough, got %v", got)
	}
}

func TestImageRestoreCropToPixels(t *testing.T) {
	t.Run("nil = whole image", func(t *testing.T) {
		x, y, w, h, err := ImageRestoreCropToPixels(nil, 800, 600)
		if err != nil || x != 0 || y != 0 || w != 800 || h != 600 {
			t.Fatalf("got %d,%d,%d,%d (%v)", x, y, w, h, err)
		}
	})
	t.Run("converts and clamps", func(t *testing.T) {
		x, y, w, h, err := ImageRestoreCropToPixels(&models.NormalizedRect{X: 0.5, Y: 0.5, Width: 0.6, Height: 0.6}, 1000, 1000)
		if err != nil {
			t.Fatal(err)
		}
		// x=500,y=500; w would be 600 but clamps to 500; same for h.
		if x != 500 || y != 500 || w != 500 || h != 500 {
			t.Fatalf("got %d,%d,%d,%d", x, y, w, h)
		}
	})
	t.Run("too small rejected", func(t *testing.T) {
		if _, _, _, _, err := ImageRestoreCropToPixels(&models.NormalizedRect{X: 0, Y: 0, Width: 0.01, Height: 0.01}, 1000, 1000); err == nil {
			t.Fatal("expected too-small rejection")
		}
	})
}

func TestOrderImageRestoreOutputsChainWorkedExample(t *testing.T) {
	// §2.1: G={realesrgan, hat}, F={gfpgan, codeformer}, chain ON, no preclean.
	// Selection order is intentionally scrambled to prove run-order normalization.
	units := orderImageRestoreOutputs(nil, ids("hat", "realesrgan"), ids("codeformer", "gfpgan"), true)
	want := []string{
		"realesrgan", "hat",
		"gfpgan_on_original", "gfpgan_on_realesrgan", "gfpgan_on_hat",
		"codeformer_on_original", "codeformer_on_realesrgan", "codeformer_on_hat",
	}
	if !eqStrings(resultIDs(units), want) {
		t.Fatalf("chain expansion mismatch:\n got %v\nwant %v", resultIDs(units), want)
	}
	if len(units) != 8 {
		t.Fatalf("expected exactly 8 units, got %d", len(units))
	}
}

func TestOrderImageRestoreOutputsPrecleanReversed(t *testing.T) {
	// §2.5: P={scunet, fbcnn} reversed, G={realesrgan}, F={} → exactly 3 units.
	units := orderImageRestoreOutputs(ids("scunet", "fbcnn"), ids("realesrgan"), nil, false)
	want := []string{"preclean_fbcnn", "preclean_scunet", "realesrgan"}
	if !eqStrings(resultIDs(units), want) {
		t.Fatalf("preclean ordering mismatch:\n got %v\nwant %v", resultIDs(units), want)
	}
}

func TestBuildPrecleanCommand(t *testing.T) {
	p := imageRestorePaths{PrecleanPython: "/venvs/preclean/bin/python", PrecleanScript: "/scripts/preclean_image.py", ModelsDir: "/models/restore", ReposDir: "/repos"}

	t.Run("fbcnn with qf override", func(t *testing.T) {
		cmd := buildPrecleanCommand(models.ImageRestoreModelFBCNN, p, "/in/original.png", "/out", 1, 10)
		if cmd.Executable != "/venvs/preclean/bin/python" {
			t.Fatalf("exe %q", cmd.Executable)
		}
		joined := strings.Join(cmd.Args, " ")
		for _, want := range []string{"/scripts/preclean_image.py", "--model fbcnn", "--input /in/original.png", "--out-dir /out", "--models-dir /models/restore", "--repos-dir /repos", "--gpu 1", "--fbcnn-qf 10"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("expected %q in %s", want, joined)
			}
		}
	})
	t.Run("fbcnn blind omits qf flag", func(t *testing.T) {
		cmd := buildPrecleanCommand(models.ImageRestoreModelFBCNN, p, "/in.png", "/out", 1, 0)
		if strings.Contains(strings.Join(cmd.Args, " "), "--fbcnn-qf") {
			t.Fatalf("blind QF must omit the flag: %v", cmd.Args)
		}
	})
	t.Run("scunet never gets a qf flag", func(t *testing.T) {
		cmd := buildPrecleanCommand(models.ImageRestoreModelSCUNet, p, "/in.png", "/out", 1, 50)
		if strings.Contains(strings.Join(cmd.Args, " "), "--fbcnn-qf") {
			t.Fatalf("scunet must not get --fbcnn-qf: %v", cmd.Args)
		}
	})
}

func TestBuildFaceRestoreCommand(t *testing.T) {
	p := imageRestorePaths{FacePython: "/venvs/face/bin/python", FaceScript: "/scripts/restore_image_faces.py", ModelsDir: "/models/restore", ReposDir: "/repos"}

	t.Run("gfpgan on original, upscale 4, no fidelity", func(t *testing.T) {
		cmd := buildFaceRestoreCommand(models.ImageRestoreModelGFPGAN, p, "/in.png", "/out", 4, 0.7, 1)
		joined := strings.Join(cmd.Args, " ")
		for _, want := range []string{"--model gfpgan", "--upscale 4", "--gpu 1"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("expected %q in %s", want, joined)
			}
		}
		if strings.Contains(joined, "--fidelity") {
			t.Fatalf("gfpgan must not get --fidelity: %s", joined)
		}
	})
	t.Run("codeformer chained, upscale 1, fidelity formatted to 2dp", func(t *testing.T) {
		cmd := buildFaceRestoreCommand(models.ImageRestoreModelCodeFormer, p, "/in.png", "/out", 1, 0.7, 1)
		joined := strings.Join(cmd.Args, " ")
		for _, want := range []string{"--model codeformer", "--upscale 1", "--fidelity 0.70"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("expected %q in %s", want, joined)
			}
		}
	})
}

func TestBuildImageRestoreGeneralCommandDelegates(t *testing.T) {
	p := imageRestorePaths{RealESRGANBin: "/bin/realesrgan", SRPython: "/venvs/sr/bin/python", FramesScript: "/scripts/restore_frames.py", ModelsDir: "/models/restore", ReposDir: "/repos"}

	re := buildImageRestoreGeneralCommand(models.ImageRestoreModelRealESRGAN, p, "/in", "/out/frames", "/out", 0, 1)
	if re.Executable != "/bin/realesrgan" {
		t.Fatalf("realesrgan exe %q", re.Executable)
	}
	swin := buildImageRestoreGeneralCommand(models.ImageRestoreModelSwinIR, p, "/in", "/out/frames", "/out", 0, 1)
	joined := strings.Join(swin.Args, " ")
	if swin.Executable != "/venvs/sr/bin/python" || !strings.Contains(joined, "--model swinir") || !strings.Contains(joined, "/scripts/restore_frames.py") {
		t.Fatalf("swinir delegation wrong: exe=%q args=%s", swin.Executable, joined)
	}
}

// ---------------------------------------------------------------------------
// Availability / capabilities (path + flag checks)
// ---------------------------------------------------------------------------

func idStrings(in []models.ImageRestoreModelID) []string {
	out := make([]string, len(in))
	for i, id := range in {
		out[i] = string(id)
	}
	return out
}

// imageRestoreTestService builds a service whose paths point at real temp
// files/dirs so availability checks pass deterministically.
func imageRestoreTestService(t *testing.T, codeformerEnabled bool) *ImageRestoreService {
	t.Helper()
	dir := t.TempDir()
	mkFile := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkDir := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	modelsDir := mkDir("models")
	reposDir := mkDir("repos")
	mkFile("models/gfpgan/GFPGANv1.4.pth")
	mkFile("models/codeformer/codeformer.pth")
	mkDir("repos/CodeFormer")
	mkFile("models/fbcnn/fbcnn_color.pth")
	mkDir("repos/FBCNN")
	mkFile("models/scunet/scunet_color_real_psnr.pth")
	mkDir("repos/SCUNet")
	mkFile("models/nafnet/NAFNet-GoPro-width64.pth")
	mkDir("repos/NAFNet")

	cfg := &config.Config{
		ImageRestoreEnabled:           true,
		ImageRestoreCodeFormerEnabled: codeformerEnabled,
		ImageRestoreMaxSourceWidth:    12000,
		ImageRestoreMaxSourceHeight:   12000,
		ImageRestoreMaxOutputPixels:   67108864,
		ImageRestoreMaxOutputs:        12,
		ImageRestoreMaxConcurrentJobs: 1,
		ImageRestoreModelTimeout:      30 * time.Second,
		MaxFileSize:                   50 * 1024 * 1024,
		RealESRGANBin:                 mkFile("bin/realesrgan"),
		AIRestorePython:               mkFile("venvs/sr/python"),
		AIRestoreFramesScript:         mkFile("scripts/restore_frames.py"),
		AIFaceRestorePython:           mkFile("venvs/face/python"),
		AIFaceRestoreScript:           mkFile("scripts/restore_image_faces.py"),
		AIPrecleanPython:              mkFile("venvs/preclean/python"),
		AIPrecleanScript:              mkFile("scripts/preclean_image.py"),
		AIRestoreModelsDir:            modelsDir,
		AIRestoreReposDir:             reposDir,
		ImageRestoreEstSecondsPerMegapixel: map[string]float64{
			"fbcnn": 3, "scunet": 8, "nafnet": 6,
			"realesrgan": 2, "swinir": 12, "hat": 18,
			"gfpgan": 6, "codeformer": 8,
		},
		ImageRestoreVRAMMiB: map[string]int64{
			"fbcnn": 3000, "scunet": 4000, "nafnet": 4000,
			"realesrgan": 3000, "swinir": 9000, "hat": 10000,
			"gfpgan": 5000, "codeformer": 6000,
		},
	}
	return NewImageRestoreService(cfg, NewJobManager(), nil, nil, nil)
}

func TestImageRestoreCodeFormerLicenseGate(t *testing.T) {
	svc := imageRestoreTestService(t, false)
	available, reason := svc.ModelAvailability(models.ImageRestoreModelCodeFormer)
	if available {
		t.Fatal("codeformer must be unavailable with the license flag off")
	}
	if !strings.Contains(reason, "disabled") {
		t.Fatalf("expected disabled reason, got %q", reason)
	}

	svc = imageRestoreTestService(t, true)
	if available, reason := svc.ModelAvailability(models.ImageRestoreModelCodeFormer); !available {
		t.Fatalf("codeformer should be available with flag on + paths present, got %q", reason)
	}
}

func TestImageRestorePrecleanIndependentAvailability(t *testing.T) {
	svc := imageRestoreTestService(t, true)
	// Wipe the preclean venv: preclean models go unavailable, general models stay.
	svc.cfg.AIPrecleanPython = filepath.Join(t.TempDir(), "missing")
	for _, id := range []models.ImageRestoreModelID{models.ImageRestoreModelFBCNN, models.ImageRestoreModelSCUNet, models.ImageRestoreModelNAFNet} {
		if ok, _ := svc.ModelAvailability(id); ok {
			t.Fatalf("%s should be unavailable without the preclean venv", id)
		}
	}
	if ok, reason := svc.ModelAvailability(models.ImageRestoreModelRealESRGAN); !ok {
		t.Fatalf("realesrgan should remain available, got %q", reason)
	}
	if ok, reason := svc.ModelAvailability(models.ImageRestoreModelSwinIR); !ok {
		t.Fatalf("swinir should remain available, got %q", reason)
	}
}

func TestImageRestoreCapabilitiesShape(t *testing.T) {
	svc := imageRestoreTestService(t, true)
	caps := svc.Capabilities()
	if !caps.Enabled || !caps.ChainSupported {
		t.Fatal("expected enabled + chain supported")
	}
	if len(caps.Models) != 8 {
		t.Fatalf("expected 8 models, got %d", len(caps.Models))
	}
	if caps.Models[0].ID != models.ImageRestoreModelFBCNN || caps.Models[0].Kind != models.ImageRestoreKindPreclean {
		t.Fatalf("unexpected first model: %+v", caps.Models[0])
	}
	if got := caps.Models[0].Scales; len(got) != 1 || got[0] != 1 {
		t.Fatalf("preclean should be 1x only, got %v", got)
	}
}

func TestImageRestoreBuildStages(t *testing.T) {
	svc := imageRestoreTestService(t, true)
	units := orderImageRestoreOutputs(ids("fbcnn"), ids("realesrgan"), ids("gfpgan"), false)
	stages := svc.BuildStages(units)
	if stages[0].Key != "queued" || stages[1].Key != "prepare" {
		t.Fatalf("expected queued, prepare first: %+v", stages[:2])
	}
	last := stages[len(stages)-1]
	if last.Key != "completed" || last.Progress != 100 {
		t.Fatalf("expected completed@100 last, got %+v", last)
	}
	// unit stage keys present in order.
	var unitKeys []string
	for _, st := range stages {
		switch st.Key {
		case "queued", "prepare", "package", "completed":
		default:
			unitKeys = append(unitKeys, st.Key)
		}
	}
	want := []string{"preclean_fbcnn", "model_realesrgan", "face_gfpgan_original"}
	if !eqStrings(unitKeys, want) {
		t.Fatalf("unit stage keys: got %v want %v", unitKeys, want)
	}
}
