package models

// AI Video Restoration — request/response contracts for
// GET /api/video-restore/capabilities and POST /api/video-restore/start.
// Restoration jobs reuse ConversionJob (mode "restore") and the shared
// /api/job/:jobId (+ SSE) machinery, so there is no bespoke job snapshot here.

// RestoreModelID identifies one of the six supported restoration /
// super-resolution models. IDs double as directory names inside the results
// tarball and as keys for the config tuning maps — they must stay lowercase
// and filesystem-safe.
type RestoreModelID string

const (
	RestoreModelRealESRGAN RestoreModelID = "realesrgan"
	RestoreModelSwinIR     RestoreModelID = "swinir"
	RestoreModelHAT        RestoreModelID = "hat"
	RestoreModelBasicVSRPP RestoreModelID = "basicvsrpp"
	RestoreModelRVRT       RestoreModelID = "rvrt"
	RestoreModelVRT        RestoreModelID = "vrt"
)

// Restore model groups. Group A ("frame") enhancers work on extracted PNGs —
// their results include both the enhanced frames and the stitched MP4. Group B
// ("video") models operate on the frame sequence natively and produce only an
// enhanced MP4.
const (
	RestoreGroupFrame = "frame"
	RestoreGroupVideo = "video"
)

// AllRestoreModelIDs is the canonical capabilities/display order.
var AllRestoreModelIDs = []RestoreModelID{
	RestoreModelRealESRGAN,
	RestoreModelSwinIR,
	RestoreModelHAT,
	RestoreModelBasicVSRPP,
	RestoreModelRVRT,
	RestoreModelVRT,
}

// RestoreModelRunOrder is the pipeline execution order, cheapest model first
// so partial results exist early if an expensive model dies late in the run.
var RestoreModelRunOrder = []RestoreModelID{
	RestoreModelRealESRGAN,
	RestoreModelBasicVSRPP,
	RestoreModelSwinIR,
	RestoreModelRVRT,
	RestoreModelHAT,
	RestoreModelVRT,
}

var restoreModelGroups = map[RestoreModelID]string{
	RestoreModelRealESRGAN: RestoreGroupFrame,
	RestoreModelSwinIR:     RestoreGroupFrame,
	RestoreModelHAT:        RestoreGroupFrame,
	RestoreModelBasicVSRPP: RestoreGroupVideo,
	RestoreModelRVRT:       RestoreGroupVideo,
	RestoreModelVRT:        RestoreGroupVideo,
}

var restoreModelDisplayNames = map[RestoreModelID]string{
	RestoreModelRealESRGAN: "Real-ESRGAN",
	RestoreModelSwinIR:     "SwinIR (Real-SR Large)",
	RestoreModelHAT:        "HAT (Real_HAT_GAN)",
	RestoreModelBasicVSRPP: "BasicVSR++",
	RestoreModelRVRT:       "RVRT",
	RestoreModelVRT:        "VRT",
}

// IsRestoreModelID reports whether raw is one of the six allowlisted model
// ids. Validation must use this rather than trusting client strings.
func IsRestoreModelID(raw string) bool {
	_, ok := restoreModelGroups[RestoreModelID(raw)]
	return ok
}

// RestoreModelGroup returns "frame" or "video" for a known model id and ""
// for anything else.
func RestoreModelGroup(id RestoreModelID) string {
	return restoreModelGroups[id]
}

// RestoreModelDisplayName returns the human-readable model name.
func RestoreModelDisplayName(id RestoreModelID) string {
	return restoreModelDisplayNames[id]
}

// RestoreModelScales returns the upscale factors selectable for a model.
// Real-ESRGAN supports 2 (x4 model + downscale on stitch) and 4; every other
// model is x4 only.
func RestoreModelScales(id RestoreModelID) []int {
	if id == RestoreModelRealESRGAN {
		return []int{2, 4}
	}
	return []int{4}
}

// RestoreStartRequest is the body of POST /api/video-restore/start. The video
// itself is uploaded beforehand through the existing
// POST /api/video-upload/presign → S3 PUT flow; s3Key references that object.
type RestoreStartRequest struct {
	S3Key            string   `json:"s3Key"`
	FileName         string   `json:"fileName,omitempty"`
	FileSizeBytes    int64    `json:"fileSizeBytes,omitempty"`
	ClipStartSeconds float64  `json:"clipStartSeconds"`
	ClipEndSeconds   float64  `json:"clipEndSeconds"`
	Models           []string `json:"models"`
	// Scale: 0 = auto (source height ≤ 540 → 4, else 2), or explicit 2/4.
	Scale         int    `json:"scale,omitempty"`
	IncludeFrames bool   `json:"includeFrames,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
}

// RestoreStartResponse acknowledges an accepted restoration job (HTTP 202).
type RestoreStartResponse struct {
	JobID string `json:"jobId"`
}

// RestoreModelInfo describes one model in the capabilities response.
type RestoreModelInfo struct {
	ID          RestoreModelID `json:"id"`
	Group       string         `json:"group"`
	DisplayName string         `json:"displayName"`
	Scales      []int          `json:"scales"`
	Available   bool           `json:"available"`
	// Reason is a short user-safe explanation when Available is false.
	Reason             string  `json:"reason,omitempty"`
	EstSecondsPerFrame float64 `json:"estSecondsPerFrame"`
}

// RestoreCapabilitiesResponse is the body of GET /api/video-restore/capabilities.
type RestoreCapabilitiesResponse struct {
	Enabled                bool               `json:"enabled"`
	MaxClipSeconds         float64            `json:"maxClipSeconds"`
	RecommendedClipSeconds float64            `json:"recommendedClipSeconds"`
	MaxFrames              int                `json:"maxFrames"`
	MaxSourceWidth         int                `json:"maxSourceWidth"`
	MaxSourceHeight        int                `json:"maxSourceHeight"`
	MaxUploadSizeBytes     int64              `json:"maxUploadSizeBytes"`
	ResultLinkTTLSeconds   int                `json:"resultLinkTtlSeconds"`
	Models                 []RestoreModelInfo `json:"models"`
}
