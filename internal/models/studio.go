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

	// EDL v3: timeline markers (≤500, sorted by t). Stored in the same sidecar.
	Markers []StudioMarker `json:"markers,omitempty"`

	// Server-incremented optimistic-concurrency counter (part 07). Bumped on
	// every tracks save; SaveProject compare-and-sets against it. Not part of
	// the editable document — the client keeps it outside its undo history.
	Revision int64 `json:"revision"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// StudioSchemaVersionCurrent is the EDL version the server emits. Older
// documents load unchanged (the new fields are additive); the client
// normalizes on load.
const StudioSchemaVersionCurrent = 3

// StudioTrack is one timeline lane.
type StudioTrack struct {
	ID    string          `json:"id"`
	Kind  StudioTrackKind `json:"kind"`
	Index int             `json:"index"` // V1=0,V2=1… / A1=0,A2=1…
	Muted bool            `json:"muted"`
	Clips []StudioClip    `json:"clips"`

	// EDL v3 (all optional; absent = default behavior). Gain/Pan are pointers
	// so "absent" (defaults 1 / 0) is distinguishable from a real 0.
	Name    string              `json:"name,omitempty"`   // ≤80 chars
	Locked  bool                `json:"locked,omitempty"` // part 18: reject edits
	Hidden  bool                `json:"hidden,omitempty"` // video: hide from composite
	Solo    bool                `json:"solo,omitempty"`   // audio: solo in the mix
	Gain    *float64            `json:"gain,omitempty"`   // 0..2 (default 1)
	Pan     *float64            `json:"pan,omitempty"`    // -1 (L) .. 1 (R) (default 0)
	Effects *StudioTrackEffects `json:"effects,omitempty"`
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

	// EDL v3 (all optional; absent = no effect). Ranges/names mirror
	// lib/studioTypes.ts (Zod) and STUDIO_V3_RANGES in effectRegistry.ts.
	// Transition supersedes TransitionInSeconds (still written for now; the
	// sanitizer upgrades legacy values and keeps the two mirrored).
	Transition *StudioTransition    `json:"transition,omitempty"`
	Keyframes  *StudioClipKeyframes `json:"keyframes,omitempty"`
	// Title makes this an assetless TITLE CLIP; the sanitizer enforces
	// AssetID XOR Title ("" = no asset).
	Title *StudioTitle `json:"title,omitempty"`
	// Speed/Reverse are mutually exclusive with Freeze (freeze wins).
	Speed         *float64      `json:"speed,omitempty"`         // 0.1..10 (default 1)
	Reverse       bool          `json:"reverse,omitempty"`       //
	MaintainPitch *bool         `json:"maintainPitch,omitempty"` // default true when absent
	Freeze        *StudioFreeze `json:"freeze,omitempty"`
	GroupID       string        `json:"groupId,omitempty"` // clip grouping (part 18)
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

// ---------------------------------------------------------------------------
// EDL v3 — typed transitions, per-property keyframes, title clips, speed,
// track mixer/effects, project markers (ADR ws/0004). Additive and inert until
// parts 14–21; ranges mirror STUDIO_V3_RANGES in lib/studio/effectRegistry.ts.
// ---------------------------------------------------------------------------

// Transition type discriminators (mirror studioTransitionTypeSchema).
const (
	StudioTransitionCrossDissolve = "crossDissolve"
	StudioTransitionDipToBlack    = "dipToBlack"
	StudioTransitionDipToWhite    = "dipToWhite"
	StudioTransitionWipeLeft      = "wipeLeft"
	StudioTransitionWipeRight     = "wipeRight"
	StudioTransitionWipeUp        = "wipeUp"
	StudioTransitionWipeDown      = "wipeDown"
	StudioTransitionPushLeft      = "pushLeft"
	StudioTransitionPushRight     = "pushRight"
	StudioTransitionPushUp        = "pushUp"
	StudioTransitionPushDown      = "pushDown"
	StudioTransitionSlideLeft     = "slideLeft"
	StudioTransitionSlideRight    = "slideRight"
	StudioTransitionSlideUp       = "slideUp"
	StudioTransitionSlideDown     = "slideDown"
)

// StudioTransition describes how a clip enters over its predecessor's overlap
// region. It supersedes TransitionInSeconds (which stays mirrored for now).
type StudioTransition struct {
	Type            string             `json:"type"`
	DurationSeconds float64            `json:"durationSeconds"`  // 0.1..5
	Params          map[string]float64 `json:"params,omitempty"` // per-type tuning (part 14)
}

// StudioKeyframe is one automation point. T is clip-local seconds; Ease is
// linear | hold | easeIn | easeOut | easeBoth.
type StudioKeyframe struct {
	T     float64 `json:"t"`
	Value float64 `json:"value"`
	Ease  string  `json:"ease"`
}

// StudioClipKeyframes holds the per-property automation lanes (each sorted by
// T, ≤64 points). Effects keys are "<effectId>.<param>" (e.g.
// "lumetri1.exposure"), clamped against the effect registry's ranges. Volume
// supersedes the legacy VolumeKeyframes (client normalize upgrades; volume
// wins when both exist).
type StudioClipKeyframes struct {
	PositionX []StudioKeyframe            `json:"positionX,omitempty"`
	PositionY []StudioKeyframe            `json:"positionY,omitempty"`
	Scale     []StudioKeyframe            `json:"scale,omitempty"`
	Rotation  []StudioKeyframe            `json:"rotation,omitempty"`
	Opacity   []StudioKeyframe            `json:"opacity,omitempty"`
	Volume    []StudioKeyframe            `json:"volume,omitempty"`
	Pan       []StudioKeyframe            `json:"pan,omitempty"`
	Effects   map[string][]StudioKeyframe `json:"effects,omitempty"`
}

// Title layer type discriminators (mirror studioTitleLayerSchema).
const (
	StudioTitleLayerText = "text"
	StudioTitleLayerRect = "rect"
	StudioTitleLayerLine = "line"
)

// StudioTitle makes a clip an assetless TITLE CLIP rendered from its layer
// stack (≤8 layers). The sanitizer enforces AssetID XOR Title.
type StudioTitle struct {
	Layers []StudioTitleLayer `json:"layers"`
}

// StudioTitleLayer is one layer of a title clip. Go has no native discriminated
// union, so — like StudioEffect — every per-type field is a pointer with
// omitempty. The sanitizer fills every field the active type requires, so a
// sanitized layer always serializes the fields the Zod union marks required
// (a pointer-to-0 survives where an omitted field would not).
type StudioTitleLayer struct {
	Type string `json:"type"` // text | rect | line

	// text. X/Y are the normalized 0..1 center of the block (like textOverlays).
	Text            *string                `json:"text,omitempty"`          // ≤500 chars
	FontFamily      *string                `json:"fontFamily,omitempty"`    //
	FontSizePx      *float64               `json:"fontSizePx,omitempty"`    // 8..500
	FontWeight      *int                   `json:"fontWeight,omitempty"`    // 400 | 700
	FillColor       *string                `json:"fillColor,omitempty"`     // #RRGGBB
	StrokeColor     *string                `json:"strokeColor,omitempty"`   // #RRGGBB
	StrokeWidthPx   *float64               `json:"strokeWidthPx,omitempty"` // 0..50
	Shadow          *StudioTitleShadow     `json:"shadow,omitempty"`
	Background      *StudioTitleBackground `json:"background,omitempty"`
	LetterSpacingPx *float64               `json:"letterSpacingPx,omitempty"` // -20..100
	LineHeight      *float64               `json:"lineHeight,omitempty"`      // 0.5..3
	Align           *string                `json:"align,omitempty"`           // left|center|right

	// text + rect origin / rect size (normalized 0..1).
	X *float64 `json:"x,omitempty"`
	Y *float64 `json:"y,omitempty"`
	W *float64 `json:"w,omitempty"`
	H *float64 `json:"h,omitempty"`

	// rect + line color; rect opacity/radius.
	Color    *string  `json:"color,omitempty"`    // #RRGGBB
	Opacity  *float64 `json:"opacity,omitempty"`  // 0..1
	RadiusPx *float64 `json:"radiusPx,omitempty"` // 0..200

	// line endpoints (normalized 0..1) + thickness.
	X1          *float64 `json:"x1,omitempty"`
	Y1          *float64 `json:"y1,omitempty"`
	X2          *float64 `json:"x2,omitempty"`
	Y2          *float64 `json:"y2,omitempty"`
	ThicknessPx *float64 `json:"thicknessPx,omitempty"` // 1..100
}

// StudioTitleShadow is a text layer's drop shadow (project-resolution px).
type StudioTitleShadow struct {
	XPx    float64 `json:"xPx"`    // -100..100
	YPx    float64 `json:"yPx"`    // -100..100
	BlurPx float64 `json:"blurPx"` // 0..100
	Color  string  `json:"color"`  // #RRGGBB
}

// StudioTitleBackground is a text layer's rounded background plate.
type StudioTitleBackground struct {
	Color      string  `json:"color"`      // #RRGGBB
	Opacity    float64 `json:"opacity"`    // 0..1
	PaddingXPx float64 `json:"paddingXPx"` // 0..200
	PaddingYPx float64 `json:"paddingYPx"` // 0..200
	RadiusPx   float64 `json:"radiusPx"`   // 0..200
}

// StudioFreeze holds a clip on a single source frame. Mutually exclusive with
// Speed/Reverse (the sanitizer enforces; freeze wins).
type StudioFreeze struct {
	SourceTime float64 `json:"sourceTime"` // seconds within the source media
}

// StudioTrackEffects are the track insert effects (3-band EQ + compressor).
type StudioTrackEffects struct {
	EQ         *StudioTrackEQ         `json:"eq,omitempty"`
	Compressor *StudioTrackCompressor `json:"compressor,omitempty"`
}

// StudioTrackEQ is a 3-band shelf EQ; gains are -12..+12 dB.
type StudioTrackEQ struct {
	LowGainDb  float64 `json:"lowGainDb"`
	MidGainDb  float64 `json:"midGainDb"`
	HighGainDb float64 `json:"highGainDb"`
}

// StudioTrackCompressor is a track dynamics compressor.
type StudioTrackCompressor struct {
	ThresholdDb float64 `json:"thresholdDb"` // -60..0
	Ratio       float64 `json:"ratio"`       // 1..20
	AttackMs    float64 `json:"attackMs"`    // 1..200
	ReleaseMs   float64 `json:"releaseMs"`   // 20..1000
	MakeupDb    float64 `json:"makeupDb"`    // 0..12
}

// StudioMarker is a named point of interest on the project timeline.
type StudioMarker struct {
	ID      string  `json:"id"`
	T       float64 `json:"t"`    // seconds
	Name    string  `json:"name"` // ≤80 chars
	Color   string  `json:"color"`
	Comment string  `json:"comment,omitempty"` // ≤500 chars
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

	// EDL v3 project-level fields.
	Markers []StudioMarker `json:"markers,omitempty"`

	// Part 07 (save safety): the revision this save was based on. When set the
	// repository compare-and-sets and a stale value yields a 409; when omitted
	// the save is legacy last-write-wins (embed back-compat — the MM client
	// always sends it).
	ExpectedRevision *int64 `json:"expectedRevision,omitempty"`
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
	Preset   string `json:"preset,omitempty"`   // quality preset key
	Loudness string `json:"loudness,omitempty"` // '' | streaming | podcast | broadcast
}

// Version history (part 09, ADR ws/0001 item 3).

// StudioVersionKind values for studio_project_versions.kind (mirrors the
// table's CHECK constraint).
const (
	StudioVersionKindAuto   = "auto"
	StudioVersionKindManual = "manual"
)

// StudioProjectVersion is the metadata for one studio_project_versions row.
// The snapshot bodies (tracks + captions sidecar) never travel to the client:
// the list endpoint returns only this shape, and restore happens server-side
// and answers with the full updated project.
type StudioProjectVersion struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	Kind      string    `json:"kind"`           // auto | manual
	Name      string    `json:"name,omitempty"` // "" for unnamed autos
	Revision  int64     `json:"revision"`       // project revision at capture
	CreatedAt time.Time `json:"createdAt"`
}

// StudioCreateVersionRequest is the body for
// POST /api/studio/projects/:id/versions. name is required for kind 'manual'.
type StudioCreateVersionRequest struct {
	Kind string `json:"kind"` // auto | manual
	Name string `json:"name,omitempty"`
}
