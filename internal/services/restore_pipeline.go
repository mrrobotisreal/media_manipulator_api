package services

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// The restoration pipeline. One goroutine per job:
//
//	queued → download → probe → extract_clip → extract_frames
//	  → model_<id> (selected models, cheap→expensive)
//	  → package → upload_result → completed
//
// A model that fails does NOT fail the job (its directory gets FAILED.txt and
// the manifest records the failure); the job fails only when ALL selected
// models fail. Every message written to the job is user-safe — raw subprocess
// output stays in server logs and command-audit rows.

const restoreResultBaseName = "restoration_results"

// restoreModelOutcome is the per-model record persisted into manifest.json.
type restoreModelOutcome struct {
	ID              string  `json:"id"`
	Group           string  `json:"group"`
	Status          string  `json:"status"` // completed | failed
	DurationSeconds float64 `json:"durationSeconds"`
	OutputWidth     int     `json:"outputWidth,omitempty"`
	OutputHeight    int     `json:"outputHeight,omitempty"`
	OutputFile      string  `json:"outputFile,omitempty"` // path relative to the tarball root
	FramesIncluded  bool    `json:"framesIncluded"`
	Error           string  `json:"error,omitempty"`
	Note            string  `json:"note,omitempty"`
}

type restoreManifest struct {
	JobID       string                 `json:"jobId"`
	GeneratedAt time.Time              `json:"generatedAt"`
	Request     restoreManifestRequest `json:"request"`
	Source      restoreManifestSource  `json:"source"`
	Models      []restoreModelOutcome  `json:"models"`
}

type restoreManifestRequest struct {
	FileName         string   `json:"fileName"`
	ClipStartSeconds float64  `json:"clipStartSeconds"`
	ClipEndSeconds   float64  `json:"clipEndSeconds"`
	RequestedScale   int      `json:"requestedScale"`
	EffectiveScale   int      `json:"effectiveScale"`
	IncludeFrames    bool     `json:"includeFrames"`
	Models           []string `json:"models"`
}

type restoreManifestSource struct {
	Width             int     `json:"width"`
	Height            int     `json:"height"`
	FPS               float64 `json:"fps"`
	FrameRateFraction string  `json:"frameRateFraction"`
	DurationSeconds   float64 `json:"durationSeconds"`
	HasAudio          bool    `json:"hasAudio"`
	VideoCodec        string  `json:"videoCodec,omitempty"`
	ClipFrames        int     `json:"clipFrames"`
}

