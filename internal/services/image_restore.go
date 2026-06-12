package services

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// ImageRestoreService runs the AI Image Restoration & Upscaling pipeline: it is
// the still-image sibling of RestoreService. One uploaded image →
// prepare (decode + optional server-side crop) → optional pre-clean cleanup
// chain → general upscalers → face enhancers (optionally chained on general
// results) → one results tarball + inline comparison grid. Jobs flow through
// the shared JobManager so the UI reuses /api/job/:jobId (+ SSE).
//
// Like the video pipeline, every failure surfaced to the job is a user-safe
// message — raw subprocess output stays in server logs and command-audit rows.
type ImageRestoreService struct {
	cfg        *config.Config
	jobManager *JobManager
	gpuMgr     *gpu.Manager
	telemetry  *telemetry.Store
	runner     *cmdaudit.Runner
	// permits caps concurrent image-restoration jobs process-wide
	// (IMAGE_RESTORE_MAX_CONCURRENT_JOBS, default 1). Jobs beyond the cap stay
	// pending with stage "queued" until a permit frees up.
	permits chan struct{}
}

// NewImageRestoreService wires the image-restoration pipeline. gpuMgr, store
// and runner may be nil — the pipeline degrades gracefully.
func NewImageRestoreService(cfg *config.Config, jm *JobManager, gpuMgr *gpu.Manager, store *telemetry.Store, runner *cmdaudit.Runner) *ImageRestoreService {
	if runner == nil {
		runner = cmdaudit.NewRunner(nil, nil)
	}
	n := cfg.ImageRestoreMaxConcurrentJobs
	if n <= 0 {
		n = 1
	}
	permits := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		permits <- struct{}{}
	}
	return &ImageRestoreService{
		cfg:        cfg,
		jobManager: jm,
		gpuMgr:     gpuMgr,
		telemetry:  store,
		runner:     runner,
		permits:    permits,
	}
}

// Fixed forensic-honesty notes (non-negotiable product copy — see §2.3/§2.5).
const (
	imageRestoreGenerativeNote = "Face enhancement is generative reconstruction — output detail is synthesized and must not be treated as a faithful recovery of the source. Use for leads/visual clarity, not identification evidence."
	imageRestoreFidelityNote   = "Pre-clean models remove degradation without generating new content — output is a filtered version of the source signal."
)

// imageRestoreMinCropPx is the smallest crop (in pixels, after normalized→pixel
// conversion) the pipeline will run a model on.
const imageRestoreMinCropPx = 64

// ImageRestoreRequest carries one validated job into Process. Preclean, Models
// and FaceModels are already normalized (allowlisted, deduped, and Preclean
// reordered to the fixed semantic run order).
type ImageRestoreRequest struct {
	JobID              string
	SourcePath         string // the saved upload on disk
	FileName           string
	FileSizeBytes      int64
	ContentType        string
	Crop               *models.NormalizedRect
	Preclean           []models.ImageRestoreModelID
	Models             []models.ImageRestoreModelID
	FaceModels         []models.ImageRestoreModelID
	Chain              bool
	Scale              int // 0 auto, 2, 4
	CodeFormerFidelity float64
	FBCNNQualityFactor int
	SessionID          string
	RequestID          string
}

// ---------------------------------------------------------------------------
// Output units
// ---------------------------------------------------------------------------

// imageRestoreUnit is one output the pipeline produces. ResultID is the
// archive/result-grid id; StageKey is its stage in the job timeline.
type imageRestoreUnit struct {
	ResultID   string
	StageKey   string
	Kind       string
	Model      models.ImageRestoreModelID
	Base       models.ImageRestoreModelID // non-empty only for chained face units
	OnOriginal bool                       // face unit running on the working source
}

