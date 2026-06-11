package services

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// RestoreService runs the AI Video Restoration pipeline: download → probe →
// extract clip → extract frames → run each selected model under a GPU lease →
// stitch → package one results tarball → upload + presign. Jobs flow through
// the shared JobManager so the UI reuses /api/job/:jobId (+ SSE).
//
// Unlike the transcode pipeline, every failure surfaced to the job is a
// user-safe message — raw subprocess output stays in server logs and command
// audit rows only.
type RestoreService struct {
	cfg        *config.Config
	jobManager *JobManager
	s3Client   *s3.Client
	s3Presign  *s3.PresignClient
	gpuMgr     *gpu.Manager
	telemetry  *telemetry.Store
	runner     *cmdaudit.Runner
	// permits is a token-bucket semaphore capping concurrent restoration jobs
	// process-wide (RESTORE_MAX_CONCURRENT_JOBS, default 1). Jobs beyond the
	// cap stay pending with stage "queued" until a permit frees up.
	permits chan struct{}
}

// NewRestoreService wires the restoration pipeline. gpuMgr, store and runner
// may be nil (telemetry DB offline) — the pipeline degrades gracefully.
func NewRestoreService(cfg *config.Config, jm *JobManager, s3Client *s3.Client, gpuMgr *gpu.Manager, store *telemetry.Store, runner *cmdaudit.Runner) *RestoreService {
	var presign *s3.PresignClient
	if s3Client != nil {
		presign = s3.NewPresignClient(s3Client)
	}
	if runner == nil {
		runner = cmdaudit.NewRunner(nil, nil)
	}
	n := cfg.RestoreMaxConcurrentJobs
	if n <= 0 {
		n = 1
	}
	permits := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		permits <- struct{}{}
	}
	return &RestoreService{
		cfg:        cfg,
		jobManager: jm,
		s3Client:   s3Client,
		s3Presign:  presign,
		gpuMgr:     gpuMgr,
		telemetry:  store,
		runner:     runner,
		permits:    permits,
	}
}

// RestoreRequest carries one validated restoration job into Process. Models
// is already normalized (allowlisted, deduped) but not necessarily in run
// order — Process orders it.
type RestoreRequest struct {
	JobID            string
	S3Key            string
	FileName         string
	FileSizeBytes    int64
	ClipStartSeconds float64
	ClipEndSeconds   float64
	Models           []models.RestoreModelID
	Scale            int // 0 = auto, 2, 4
	IncludeFrames    bool
	SessionID        string
	RequestID        string
}

// ---------------------------------------------------------------------------
// Validation helpers (pure — unit-tested in restore_test.go). Every returned
// error message is user-safe and may be sent to clients verbatim.
// ---------------------------------------------------------------------------

// restoreMinClipSeconds is the smallest selectable window.
const restoreMinClipSeconds = 0.5

// ValidateRestoreClipWindow checks the requested snippet window against the
// configured maximum. The boundary is inclusive: a window of exactly
// maxSeconds is allowed.
func ValidateRestoreClipWindow(start, end, maxSeconds float64) error {
	if start < 0 {
		return fmt.Errorf("Clip start cannot be negative")
	}
	if end <= start {
		return fmt.Errorf("Clip end must be after clip start")
	}
	window := end - start
	if window < restoreMinClipSeconds {
		return fmt.Errorf("Selected clip is too short — select at least %.1f seconds", restoreMinClipSeconds)
	}
	if window > maxSeconds {
		return fmt.Errorf("Selected clip is too long — the maximum is %g seconds", maxSeconds)
	}
	return nil
}

// NormalizeRestoreModels lowercases, trims, allowlists and dedupes the
// client-supplied model ids (preserving first-seen order). Unknown ids are
// rejected without echoing raw client input back.
func NormalizeRestoreModels(raw []string) ([]models.RestoreModelID, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("Select at least one restoration model")
	}
	seen := make(map[models.RestoreModelID]bool, len(raw))
	out := make([]models.RestoreModelID, 0, len(raw))
	for _, r := range raw {
		id := models.RestoreModelID(strings.ToLower(strings.TrimSpace(r)))
		if !models.IsRestoreModelID(string(id)) {
			return nil, fmt.Errorf("One or more selected models are not recognized")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Select at least one restoration model")
	}
	return out, nil
}