// Process runs one restoration job end to end. Launched as a goroutine with
// context.Background(); the whole job (including queue wait) runs under a
// cfg.CommandTimeout deadline, with a tighter RestoreModelTimeout per model.
func (s *RestoreService) Process(ctx context.Context, req RestoreRequest) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.CommandTimeout)
	defer cancel()

	ordered := orderRestoreModels(req.Models)
	stages := s.BuildStages(ordered)
	advance := func(key string, status models.TranscodeStageStatus, progress int, message string) {
		stages = updateStage(stages, key, status, progress, message)
		_ = s.jobManager.ReplaceStages(req.JobID, stages, key)
		if progress > 0 {
			_ = s.jobManager.UpdateJobProgress(req.JobID, progress)
		}
	}
	startedAt := time.Now()

	// --- queued -----------------------------------------------------------
	// The job stays "pending" while it waits for a permit so queue state is
	// visible to the UI; only the stage spinner moves.
	advance("queued", models.StageStatusProcessing, 0, "")
	release, err := s.acquireRestorePermit(ctx)
	if err != nil {
		s.failRestore(req, stages, "Restoration timed out while waiting in the queue", err)
		return
	}
	defer release()
	if err := s.jobManager.UpdateJobStatus(req.JobID, models.StatusProcessing); err != nil {
		return
	}
	advance("queued", models.StageStatusCompleted, restoreProgressQueued, "")

	// --- workspace --------------------------------------------------------
	// Everything lives under the job-aware cleanup roots, in dirs named
	// exactly the job ID. The bulky work tree is removed by the deferred
	// cleanup below; only the tarball survives for the cleanup worker's
	// retention window.
	jobUploadDir := filepath.Join(s.cfg.UploadDir, req.JobID)
	jobOutputDir := filepath.Join(s.cfg.OutputDir, req.JobID)
	workDir := filepath.Join(jobOutputDir, "work")
	stageRoot := filepath.Join(jobOutputDir, "stage")
	resultsDir := filepath.Join(stageRoot, restoreResultBaseName)
	defer func() {
		_ = os.RemoveAll(workDir)
		_ = os.RemoveAll(stageRoot)
		_ = os.RemoveAll(jobUploadDir)
	}()
	for _, dir := range []string{jobUploadDir, workDir, resultsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.failRestore(req, stages, "Failed to prepare the job workspace", err)
			return
		}
	}

	// --- download ---------------------------------------------------------
	advance("download", models.StageStatusProcessing, 0, "")
	sourcePath := filepath.Join(jobUploadDir, "source"+strings.ToLower(filepath.Ext(req.S3Key)))
	if err := s.downloadRestoreSource(ctx, req.S3Key, sourcePath); err != nil {
		s.failRestore(req, stages, "Failed to download the uploaded video", err)
		return
	}
	advance("download", models.StageStatusCompleted, restoreProgressDownload, "")

	// --- probe ------------------------------------------------------------
	advance("probe", models.StageStatusProcessing, 0, "")
	probe, err := ProbeVideoReport(ctx, sourcePath)
	if err != nil {
		s.failRestore(req, stages, "Could not read the uploaded video", err)
		return
	}
	if probe.Width <= 0 || probe.Height <= 0 {
		s.failRestore(req, stages, "The uploaded file does not contain a video stream", nil)
		return
	}
	if probe.Width > s.cfg.RestoreMaxSourceWidth || probe.Height > s.cfg.RestoreMaxSourceHeight {
		s.failRestore(req, stages, fmt.Sprintf("Source resolution exceeds the maximum of %dx%d", s.cfg.RestoreMaxSourceWidth, s.cfg.RestoreMaxSourceHeight), nil)
		return
	}
	// Small tolerance for container metadata drift.
	if probe.DurationSeconds > 0 && req.ClipEndSeconds > probe.DurationSeconds+0.25 {
		s.failRestore(req, stages, "Selected clip extends past the end of the video", nil)
		return
	}
	window := req.ClipEndSeconds - req.ClipStartSeconds
	if err := ValidateRestoreFrameBudget(window, probe.FPS, s.cfg.RestoreMaxFrames); err != nil {
		s.failRestore(req, stages, err.Error(), nil)
		return
	}
	scale, err := ResolveRestoreScale(req.Scale, probe.Height)
	if err != nil {
		s.failRestore(req, stages, err.Error(), nil)
		return
	}
	// Stitching must reuse the EXACT source frame-rate fraction (e.g.
	// "30000/1001") so the re-muxed audio stays in sync.
	fpsFraction := strings.TrimSpace(probe.FrameRate)
	if fpsFraction == "" || fpsFraction == "0/0" {
		if probe.FPS <= 0 {
			s.failRestore(req, stages, "Could not determine the video frame rate", nil)
			return
		}
		fpsFraction = strconv.FormatFloat(probe.FPS, 'f', -1, 64)
	}
	hasAudio := probe.HasAudio
	advance("probe", models.StageStatusCompleted, restoreProgressProbe, "")

	// --- extract_clip -----------------------------------------------------
	advance("extract_clip", models.StageStatusProcessing, 0, "")
	clipPath := filepath.Join(workDir, "clip.mp4")
	clipArgs := buildRestoreClipArgs(sourcePath, req.ClipStartSeconds, window, clipPath)
	if err := s.runRestoreFFmpeg(ctx, req, "extract_clip", clipArgs); err != nil {
		s.failRestore(req, stages, "Failed to extract the selected clip", err)
		return
	}
	advance("extract_clip", models.StageStatusCompleted, restoreProgressExtractClip, "")

	// --- extract_frames ---------------------------------------------------
	advance("extract_frames", models.StageStatusProcessing, 0, "")
	framesDir := filepath.Join(workDir, "frames")
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		s.failRestore(req, stages, "Failed to prepare the frames workspace", err)
		return
	}
	frameArgs := []string{"-y", "-i", clipPath, "-vsync", "0", filepath.Join(framesDir, "%06d.png")}
	if err := s.runRestoreFFmpeg(ctx, req, "extract_frames", frameArgs); err != nil {
		s.failRestore(req, stages, "Failed to extract frames from the selected clip", err)
		return
	}
	frameCount, err := trimRestoreFrameOverflow(framesDir, s.cfg.RestoreMaxFrames)
	if err != nil {
		s.failRestore(req, stages, "Failed to extract frames from the selected clip", err)
		return
	}
	if frameCount == 0 {
		s.failRestore(req, stages, "No frames could be extracted from the selected clip", nil)
		return
	}
	advance("extract_frames", models.StageStatusCompleted, restoreProgressExtractFrames, "")

	// --- model runs -------------------------------------------------------
	outcomes := make([]restoreModelOutcome, 0, len(ordered))
	prevCheckpoint := restoreProgressExtractFrames
	for _, id := range ordered {
		stageKey := restoreModelStageKey(id)
		checkpoint := stageCheckpoint(stages, stageKey)
		advance(stageKey, models.StageStatusProcessing, 0, "")
		outcome := s.runRestoreModel(ctx, req, restoreModelRun{
			ID:            id,
			Scale:         scale,
			FramesDir:     framesDir,
			WorkDir:       workDir,
			ResultsDir:    resultsDir,
			ClipPath:      clipPath,
			FPSFraction:   fpsFraction,
			HasAudio:      hasAudio,
			IncludeFrames: req.IncludeFrames,
			FrameCount:    frameCount,
			SourceWidth:   probe.Width,
			SourceHeight:  probe.Height,
			ProgressFloor: prevCheckpoint,
			ProgressCeil:  checkpoint,
		})
		outcomes = append(outcomes, outcome)
		s.recordModelEvent(req, stageKey, outcome)
		if outcome.Status == "completed" {
			advance(stageKey, models.StageStatusCompleted, checkpoint, "")
		} else {
			advance(stageKey, models.StageStatusFailed, 0, outcome.Error)
			// Keep the overall bar moving past the failed model's span.
			_ = s.jobManager.UpdateJobProgress(req.JobID, checkpoint)
		}
		prevCheckpoint = checkpoint
	}

	// --- package ----------------------------------------------------------
	advance("package", models.StageStatusProcessing, 0, "")
	if restoreJobFailedEntirely(outcomes) {
		s.failRestore(req, stages, "All selected models failed — no results to package", nil)
		s.recordToolUsage(req, ordered, window, scale, probe, startedAt, 0, false)
		return
	}
	originalDir := filepath.Join(resultsDir, "original")
	if err := os.MkdirAll(originalDir, 0o755); err != nil {
		s.failRestore(req, stages, "Failed to package the results", err)
		return
	}
	if err := copyRestoreFile(clipPath, filepath.Join(originalDir, "clip.mp4")); err != nil {
		s.failRestore(req, stages, "Failed to package the results", err)
		return
	}
	manifest := restoreManifest{
		JobID:       req.JobID,
		GeneratedAt: time.Now().UTC(),
		Request: restoreManifestRequest{
			FileName:         req.FileName,
			ClipStartSeconds: req.ClipStartSeconds,
			ClipEndSeconds:   req.ClipEndSeconds,
			RequestedScale:   req.Scale,
			EffectiveScale:   scale,
			IncludeFrames:    req.IncludeFrames,
			Models:           restoreOutcomeModelIDs(ordered),
		},
		Source: restoreManifestSource{
			Width:             probe.Width,
			Height:            probe.Height,
			FPS:               probe.FPS,
			FrameRateFraction: fpsFraction,
			DurationSeconds:   probe.DurationSeconds,
			HasAudio:          hasAudio,
			VideoCodec:        probe.VideoCodec,
			ClipFrames:        frameCount,
		},
		Models: outcomes,
	}
	if err := writeJSON(filepath.Join(resultsDir, "manifest.json"), manifest); err != nil {
		s.failRestore(req, stages, "Failed to package the results", err)
		return
	}
	if err := writeRestoreReadme(filepath.Join(resultsDir, "README.txt"), manifest); err != nil {
		s.failRestore(req, stages, "Failed to package the results", err)
		return
	}
	tarPath := filepath.Join(jobOutputDir, restoreResultBaseName+".tar.gz")
	tarSize, err := createTarGz(stageRoot, tarPath)
	if err != nil {
		s.failRestore(req, stages, "Failed to package the results", err)
		return
	}
	advance("package", models.StageStatusCompleted, restoreProgressPackage, "")

	// --- upload_result ----------------------------------------------------
	advance("upload_result", models.StageStatusProcessing, 0, "")
	resultKey, presignedURL, expiresAt, err := s.uploadRestoreResult(ctx, req, tarPath)
	if err != nil {
		s.failRestore(req, stages, "Failed to upload the results package", err)
		return
	}
	_ = s.jobManager.SetResultMetadata(req.JobID, resultKey, restoreResultBaseName+".tar.gz", expiresAt)
	_ = s.jobManager.SetResultSize(req.JobID, tarSize)
	_ = s.jobManager.UpdateJobResult(req.JobID, presignedURL)
	advance("upload_result", models.StageStatusCompleted, restoreProgressUpload, "")
	advance("completed", models.StageStatusCompleted, 100, "")
	_ = s.jobManager.UpdateJobStatus(req.JobID, models.StatusCompleted)

	s.recordToolUsage(req, ordered, window, scale, probe, startedAt, tarSize, true)
}