// orderImageRestoreOutputs expands a validated selection into the full,
// ordered list of output units.
//
// Order: pre-clean units (fixed semantic run order — they must run first since
// everything depends on the working source), then general models (cheapest
// first), then the face band. Within the face band, units are grouped per face
// model (gfpgan before codeformer): each face's on-original run, then — when
// chaining is on — that face run on each general result in general run order.
//
// This grouping reproduces the product-owner worked example in §2.1 verbatim
// (realesrgan, hat, gfpgan_on_original, gfpgan_on_realesrgan, gfpgan_on_hat,
// codeformer_on_original, codeformer_on_realesrgan, codeformer_on_hat) and the
// reversed pre-clean example in §2.5 (fbcnn, scunet, realesrgan). It is a
// deliberate, documented refinement of §3.3's band wording: the cheap face
// model still fully completes before the expensive one, so partials appear
// early.
func orderImageRestoreOutputs(p, g, f []models.ImageRestoreModelID, chain bool) []imageRestoreUnit {
	units := make([]imageRestoreUnit, 0, len(p)+len(g)+len(f)+len(f)*len(g))
	for _, id := range orderByReference(p, models.ImageRestorePrecleanRunOrder) {
		units = append(units, imageRestoreUnit{
			ResultID: "preclean_" + string(id),
			StageKey: "preclean_" + string(id),
			Kind:     models.ImageRestoreKindPreclean,
			Model:    id,
		})
	}
	general := orderByReference(g, models.ImageRestoreRunOrder)
	for _, id := range general {
		units = append(units, imageRestoreUnit{
			ResultID: string(id),
			StageKey: "model_" + string(id),
			Kind:     models.ImageRestoreKindGeneral,
			Model:    id,
		})
	}
	for _, fid := range orderByReference(f, models.ImageRestoreFaceRunOrder) {
		units = append(units, imageRestoreUnit{
			ResultID:   string(fid) + "_on_original",
			StageKey:   "face_" + string(fid) + "_original",
			Kind:       models.ImageRestoreKindFace,
			Model:      fid,
			OnOriginal: true,
		})
		if !chain {
			continue
		}
		for _, gid := range general {
			units = append(units, imageRestoreUnit{
				ResultID: string(fid) + "_on_" + string(gid),
				StageKey: "face_" + string(fid) + "_on_" + string(gid),
				Kind:     models.ImageRestoreKindFace,
				Model:    fid,
				Base:     gid,
			})
		}
	}
	return units
}

// orderByReference returns the members of selected that appear in reference, in
// reference order. Used to normalize selection order to a canonical run order.
func orderByReference(selected, reference []models.ImageRestoreModelID) []models.ImageRestoreModelID {
	want := make(map[models.ImageRestoreModelID]bool, len(selected))
	for _, id := range selected {
		want[id] = true
	}
	out := make([]models.ImageRestoreModelID, 0, len(selected))
	for _, id := range reference {
		if want[id] {
			out = append(out, id)
		}
	}
	return out
}

// imageRestoreShortName is a compact label used inside chained-unit labels.
func imageRestoreShortName(id models.ImageRestoreModelID) string {
	switch id {
	case models.ImageRestoreModelFBCNN:
		return "FBCNN"
	case models.ImageRestoreModelSCUNet:
		return "SCUNet"
	case models.ImageRestoreModelNAFNet:
		return "NAFNet"
	case models.ImageRestoreModelRealESRGAN:
		return "Real-ESRGAN"
	case models.ImageRestoreModelSwinIR:
		return "SwinIR"
	case models.ImageRestoreModelHAT:
		return "HAT"
	case models.ImageRestoreModelGFPGAN:
		return "GFPGAN"
	case models.ImageRestoreModelCodeFormer:
		return "CodeFormer"
	}
	return string(id)
}

// imageRestoreUnitLabel returns the human-readable label for a unit.
func imageRestoreUnitLabel(u imageRestoreUnit) string {
	switch u.Kind {
	case models.ImageRestoreKindPreclean:
		switch u.Model {
		case models.ImageRestoreModelFBCNN:
			return "After FBCNN artifact removal"
		case models.ImageRestoreModelSCUNet:
			return "After SCUNet denoise"
		case models.ImageRestoreModelNAFNet:
			return "After NAFNet deblur"
		}
		return "Pre-clean"
	case models.ImageRestoreKindFace:
		if u.OnOriginal {
			return models.ImageRestoreModelDisplayName(u.Model)
		}
		return imageRestoreShortName(u.Model) + " on " + imageRestoreShortName(u.Base) + " result"
	default:
		return models.ImageRestoreModelDisplayName(u.Model)
	}
}

// ---------------------------------------------------------------------------
// Validation helpers (pure — unit-tested in image_restore_test.go). Every
// returned error message is user-safe and may be sent to clients verbatim.
// ---------------------------------------------------------------------------

