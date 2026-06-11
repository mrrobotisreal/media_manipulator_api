package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func TestBuildStagesPerSelection(t *testing.T) {
	svc := restoreTestService(t, true)

	t.Run("single model", func(t *testing.T) {
		stages := svc.BuildStages([]models.RestoreModelID{models.RestoreModelRealESRGAN})
		wantKeys := []string{"queued", "download", "probe", "extract_clip", "extract_frames", "model_realesrgan", "package", "upload_result", "completed"}
		if len(stages) != len(wantKeys) {
			t.Fatalf("expected %d stages, got %d: %+v", len(wantKeys), len(stages), stages)
		}
		for i, k := range wantKeys {
			if stages[i].Key != k {
				t.Fatalf("stage %d: expected key %q, got %q", i, k, stages[i].Key)
			}
			if stages[i].Status != models.StageStatusPending {
				t.Fatalf("stage %q: expected pending, got %s", k, stages[i].Status)
			}
		}
	})

	t.Run("all six in run order", func(t *testing.T) {
		stages := svc.BuildStages(models.AllRestoreModelIDs)
		var modelKeys []string
		for _, st := range stages {
			if strings.HasPrefix(st.Key, "model_") {
				modelKeys = append(modelKeys, st.Key)
			}
		}
		want := []string{"model_realesrgan", "model_basicvsrpp", "model_swinir", "model_rvrt", "model_hat", "model_vrt"}
		if len(modelKeys) != len(want) {
			t.Fatalf("expected %v, got %v", want, modelKeys)
		}
		for i := range want {
			if modelKeys[i] != want[i] {
				t.Fatalf("expected run order %v, got %v", want, modelKeys)
			}
		}
		// Checkpoints are monotonically increasing and capped by the span end.
		prev := restoreModelSpanStart
		for _, st := range stages {
			if !strings.HasPrefix(st.Key, "model_") {
				continue
			}
			if st.Progress < prev || st.Progress > restoreModelSpanEnd {
				t.Fatalf("stage %s checkpoint %d out of order/range (prev %d)", st.Key, st.Progress, prev)
			}
			prev = st.Progress
		}
		if last := stages[len(stages)-1]; last.Key != "completed" || last.Progress != 100 {
			t.Fatalf("expected trailing completed@100 stage, got %+v", last)
		}
	})

	t.Run("expensive model gets bigger span share", func(t *testing.T) {
		stages := svc.BuildStages([]models.RestoreModelID{models.RestoreModelRealESRGAN, models.RestoreModelVRT})
		re := stageCheckpoint(stages, "model_realesrgan")
		vrt := stageCheckpoint(stages, "model_vrt")
		reSpan := re - restoreModelSpanStart
		vrtSpan := vrt - re
		if vrtSpan <= reSpan {
			t.Fatalf("expected VRT (7.0 spf) to get a larger progress span than Real-ESRGAN (0.8 spf): re=%d vrt=%d", reSpan, vrtSpan)
		}
	})
}

func TestBuildRestoreStitchArgs(t *testing.T) {
	t.Run("with audio", func(t *testing.T) {
		args := buildRestoreStitchArgs("30000/1001", "/w/out/hat/frames", "/w/clip.mp4", true, false, "/r/hat/hat_x4.mp4")
		joined := strings.Join(args, " ")
		for _, want := range []string{
			"-framerate 30000/1001",
			"-i /w/out/hat/frames/%06d.png",
			"-i /w/clip.mp4",
			"-map 0:v",
			"-map 1:a",
			"-c:a aac",
			"-c:v libx264",
			"-crf 16",
			"-preset slow",
			"-pix_fmt yuv420p",
			"-movflags +faststart",
			"-shortest",
		} {
			if !strings.Contains(joined, want) {
				t.Fatalf("expected stitch args to contain %q, got: %s", want, joined)
			}
		}
		if strings.Contains(joined, "-vf") {
			t.Fatalf("did not expect a scale filter at x4: %s", joined)
		}
	})

	t.Run("no audio omits audio mapping", func(t *testing.T) {
		args := buildRestoreStitchArgs("25/1", "/w/f", "/w/clip.mp4", false, false, "/r/o.mp4")
		joined := strings.Join(args, " ")
		for _, banned := range []string{"-map", "-c:a", "clip.mp4"} {
			if strings.Contains(joined, banned) {
				t.Fatalf("expected no audio inputs in %s", joined)
			}
		}
	})

	t.Run("x2 path adds lanczos downscale", func(t *testing.T) {
		args := buildRestoreStitchArgs("24/1", "/w/f", "/w/clip.mp4", true, true, "/r/o.mp4")
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-vf scale=iw/2:ih/2:flags=lanczos") {
			t.Fatalf("expected lanczos downscale filter, got: %s", joined)
		}
	})

	t.Run("fps fraction passes through verbatim", func(t *testing.T) {
		args := buildRestoreStitchArgs("24000/1001", "/f", "/c", false, false, "/o")
		for i, a := range args {
			if a == "-framerate" {
				if args[i+1] != "24000/1001" {
					t.Fatalf("expected exact fraction 24000/1001, got %q", args[i+1])
				}
				return
			}
		}
		t.Fatal("missing -framerate flag")
	})
}

