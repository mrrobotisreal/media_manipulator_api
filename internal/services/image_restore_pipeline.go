package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// The image-restoration pipeline. One goroutine per job:
//
//	queued → prepare
//	  → preclean_<id> (fixed semantic order; each on the previous result)
//	  → model_<id> (general upscalers, cheap→expensive)
//	  → face_<id>_original / face_<id>_on_<generalId> (face enhancers)
//	  → package → completed
//
// A unit that fails does NOT fail the job (it gets a <resultId>.FAILED.txt and a
// manifest outcome records the failure); the job fails only when ALL units
// fail. A failed pre-clean unit additionally triggers the working-source
// fallback: the pipeline continues from the last successful intermediate. Every
// message written to the job is user-safe — raw subprocess output stays in
// server logs and command-audit rows.

const imageRestoreResultBaseName = "image_restoration_results"

// imageRestoreCropPx records the applied crop in pixels for the manifest.
type imageRestoreCropPx struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// imageRestoreOutcome is the per-unit record persisted into manifest.json and
// projected into the results-listing response.
type imageRestoreOutcome struct {
	ResultID        string   `json:"resultId"`
	Label           string   `json:"label"`
	Kind            string   `json:"kind"`
	BaseModel       string   `json:"baseModel,omitempty"`
	Status          string   `json:"status"` // completed | failed
	DurationSeconds float64  `json:"durationSeconds"`
	OutputWidth     int      `json:"outputWidth,omitempty"`
	OutputHeight    int      `json:"outputHeight,omitempty"`
	FileName        string   `json:"fileName,omitempty"` // basename inside the results dir / archive
	SizeBytes       int64    `json:"sizeBytes,omitempty"`
	Error           string   `json:"error,omitempty"`
	GenerativeNote  string   `json:"generativeNote,omitempty"`
	FidelityNote    string   `json:"fidelityNote,omitempty"`
	PrecleanApplied []string `json:"precleanApplied"`
}

type imageRestoreManifestRequest struct {
	Crop               *models.NormalizedRect `json:"crop,omitempty"`
	Preclean           []string               `json:"preclean"`
	Models             []string               `json:"models"`
	FaceModels         []string               `json:"faceModels"`
	Chain              bool                   `json:"chain"`
	RequestedScale     int                    `json:"requestedScale"`
	EffectiveScale     int                    `json:"effectiveScale"`
	CodeFormerFidelity float64                `json:"codeformerFidelity"`
	FBCNNQualityFactor int                    `json:"fbcnnQualityFactor"`
}

type imageRestoreManifestSource struct {
	Width         int                `json:"width"`
	Height        int                `json:"height"`
	CropAppliedPx imageRestoreCropPx `json:"cropAppliedPx"`
}

