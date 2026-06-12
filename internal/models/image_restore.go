package models

// AI Image Restoration & Upscaling — request/response contracts for
// GET  /api/image-restore/capabilities
// POST /api/image-restore/start                 (multipart: image + options JSON)
// GET  /api/image-restore/:jobId/results
// GET  /api/image-restore/:jobId/result/:resultId
//
// This is the still-image sibling of AI Video Restoration. Jobs reuse
// ConversionJob (mode "image_restore") and the shared /api/job/:jobId (+ SSE)
// machinery, so there is no bespoke job snapshot here.

// ImageRestoreModelID identifies one of the eight supported models. IDs double
// as result-id fragments inside the results archive and as keys for the config
// tuning maps — they must stay lowercase and filesystem-safe.
type ImageRestoreModelID string

const (
	ImageRestoreModelFBCNN      ImageRestoreModelID = "fbcnn"      // kind: preclean
	ImageRestoreModelSCUNet     ImageRestoreModelID = "scunet"     // kind: preclean
	ImageRestoreModelNAFNet     ImageRestoreModelID = "nafnet"     // kind: preclean
	ImageRestoreModelRealESRGAN ImageRestoreModelID = "realesrgan" // kind: general
	ImageRestoreModelSwinIR     ImageRestoreModelID = "swinir"     // kind: general
	ImageRestoreModelHAT        ImageRestoreModelID = "hat"        // kind: general
	ImageRestoreModelGFPGAN     ImageRestoreModelID = "gfpgan"     // kind: face
	ImageRestoreModelCodeFormer ImageRestoreModelID = "codeformer" // kind: face
)

// Model kinds. Pre-clean models are fidelity-preserving cleanup (no synthesis,
// always 1x); general models are upscalers; face models are generative face
// priors.
const (
	ImageRestoreKindPreclean = "preclean"
	ImageRestoreKindGeneral  = "general"
	ImageRestoreKindFace     = "face"
)

// AllImageRestoreModelIDs is the canonical capabilities/display order:
// pre-clean (in semantic run order), then general (cheapest first), then face.
var AllImageRestoreModelIDs = []ImageRestoreModelID{
	ImageRestoreModelFBCNN,
	ImageRestoreModelSCUNet,
	ImageRestoreModelNAFNet,
	ImageRestoreModelRealESRGAN,
	ImageRestoreModelSwinIR,
	ImageRestoreModelHAT,
	ImageRestoreModelGFPGAN,
	ImageRestoreModelCodeFormer,
}

// ImageRestorePrecleanRunOrder is the FIXED semantic order pre-clean models run
// in, regardless of selection order: compression-artifact removal must come off
// before denoising, and deblurring runs last on the cleanest signal. This is a
// semantic order, NOT cheapest-first.
var ImageRestorePrecleanRunOrder = []ImageRestoreModelID{
	ImageRestoreModelFBCNN,
	ImageRestoreModelSCUNet,
	ImageRestoreModelNAFNet,
}

// ImageRestoreRunOrder is the general-model execution order, cheapest first so
// partial results exist early if an expensive model dies late in the run.
var ImageRestoreRunOrder = []ImageRestoreModelID{
	ImageRestoreModelRealESRGAN,
	ImageRestoreModelSwinIR,
	ImageRestoreModelHAT,
}

// ImageRestoreFaceRunOrder is the face-model execution order (gfpgan before
// codeformer — gfpgan is cheaper).
var ImageRestoreFaceRunOrder = []ImageRestoreModelID{
	ImageRestoreModelGFPGAN,
	ImageRestoreModelCodeFormer,
}

var imageRestoreModelKinds = map[ImageRestoreModelID]string{
	ImageRestoreModelFBCNN:      ImageRestoreKindPreclean,
	ImageRestoreModelSCUNet:     ImageRestoreKindPreclean,
	ImageRestoreModelNAFNet:     ImageRestoreKindPreclean,
	ImageRestoreModelRealESRGAN: ImageRestoreKindGeneral,
	ImageRestoreModelSwinIR:     ImageRestoreKindGeneral,
	ImageRestoreModelHAT:        ImageRestoreKindGeneral,
	ImageRestoreModelGFPGAN:     ImageRestoreKindFace,
	ImageRestoreModelCodeFormer: ImageRestoreKindFace,
}

var imageRestoreModelDisplayNames = map[ImageRestoreModelID]string{
	ImageRestoreModelFBCNN:      "FBCNN (JPEG artifact removal)",
	ImageRestoreModelSCUNet:     "SCUNet (denoise)",
	ImageRestoreModelNAFNet:     "NAFNet (deblur)",
	ImageRestoreModelRealESRGAN: "Real-ESRGAN",
	ImageRestoreModelSwinIR:     "SwinIR (Real-SR Large)",
	ImageRestoreModelHAT:        "HAT (Real_HAT_GAN)",
	ImageRestoreModelGFPGAN:     "GFPGAN v1.4",
	ImageRestoreModelCodeFormer: "CodeFormer",
}