// ResolveRestoreScale resolves the requested scale against the source height.
// 0 means auto: sources at or below 540p get x4, taller sources get x2.
// Explicit x4 is rejected for sources taller than 1080p (the output would be
// enormous); x4 on 541–1080p sources is allowed.
func ResolveRestoreScale(scale, sourceHeight int) (int, error) {
	switch scale {
	case 0:
		if sourceHeight > 0 && sourceHeight <= 540 {
			return 4, nil
		}
		return 2, nil
	case 2:
		return 2, nil
	case 4:
		if sourceHeight > 1080 {
			return 0, fmt.Errorf("4x upscaling is only available for sources up to 1080p — choose 2x instead")
		}
		return 4, nil
	default:
		return 0, fmt.Errorf("Scale must be 0 (auto), 2, or 4")
	}
}

// EstimateRestoreFrames estimates how many frames the selected window covers.
func EstimateRestoreFrames(windowSeconds, fps float64) int {
	if windowSeconds <= 0 || fps <= 0 {
		return 0
	}
	return int(windowSeconds*fps + 0.5)
}

// ValidateRestoreFrameBudget rejects windows whose estimated frame count
// exceeds maxFrames, telling the user the maximum duration for THEIR fps.
func ValidateRestoreFrameBudget(windowSeconds, fps float64, maxFrames int) error {
	if maxFrames <= 0 || fps <= 0 {
		return nil
	}
	if EstimateRestoreFrames(windowSeconds, fps) <= maxFrames {
		return nil
	}
	maxSeconds := float64(maxFrames) / fps
	return fmt.Errorf("Selected clip covers too many frames — at %.4g fps the maximum clip length is %.1f seconds", fps, maxSeconds)
}