type imageRestoreOriginal struct {
	FileName  string `json:"fileName"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	SizeBytes int64  `json:"sizeBytes"`
}

type imageRestoreManifest struct {
	JobID       string                      `json:"jobId"`
	GeneratedAt time.Time                   `json:"generatedAt"`
	Request     imageRestoreManifestRequest `json:"request"`
	Source      imageRestoreManifestSource  `json:"source"`
	Original    imageRestoreOriginal        `json:"original"`
	Outcomes    []imageRestoreOutcome       `json:"outcomes"`
}

// Process runs one image-restoration job end to end. Launched as a goroutine
// with context.Background(); the whole job runs under cfg.CommandTimeout, with
// a tighter ImageRestoreModelTimeout per unit.
func (s *ImageRestoreService) Process(ctx context.Context, req ImageRestoreRequest) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.CommandTimeout)
	defer cancel()

	units := orderImageRestoreOutputs(req.Preclean, req.Models, req.FaceModels, req.Chain)
	stages := s.BuildStages(units)
	advance := func(key string, status models.TranscodeStageStatus, progress int, message string) {
		stages = updateStage(stages, key, status, progress, message)
		_ = s.jobManager.ReplaceStages(req.JobID, stages, key)
		if progress > 0 {
			_ = s.jobManager.UpdateJobProgress(req.JobID, progress)
		}
	}
	startedAt := time.Now()

	// --- queued -----------------------------------------------------------
	advance("queued", models.StageStatusProcessing, 0, "")
	release, err := s.acquireImageRestorePermit(ctx)
	if err != nil {
		s.failImageRestore(req, stages, "Restoration timed out while waiting in the queue", err)
		return
	}
	defer release()
	if err := s.jobManager.UpdateJobStatus(req.JobID, models.StatusProcessing); err != nil {
		return
	}
	advance("queued", models.StageStatusCompleted, imageRestoreProgressQueued, "")

	// --- workspace --------------------------------------------------------
	// The bulky work tree is removed on exit; the stage tree (results dir +
	// tarball) survives for the cleanup worker's retention window so the
	// results/preview endpoints can stream PNGs.
	jobOutputDir := filepath.Join(s.cfg.OutputDir, req.JobID)
	jobUploadDir := filepath.Join(s.cfg.UploadDir, req.JobID)
	workDir := filepath.Join(jobOutputDir, "work")
	stageRoot := filepath.Join(jobOutputDir, "stage")
	resultsDir := filepath.Join(stageRoot, imageRestoreResultBaseName)
	defer func() {
		_ = os.RemoveAll(workDir)
		_ = os.RemoveAll(jobUploadDir)
	}()
	for _, dir := range []string{workDir, resultsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.failImageRestore(req, stages, "Failed to prepare the job workspace", err)
			return
		}
	}

	// --- prepare ----------------------------------------------------------
	advance("prepare", models.StageStatusProcessing, 0, "")
	prep, err := s.prepareImageRestore(ctx, req, workDir, resultsDir)
	if err != nil {
		s.failImageRestore(req, stages, err.Error(), nil)
		return
	}
	advance("prepare", models.StageStatusCompleted, imageRestoreProgressPrepare, "")

	// --- run units --------------------------------------------------------
	workingSource := prep.workingSource
	precleanApplied := []string{}
	generalResults := map[models.ImageRestoreModelID]string{}
	outcomes := make([]imageRestoreOutcome, 0, len(units))
	prevCheckpoint := imageRestoreProgressPrepare
	for _, u := range units {
		checkpoint := stageCheckpoint(stages, u.StageKey)
		advance(u.StageKey, models.StageStatusProcessing, 0, "")
		var outcome imageRestoreOutcome
		switch u.Kind {
		case models.ImageRestoreKindPreclean:
			outcome = s.runPrecleanUnit(ctx, req, u, prep, workingSource, resultsDir, workDir, append([]string{}, precleanApplied...), prevCheckpoint, checkpoint)
			// A failed pre-clean unit leaves the working source unchanged, so the
			// next unit continues from the last successful intermediate (§2.5).
			workingSource, precleanApplied = imageRestorePrecleanStep(workDir, workingSource, precleanApplied, string(u.Model), outcome.Status == "completed")
			if outcome.Status == "completed" {
				outcome.PrecleanApplied = append([]string{}, precleanApplied...)
			}
		case models.ImageRestoreKindGeneral:
			outcome = s.runGeneralUnit(ctx, req, u, prep, workingSource, resultsDir, workDir, append([]string{}, precleanApplied...), prevCheckpoint, checkpoint)
			if outcome.Status == "completed" {
				generalResults[u.Model] = filepath.Join(resultsDir, u.ResultID+".png")
			}
		case models.ImageRestoreKindFace:
			outcome = s.runFaceUnit(ctx, req, u, prep, workingSource, generalResults, resultsDir, workDir, append([]string{}, precleanApplied...), prevCheckpoint, checkpoint)
		}
		outcomes = append(outcomes, outcome)
		s.recordUnitEvent(req, outcome)
		if outcome.Status == "completed" {
			advance(u.StageKey, models.StageStatusCompleted, checkpoint, "")
		} else {
			advance(u.StageKey, models.StageStatusFailed, 0, outcome.Error)
			_ = s.jobManager.UpdateJobProgress(req.JobID, checkpoint)
		}
		prevCheckpoint = checkpoint
	}

	// --- package ----------------------------------------------------------
	advance("package", models.StageStatusProcessing, 0, "")
	if imageRestoreJobFailedEntirely(outcomes) {
		s.failImageRestore(req, stages, "All selected models failed — no results to package", nil)
		s.recordToolUsage(req, units, prep, startedAt, 0, false)
		return
	}
	manifest := imageRestoreManifest{
		JobID:       req.JobID,
		GeneratedAt: time.Now().UTC(),
		Request: imageRestoreManifestRequest{
			Crop:               req.Crop,
			Preclean:           imageRestoreIDStrings(req.Preclean),
			Models:             imageRestoreIDStrings(req.Models),
			FaceModels:         imageRestoreIDStrings(req.FaceModels),
			Chain:              req.Chain,
			RequestedScale:     req.Scale,
			EffectiveScale:     prep.scale,
			CodeFormerFidelity: req.CodeFormerFidelity,
			FBCNNQualityFactor: req.FBCNNQualityFactor,
		},
		Source: imageRestoreManifestSource{
			Width:         prep.srcWidth,
			Height:        prep.srcHeight,
			CropAppliedPx: prep.cropPx,
		},
		Original: imageRestoreOriginal{
			FileName:  "original.png",
			Width:     prep.cropPx.Width,
			Height:    prep.cropPx.Height,
			SizeBytes: fileSizeBytes(prep.originalPath),
		},
		Outcomes: outcomes,
	}
	if err := writeJSON(filepath.Join(resultsDir, "manifest.json"), manifest); err != nil {
		s.failImageRestore(req, stages, "Failed to package the results", err)
		return
	}
	if err := writeImageRestoreReadme(filepath.Join(resultsDir, "README.txt"), manifest); err != nil {
		s.failImageRestore(req, stages, "Failed to package the results", err)
		return
	}
	tarPath := filepath.Join(jobOutputDir, imageRestoreResultBaseName+".tar.gz")
	tarSize, err := createTarGz(stageRoot, tarPath)
	if err != nil {
		s.failImageRestore(req, stages, "Failed to package the results", err)
		return
	}
	advance("package", models.StageStatusCompleted, imageRestoreProgressPackage, "")

	// --- completed --------------------------------------------------------
	_ = s.jobManager.SetResultSize(req.JobID, tarSize)
	if err := s.jobManager.UpdateJobResult(req.JobID, "/api/download/"+req.JobID); err != nil {
		s.failImageRestore(req, stages, "Failed to finalize the results", err)
		return
	}
	advance("completed", models.StageStatusCompleted, 100, "")
	_ = s.jobManager.UpdateJobStatus(req.JobID, models.StatusCompleted)
	s.recordToolUsage(req, units, prep, startedAt, tarSize, true)
}

// imageRestorePrep is the output of the prepare stage.
type imageRestorePrep struct {
	workingSource string // cropped, normalized PNG every model starts from
	originalPath  string // resultsDir/original.png (== workingSource content)
	srcWidth      int
	srcHeight     int
	cropPx        imageRestoreCropPx
	scale         int // effective scale (2 or 4)
}

// prepareImageRestore decodes + auto-orients the upload, applies the crop
// server-side, resolves the effective scale and re-checks the pixel budget.
// Returned errors are user-safe.
func (s *ImageRestoreService) prepareImageRestore(ctx context.Context, req ImageRestoreRequest, workDir, resultsDir string) (imageRestorePrep, error) {
	var prep imageRestorePrep
	normalized := filepath.Join(workDir, "normalized.png")
	if err := s.runImageMagick(ctx, req, "prepare", []string{req.SourcePath, "-auto-orient", normalized}); err != nil {
		return prep, fmt.Errorf("Could not read the uploaded image")
	}
	w, h, err := s.imageDimensions(ctx, req, normalized)
	if err != nil || w <= 0 || h <= 0 {
		return prep, fmt.Errorf("Could not read the uploaded image")
	}
	if w > s.cfg.ImageRestoreMaxSourceWidth || h > s.cfg.ImageRestoreMaxSourceHeight {
		return prep, fmt.Errorf("Image resolution exceeds the maximum of %dx%d", s.cfg.ImageRestoreMaxSourceWidth, s.cfg.ImageRestoreMaxSourceHeight)
	}

	if err := ValidateImageRestoreCrop(req.Crop); err != nil {
		return prep, err
	}
	cx, cy, cw, ch, err := ImageRestoreCropToPixels(req.Crop, w, h)
	if err != nil {
		return prep, err
	}
	scale, err := ResolveImageRestoreScale(req.Scale, ch)
	if err != nil {
		return prep, err
	}
	if err := ValidateImageRestoreOutputPixels(cw, ch, scale, s.cfg.ImageRestoreMaxOutputPixels); err != nil {
		return prep, err
	}

	workingSource := filepath.Join(workDir, "working_source.png")
	cropArg := fmt.Sprintf("%dx%d+%d+%d", cw, ch, cx, cy)
	if err := s.runImageMagick(ctx, req, "prepare", []string{normalized, "-crop", cropArg, "+repage", workingSource}); err != nil {
		return prep, fmt.Errorf("Could not apply the selected crop")
	}
	originalPath := filepath.Join(resultsDir, "original.png")
	if err := copyRestoreFile(workingSource, originalPath); err != nil {
		return prep, fmt.Errorf("Failed to prepare the image")
	}

	prep = imageRestorePrep{
		workingSource: workingSource,
		originalPath:  originalPath,
		srcWidth:      w,
		srcHeight:     h,
		cropPx:        imageRestoreCropPx{X: cx, Y: cy, Width: cw, Height: ch},
		scale:         scale,
	}
	return prep, nil
}

// newImageRestoreOutcome seeds an outcome with the unit's static metadata.
func newImageRestoreOutcome(u imageRestoreUnit, precleanApplied []string) imageRestoreOutcome {
	o := imageRestoreOutcome{
		ResultID:        u.ResultID,
		Label:           imageRestoreUnitLabel(u),
		Kind:            u.Kind,
		FileName:        u.ResultID + ".png",
		PrecleanApplied: precleanApplied,
	}
	if u.Base != "" {
		o.BaseModel = string(u.Base)
	}
	switch u.Kind {
	case models.ImageRestoreKindFace:
		o.GenerativeNote = imageRestoreGenerativeNote
	case models.ImageRestoreKindPreclean:
		o.FidelityNote = imageRestoreFidelityNote
	}
	return o
}

// runPrecleanUnit runs one pre-clean model on the current working source. On
// success the cleaned image becomes the new working source (handled by the
// caller) and is filed as this unit's result. On failure the working source is
// left unchanged so the pipeline continues from the last good intermediate.
func (s *ImageRestoreService) runPrecleanUnit(ctx context.Context, req ImageRestoreRequest, u imageRestoreUnit, prep imageRestorePrep, workingSource, resultsDir, workDir string, precleanApplied []string, floor, ceil int) imageRestoreOutcome {
	started := time.Now()
	outcome := newImageRestoreOutcome(u, precleanApplied)
	outDir := filepath.Join(workDir, "preclean_"+string(u.Model))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Could not prepare the model workspace", err)
	}
	cmd := buildPrecleanCommand(u.Model, s.imagePaths(), workingSource, outDir, s.cfg.AICUDAGPU, req.FBCNNQualityFactor)
	if err, userMsg := s.runImageUnit(ctx, req, u, cmd, gpu.TaskRestoreSR, s.cfg.AICUDAGPU, floor, ceil); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, userMsg, err)
	}
	cleaned := filepath.Join(outDir, "cleaned.png")
	dst := filepath.Join(resultsDir, u.ResultID+".png")
	if err := copyRestoreFile(cleaned, dst); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Pre-clean produced no output", err)
	}
	outcome.Status = "completed"
	outcome.DurationSeconds = time.Since(started).Seconds()
	outcome.OutputWidth = prep.cropPx.Width
	outcome.OutputHeight = prep.cropPx.Height
	outcome.SizeBytes = fileSizeBytes(dst)
	return outcome
}

// runGeneralUnit runs one general upscaler on the working source (reusing the
// video command builder + a one-image frames dir), then applies the x2 Lanczos
// downscale when the effective scale is 2.
func (s *ImageRestoreService) runGeneralUnit(ctx context.Context, req ImageRestoreRequest, u imageRestoreUnit, prep imageRestorePrep, workingSource, resultsDir, workDir string, precleanApplied []string, floor, ceil int) imageRestoreOutcome {
	started := time.Now()
	outcome := newImageRestoreOutcome(u, precleanApplied)
	base := filepath.Join(workDir, "general_"+string(u.Model))
	inFrames := filepath.Join(base, "in")
	outDir := filepath.Join(base, "out")
	outFrames := filepath.Join(outDir, "frames")
	for _, d := range []string{inFrames, outFrames} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Could not prepare the model workspace", err)
		}
	}
	if err := copyRestoreFile(workingSource, filepath.Join(inFrames, "image.png")); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Could not prepare the model workspace", err)
	}

	task := gpu.TaskRestoreSR
	cudaIndex := s.cfg.AIRestoreCUDAGPU
	if u.Model == models.ImageRestoreModelRealESRGAN {
		task = gpu.TaskRealESRGAN
	}
	cmd := buildImageRestoreGeneralCommand(u.Model, s.imagePaths(), inFrames, outFrames, outDir, cudaIndex, s.cfg.AIRestoreVulkanGPU)
	if err, userMsg := s.runImageUnit(ctx, req, u, cmd, task, cudaIndex, floor, ceil); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, userMsg, err)
	}
	produced := filepath.Join(outFrames, "image.png")
	if _, err := os.Stat(produced); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Model produced no output", err)
	}
	dst := filepath.Join(resultsDir, u.ResultID+".png")
	if prep.scale == 2 {
		if err := s.runImageMagick(ctx, req, u.StageKey, []string{produced, "-filter", "Lanczos", "-resize", "50%", dst}); err != nil {
			return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Failed to downscale the enhanced image", err)
		}
	} else if err := copyRestoreFile(produced, dst); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Failed to store the enhanced image", err)
	}
	outcome.Status = "completed"
	outcome.DurationSeconds = time.Since(started).Seconds()
	outcome.OutputWidth = prep.cropPx.Width * prep.scale
	outcome.OutputHeight = prep.cropPx.Height * prep.scale
	outcome.SizeBytes = fileSizeBytes(dst)
	return outcome
}

// runFaceUnit runs one face model either on the working source (upscale =
// effective scale) or — when chained — on a general result (upscale 1). A
// chained unit whose base general model failed is marked failed without
// execution.
func (s *ImageRestoreService) runFaceUnit(ctx context.Context, req ImageRestoreRequest, u imageRestoreUnit, prep imageRestorePrep, workingSource string, generalResults map[models.ImageRestoreModelID]string, resultsDir, workDir string, precleanApplied []string, floor, ceil int) imageRestoreOutcome {
	started := time.Now()
	outcome := newImageRestoreOutcome(u, precleanApplied)

	input := workingSource
	upscale := prep.scale
	if !u.OnOriginal {
		baseResult, ok := generalResults[u.Base]
		if !ok {
			return s.finishImageUnitFailed(req, outcome, resultsDir, started,
				fmt.Sprintf("Skipped because %s did not produce a result to enhance", imageRestoreShortName(u.Base)), nil)
		}
		input = baseResult
		upscale = 1
	}

	outDir := filepath.Join(workDir, "face_"+u.ResultID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Could not prepare the model workspace", err)
	}
	cmd := buildFaceRestoreCommand(u.Model, s.imagePaths(), input, outDir, upscale, req.CodeFormerFidelity, s.cfg.AICUDAGPU)
	if err, userMsg := s.runImageUnit(ctx, req, u, cmd, gpu.TaskRestoreSR, s.cfg.AICUDAGPU, floor, ceil); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, userMsg, err)
	}
	produced := filepath.Join(outDir, "restored.png")
	if _, err := os.Stat(produced); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Face enhancement produced no output", err)
	}
	dst := filepath.Join(resultsDir, u.ResultID+".png")
	if err := copyRestoreFile(produced, dst); err != nil {
		return s.finishImageUnitFailed(req, outcome, resultsDir, started, "Failed to store the enhanced image", err)
	}
	outcome.Status = "completed"
	outcome.DurationSeconds = time.Since(started).Seconds()
	outcome.OutputWidth = prep.cropPx.Width * prep.scale
	outcome.OutputHeight = prep.cropPx.Height * prep.scale
	outcome.SizeBytes = fileSizeBytes(dst)
	return outcome
}

// runImageUnit acquires a GPU lease, runs the model subprocess under the
// per-unit timeout with live progress, and maps a failure to a user-safe
// message (preferring the script's own "ERROR:" line). Returns (nil, "") on
// success.
func (s *ImageRestoreService) runImageUnit(ctx context.Context, req ImageRestoreRequest, u imageRestoreUnit, cmd restoreModelCommand, task gpu.TaskType, cudaIndex, floor, ceil int) (error, string) {
	gpuReq := GPURequest{
		Kind:            "image-restore-" + string(u.Model),
		VRAMRequiredMiB: s.cfg.ImageRestoreVRAMMiB[string(u.Model)],
	}
	if task == gpu.TaskRealESRGAN {
		gpuReq.PreferredOrder = []int{s.cfg.AIRestoreVulkanGPU, s.cfg.AIRestoreCUDAGPU}
	} else {
		force := cudaIndex
		gpuReq.ForceIndex = &force
	}
	_, releaseGPU, err := SharedGPUScheduler().Acquire(ctx, gpuReq)
	if err != nil {
		return err, "Timed out waiting for a free GPU"
	}
	defer releaseGPU()
	var lease *gpu.Lease
	if s.gpuMgr != nil {
		lease, _ = s.gpuMgr.Acquire(ctx, task, "image_restore", req.JobID, req.RequestID)
	}

	onProgress := s.unitProgressFunc(req.JobID, floor, ceil)
	progressOut := newRestoreProgressWriter(onProgress)
	unitCtx, cancelUnit := context.WithTimeout(ctx, s.cfg.ImageRestoreModelTimeout)
	defer cancelUnit()

	res, runErr := s.runner.Run(unitCtx, cmdaudit.Spec{
		Tool:       "image_restore",
		Stage:      u.StageKey,
		Executable: cmd.Executable,
		Args:       cmd.Args,
		ExtraEnv:   cmd.ExtraEnv,
		Stdout:     progressOut,
		RequestID:  req.RequestID,
		JobID:      req.JobID,
	})
	if lease != nil {
		lease.Release(context.Background(), runErr)
	}
	if runErr != nil {
		if res.TimedOut || unitCtx.Err() != nil {
			return runErr, "Model timed out — try a smaller crop or 2x scale"
		}
		if scripted := extractImageRestoreError(res.Stderr); scripted != "" {
			return runErr, scripted
		}
		log.Printf("image-restore job %s unit %s output tail: %s", req.JobID, u.ResultID, commandTail(res.Stderr, 2000))
		return runErr, "Model failed while processing the image"
	}
	return nil, ""
}

// finishImageUnitFailed records a failed unit (FAILED.txt marker + outcome).
func (s *ImageRestoreService) finishImageUnitFailed(req ImageRestoreRequest, outcome imageRestoreOutcome, resultsDir string, started time.Time, userMsg string, err error) imageRestoreOutcome {
	if err != nil {
		log.Printf("image-restore job %s unit %s: %s: %v", req.JobID, outcome.ResultID, userMsg, err)
	} else {
		log.Printf("image-restore job %s unit %s: %s", req.JobID, outcome.ResultID, userMsg)
	}
	outcome.Status = "failed"
	outcome.Error = userMsg
	outcome.DurationSeconds = time.Since(started).Seconds()
	writeImageRestoreFailedMarker(resultsDir, outcome.ResultID, userMsg)
	return outcome
}

// unitProgressFunc maps a model's done/total into the job's overall progress
// span for that unit, throttled to whole-percent changes.
func (s *ImageRestoreService) unitProgressFunc(jobID string, floor, ceil int) func(done, total int) {
	lastPct := -1
	span := ceil - floor
	return func(done, total int) {
		if total <= 0 {
			return
		}
		pct := floor + span*done/total
		if pct <= lastPct {
			return
		}
		lastPct = pct
		_ = s.jobManager.UpdateJobProgress(jobID, pct)
	}
}

// imagePaths assembles the command-builder paths from config.
func (s *ImageRestoreService) imagePaths() imageRestorePaths {
	return imageRestorePaths{
		RealESRGANBin:  s.cfg.RealESRGANBin,
		SRPython:       s.cfg.AIRestorePython,
		FramesScript:   s.cfg.AIRestoreFramesScript,
		PrecleanPython: s.cfg.AIPrecleanPython,
		PrecleanScript: s.cfg.AIPrecleanScript,
		FacePython:     s.cfg.AIFaceRestorePython,
		FaceScript:     s.cfg.AIFaceRestoreScript,
		ModelsDir:      s.cfg.AIRestoreModelsDir,
		ReposDir:       s.cfg.AIRestoreReposDir,
	}
}

// failImageRestore marks the job failed with a USER-SAFE message.
func (s *ImageRestoreService) failImageRestore(req ImageRestoreRequest, stages []models.TranscodeJobStage, userMsg string, err error) {
	if err != nil {
		log.Printf("image-restore job %s: %s: %v", req.JobID, userMsg, err)
	} else {
		log.Printf("image-restore job %s: %s", req.JobID, userMsg)
	}
	failed := updateStageStatus(stages, "completed", models.StageStatusFailed, userMsg)
	_ = s.jobManager.ReplaceStages(req.JobID, failed, "failed")
	_ = s.jobManager.UpdateJobError(req.JobID, userMsg)
}

// runImageMagick shells out to ImageMagick (convert, resolved to `magick
// convert` on IM7) through the audited runner.
func (s *ImageRestoreService) runImageMagick(ctx context.Context, req ImageRestoreRequest, stage string, args []string) error {
	exe, finalArgs := resolveImageMagickConvertCommand("convert", args)
	res, err := s.runner.Run(ctx, cmdaudit.Spec{
		Tool:       "image_restore",
		Stage:      stage,
		Executable: exe,
		Args:       finalArgs,
		RequestID:  req.RequestID,
		JobID:      req.JobID,
	})
	if err != nil {
		if res.TimedOut || ctx.Err() != nil {
			return fmt.Errorf("imagemagick %s timed out", stage)
		}
		return fmt.Errorf("imagemagick %s failed (exit %d): %s", stage, res.ExitCode, commandTail(res.Stderr, 500))
	}
	return nil
}

// imageDimensions reads "<width> <height>" from ImageMagick identify.
func (s *ImageRestoreService) imageDimensions(ctx context.Context, req ImageRestoreRequest, path string) (int, int, error) {
	var buf bytes.Buffer
	exe, args := resolveIdentifyFormatCommand("%w %h", path)
	res, err := s.runner.Run(ctx, cmdaudit.Spec{
		Tool:       "image_restore",
		Stage:      "prepare",
		Executable: exe,
		Args:       args,
		Stdout:     &buf,
		RequestID:  req.RequestID,
		JobID:      req.JobID,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("identify failed (exit %d): %s", res.ExitCode, commandTail(res.Stderr, 300))
	}
	parts := strings.Fields(strings.TrimSpace(buf.String()))
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("could not parse identify output %q", buf.String())
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("could not parse dimensions %q", buf.String())
	}
	return w, h, nil
}

// resolveIdentifyFormatCommand picks `magick identify` on IM7 or `identify` on
// IM6, formatting a single -format query.
func resolveIdentifyFormatCommand(format, path string) (string, []string) {
	if _, err := exec.LookPath("magick"); err == nil {
		return "magick", []string{"identify", "-format", format, path}
	}
	return "identify", []string{"-format", format, path}
}

// extractImageRestoreError returns the last user-safe "ERROR: <msg>" line the
// wrapper scripts print, or "" if none.
func extractImageRestoreError(stderr string) string {
	var found string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "ERROR:"); ok {
			found = strings.TrimSpace(rest)
		}
	}
	return found
}