// failRestore marks the active stage and the job failed with a USER-SAFE
// message; the underlying error goes to server logs only.
func (s *RestoreService) failRestore(req RestoreRequest, stages []models.TranscodeJobStage, userMsg string, err error) {
	if err != nil {
		log.Printf("video-restore job %s: %s: %v", req.JobID, userMsg, err)
	} else {
		log.Printf("video-restore job %s: %s", req.JobID, userMsg)
	}
	failed := updateStageStatus(stages, "completed", models.StageStatusFailed, userMsg)
	_ = s.jobManager.ReplaceStages(req.JobID, failed, "failed")
	_ = s.jobManager.UpdateJobError(req.JobID, userMsg)
}

// downloadRestoreSource streams the S3 object to disk (no full-buffer reads —
// sources can be a gigabyte).
func (s *RestoreService) downloadRestoreSource(ctx context.Context, key, path string) error {
	if s.s3Client == nil {
		return fmt.Errorf("s3 client not configured")
	}
	obj, err := s.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 get: %w", err)
	}
	defer obj.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, obj.Body); err != nil {
		return fmt.Errorf("s3 stream: %w", err)
	}
	return f.Close()
}

// runRestoreFFmpeg shells out to ffmpeg through the audited runner.
func (s *RestoreService) runRestoreFFmpeg(ctx context.Context, req RestoreRequest, stage string, args []string) error {
	res, err := s.runner.Run(ctx, cmdaudit.Spec{
		Tool:       "video_restore",
		Stage:      stage,
		Executable: "ffmpeg",
		Args:       args,
		RequestID:  req.RequestID,
		JobID:      req.JobID,
	})
	if err != nil {
		if res.TimedOut || ctx.Err() != nil {
			return fmt.Errorf("ffmpeg %s timed out", stage)
		}
		return fmt.Errorf("ffmpeg %s failed (exit %d): %s", stage, res.ExitCode, commandTail(res.Stderr, 1000))
	}
	return nil
}

