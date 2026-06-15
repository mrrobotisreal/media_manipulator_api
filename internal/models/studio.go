package models

import "time"

// Content Studio EDL (Edit Decision List) — the shared contract between the Go
// API and the React editor. Field names are camelCase in JSON and MUST stay
// identical to the Zod schema in the frontend (src/lib/studioTypes.ts). The
// browser previews from this list (Option A: pooled <video> elements + Web
// Audio); the server only re-reads it at export time to build the ffmpeg
// filter_complex graph.

// StudioMediaKind is the broad media class of an ingested source asset.
type StudioMediaKind string

const (
	StudioMediaKindVideo StudioMediaKind = "video"
	StudioMediaKindAudio StudioMediaKind = "audio"
	// StudioMediaKindLUT is a .cube 3D LUT asset (no proxy/sprite/peaks; served
	// raw via /file and applied by the lut effect).
	StudioMediaKindLUT StudioMediaKind = "lut"
)

// StudioTrackKind is the lane type a clip lives on. Video tracks composite
// top-down (higher index overlays lower); audio tracks sum through the mixer.
type StudioTrackKind string

const (
	StudioTrackKindVideo StudioTrackKind = "video"
	StudioTrackKindAudio StudioTrackKind = "audio"
)

// StudioProject is the persisted editor document. `Tracks` is stored as a
// single JSONB column on studio_projects for now (see the init_content_studio
// migration); we can normalize into per-clip rows later without changing this
// contract.
type StudioProject struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	SchemaVersion   int           `json:"schemaVersion"`   // EDL version (current = 2)
	FPS             float64       `json:"fps"`             // timeline framerate (e.g. 30)
	Width           int           `json:"width"`           // timeline resolution
	Height          int           `json:"height"`          //
	DurationSeconds float64       `json:"durationSeconds"` // computed = end of last clip
	Tracks          []StudioTrack `json:"tracks"`

	// EDL v2 project-level fields, persisted in the studio_projects.captions
	// JSONB sidecar (kept out of the tracks column so the autosave PUT and the
	// caption-generate job don't clobber each other).
	Captions        []StudioCaptionCue  `json:"captions"`
	CaptionStyle    *StudioCaptionStyle `json:"captionStyle,omitempty"`
	CaptionsEnabled bool                `json:"captionsEnabled"`
	Audio           *StudioAudioConfig  `json:"audio,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// StudioSchemaVersionCurrent is the EDL version the server emits. v1 documents
// load unchanged (the new fields are additive); the client normalizes on load.
const StudioSchemaVersionCurrent = 2

// StudioTrack is one timeline lane.
type StudioTrack struct {
	ID    string          `json:"id"`
	Kind  StudioTrackKind `json:"kind"`
	Index int             `json:"index"` // V1=0,V2=1… / A1=0,A2=1…
	Muted bool            `json:"muted"`
	Clips []StudioClip    `json:"clips"`
}

// StudioClip is a placement of a source asset's stream on the timeline. The
// effective duration is SourceOut - SourceIn.
type StudioClip struct {
	ID            string   `json:"id"`
	AssetID       string   `json:"assetId"`           // FK -> studio_assets
	StreamIndex   int      `json:"streamIndex"`       // which source stream (usually 0 video / 0 audio)
	TimelineStart float64  `json:"timelineStart"`     // seconds on the timeline
	SourceIn      float64  `json:"sourceIn"`          // in-point within the source media (seconds)
	SourceOut     float64  `json:"sourceOut"`         // out-point within the source media (seconds)
	Volume        *float64 `json:"volume,omitempty"`  // 0..2 for audio (default 1; >1 = boost)
	Opacity       *float64 `json:"opacity,omitempty"` // 0..1 for video (default 1)

	// Phase 5 effects.
	// TransitionInSeconds is a cross-dissolve from the previous clip on the same
	// track into this one; the clip overlaps its predecessor by this duration.
	TransitionInSeconds *float64            `json:"transitionInSeconds,omitempty"`
	Adjustments         *StudioAdjustments  `json:"adjustments,omitempty"`
	TextOverlays        []StudioTextOverlay `json:"textOverlays,omitempty"`

	// EDL v2 effects (all optional; absent = no effect). Ranges/names mirror
	// lib/studioTypes.ts (Zod) and lib/studio/effectRegistry.ts.
	Transform       *StudioTransform       `json:"transform,omitempty"`
	Crop            *StudioCrop            `json:"crop,omitempty"`
	BlendMode       string                 `json:"blendMode,omitempty"`
	Effects         []StudioEffect         `json:"effects,omitempty"`
	VolumeKeyframes []StudioVolumeKeyframe `json:"volumeKeyframes,omitempty"`
	Pan             *float64               `json:"pan,omitempty"` // -1 (L) .. 1 (R)
}

// StudioAdjustments are per-clip color tweaks mapped onto ffmpeg's eq filter
// (and CSS filters in preview). brightness -1..1 (0 = none), contrast 0..2
// (1 = none), saturation 0..2 (1 = none).
type StudioAdjustments struct {
	Brightness float64 `json:"brightness"`
	Contrast   float64 `json:"contrast"`
	Saturation float64 `json:"saturation"`
}

// StudioTextOverlay is a text/location label drawn over a clip (e.g. on drone
// footage). X/Y are normalized 0..1 positions within the frame; FontSize is in
// project-resolution pixels; Color is a #RRGGBB hex.
type StudioTextOverlay struct {
	ID       string  `json:"id"`
	Text     string  `json:"text"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	FontSize float64 `json:"fontSize"`
	Color    string  `json:"color"`
}

