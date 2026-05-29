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
	FPS             float64       `json:"fps"`             // timeline framerate (e.g. 30)
	Width           int           `json:"width"`           // timeline resolution
	Height          int           `json:"height"`          //
	DurationSeconds float64       `json:"durationSeconds"` // computed = end of last clip
	Tracks          []StudioTrack `json:"tracks"`
	CreatedAt       time.Time     `json:"createdAt"`
	UpdatedAt       time.Time     `json:"updatedAt"`
}

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
	Volume        *float64 `json:"volume,omitempty"`  // 0..1 for audio (default 1)
	Opacity       *float64 `json:"opacity,omitempty"` // 0..1 for video (default 1)

	// Phase 5 effects.
	// TransitionInSeconds is a cross-dissolve from the previous clip on the same
	// track into this one; the clip overlaps its predecessor by this duration.
	TransitionInSeconds *float64            `json:"transitionInSeconds,omitempty"`
	Adjustments         *StudioAdjustments  `json:"adjustments,omitempty"`
	TextOverlays        []StudioTextOverlay `json:"textOverlays,omitempty"`
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
	Name   string        `json:"name"`
	FPS    float64       `json:"fps"`
	Width  int           `json:"width"`
	Height int           `json:"height"`
	Tracks []StudioTrack `json:"tracks"`
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

// StudioExportRequest is the body for POST /api/studio/projects/:id/export.
type StudioExportRequest struct {
	FileName string `json:"fileName,omitempty"`
	Preset   string `json:"preset,omitempty"` // quality preset key
}