// trimRestoreFrameOverflow counts the extracted PNGs and removes any tail
// beyond maxFrames (the fps estimate can be slightly off for VFR sources).
// Returns the final frame count.
func trimRestoreFrameOverflow(framesDir string, maxFrames int) (int, error) {
	names, err := listRestorePNGs(framesDir)
	if err != nil {
		return 0, err
	}
	if maxFrames > 0 && len(names) > maxFrames {
		log.Printf("video-restore: trimming %d excess frames (fps estimate was off)", len(names)-maxFrames)
		for _, name := range names[maxFrames:] {
			if err := os.Remove(filepath.Join(framesDir, name)); err != nil {
				return 0, err
			}
		}
		names = names[:maxFrames]
	}
	return len(names), nil
}

func listRestorePNGs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".png") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func countRestorePNGs(dir string) int {
	names, err := listRestorePNGs(dir)
	if err != nil {
		return 0
	}
	return len(names)
}

// restoreModelRun bundles everything one model run needs.
type restoreModelRun struct {
	ID            models.RestoreModelID
	Scale         int // effective scale (2 or 4)
	FramesDir     string
	WorkDir       string
	ResultsDir    string
	ClipPath      string
	FPSFraction   string
	HasAudio      bool
	IncludeFrames bool
	FrameCount    int
	SourceWidth   int
	SourceHeight  int
	ProgressFloor int
	ProgressCeil  int
}