func TestBuildRestoreClipArgs(t *testing.T) {
	args := buildRestoreClipArgs("/u/source.mp4", 12.4, 10, "/w/clip.mp4")
	joined := strings.Join(args, " ")
	// -ss must come BEFORE -i for fast, frame-accurate (re-encoded) seeking.
	ssIdx := strings.Index(joined, "-ss 12.400")
	inIdx := strings.Index(joined, "-i /u/source.mp4")
	if ssIdx == -1 || inIdx == -1 || ssIdx > inIdx {
		t.Fatalf("expected -ss before -i, got: %s", joined)
	}
	for _, want := range []string{"-t 10.000", "-c:v libx264", "-crf 10", "-preset medium", "-pix_fmt yuv420p", "-c:a aac", "-movflags +faststart"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected clip args to contain %q, got: %s", want, joined)
		}
	}
}

func TestBuildRestoreModelCommands(t *testing.T) {
	paths := restoreModelPaths{
		RealESRGANBin: "/bin/realesrgan",
		Python:        "/venvs/sr/bin/python",
		MMPython:      "/venvs/mm/bin/python",
		FramesScript:  "/scripts/restore_frames.py",
		VideoScript:   "/scripts/restore_video.py",
		ModelsDir:     "/models/restore",
		ReposDir:      "/repos",
	}

	t.Run("realesrgan uses binary in directory mode", func(t *testing.T) {
		cmd := buildRestoreModelCommand(models.RestoreModelRealESRGAN, paths, "/in", "/out/frames", "/out", 0, 1)
		if cmd.Executable != "/bin/realesrgan" {
			t.Fatalf("unexpected exe %q", cmd.Executable)
		}
		joined := strings.Join(cmd.Args, " ")
		for _, want := range []string{"-i /in", "-o /out/frames", "-n realesrgan-x4plus", "-s 4", "-g 1", "-t 256", "-f png"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("expected %q in %s", want, joined)
			}
		}
		if !strings.Contains(strings.Join(cmd.ExtraEnv, " "), "VK_ICD_FILENAMES") {
			t.Fatalf("expected vulkan env, got %v", cmd.ExtraEnv)
		}
	})

	t.Run("swinir and hat use the frames script", func(t *testing.T) {
		for _, id := range []models.RestoreModelID{models.RestoreModelSwinIR, models.RestoreModelHAT} {
			cmd := buildRestoreModelCommand(id, paths, "/in", "/out/frames", "/out", 0, 1)
			if cmd.Executable != "/venvs/sr/bin/python" {
				t.Fatalf("%s: unexpected exe %q", id, cmd.Executable)
			}
			joined := strings.Join(cmd.Args, " ")
			for _, want := range []string{"/scripts/restore_frames.py", "--model " + string(id), "--tile 320", "--tile-overlap 32", "--gpu 0", "--models-dir /models/restore", "--repos-dir /repos"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("%s: expected %q in %s", id, want, joined)
				}
			}
			if !strings.Contains(strings.Join(cmd.ExtraEnv, " "), "CUDA_VISIBLE_DEVICES=0") {
				t.Fatalf("%s: expected CUDA_VISIBLE_DEVICES, got %v", id, cmd.ExtraEnv)
			}
		}
	})

	t.Run("basicvsrpp uses the mm python", func(t *testing.T) {
		cmd := buildRestoreModelCommand(models.RestoreModelBasicVSRPP, paths, "/in", "/out/frames", "/out", 0, 1)
		if cmd.Executable != "/venvs/mm/bin/python" {
			t.Fatalf("unexpected exe %q", cmd.Executable)
		}
		if !strings.Contains(strings.Join(cmd.Args, " "), "--max-seq-len 16") {
			t.Fatalf("expected --max-seq-len 16 in %v", cmd.Args)
		}
	})

	t.Run("rvrt and vrt tile settings differ", func(t *testing.T) {
		rvrt := strings.Join(buildRestoreModelCommand(models.RestoreModelRVRT, paths, "/in", "/of", "/out", 0, 1).Args, " ")
		vrt := strings.Join(buildRestoreModelCommand(models.RestoreModelVRT, paths, "/in", "/of", "/out", 0, 1).Args, " ")
		if !strings.Contains(rvrt, "--tile 30,128,128") || !strings.Contains(rvrt, "--tile-overlap 2,20,20") {
			t.Fatalf("rvrt tiles wrong: %s", rvrt)
		}
		if !strings.Contains(vrt, "--tile 12,128,128") {
			t.Fatalf("vrt tiles wrong: %s", vrt)
		}
	})
}