// IsImageRestoreModelID reports whether raw is one of the eight allowlisted
// model ids. Validation must use this rather than trusting client strings.
func IsImageRestoreModelID(raw string) bool {
	_, ok := imageRestoreModelKinds[ImageRestoreModelID(raw)]
	return ok
}

// ImageRestoreModelKind returns "preclean", "general" or "face" for a known
// model id and "" for anything else.
func ImageRestoreModelKind(id ImageRestoreModelID) string {
	return imageRestoreModelKinds[id]
}

// ImageRestoreModelDisplayName returns the human-readable model name.
func ImageRestoreModelDisplayName(id ImageRestoreModelID) string {
	return imageRestoreModelDisplayNames[id]
}

// ImageRestoreModelScales returns the upscale factors the model is documented
// for. Pre-clean models never change dimensions (1x); general and face models
// run their native x4 networks and downscale for an effective x2.
func ImageRestoreModelScales(id ImageRestoreModelID) []int {
	if ImageRestoreModelKind(id) == ImageRestoreKindPreclean {
		return []int{1}
	}
	return []int{2, 4}
}

// ---------------------------------------------------------------------------
// Request / response contracts
// ---------------------------------------------------------------------------

// ImageRestoreOptions is the JSON `options` field of the POST
// /api/image-restore/start multipart form (the `image` field carries the file).
// At least one model across preclean+models+faceModels is required.
type ImageRestoreOptions struct {
	Crop               *NormalizedRect `json:"crop,omitempty"`               // nil/omitted = whole image
	Preclean           []string        `json:"preclean,omitempty"`           // preclean ids; normalized to fixed run order
	Models             []string        `json:"models"`                       // general model ids
	FaceModels         []string        `json:"faceModels,omitempty"`         // face model ids
	Chain              bool            `json:"chain,omitempty"`              // re-run face models on each general result
	Scale              int             `json:"scale,omitempty"`              // 0 auto, 2, 4
	CodeFormerFidelity float64         `json:"codeformerFidelity,omitempty"` // 0..1, default 0.7
	FBCNNQualityFactor int             `json:"fbcnnQualityFactor,omitempty"` // 0 = blind/auto, else 1..100
	SessionID          string          `json:"sessionId,omitempty"`
}

// ImageRestoreStartResponse acknowledges an accepted job (HTTP 202).
type ImageRestoreStartResponse struct {
	JobID string `json:"jobId"`
}

// ImageRestoreModelInfo describes one model in the capabilities response.
type ImageRestoreModelInfo struct {
	ID          ImageRestoreModelID `json:"id"`
	Kind        string              `json:"kind"`
	DisplayName string              `json:"displayName"`
	Scales      []int               `json:"scales"`
	Available   bool                `json:"available"`
	// Reason is a short user-safe explanation when Available is false.
	Reason                 string  `json:"reason,omitempty"`
	EstSecondsPerMegapixel float64 `json:"estSecondsPerMegapixel"`
}

// ImageRestoreCapabilitiesResponse is the body of
// GET /api/image-restore/capabilities.
type ImageRestoreCapabilitiesResponse struct {
	Enabled            bool                    `json:"enabled"`
	MaxSourceWidth     int                     `json:"maxSourceWidth"`
	MaxSourceHeight    int                     `json:"maxSourceHeight"`
	MaxUploadSizeBytes int64                   `json:"maxUploadSizeBytes"`
	MaxOutputPixels    int64                   `json:"maxOutputPixels"`
	MaxOutputs         int                     `json:"maxOutputs"`
	ChainSupported     bool                    `json:"chainSupported"`
	Models             []ImageRestoreModelInfo `json:"models"`
}

// ImageRestoreResultEntry describes one image in the inline comparison grid.
type ImageRestoreResultEntry struct {
	ID             string `json:"id"`                  // e.g. "preclean_fbcnn", "realesrgan", "gfpgan_on_original", "codeformer_on_hat"
	Label          string `json:"label"`               // display label, e.g. "After FBCNN artifact removal"
	Kind           string `json:"kind"`                // preclean | general | face
	BaseModel      string `json:"baseModel,omitempty"` // for chained outputs
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	FileName       string `json:"fileName"` // relative name inside the archive
	SizeBytes      int64  `json:"sizeBytes"`
	Status         string `json:"status"` // completed | failed
	Error          string `json:"error,omitempty"`
	GenerativeNote string `json:"generativeNote,omitempty"` // set on face outputs, see §2.3
	FidelityNote   string `json:"fidelityNote,omitempty"`   // set on preclean outputs, see §2.5
}

// ImageRestoreResultsResponse is the body of
// GET /api/image-restore/:jobId/results.
type ImageRestoreResultsResponse struct {
	JobID    string                    `json:"jobId"`
	Original ImageRestoreResultEntry   `json:"original"` // the (cropped) prepared source
	Results  []ImageRestoreResultEntry `json:"results"`
}