// runRestoreModel executes one model under a GPU lease, stitches its output,
// and files the result (or a FAILED.txt) under the results dir. Never fails
// the job — failures are reported through the outcome.
func (s *RestoreService) runRestoreModel(ctx context.Context, req RestoreRequest, run restoreModelRun) restoreModelOutcome {
	id := run.ID
	started := time.Now()
	outcome := restoreModelOutcome{
		ID:    string(id),
		Group: models.RestoreModelGroup(id),
	}
	finishFailed := func(userMsg string, err error) restoreModelOutcome {
		if err != nil {
			log.Printf("video-restore job %s model %s: %s: %v", req.JobID, id, userMsg, err)
		} else {
			log.Printf("video-restore job %s model %s: %s", req.JobID, id, userMsg)
		}
		outcome.Status = "failed"
		outcome.Error = userMsg
		outcome.DurationSeconds = time.Since(started).Seconds()
		writeRestoreFailedMarker(filepath.Join(run.ResultsDir, string(id)), userMsg)
		return outcome
	}

	modelDir := filepath.Join(run.ResultsDir, string(id))
	outDir := filepath.Join(run.WorkDir, "out", string(id))
	outFramesDir := filepath.Join(outDir, "frames")
	if err := os.MkdirAll(outFramesDir, 0o755); err != nil {
		return finishFailed("Could not prepare the model workspace", err)
	}
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return finishFailed("Could not prepare the model workspace", err)
	}
	// Free the bulky x4 PNG tree as soon as the model is done with it.
	defer func() { _ = os.RemoveAll(outDir) }()

	// GPU: the process-wide scheduler gates capacity; the gpu.Manager lease
	// records the run in mm_gpu_jobs + Prometheus.
	gpuReq := GPURequest{
		Kind:            "restore-" + string(id),
		VRAMRequiredMiB: s.cfg.RestoreVRAMMiB[string(id)],
	}
	task := gpu.TaskRestoreSR
	if id == models.RestoreModelRealESRGAN {
		task = gpu.TaskRealESRGAN
		// realesrgan is light — prefer the small card, fall back to the big one.
		gpuReq.PreferredOrder = []int{s.cfg.AIRestoreVulkanGPU, s.cfg.AIRestoreCUDAGPU}
	} else {
		// PyTorch models need the 16GB card — pin it.
		force := s.cfg.AIRestoreCUDAGPU
		gpuReq.ForceIndex = &force
	}
	gpuIdx, releaseGPU, err := SharedGPUScheduler().Acquire(ctx, gpuReq)
	if err != nil {
		return finishFailed("Timed out waiting for a free GPU", err)
	}
	defer releaseGPU()
	lease, _ := s.gpuMgr.Acquire(ctx, task, "video_restore", req.JobID, req.RequestID)

	cudaIndex := s.cfg.AIRestoreCUDAGPU
	if id != models.RestoreModelRealESRGAN && gpuIdx >= 0 {
		cudaIndex = gpuIdx
	}
	cmd := buildRestoreModelCommand(id, restoreModelPaths{
		RealESRGANBin: s.cfg.RealESRGANBin,
		Python:        s.cfg.AIRestorePython,
		MMPython:      s.cfg.AIRestoreMMPython,
		FramesScript:  s.cfg.AIRestoreFramesScript,
		VideoScript:   s.cfg.AIRestoreVideoScript,
		ModelsDir:     s.cfg.AIRestoreModelsDir,
		ReposDir:      s.cfg.AIRestoreReposDir,
	}, run.FramesDir, outFramesDir, outDir, cudaIndex, s.cfg.AIRestoreVulkanGPU)

	// Live progress: python wrappers emit "PROGRESS n/N" on stdout; the
	// realesrgan binary doesn't, so a watcher counts output PNGs instead.
	onProgress := s.modelProgressFunc(req.JobID, run)
	progressOut := newRestoreProgressWriter(onProgress)
	modelCtx, cancelModel := context.WithTimeout(ctx, s.cfg.RestoreModelTimeout)
	defer cancelModel()
	if id == models.RestoreModelRealESRGAN {
		stopWatch := watchRestoreOutputDir(modelCtx, outFramesDir, run.FrameCount, onProgress)
		defer stopWatch()
	}

	res, runErr := s.runner.Run(modelCtx, cmdaudit.Spec{
		Tool:       "video_restore",
		Stage:      restoreModelStageKey(id),
		Executable: cmd.Executable,
		Args:       cmd.Args,
		ExtraEnv:   cmd.ExtraEnv,
		Stdout:     progressOut,
		RequestID:  req.RequestID,
		JobID:      req.JobID,
	})
	lease.Release(context.Background(), runErr)
	releaseGPU()
	if runErr != nil {
		if res.TimedOut || modelCtx.Err() != nil {
			return finishFailed("Model timed out — try a shorter clip", runErr)
		}
		log.Printf("video-restore job %s model %s output tail: %s", req.JobID, id, commandTail(res.Stderr, 2000))
		return finishFailed("Model failed while processing the clip", runErr)
	}

	// Integrity: every input frame must have an enhanced counterpart, or the
	// stitched video would silently drop the tail.
	outCount := countRestorePNGs(outFramesDir)
	if outCount != run.FrameCount {
		return finishFailed("Model produced incomplete output", fmt.Errorf("expected %d frames, found %d", run.FrameCount, outCount))
	}

	// Stitch — uniform for all six models, exact source fps fraction, audio
	// re-muxed from the reference clip when present.
	downscale := run.Scale == 2
	outName := fmt.Sprintf("%s_x%d.mp4", id, run.Scale)
	outPath := filepath.Join(modelDir, outName)
	stitchArgs := buildRestoreStitchArgs(run.FPSFraction, outFramesDir, run.ClipPath, run.HasAudio, downscale, outPath)
	if err := s.runRestoreFFmpeg(modelCtx, req, "stitch_"+string(id), stitchArgs); err != nil {
		return finishFailed("Failed to encode the enhanced video", err)
	}

	// Group A + includeFrames: keep the enhanced frames in the results.
	// Everything else: the deferred RemoveAll(outDir) reclaims the disk.
	if run.IncludeFrames && models.RestoreModelGroup(id) == models.RestoreGroupFrame {
		dst := filepath.Join(modelDir, "frames")
		if err := os.Rename(outFramesDir, dst); err != nil {
			log.Printf("video-restore job %s model %s: keeping frames failed: %v", req.JobID, id, err)
			outcome.Note = "Enhanced frames could not be included"
		} else {
			outcome.FramesIncluded = true
		}
	}

	outcome.Status = "completed"
	outcome.DurationSeconds = time.Since(started).Seconds()
	outcome.OutputWidth = run.SourceWidth * run.Scale
	outcome.OutputHeight = run.SourceHeight * run.Scale
	outcome.OutputFile = restoreResultBaseName + "/" + string(id) + "/" + outName
	if downscale {
		note := "Ran the native x4 network, downscaled to x2 during stitching"
		if outcome.Note != "" {
			outcome.Note += "; " + note
		} else {
			outcome.Note = note
		}
	}
	return outcome
}