// normalizeImageRestoreList lowercases/trims/allowlists/dedupes a single list
// against the given kind, preserving first-seen order.
func normalizeImageRestoreList(raw []string, kind string) ([]models.ImageRestoreModelID, error) {
	seen := make(map[models.ImageRestoreModelID]bool, len(raw))
	out := make([]models.ImageRestoreModelID, 0, len(raw))
	for _, r := range raw {
		id := models.ImageRestoreModelID(strings.ToLower(strings.TrimSpace(r)))
		if !models.IsImageRestoreModelID(string(id)) {
			return nil, fmt.Errorf("One or more selected models are not recognized")
		}
		if models.ImageRestoreModelKind(id) != kind {
			return nil, fmt.Errorf("One or more selected models are not valid for this option")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// NormalizeImageRestoreModels normalizes the general and face model lists,
// rejecting cross-kind contamination (a general id in the face list, etc.).
func NormalizeImageRestoreModels(general, face []string) (g, f []models.ImageRestoreModelID, err error) {
	g, err = normalizeImageRestoreList(general, models.ImageRestoreKindGeneral)
	if err != nil {
		return nil, nil, err
	}
	f, err = normalizeImageRestoreList(face, models.ImageRestoreKindFace)
	if err != nil {
		return nil, nil, err
	}
	return g, f, nil
}

// NormalizeImageRestorePreclean normalizes the pre-clean list and reorders it
// to the fixed semantic run order regardless of request order (§2.5).
func NormalizeImageRestorePreclean(preclean []string) ([]models.ImageRestoreModelID, error) {
	p, err := normalizeImageRestoreList(preclean, models.ImageRestoreKindPreclean)
	if err != nil {
		return nil, err
	}
	return orderByReference(p, models.ImageRestorePrecleanRunOrder), nil
}

// ValidateImageRestoreSelection requires at least one model across all kinds.
func ValidateImageRestoreSelection(p, g, f []models.ImageRestoreModelID) error {
	if len(p)+len(g)+len(f) < 1 {
		return fmt.Errorf("Select at least one model")
	}
	return nil
}

// ValidateImageRestoreChain enforces that chaining requires at least one face
// model AND at least one general model.
func ValidateImageRestoreChain(chain bool, g, f []models.ImageRestoreModelID) error {
	if chain && (len(f) < 1 || len(g) < 1) {
		return fmt.Errorf("Chaining requires at least one face model and at least one upscaling model")
	}
	return nil
}

// CountImageRestoreOutputs returns p + g + f + (chain ? f*g : 0).
func CountImageRestoreOutputs(p, g, f int, chain bool) int {
	total := p + g + f
	if chain {
		total += f * g
	}
	return total
}

// ValidateImageRestoreOutputBudget rejects selections that would produce more
// than max output units.
func ValidateImageRestoreOutputBudget(count, max int) error {
	if max > 0 && count > max {
		return fmt.Errorf("That selection would produce %d outputs — the maximum is %d. Deselect some models or turn off chaining", count, max)
	}
	return nil
}

// ValidateFBCNNQualityFactor accepts 0 (blind/auto) or 1..100.
func ValidateFBCNNQualityFactor(qf int) error {
	if qf == 0 {
		return nil
	}
	if qf < 1 || qf > 100 {
		return fmt.Errorf("FBCNN quality factor must be between 1 and 100, or 0 for automatic")
	}
	return nil
}

// ValidateImageRestoreCrop validates a normalized crop rect (nil = whole image).
func ValidateImageRestoreCrop(r *models.NormalizedRect) error {
	if r == nil {
		return nil
	}
	const eps = 1e-6
	for _, v := range []float64{r.X, r.Y, r.Width, r.Height} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("Crop region is invalid")
		}
	}
	if r.X < 0 || r.Y < 0 {
		return fmt.Errorf("Crop region is invalid")
	}
	if r.Width <= 0 || r.Height <= 0 {
		return fmt.Errorf("Crop region is invalid")
	}
	if r.X+r.Width > 1+eps || r.Y+r.Height > 1+eps {
		return fmt.Errorf("Crop region extends past the edge of the image")
	}
	return nil
}

// ResolveImageRestoreScale resolves the requested scale against the crop
// height. 0 means auto: crops at or below 540px get x4, taller crops get x2.
// Output-size safety is the pixel budget (ValidateImageRestoreOutputPixels),
// not a hard resolution rule — stills legitimately go bigger than video.
func ResolveImageRestoreScale(scale, cropHeightPx int) (int, error) {
	switch scale {
	case 0:
		if cropHeightPx > 0 && cropHeightPx <= 540 {
			return 4, nil
		}
		return 2, nil
	case 2:
		return 2, nil
	case 4:
		return 4, nil
	default:
		return 0, fmt.Errorf("Scale must be 0 (auto), 2, or 4")
	}
}

// ValidateImageRestoreOutputPixels rejects crops whose upscaled output would
// exceed the configured pixel budget, telling the user the options for THEIR
// crop.
func ValidateImageRestoreOutputPixels(cropW, cropH, scale int, maxPixels int64) error {
	if maxPixels <= 0 {
		return nil
	}
	out := int64(cropW*scale) * int64(cropH*scale)
	if out <= maxPixels {
		return nil
	}
	return fmt.Errorf("The upscaled output would be too large (%d megapixels, the maximum is %d) — choose 2x or a smaller crop", out/1_000_000, maxPixels/1_000_000)
}

// ClampCodeFormerFidelity defaults NaN/0 to 0.7 and clamps to [0,1].
func ClampCodeFormerFidelity(w float64) float64 {
	if math.IsNaN(w) || w == 0 {
		return 0.7
	}
	if w < 0 {
		return 0
	}
	if w > 1 {
		return 1
	}
	return w
}

// ImageRestoreCropToPixels converts a normalized crop to integer pixel bounds
// against the prepared source dimensions, clamping to the image and enforcing
// the 64×64 minimum. A nil rect means the whole image.
func ImageRestoreCropToPixels(r *models.NormalizedRect, imgW, imgH int) (x, y, w, h int, err error) {
	if imgW <= 0 || imgH <= 0 {
		return 0, 0, 0, 0, fmt.Errorf("Could not read the image dimensions")
	}
	if r == nil {
		return 0, 0, imgW, imgH, nil
	}
	x = int(math.Round(r.X * float64(imgW)))
	y = int(math.Round(r.Y * float64(imgH)))
	w = int(math.Round(r.Width * float64(imgW)))
	h = int(math.Round(r.Height * float64(imgH)))
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= imgW {
		x = imgW - 1
	}
	if y >= imgH {
		y = imgH - 1
	}
	if x+w > imgW {
		w = imgW - x
	}
	if y+h > imgH {
		h = imgH - y
	}
	if w < imageRestoreMinCropPx || h < imageRestoreMinCropPx {
		return 0, 0, 0, 0, fmt.Errorf("The selected crop is too small — select an area at least %d×%d pixels", imageRestoreMinCropPx, imageRestoreMinCropPx)
	}
	return x, y, w, h, nil
}

// ---------------------------------------------------------------------------
// Availability + capabilities
// ---------------------------------------------------------------------------

// ModelAvailability reports whether a model can run right now (feature flags
// plus on-disk script/venv/binary/weights/repo checks) and, when it cannot, a
// short user-safe reason. General/face/preclean kinds are independent: a
// missing pre-clean venv never disables the general models.
func (s *ImageRestoreService) ModelAvailability(id models.ImageRestoreModelID) (bool, string) {
	modelsDir := s.cfg.AIRestoreModelsDir
	reposDir := s.cfg.AIRestoreReposDir
	switch id {
	case models.ImageRestoreModelRealESRGAN:
		if !statOK(s.cfg.RealESRGANBin) {
			return false, "Real-ESRGAN is not installed on this server"
		}
	case models.ImageRestoreModelSwinIR, models.ImageRestoreModelHAT:
		if !statOK(s.cfg.AIRestorePython) || !statOK(s.cfg.AIRestoreFramesScript) {
			return false, "This model's environment is not installed on this server"
		}
	case models.ImageRestoreModelGFPGAN:
		if !statOK(s.cfg.AIFaceRestorePython) || !statOK(s.cfg.AIFaceRestoreScript) {
			return false, "Face enhancement is not installed on this server"
		}
		if !statOK(modelsDir + "/gfpgan/GFPGANv1.4.pth") {
			return false, "The GFPGAN model weights are not installed on this server"
		}
	case models.ImageRestoreModelCodeFormer:
		if !s.cfg.ImageRestoreCodeFormerEnabled {
			return false, "CodeFormer is currently disabled on this server"
		}
		if !statOK(s.cfg.AIFaceRestorePython) || !statOK(s.cfg.AIFaceRestoreScript) {
			return false, "Face enhancement is not installed on this server"
		}
		if !statOK(modelsDir+"/codeformer/codeformer.pth") || !statOK(reposDir+"/CodeFormer") {
			return false, "The CodeFormer model is not installed on this server"
		}
	case models.ImageRestoreModelFBCNN:
		return s.precleanAvailability("fbcnn", "FBCNN", modelsDir+"/fbcnn/fbcnn_color.pth", reposDir+"/FBCNN")
	case models.ImageRestoreModelSCUNet:
		return s.precleanAvailability("scunet", "SCUNet", modelsDir+"/scunet/scunet_color_real_psnr.pth", reposDir+"/SCUNet")
	case models.ImageRestoreModelNAFNet:
		return s.precleanAvailability("nafnet", "NAFNet", modelsDir+"/nafnet/NAFNet-GoPro-width64.pth", reposDir+"/NAFNet")
	default:
		return false, "Unknown model"
	}
	return true, ""
}

func (s *ImageRestoreService) precleanAvailability(_, display, weights, repo string) (bool, string) {
	if !statOK(s.cfg.AIPrecleanPython) || !statOK(s.cfg.AIPrecleanScript) {
		return false, "The pre-clean models are not installed on this server"
	}
	if !statOK(weights) {
		return false, "The " + display + " model weights are not installed on this server"
	}
	if !statOK(repo) {
		return false, "The " + display + " model is not installed on this server"
	}
	return true, ""
}

// Capabilities assembles the GET /api/image-restore/capabilities response.
func (s *ImageRestoreService) Capabilities() models.ImageRestoreCapabilitiesResponse {
	infos := make([]models.ImageRestoreModelInfo, 0, len(models.AllImageRestoreModelIDs))
	for _, id := range models.AllImageRestoreModelIDs {
		available, reason := s.ModelAvailability(id)
		infos = append(infos, models.ImageRestoreModelInfo{
			ID:                     id,
			Kind:                   models.ImageRestoreModelKind(id),
			DisplayName:            models.ImageRestoreModelDisplayName(id),
			Scales:                 models.ImageRestoreModelScales(id),
			Available:              available,
			Reason:                 reason,
			EstSecondsPerMegapixel: s.cfg.ImageRestoreEstSecondsPerMegapixel[string(id)],
		})
	}
	return models.ImageRestoreCapabilitiesResponse{
		Enabled:            s.cfg.ImageRestoreEnabled,
		MaxSourceWidth:     s.cfg.ImageRestoreMaxSourceWidth,
		MaxSourceHeight:    s.cfg.ImageRestoreMaxSourceHeight,
		MaxUploadSizeBytes: s.cfg.MaxFileSize,
		MaxOutputPixels:    s.cfg.ImageRestoreMaxOutputPixels,
		MaxOutputs:         s.cfg.ImageRestoreMaxOutputs,
		ChainSupported:     true,
		Models:             infos,
	}
}

// ---------------------------------------------------------------------------
// Stage timeline
// ---------------------------------------------------------------------------

const (
	imageRestoreProgressQueued  = 2
	imageRestoreProgressPrepare = 6
	imageRestoreUnitSpanStart   = 8
	imageRestoreUnitSpanEnd     = 92
	imageRestoreProgressPackage = 96
)

// BuildStages pre-populates the full stage timeline for an image-restoration
// job. Output-unit stages share the span between imageRestoreUnitSpanStart and
// imageRestoreUnitSpanEnd, weighted by each model's estimated
// seconds-per-megapixel so the overall bar tracks wall-clock time roughly.
func (s *ImageRestoreService) BuildStages(units []imageRestoreUnit) []models.TranscodeJobStage {
	stages := []models.TranscodeJobStage{
		{Key: "queued", Label: "Queued", Status: models.StageStatusPending, Progress: imageRestoreProgressQueued},
		{Key: "prepare", Label: "Preparing image", Status: models.StageStatusPending, Progress: imageRestoreProgressPrepare},
	}

	var totalWeight float64
	for _, u := range units {
		totalWeight += s.unitWeight(u)
	}
	span := float64(imageRestoreUnitSpanEnd - imageRestoreUnitSpanStart)
	var accumulated float64
	for _, u := range units {
		accumulated += s.unitWeight(u)
		checkpoint := imageRestoreUnitSpanEnd
		if totalWeight > 0 {
			checkpoint = imageRestoreUnitSpanStart + int(span*accumulated/totalWeight)
		}
		stages = append(stages, models.TranscodeJobStage{
			Key:      u.StageKey,
			Label:    imageRestoreUnitLabel(u),
			Status:   models.StageStatusPending,
			Progress: checkpoint,
		})
	}

	stages = append(stages,
		models.TranscodeJobStage{Key: "package", Label: "Packaging results", Status: models.StageStatusPending, Progress: imageRestoreProgressPackage},
		models.TranscodeJobStage{Key: "completed", Label: "Completed", Status: models.StageStatusPending, Progress: 100},
	)
	return stages
}

func (s *ImageRestoreService) unitWeight(u imageRestoreUnit) float64 {
	w := s.cfg.ImageRestoreEstSecondsPerMegapixel[string(u.Model)]
	if w <= 0 {
		w = 1
	}
	return w
}

// acquireImageRestorePermit blocks until a job slot frees up (or ctx ends).
func (s *ImageRestoreService) acquireImageRestorePermit(ctx context.Context) (func(), error) {
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