// imageRestorePrecleanStep advances the working-source / precleanApplied state
// after one pre-clean unit. On success the cleaned output becomes the new
// working source and the model is appended to the applied list; on failure both
// are left unchanged, so downstream units consume the last good intermediate.
func imageRestorePrecleanStep(workDir, prevSource string, prevApplied []string, model string, success bool) (string, []string) {
	if !success {
		return prevSource, prevApplied
	}
	return filepath.Join(workDir, "preclean_"+model, "cleaned.png"), append(append([]string{}, prevApplied...), model)
}

// imageRestoreJobFailedEntirely reports whether every unit failed.
func imageRestoreJobFailedEntirely(outcomes []imageRestoreOutcome) bool {
	for _, o := range outcomes {
		if o.Status == "completed" {
			return false
		}
	}
	return true
}

func writeImageRestoreFailedMarker(resultsDir, resultID, userMsg string) {
	content := "This model failed to process the image.\n\nReason: " + userMsg + "\n\nSee manifest.json at the root of this archive for the full run summary.\n"
	_ = os.WriteFile(filepath.Join(resultsDir, resultID+".FAILED.txt"), []byte(content), 0o644)
}

func imageRestoreIDStrings(ids []models.ImageRestoreModelID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

func fileSizeBytes(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// writeImageRestoreReadme explains the archive contents.
func writeImageRestoreReadme(path string, m imageRestoreManifest) error {
	var b strings.Builder
	b.WriteString("AI Image Restoration & Upscaling — results package\n")
	b.WriteString("====================================================\n\n")
	fmt.Fprintf(&b, "Job: %s\nGenerated: %s\n", m.JobID, m.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "Source: %dx%d, cropped to %dx%d, upscaled x%d\n\n", m.Source.Width, m.Source.Height, m.Source.CropAppliedPx.Width, m.Source.CropAppliedPx.Height, m.Request.EffectiveScale)
	b.WriteString("Contents\n--------\n")
	b.WriteString("manifest.json  Machine-readable run summary (request, source, per-unit outcomes).\n")
	b.WriteString("original.png   The prepared (cropped) source, for A/B comparison.\n")
	for _, o := range m.Outcomes {
		if o.Status == "completed" {
			fmt.Fprintf(&b, "%-28s %s (%dx%d)\n", o.ResultID+".png", o.Label, o.OutputWidth, o.OutputHeight)
		} else {
			fmt.Fprintf(&b, "%-28s FAILED — see %s.FAILED.txt\n", o.ResultID+".png", o.ResultID)
		}
	}
	b.WriteString("\nPre-clean outputs are non-generative (filtered signal). Face-enhancement\n")
	b.WriteString("outputs are GENERATIVE reconstructions — synthesized detail suitable for\n")
	b.WriteString("clarity and leads, NOT for identification evidence.\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// recordUnitEvent writes one privacy-safe per-unit timing/status row.
func (s *ImageRestoreService) recordUnitEvent(req ImageRestoreRequest, outcome imageRestoreOutcome) {
	if s.telemetry == nil {
		return
	}
	s.telemetry.InsertJobEvent(context.Background(), telemetry.JobEvent{
		JobID:     req.JobID,
		RequestID: req.RequestID,
		EventName: "image_restore_unit",
		Stage:     outcome.ResultID,
		Status:    outcome.Status,
		Properties: map[string]any{
			"kind":            outcome.Kind,
			"durationSeconds": outcome.DurationSeconds,
		},
	})
}

// recordToolUsage writes the single privacy-safe tool-usage event for the whole
// job: derived metadata only — never filenames, paths, crop coordinates, or
// content.
func (s *ImageRestoreService) recordToolUsage(req ImageRestoreRequest, units []imageRestoreUnit, prep imageRestorePrep, startedAt time.Time, outputBytes int64, success bool) {
	if s.telemetry == nil {
		return
	}
	ok := success
	s.telemetry.InsertToolUsage(context.Background(), telemetry.ToolUsage{
		SessionID:   req.SessionID,
		RequestID:   req.RequestID,
		JobID:       req.JobID,
		Tool:        "image_restore",
		MediaKind:   "image",
		Action:      "restore",
		Success:     &ok,
		DurationMS:  int(time.Since(startedAt) / time.Millisecond),
		InputBytes:  req.FileSizeBytes,
		OutputBytes: outputBytes,
		Options: map[string]any{
			"precleanCount":  len(req.Preclean),
			"modelCount":     len(req.Models),
			"faceModelCount": len(req.FaceModels),
			"chain":          req.Chain,
			"scale":          prep.scale,
			"outputCount":    len(units),
			"cropUsed":       req.Crop != nil,
			"sizeBucket":     imageRestoreSizeBucket(prep.cropPx.Height),
		},
	})
}

// imageRestoreSizeBucket maps a crop height onto a coarse ladder so telemetry
// never stores exact dimensions.
func imageRestoreSizeBucket(height int) string {
	switch {
	case height <= 0:
		return "unknown"
	case height <= 540:
		return "small"
	case height <= 1080:
		return "medium"
	case height <= 2160:
		return "large"
	default:
		return "xlarge"
	}
}

// ---------------------------------------------------------------------------
// Handler-facing helpers (stage planning + manifest-backed results listing)
// ---------------------------------------------------------------------------

// PlanStages builds the full stage timeline for a request so the handler can
// pre-populate it (making "queued" visible immediately).
func (s *ImageRestoreService) PlanStages(req ImageRestoreRequest) []models.TranscodeJobStage {
	units := orderImageRestoreOutputs(req.Preclean, req.Models, req.FaceModels, req.Chain)
	return s.BuildStages(units)
}

// resultsDirFor returns the on-disk results directory for a job.
func (s *ImageRestoreService) resultsDirFor(jobID string) string {
	return filepath.Join(s.cfg.OutputDir, jobID, "stage", imageRestoreResultBaseName)
}

// readManifest loads a completed job's manifest from disk.
func (s *ImageRestoreService) readManifest(jobID string) (imageRestoreManifest, error) {
	var m imageRestoreManifest
	raw, err := os.ReadFile(filepath.Join(s.resultsDirFor(jobID), "manifest.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	return m, nil
}

// Results projects a completed job's manifest into the API response. No
// filesystem paths are leaked — only result ids the preview endpoint resolves.
func (s *ImageRestoreService) Results(jobID string) (models.ImageRestoreResultsResponse, error) {
	var resp models.ImageRestoreResultsResponse
	m, err := s.readManifest(jobID)
	if err != nil {
		return resp, err
	}
	resp.JobID = jobID
	resp.Original = models.ImageRestoreResultEntry{
		ID:        "original",
		Label:     "Original (prepared)",
		Width:     m.Original.Width,
		Height:    m.Original.Height,
		FileName:  imageRestoreResultBaseName + "/original.png",
		SizeBytes: m.Original.SizeBytes,
		Status:    "completed",
	}
	resp.Results = make([]models.ImageRestoreResultEntry, 0, len(m.Outcomes))
	for _, o := range m.Outcomes {
		resp.Results = append(resp.Results, models.ImageRestoreResultEntry{
			ID:             o.ResultID,
			Label:          o.Label,
			Kind:           o.Kind,
			BaseModel:      o.BaseModel,
			Width:          o.OutputWidth,
			Height:         o.OutputHeight,
			FileName:       imageRestoreResultBaseName + "/" + o.FileName,
			SizeBytes:      o.SizeBytes,
			Status:         o.Status,
			Error:          o.Error,
			GenerativeNote: o.GenerativeNote,
			FidelityNote:   o.FidelityNote,
		})
	}
	return resp, nil
}

// ResultImagePath resolves a result id to an on-disk PNG using the manifest
// ONLY — the client-supplied id is matched against manifest entries, never
// joined to a path directly. Returns an error for unknown/failed ids.
func (s *ImageRestoreService) ResultImagePath(jobID, resultID string) (string, error) {
	dir := s.resultsDirFor(jobID)
	if resultID == "original" {
		p := filepath.Join(dir, "original.png")
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return p, nil
	}
	m, err := s.readManifest(jobID)
	if err != nil {
		return "", err
	}
	for _, o := range m.Outcomes {
		if o.ResultID == resultID && o.Status == "completed" && o.FileName != "" {
			p := filepath.Join(dir, o.FileName)
			if _, err := os.Stat(p); err != nil {
				return "", err
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("result not found")
}