// modelProgressFunc maps a model's done/total progress into the job's overall
// progress span for that model, throttled to whole-percent changes.
func (s *RestoreService) modelProgressFunc(jobID string, run restoreModelRun) func(done, total int) {
	lastPct := -1
	span := run.ProgressCeil - run.ProgressFloor
	return func(done, total int) {
		if total <= 0 {
			return
		}
		pct := run.ProgressFloor + span*done/total
		if pct <= lastPct {
			return
		}
		lastPct = pct
		_ = s.jobManager.UpdateJobProgress(jobID, pct)
	}
}

// watchRestoreOutputDir polls the output dir every 2s and reports the PNG
// count as progress — used for the realesrgan binary, which emits no
// machine-readable progress. Returns a stop func.
func watchRestoreOutputDir(ctx context.Context, dir string, total int, onProgress func(done, total int)) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				onProgress(countRestorePNGs(dir), total)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// restoreJobFailedEntirely implements the partial-failure rule: a model
// failure never fails the job UNLESS every selected model failed.
func restoreJobFailedEntirely(outcomes []restoreModelOutcome) bool {
	for _, o := range outcomes {
		if o.Status == "completed" {
			return false
		}
	}
	return true
}

// stageCheckpoint returns the planned completion progress for a stage key.
func stageCheckpoint(stages []models.TranscodeJobStage, key string) int {
	for _, st := range stages {
		if st.Key == key {
			return st.Progress
		}
	}
	return 0
}

func restoreOutcomeModelIDs(ids []models.RestoreModelID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

func writeRestoreFailedMarker(modelDir, userMsg string) {
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return
	}
	content := "This model failed to process the clip.\n\nReason: " + userMsg + "\n\nSee manifest.json at the root of this archive for the full run summary.\n"
	_ = os.WriteFile(filepath.Join(modelDir, "FAILED.txt"), []byte(content), 0o644)
}

func copyRestoreFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// writeRestoreReadme explains every directory in the tarball.
func writeRestoreReadme(path string, m restoreManifest) error {
	var b strings.Builder
	b.WriteString("AI Video Restoration — results package\n")
	b.WriteString("=======================================\n\n")
	fmt.Fprintf(&b, "Job: %s\nGenerated: %s\n", m.JobID, m.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "Clip: %.2fs – %.2fs of %q (%dx%d @ %s fps)\n", m.Request.ClipStartSeconds, m.Request.ClipEndSeconds, m.Request.FileName, m.Source.Width, m.Source.Height, m.Source.FrameRateFraction)
	fmt.Fprintf(&b, "Upscale: x%d\n\n", m.Request.EffectiveScale)
	b.WriteString("Contents\n--------\n")
	b.WriteString("manifest.json     Machine-readable run summary: request parameters, source\n")
	b.WriteString("                  probe, and per-model status, timings, and output dimensions.\n")
	b.WriteString("original/clip.mp4 The trimmed source snippet (with audio when present), for\n")
	b.WriteString("                  A/B comparison against every model's output.\n")
	for _, mo := range m.Models {
		dir := mo.ID + "/"
		switch {
		case mo.Status != "completed":
			fmt.Fprintf(&b, "%-17s This model FAILED — see FAILED.txt inside and manifest.json.\n", dir)
		case mo.Group == "frame":
			fmt.Fprintf(&b, "%-17s Enhanced MP4 (%s)", dir, filepath.Base(mo.OutputFile))
			if mo.FramesIncluded {
				b.WriteString(" plus the enhanced frames under frames/.")
			} else {
				b.WriteString(".")
			}
			b.WriteString("\n")
		default:
			fmt.Fprintf(&b, "%-17s Enhanced MP4 (%s). Video-restoration models work on the\n", dir, filepath.Base(mo.OutputFile))
			b.WriteString("                  whole sequence, so no per-frame outputs are included.\n")
		}
	}
	b.WriteString("\nEvery MP4 uses the exact source frame rate, so players can A/B them in\n")
	b.WriteString("sync. Frame-by-frame enhancers (Real-ESRGAN, SwinIR, HAT) and video\n")
	b.WriteString("restoration models (BasicVSR++, RVRT, VRT) trade speed for temporal\n")
	b.WriteString("consistency differently — compare moving detail, not just still frames.\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// uploadRestoreResult uploads the (multi-GB) tarball via a multipart upload
// and presigns a download URL with the long restore TTL — the archives are
// huge, so the default 30-minute result TTL would be hostile. The presigned
// GET URL itself has no size ceiling: S3 GET serves objects up to 5TB, and the
// signature is only checked when the download STARTS, so a long transfer that
// outlives the TTL still completes.
func (s *RestoreService) uploadRestoreResult(ctx context.Context, req RestoreRequest, localPath string) (string, string, time.Time, error) {
	if s.s3Client == nil || s.s3Presign == nil {
		return "", "", time.Time{}, fmt.Errorf("S3 client not configured")
	}
	prefix := s.cfg.S3ResultPrefix
	if prefix == "" {
		prefix = "results"
	}
	day := time.Now().UTC().Format("20060102")
	sessionPart := req.SessionID
	if sessionPart == "" {
		sessionPart = "anon"
	}
	resultKey := fmt.Sprintf("%s/%s/%s/%s/%s.tar.gz", prefix, day, sessionPart, req.JobID, restoreResultBaseName)

	// The results tarball routinely exceeds S3's 5GB single-PUT ceiling, so
	// PutObject can't be used — `aws s3 cp` does a multipart upload
	// transparently (parts sized automatically, up to 5TB total).
	if err := s.uploadLargeFileToS3(ctx, req, localPath, resultKey, "application/gzip"); err != nil {
		return "", "", time.Time{}, err
	}
	ttl := s.cfg.RestoreResultPresignTTL
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	disposition := fmt.Sprintf(`attachment; filename="%s.tar.gz"`, restoreResultBaseName)
	presigned, err := s.s3Presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(s.cfg.S3Bucket),
		Key:                        aws.String(resultKey),
		ResponseContentDisposition: aws.String(disposition),
	}, func(o *s3.PresignOptions) {
		o.Expires = ttl
	})
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("presign: %w", err)
	}
	return resultKey, presigned.URL, time.Now().UTC().Add(ttl), nil
}