// StudioTransform is a clip's 2D motion. X/Y are normalized center offsets
// (fractions of project width/height); Scale is uniform; RotationDeg is degrees
// clockwise. Identity = {0,0,1,0}.
type StudioTransform struct {
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	Scale       float64 `json:"scale"`
	RotationDeg float64 `json:"rotationDeg"`
}

// StudioCrop holds normalized fractions cropped from each edge of the SOURCE
// frame. Left+Right must be < 1 and Top+Bottom < 1.
type StudioCrop struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
}

// Effect type + blend mode discriminators (mirror the Zod enums).
const (
	StudioEffectLumetri   = "lumetri"
	StudioEffectLUT       = "lut"
	StudioEffectChromaKey = "chromakey"

	StudioBlendNormal     = "normal"
	StudioBlendMultiply   = "multiply"
	StudioBlendScreen     = "screen"
	StudioBlendOverlay    = "overlay"
	StudioBlendLighten    = "lighten"
	StudioBlendDarken     = "darken"
	StudioBlendAddition   = "addition"
	StudioBlendDifference = "difference"
)

// StudioEffect is one entry in a clip's ordered effect stack. Go has no native
// discriminated union, so every per-type field is a pointer with omitempty: a
// lumetri effect round-trips through JSONB without lut/chromakey keys (and
// vice-versa), while a genuine 0 value (e.g. exposure=0) survives because it is
// a pointer-to-0, not an omitted field.
type StudioEffect struct {
	ID      string `json:"id"`
	Type    string `json:"type"` // lumetri | lut | chromakey
	Enabled bool   `json:"enabled"`

	// lumetri
	Exposure    *float64 `json:"exposure,omitempty"`    // -3..3 (stops)
	Contrast    *float64 `json:"contrast,omitempty"`    // 0..2
	Saturation  *float64 `json:"saturation,omitempty"`  // 0..2
	Temperature *float64 `json:"temperature,omitempty"` // -100..100
	Tint        *float64 `json:"tint,omitempty"`        // -100..100
	Vibrance    *float64 `json:"vibrance,omitempty"`    // -2..2

	// lut
	LutAssetID *string  `json:"lutAssetId,omitempty"`
	Intensity  *float64 `json:"intensity,omitempty"` // 0..1

	// chromakey
	KeyColor   *string  `json:"keyColor,omitempty"`   // #RRGGBB
	Similarity *float64 `json:"similarity,omitempty"` // 0.01..1
	Blend      *float64 `json:"blend,omitempty"`      // 0..1
	Despill    *float64 `json:"despill,omitempty"`    // 0..1
}

// StudioVolumeKeyframe is a volume-automation point. T is seconds from the
// clip's timeline start; Gain is 0..2. When a clip has any keyframes they
// override the flat Volume.
type StudioVolumeKeyframe struct {
	T    float64 `json:"t"`
	Gain float64 `json:"gain"`
}

// StudioCaptionCue is one caption line on the project's caption lane.
type StudioCaptionCue struct {
	ID           string  `json:"id"`
	StartSeconds float64 `json:"startSeconds"`
	EndSeconds   float64 `json:"endSeconds"`
	Text         string  `json:"text"`
}

// StudioCaptionStyle is the project-wide caption appearance. FontSizePct is a
// percentage of project height; Position is "bottom" | "top".
type StudioCaptionStyle struct {
	FontSizePct       float64 `json:"fontSizePct"`
	Color             string  `json:"color"`
	BackgroundColor   string  `json:"backgroundColor"`
	BackgroundOpacity float64 `json:"backgroundOpacity"`
	Position          string  `json:"position"`
	MaxWidthPct       float64 `json:"maxWidthPct"`
}

// StudioAudioConfig is the project-level auto-ducking configuration. The export
// applies it via sidechaincompress; the preview approximates it with presence-
// driven gain ramps.
type StudioAudioConfig struct {
	DuckingEnabled   bool    `json:"duckingEnabled"`
	DuckVoiceTrackID string  `json:"duckVoiceTrackId,omitempty"`
	DuckAmountDb     float64 `json:"duckAmountDb"`
	DuckAttackMs     float64 `json:"duckAttackMs"`
	DuckReleaseMs    float64 `json:"duckReleaseMs"`
}