// orderRestoreModels returns the selected models in pipeline run order
// (cheapest first, so partial results exist early).
func orderRestoreModels(selected []models.RestoreModelID) []models.RestoreModelID {
	want := make(map[models.RestoreModelID]bool, len(selected))
	for _, id := range selected {
		want[id] = true
	}
	out := make([]models.RestoreModelID, 0, len(selected))
	for _, id := range models.RestoreModelRunOrder {
		if want[id] {
			out = append(out, id)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Availability + capabilities
// ---------------------------------------------------------------------------

func statOK(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// ModelAvailability reports whether a model can run right now (feature flags
// plus on-disk script/venv/binary checks) and, when it cannot, a short
// user-safe reason.
func (s *RestoreService) ModelAvailability(id models.RestoreModelID) (bool, string) {
	switch id {
	case models.RestoreModelRealESRGAN:
		if !statOK(s.cfg.RealESRGANBin) {
			return false, "Real-ESRGAN is not installed on this server"
		}
	case models.RestoreModelSwinIR, models.RestoreModelHAT:
		if !statOK(s.cfg.AIRestorePython) || !statOK(s.cfg.AIRestoreFramesScript) {
			return false, "This model's environment is not installed on this server"
		}
	case models.RestoreModelBasicVSRPP:
		if !s.cfg.RestoreBasicVSRPPEnabled {
			return false, "BasicVSR++ is currently disabled on this server"
		}
		if !statOK(s.cfg.AIRestoreMMPython) || !statOK(s.cfg.AIRestoreVideoScript) {
			return false, "This model's environment is not installed on this server"
		}
	case models.RestoreModelRVRT, models.RestoreModelVRT:
		if !statOK(s.cfg.AIRestorePython) || !statOK(s.cfg.AIRestoreVideoScript) {
			return false, "This model's environment is not installed on this server"
		}
	default:
		return false, "Unknown model"
	}
	return true, ""
}

// Capabilities assembles the GET /api/video-restore/capabilities response.
func (s *RestoreService) Capabilities() models.RestoreCapabilitiesResponse {
	infos := make([]models.RestoreModelInfo, 0, len(models.AllRestoreModelIDs))
	for _, id := range models.AllRestoreModelIDs {
		available, reason := s.ModelAvailability(id)
		infos = append(infos, models.RestoreModelInfo{
			ID:                 id,
			Group:              models.RestoreModelGroup(id),
			DisplayName:        models.RestoreModelDisplayName(id),
			Scales:             models.RestoreModelScales(id),
			Available:          available,
			Reason:             reason,
			EstSecondsPerFrame: s.cfg.RestoreEstSecondsPerFrame[string(id)],
		})
	}
	return models.RestoreCapabilitiesResponse{
		Enabled:                s.cfg.RestoreEnabled,
		MaxClipSeconds:         s.cfg.RestoreMaxClipSeconds,
		RecommendedClipSeconds: s.cfg.RestoreRecommendedClipSeconds,
		MaxFrames:              s.cfg.RestoreMaxFrames,
		MaxSourceWidth:         s.cfg.RestoreMaxSourceWidth,
		MaxSourceHeight:        s.cfg.RestoreMaxSourceHeight,
		MaxUploadSizeBytes:     s.cfg.MaxVideoUpload,
		ResultLinkTTLSeconds:   int(s.cfg.RestoreResultPresignTTL.Seconds()),
		Models:                 infos,
	}
}

// ---------------------------------------------------------------------------
// Stage timeline
// ---------------------------------------------------------------------------

// Progress checkpoints for the fixed (non-model) stages. The model stages
// share the span between restoreModelSpanStart and restoreModelSpanEnd,
// weighted by each model's estimated seconds-per-frame so the overall bar
// tracks wall-clock time roughly.
const (
	restoreProgressQueued        = 2
	restoreProgressDownload      = 5
	restoreProgressProbe         = 8
	restoreProgressExtractClip   = 11
	restoreProgressExtractFrames = 14
	restoreModelSpanStart        = 15
	restoreModelSpanEnd          = 88
	restoreProgressPackage       = 93
	restoreProgressUpload        = 98
)

// BuildStages pre-populates the full stage timeline for a restoration job.
// Only the selected models appear, in run order. All stages start pending —
// the queued stage flips to processing the moment Process starts waiting for
// a permit, which is what makes queue position visible to the UI.
func (s *RestoreService) BuildStages(selected []models.RestoreModelID) []models.TranscodeJobStage {
	ordered := orderRestoreModels(selected)
	stages := []models.TranscodeJobStage{
		{Key: "queued", Label: "Queued", Status: models.StageStatusPending, Progress: restoreProgressQueued},
		{Key: "download", Label: "Downloading source", Status: models.StageStatusPending, Progress: restoreProgressDownload},
		{Key: "probe", Label: "Probing source", Status: models.StageStatusPending, Progress: restoreProgressProbe},
		{Key: "extract_clip", Label: "Extracting clip", Status: models.StageStatusPending, Progress: restoreProgressExtractClip},
		{Key: "extract_frames", Label: "Extracting frames", Status: models.StageStatusPending, Progress: restoreProgressExtractFrames},
	}

	var totalWeight float64
	for _, id := range ordered {
		totalWeight += s.modelWeight(id)
	}
	span := float64(restoreModelSpanEnd - restoreModelSpanStart)
	var accumulated float64
	for _, id := range ordered {
		accumulated += s.modelWeight(id)
		checkpoint := restoreModelSpanEnd
		if totalWeight > 0 {
			checkpoint = restoreModelSpanStart + int(span*accumulated/totalWeight)
		}
		stages = append(stages, models.TranscodeJobStage{
			Key:      restoreModelStageKey(id),
			Label:    models.RestoreModelDisplayName(id),
			Status:   models.StageStatusPending,
			Progress: checkpoint,
		})
	}

	stages = append(stages,
		models.TranscodeJobStage{Key: "package", Label: "Packaging results", Status: models.StageStatusPending, Progress: restoreProgressPackage},
		models.TranscodeJobStage{Key: "upload_result", Label: "Uploading results", Status: models.StageStatusPending, Progress: restoreProgressUpload},
		models.TranscodeJobStage{Key: "completed", Label: "Completed", Status: models.StageStatusPending, Progress: 100},
	)
	return stages
}

func restoreModelStageKey(id models.RestoreModelID) string {
	return "model_" + string(id)
}

func (s *RestoreService) modelWeight(id models.RestoreModelID) float64 {
	w := s.cfg.RestoreEstSecondsPerFrame[string(id)]
	if w <= 0 {
		w = 1
	}
	return w
}

// acquireRestorePermit blocks until a job slot frees up (or ctx ends).
// Returns a sync-safe release func.
func (s *RestoreService) acquireRestorePermit(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.permits:
	}
	var once sync.Once
	return func() {
		once.Do(func() { s.permits <- struct{}{} })
	}, nil
}