// uploadLargeFileToS3 uploads localPath to s3://bucket/key with `aws s3 cp`,
// which does a multipart upload automatically — required because the
// restoration tarball routinely exceeds S3's 5GB single-PUT limit that
// PutObject is bound by. Credentials, region and (optional) endpoint come from
// the same process environment the SDK client already reads, so there is no
// separate credential wiring. The upload is given its own generous timeout,
// detached from the whole-job deadline, so a slow client link is never starved
// by time already spent in the GPU stages.
func (s *RestoreService) uploadLargeFileToS3(ctx context.Context, req RestoreRequest, localPath, key, contentType string) error {
	bin := strings.TrimSpace(s.cfg.AWSCLIBin)
	if bin == "" {
		bin = "aws"
	}
	dst := fmt.Sprintf("s3://%s/%s", s.cfg.S3Bucket, key)
	// Passing --content-type explicitly stops the CLI from guessing
	// "application/x-tar" + "Content-Encoding: gzip" for a .tar.gz, which would
	// make browsers silently gunzip the download and corrupt the archive.
	args := []string{
		"s3", "cp", localPath, dst,
		"--only-show-errors",
		"--content-type", contentType,
	}
	if region := strings.TrimSpace(s.cfg.AWSRegion); region != "" {
		args = append(args, "--region", region)
	}
	if endpoint := strings.TrimSpace(s.cfg.S3Endpoint); endpoint != "" {
		args = append(args, "--endpoint-url", endpoint)
	}

	timeout := s.cfg.RestoreUploadTimeout
	if timeout <= 0 {
		timeout = 3 * time.Hour
	}
	// WithoutCancel detaches from the (possibly nearly-exhausted) job deadline
	// while keeping the value chain; the dedicated timeout then bounds the
	// transfer on its own terms.
	uploadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	res, err := s.runner.Run(uploadCtx, cmdaudit.Spec{
		Tool:       "video_restore",
		Stage:      "upload_result",
		Executable: bin,
		Args:       args,
		RequestID:  req.RequestID,
		JobID:      req.JobID,
	})
	if err != nil {
		if res.TimedOut || uploadCtx.Err() != nil {
			return fmt.Errorf("s3 upload timed out after %s", timeout)
		}
		return fmt.Errorf("s3 upload failed (exit %d): %s", res.ExitCode, commandTail(res.Stderr, 1000))
	}
	return nil
}

// recordModelEvent writes one per-model timing/status row (privacy-safe:
// model id, duration, status — never filenames or paths).
func (s *RestoreService) recordModelEvent(req RestoreRequest, stageKey string, outcome restoreModelOutcome) {
	if s.telemetry == nil {
		return
	}
	status := outcome.Status
	s.telemetry.InsertJobEvent(context.Background(), telemetry.JobEvent{
		JobID:     req.JobID,
		RequestID: req.RequestID,
		EventName: "restore_model",
		Stage:     stageKey,
		Status:    status,
		Properties: map[string]any{
			"model":           outcome.ID,
			"group":           outcome.Group,
			"durationSeconds": outcome.DurationSeconds,
			"framesIncluded":  outcome.FramesIncluded,
		},
	})
}

// recordToolUsage writes the single privacy-safe tool-usage event for the
// whole job: derived metadata only (models, clip seconds, resolution bucket,
// timings, success) — never filenames, paths, or content.
func (s *RestoreService) recordToolUsage(req RestoreRequest, ordered []models.RestoreModelID, window float64, scale int, probe *models.VideoProbeResponse, startedAt time.Time, outputBytes int64, success bool) {
	if s.telemetry == nil {
		return
	}
	ok := success
	s.telemetry.InsertToolUsage(context.Background(), telemetry.ToolUsage{
		SessionID:   req.SessionID,
		RequestID:   req.RequestID,
		JobID:       req.JobID,
		Tool:        "video_restore",
		MediaKind:   "video",
		Action:      "restore",
		Success:     &ok,
		DurationMS:  int(time.Since(startedAt) / time.Millisecond),
		InputBytes:  req.FileSizeBytes,
		OutputBytes: outputBytes,
		Options: map[string]any{
			"models":           restoreOutcomeModelIDs(ordered),
			"modelCount":       len(ordered),
			"clipSeconds":      window,
			"scale":            scale,
			"includeFrames":    req.IncludeFrames,
			"resolutionBucket": restoreResolutionBucket(probe.Height),
		},
	})
}

// restoreResolutionBucket maps a source height onto a coarse ladder so
// telemetry never stores exact dimensions.
func restoreResolutionBucket(height int) string {
	switch {
	case height <= 0:
		return "unknown"
	case height <= 240:
		return "240p"
	case height <= 360:
		return "360p"
	case height <= 480:
		return "480p"
	case height <= 540:
		return "540p"
	case height <= 720:
		return "720p"
	case height <= 1080:
		return "1080p"
	default:
		return "above1080p"
	}
}