// StudioAsset is an ingested source file plus its derived proxy + filmstrip and
// the ffprobe-derived metadata. `ProbeJSON` keeps the full payload for
// debugging and is stored in studio_assets.probe_json.
type StudioAsset struct {
	ID                 string          `json:"id"`
	ProjectID          string          `json:"projectId"`
	OriginalFileName   string          `json:"originalFileName"`
	S3KeyOriginal      string          `json:"s3KeyOriginal"`
	S3KeyProxy         string          `json:"s3KeyProxy,omitempty"`
	ThumbnailSpriteURL string          `json:"thumbnailSpriteUrl,omitempty"` // filmstrip
	S3KeyPeaks         string          `json:"s3KeyPeaks,omitempty"`         // audio waveform peaks JSON
	MediaKind          StudioMediaKind `json:"mediaKind"`
	DurationSeconds    float64         `json:"durationSeconds"`
	Width              *int            `json:"width,omitempty"`
	Height             *int            `json:"height,omitempty"`
	FPS                *float64        `json:"fps,omitempty"`
	VideoCodec         string          `json:"videoCodec,omitempty"`
	AudioCodec         string          `json:"audioCodec,omitempty"`
	HasAudio           bool            `json:"hasAudio"`
	SampleRate         *int            `json:"sampleRate,omitempty"`
	Channels           *int            `json:"channels,omitempty"`
	ProbeJSON          any             `json:"probeJson"` // full ffprobe payload for debugging
	CreatedAt          time.Time       `json:"createdAt"`
}

// ---------------------------------------------------------------------------
// Request / response payloads for the /api/studio/* endpoints.
// ---------------------------------------------------------------------------

// StudioCreateProjectRequest is the body for POST /api/studio/projects.
type StudioCreateProjectRequest struct {
	Name   string  `json:"name"`
	FPS    float64 `json:"fps"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
}

// StudioSaveProjectRequest is the body for PUT /api/studio/projects/:id. The
// editor saves the whole track/clip tree at once.
type StudioSaveProjectRequest struct {
	Name          string        `json:"name"`
	SchemaVersion int           `json:"schemaVersion,omitempty"`
	FPS           float64       `json:"fps"`
	Width         int           `json:"width"`
	Height        int           `json:"height"`
	Tracks        []StudioTrack `json:"tracks"`

	// EDL v2 project-level fields, written transactionally with tracks.
	// CaptionsEnabled is a pointer so an omitted field keeps the stored value
	// (defaults to true on a brand-new project).
	Captions        []StudioCaptionCue  `json:"captions,omitempty"`
	CaptionStyle    *StudioCaptionStyle `json:"captionStyle,omitempty"`
	CaptionsEnabled *bool               `json:"captionsEnabled,omitempty"`
	Audio           *StudioAudioConfig  `json:"audio,omitempty"`
}

// StudioAssetPresignRequest mirrors VideoUploadPresignRequest but for Content
// Studio source media (video OR audio), scoped to a project.
type StudioAssetPresignRequest struct {
	ProjectID     string `json:"projectId"`
	FileName      string `json:"fileName"`
	ContentType   string `json:"contentType"`
	FileSizeBytes int64  `json:"fileSizeBytes"`
	SessionID     string `json:"sessionId,omitempty"`
}

// StudioAssetPresignResponse is the presigned PUT target plus the S3 key the
// client echoes back on complete.
type StudioAssetPresignResponse struct {
	UploadURL string `json:"uploadUrl"`
	S3Key     string `json:"s3Key"`
	Bucket    string `json:"bucket"`
	ExpiresAt string `json:"expiresAt"`
}

// StudioAssetCompleteRequest finalizes an upload: the server HEAD-verifies the
// object, probes it, and kicks off the proxy + filmstrip ingest job.
type StudioAssetCompleteRequest struct {
	ProjectID     string `json:"projectId"`
	S3Key         string `json:"s3Key"`
	FileName      string `json:"fileName"`
	ContentType   string `json:"contentType,omitempty"`
	FileSizeBytes int64  `json:"fileSizeBytes,omitempty"`
}

// StudioAssetCompleteResponse returns the new asset row plus the ingest jobId
// so the client can poll /api/job/:jobId for proxy readiness.
type StudioAssetCompleteResponse struct {
	Asset *StudioAsset `json:"asset"`
	JobID string       `json:"jobId"`
}

// StudioCaptionsGenerateRequest is the body for
// POST /api/studio/projects/:id/captions/generate.
type StudioCaptionsGenerateRequest struct {
	Language string `json:"language,omitempty"`
}

// StudioAssetDeriveRequest is the body for POST /api/studio/assets/:id/derive:
// it creates a new asset from an AI transform of an existing audio-bearing one.
type StudioAssetDeriveRequest struct {
	Operation string `json:"operation"` // voice_clean | split_vocals | split_instrumental
}

const (
	StudioDeriveVoiceClean        = "voice_clean"
	StudioDeriveSplitVocals       = "split_vocals"
	StudioDeriveSplitInstrumental = "split_instrumental"
)

// StudioExportRequest is the body for POST /api/studio/projects/:id/export.
type StudioExportRequest struct {
	FileName string `json:"fileName,omitempty"`
	Preset   string `json:"preset,omitempty"`  // quality preset key
	Loudness string `json:"loudness,omitempty"` // '' | streaming | podcast | broadcast
}