func TestParseRestoreProgressLine(t *testing.T) {
	tests := []struct {
		line     string
		done     int
		total    int
		ok       bool
	}{
		{"PROGRESS 5/100\n", 5, 100, true},
		{"  PROGRESS 100/100", 100, 100, true},
		{"PROGRESS 150/100", 100, 100, true}, // clamped
		{"PROGRESS 5 of 100", 0, 0, false},
		{"progress 5/100", 0, 0, false},
		{"PROGRESS x/y", 0, 0, false},
		{"PROGRESS 5/0", 0, 0, false},
		{"+ ffmpeg -y ...", 0, 0, false},
		{`{"stage": "probe"}`, 0, 0, false},
	}
	for _, tt := range tests {
		done, total, ok := parseRestoreProgressLine(tt.line)
		if ok != tt.ok || done != tt.done || total != tt.total {
			t.Fatalf("parse %q: got (%d,%d,%v), want (%d,%d,%v)", tt.line, done, total, ok, tt.done, tt.total, tt.ok)
		}
	}
}

func TestRestoreProgressWriterHandlesChunkedWrites(t *testing.T) {
	var updates [][2]int
	w := newRestoreProgressWriter(func(done, total int) { updates = append(updates, [2]int{done, total}) })
	// A PROGRESS line split across writes, with noise in between.
	_, _ = w.Write([]byte("loading weights...\nPROG"))
	_, _ = w.Write([]byte("RESS 1/3\nsome noise\nPROGRESS 2/3\nPRO"))
	_, _ = w.Write([]byte("GRESS 3/3\n"))
	if len(updates) != 3 {
		t.Fatalf("expected 3 updates, got %v", updates)
	}
	for i, want := range [][2]int{{1, 3}, {2, 3}, {3, 3}} {
		if updates[i] != want {
			t.Fatalf("update %d: got %v, want %v", i, updates[i], want)
		}
	}
}

func TestWriteRestoreManifestAndReadme(t *testing.T) {
	dir := t.TempDir()
	m := restoreManifest{
		JobID:       "job-123",
		GeneratedAt: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		Request: restoreManifestRequest{
			FileName: "old_family_video.mp4", ClipStartSeconds: 12.4, ClipEndSeconds: 22.4,
			RequestedScale: 0, EffectiveScale: 4, IncludeFrames: true,
			Models: []string{"realesrgan", "vrt"},
		},
		Source: restoreManifestSource{Width: 640, Height: 480, FPS: 29.97, FrameRateFraction: "30000/1001", DurationSeconds: 60, HasAudio: true, ClipFrames: 300},
		Models: []restoreModelOutcome{
			{ID: "realesrgan", Group: "frame", Status: "completed", DurationSeconds: 240, OutputWidth: 2560, OutputHeight: 1920, OutputFile: "restoration_results/realesrgan/realesrgan_x4.mp4", FramesIncluded: true},
			{ID: "vrt", Group: "video", Status: "failed", Error: "Model timed out — try a shorter clip"},
		},
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := writeJSON(manifestPath, m); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed restoreManifest
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if parsed.Source.FrameRateFraction != "30000/1001" {
		t.Fatalf("expected exact fps fraction in manifest, got %q", parsed.Source.FrameRateFraction)
	}
	if parsed.Models[1].Status != "failed" || parsed.Models[1].Error == "" {
		t.Fatalf("expected failure recorded in manifest, got %+v", parsed.Models[1])
	}

	readmePath := filepath.Join(dir, "README.txt")
	if err := writeRestoreReadme(readmePath, m); err != nil {
		t.Fatalf("writeRestoreReadme: %v", err)
	}
	readme, _ := os.ReadFile(readmePath)
	text := string(readme)
	for _, want := range []string{"manifest.json", "original/clip.mp4", "realesrgan/", "vrt/", "FAILED.txt"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected README to mention %q:\n%s", want, text)
		}
	}
}

func TestWriteRestoreFailedMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vrt")
	writeRestoreFailedMarker(dir, "Model timed out — try a shorter clip")
	raw, err := os.ReadFile(filepath.Join(dir, "FAILED.txt"))
	if err != nil {
		t.Fatalf("expected FAILED.txt: %v", err)
	}
	if !strings.Contains(string(raw), "timed out") {
		t.Fatalf("expected reason in FAILED.txt, got %q", string(raw))
	}
}

