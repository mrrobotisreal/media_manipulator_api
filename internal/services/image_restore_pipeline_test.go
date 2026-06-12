package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// The working-source fallback: a failed pre-clean unit must leave the working
// source unchanged and NOT appear in precleanApplied, so every downstream unit
// consumes the last good intermediate.
func TestImageRestorePrecleanStepFallback(t *testing.T) {
	workDir := "/work"
	src := "/work/working_source.png"
	applied := []string{}

	// fbcnn succeeds → working source advances, applied = [fbcnn].
	src, applied = imageRestorePrecleanStep(workDir, src, applied, "fbcnn", true)
	if src != filepath.Join(workDir, "preclean_fbcnn", "cleaned.png") {
		t.Fatalf("after fbcnn success, source = %q", src)
	}
	if !eqStrings(applied, []string{"fbcnn"}) {
		t.Fatalf("applied = %v", applied)
	}

	// scunet FAILS → source unchanged (still fbcnn's), applied unchanged.
	prevSrc := src
	src, applied = imageRestorePrecleanStep(workDir, src, applied, "scunet", false)
	if src != prevSrc {
		t.Fatalf("after scunet failure, source must stay at last good intermediate, got %q", src)
	}
	if !eqStrings(applied, []string{"fbcnn"}) {
		t.Fatalf("scunet failure must not record in precleanApplied, got %v", applied)
	}

	// nafnet succeeds → advances from the fbcnn intermediate, applied = [fbcnn, nafnet].
	src, applied = imageRestorePrecleanStep(workDir, src, applied, "nafnet", true)
	if src != filepath.Join(workDir, "preclean_nafnet", "cleaned.png") {
		t.Fatalf("after nafnet success, source = %q", src)
	}
	if !eqStrings(applied, []string{"fbcnn", "nafnet"}) {
		t.Fatalf("precleanApplied must reflect reality (scunet skipped), got %v", applied)
	}
}

// A chained face unit whose base general model produced no result must be
// marked failed WITHOUT executing any subprocess.
func TestRunFaceUnitChainedOnFailedBase(t *testing.T) {
	svc := imageRestoreTestService(t, true)
	resultsDir := t.TempDir()
	workDir := t.TempDir()
	unit := imageRestoreUnit{
		ResultID: "gfpgan_on_realesrgan",
		StageKey: "face_gfpgan_on_realesrgan",
		Kind:     models.ImageRestoreKindFace,
		Model:    models.ImageRestoreModelGFPGAN,
		Base:     models.ImageRestoreModelRealESRGAN,
	}
	prep := imageRestorePrep{cropPx: imageRestoreCropPx{Width: 100, Height: 100}, scale: 4}

	outcome := svc.runFaceUnit(context.Background(), ImageRestoreRequest{JobID: "t"}, unit, prep,
		"/nonexistent/working.png", map[models.ImageRestoreModelID]string{}, resultsDir, workDir, nil, 10, 20)

	if outcome.Status != "failed" {
		t.Fatalf("expected failed without execution, got %+v", outcome)
	}
	if !strings.Contains(outcome.Error, "Real-ESRGAN") {
		t.Fatalf("error should reference the base model, got %q", outcome.Error)
	}
	if _, err := os.Stat(filepath.Join(resultsDir, "gfpgan_on_realesrgan.FAILED.txt")); err != nil {
		t.Fatalf("expected FAILED.txt marker: %v", err)
	}
}

// Results() must project a written manifest into the response, and
// ResultImagePath() must resolve only ids present in the manifest.
func TestImageRestoreResultsAndPathResolution(t *testing.T) {
	svc := imageRestoreTestService(t, true)
	jobID := "job-xyz"
	resultsDir := svc.resultsDirFor(jobID)
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Lay down the original + one completed result PNG.
	if err := os.WriteFile(filepath.Join(resultsDir, "original.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultsDir, "realesrgan.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := imageRestoreManifest{
		JobID:       jobID,
		GeneratedAt: time.Now().UTC(),
		Source:      imageRestoreManifestSource{Width: 200, Height: 200, CropAppliedPx: imageRestoreCropPx{Width: 100, Height: 100}},
		Original:    imageRestoreOriginal{FileName: "original.png", Width: 100, Height: 100, SizeBytes: 3},
		Outcomes: []imageRestoreOutcome{
			{ResultID: "realesrgan", Label: "Real-ESRGAN", Kind: "general", Status: "completed", OutputWidth: 400, OutputHeight: 400, FileName: "realesrgan.png", SizeBytes: 3},
			{ResultID: "gfpgan_on_original", Label: "GFPGAN v1.4", Kind: "face", Status: "failed", Error: "No faces were detected in this image", GenerativeNote: imageRestoreGenerativeNote},
		},
	}
	if err := writeJSON(filepath.Join(resultsDir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.Results(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Original.ID != "original" || resp.Original.Width != 100 {
		t.Fatalf("unexpected original entry: %+v", resp.Original)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[1].GenerativeNote == "" || resp.Results[1].Status != "failed" {
		t.Fatalf("expected failed face entry to carry generative note: %+v", resp.Results[1])
	}

	// resultId resolution: known completed id + original resolve; unknown + failed reject.
	if _, err := svc.ResultImagePath(jobID, "realesrgan"); err != nil {
		t.Fatalf("expected realesrgan to resolve: %v", err)
	}
	if _, err := svc.ResultImagePath(jobID, "original"); err != nil {
		t.Fatalf("expected original to resolve: %v", err)
	}
	if _, err := svc.ResultImagePath(jobID, "gfpgan_on_original"); err == nil {
		t.Fatal("a failed unit must not resolve to an image")
	}
	if _, err := svc.ResultImagePath(jobID, "../../etc/passwd"); err == nil {
		t.Fatal("an unknown/traversal id must be rejected")
	}
}

func TestImageRestoreJobFailedEntirely(t *testing.T) {
	ok := imageRestoreOutcome{Status: "completed"}
	bad := imageRestoreOutcome{Status: "failed"}
	if imageRestoreJobFailedEntirely([]imageRestoreOutcome{bad, ok, bad}) {
		t.Fatal("one success means the job must not fail")
	}
	if !imageRestoreJobFailedEntirely([]imageRestoreOutcome{bad, bad}) {
		t.Fatal("all failures means the job fails")
	}
}

func TestExtractImageRestoreError(t *testing.T) {
	if got := extractImageRestoreError("loading\nERROR: No faces were detected in this image\n"); got != "No faces were detected in this image" {
		t.Fatalf("got %q", got)
	}
	if got := extractImageRestoreError("just noise\n"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