func TestTrimRestoreFrameOverflow(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 10; i++ {
		name := filepath.Join(dir, fmtFrameName(i))
		if err := os.WriteFile(name, []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	count, err := trimRestoreFrameOverflow(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	if count != 7 {
		t.Fatalf("expected 7 frames after trim, got %d", count)
	}
	names, _ := listRestorePNGs(dir)
	if len(names) != 7 || names[len(names)-1] != fmtFrameName(7) {
		t.Fatalf("expected tail trimmed, got %v", names)
	}
	// Under the cap: untouched.
	count, err = trimRestoreFrameOverflow(dir, 450)
	if err != nil || count != 7 {
		t.Fatalf("expected 7 frames untouched, got %d (%v)", count, err)
	}
}

func fmtFrameName(i int) string {
	return fmt.Sprintf("%06d.png", i)
}

func TestRestoreSemaphoreQueueing(t *testing.T) {
	svc := restoreTestService(t, true) // RestoreMaxConcurrentJobs = 1

	ctx := context.Background()
	release1, err := svc.acquireRestorePermit(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Second acquire must block until the first releases.
	got := make(chan struct{})
	go func() {
		release2, err := svc.acquireRestorePermit(ctx)
		if err == nil {
			release2()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second job acquired a permit while the first held it")
	case <-time.After(100 * time.Millisecond):
	}

	release1()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("second job never acquired the permit after release")
	}

	// Cancellation while queued returns an error instead of hanging.
	release3, err := svc.acquireRestorePermit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release3()
	cancelCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := svc.acquireRestorePermit(cancelCtx); err == nil {
		t.Fatal("expected ctx error while queued")
	}

	// Double-release must not over-fill the pool.
	release1()
	release1()
	if len(svc.permits) > cap(svc.permits) {
		t.Fatal("permit pool overfilled")
	}
}

func TestRestoreJobFailedEntirely(t *testing.T) {
	ok := restoreModelOutcome{Status: "completed"}
	bad := restoreModelOutcome{Status: "failed", Error: "Model failed while processing the clip"}

	if restoreJobFailedEntirely([]restoreModelOutcome{bad, ok, bad}) {
		t.Fatal("1 of 3 succeeded — the job must NOT fail")
	}
	if !restoreJobFailedEntirely([]restoreModelOutcome{bad, bad, bad}) {
		t.Fatal("all models failed — the job MUST fail")
	}
	if !restoreJobFailedEntirely(nil) {
		t.Fatal("no outcomes counts as total failure")
	}
}

// A model whose subprocess cannot even start must produce a failed outcome
// with a user-safe error and a FAILED.txt marker — never a panic, and never
// raw exec detail in the outcome.
func TestRunRestoreModelFailureIsolation(t *testing.T) {
	svc := restoreTestService(t, true)
	// Point the model at an executable that does not exist.
	svc.cfg.AIRestorePython = filepath.Join(t.TempDir(), "missing-python")

	work := t.TempDir()
	framesDir := filepath.Join(work, "frames")
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(framesDir, "000001.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	resultsDir := filepath.Join(work, "results")
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	outcome := svc.runRestoreModel(context.Background(), RestoreRequest{JobID: "test-job"}, restoreModelRun{
		ID:            models.RestoreModelSwinIR,
		Scale:         4,
		FramesDir:     framesDir,
		WorkDir:       work,
		ResultsDir:    resultsDir,
		ClipPath:      filepath.Join(work, "clip.mp4"),
		FPSFraction:   "30/1",
		HasAudio:      false,
		IncludeFrames: false,
		FrameCount:    1,
		SourceWidth:   320,
		SourceHeight:  240,
		ProgressFloor: 15,
		ProgressCeil:  50,
	})
	if outcome.Status != "failed" {
		t.Fatalf("expected failed outcome, got %+v", outcome)
	}
	if outcome.Error == "" || strings.Contains(outcome.Error, "missing-python") {
		t.Fatalf("expected a user-safe error without server paths, got %q", outcome.Error)
	}
	if _, err := os.Stat(filepath.Join(resultsDir, "swinir", "FAILED.txt")); err != nil {
		t.Fatalf("expected FAILED.txt marker: %v", err)
	}
}

func TestRestoreResolutionBucket(t *testing.T) {
	cases := map[int]string{0: "unknown", 240: "240p", 360: "360p", 480: "480p", 540: "540p", 720: "720p", 1080: "1080p", 1440: "above1080p"}
	for h, want := range cases {
		if got := restoreResolutionBucket(h); got != want {
			t.Fatalf("bucket(%d) = %q, want %q", h, got, want)
		}
	}
}
