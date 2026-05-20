package models

// TranscodeProtocol selects the streaming protocol output the user wants.
type TranscodeProtocol string

const (
	TranscodeProtocolHLS  TranscodeProtocol = "hls"
	TranscodeProtocolDASH TranscodeProtocol = "dash"
)

// DashCodec selects the video codec used inside the DASH package.
type DashCodec string

const (
	DashCodecAV1 DashCodec = "av1"
	DashCodecVP9 DashCodec = "vp9"
)

// TranscodeStageStatus tracks per-stage lifecycle inside a transcode job.
type TranscodeStageStatus string

const (
	StageStatusPending    TranscodeStageStatus = "pending"
	StageStatusProcessing TranscodeStageStatus = "processing"
	StageStatusCompleted  TranscodeStageStatus = "completed"
	StageStatusSkipped    TranscodeStageStatus = "skipped"
	StageStatusFailed     TranscodeStageStatus = "failed"
)

// VideoProbeRequest is the body of POST /api/video-transcode/probe.
// The s3Key must already exist under videos/YYYYMMDD/<sessionID>/<uuid>.<ext>.
type VideoProbeRequest struct {
	S3Key         string `json:"s3Key"`
	FileName      string `json:"fileName,omitempty"`
	ContentType   string `json:"contentType,omitempty"`
	FileSizeBytes int64  `json:"fileSizeBytes,omitempty"`
}

// FFProbeStreamInfo is a minimal, UI-friendly view of an ffprobe stream entry.
type FFProbeStreamInfo struct {
	Index      int     `json:"index"`
	CodecType  string  `json:"codecType"`
	CodecName  string  `json:"codecName,omitempty"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	FrameRate  float64 `json:"frameRate,omitempty"`
	BitrateBps int64   `json:"bitrateBps,omitempty"`
	SampleRate string  `json:"sampleRate,omitempty"`
	Channels   int     `json:"channels,omitempty"`
	Duration   float64 `json:"duration,omitempty"`
}

// TranscodeQualityRung describes one row in the rendered quality selector.
// Enabled rungs can be selected; disabled rungs render with a tooltip whose
// text is dictated by DisabledReason / PremiumOnly / SourceTooSmall.
type TranscodeQualityRung struct {
	Label            string `json:"label"`
	Height           int    `json:"height"`
	BitrateKbps      int    `json:"bitrateKbps"`
	AudioBitrateKbps int    `json:"audioBitrateKbps"`
	Selected         bool   `json:"selected"`
	Enabled          bool   `json:"enabled"`
	DisabledReason   string `json:"disabledReason,omitempty"`
	PremiumOnly      bool   `json:"premiumOnly,omitempty"`
	SourceTooSmall   bool   `json:"sourceTooSmall,omitempty"`
}

// VideoProbeResponse is what the UI needs to render the transcode form.
type VideoProbeResponse struct {
	S3Key           string                 `json:"s3Key"`
	Bucket          string                 `json:"bucket"`
	FileName        string                 `json:"fileName"`
	ContentType     string                 `json:"contentType"`
	FileSizeBytes   int64                  `json:"fileSizeBytes"`
	Width           int                    `json:"width"`
	Height          int                    `json:"height"`
	MaxQualityLabel string                 `json:"maxQualityLabel"`
	DurationSeconds float64                `json:"durationSeconds"`
	FPS             float64                `json:"fps"`
	FrameRate       string                 `json:"frameRate,omitempty"`
	HasAudio        bool                   `json:"hasAudio"`
	VideoCodec      string                 `json:"videoCodec,omitempty"`
	AudioCodec      string                 `json:"audioCodec,omitempty"`
	VideoBitrateBps int64                  `json:"videoBitrateBps,omitempty"`
	AudioBitrateBps int64                  `json:"audioBitrateBps,omitempty"`
	FormatName      string                 `json:"formatName,omitempty"`
	ContainerFormat string                 `json:"containerFormat,omitempty"`
	Streams         []FFProbeStreamInfo    `json:"streams,omitempty"`
	SelectableRungs []TranscodeQualityRung `json:"selectableRungs"`
	DisabledRungs   []TranscodeQualityRung `json:"disabledRungs"`
	Warnings        []string               `json:"warnings,omitempty"`
	SourceTooSmall  bool                   `json:"sourceTooSmall,omitempty"`
}

// TranscodeStartRequest kicks off a new transcode job. Validated server-side.
type TranscodeStartRequest struct {
	S3Key               string            `json:"s3Key"`
	FileName            string            `json:"fileName,omitempty"`
	ContentType         string            `json:"contentType,omitempty"`
	FileSizeBytes       int64             `json:"fileSizeBytes,omitempty"`
	Protocol            TranscodeProtocol `json:"protocol"`
	DashCodec           DashCodec         `json:"dashCodec,omitempty"`
	QualityRungs        []string          `json:"qualityRungs"`
	GenerateCaptions    bool              `json:"generateCaptions,omitempty"`
	GenerateStoryboards bool              `json:"generateStoryboards,omitempty"`
	SessionID           string            `json:"sessionId,omitempty"`
	Options             map[string]any    `json:"options,omitempty"`
}

// TranscodeStartResponse is returned immediately after a job is enqueued.
type TranscodeStartResponse struct {
	JobID         string                 `json:"jobId"`
	Probe         *VideoProbeResponse    `json:"probe,omitempty"`
	SelectedRungs []TranscodeQualityRung `json:"selectedRungs,omitempty"`
	Message       string                 `json:"message,omitempty"`
}

// TranscodeVariant describes one rendition in the final package.
type TranscodeVariant struct {
	Label           string  `json:"label"`
	Height          int     `json:"height"`
	Width           int     `json:"width,omitempty"`
	BitrateKbps     int     `json:"bitrateKbps"`
	FrameRate       float64 `json:"frameRate,omitempty"`
	VideoCodec      string  `json:"videoCodec,omitempty"`
	AudioCodec      string  `json:"audioCodec,omitempty"`
	PlaylistPath    string  `json:"playlistPath,omitempty"`
	InitSegmentPath string  `json:"initSegmentPath,omitempty"`
	SegmentCount    int     `json:"segmentCount,omitempty"`
	SegmentSeconds  int     `json:"segmentSeconds,omitempty"`
	OutputBytes     int64   `json:"outputBytes,omitempty"`
}

// TranscodeJobResult is persisted into the job result.json and surfaced to the UI.
type TranscodeJobResult struct {
	DownloadURL         string             `json:"downloadUrl"`
	ResultS3Key         string             `json:"resultS3Key"`
	Bucket              string             `json:"bucket"`
	FileName            string             `json:"fileName"`
	ExpiresAt           string             `json:"expiresAt,omitempty"`
	PackageBytes        int64              `json:"packageBytes"`
	ManifestPath        string             `json:"manifestPath,omitempty"`
	Variants            []TranscodeVariant `json:"variants,omitempty"`
	CaptionsIncluded    bool               `json:"captionsIncluded"`
	StoryboardsIncluded bool               `json:"storyboardsIncluded"`
}

// TranscodeJobStage represents one row in the live progress timeline returned
// from GET /api/job/:jobId for transcode jobs.
type TranscodeJobStage struct {
	Key          string               `json:"key"`
	Label        string               `json:"label"`
	Status       TranscodeStageStatus `json:"status"`
	Progress     int                  `json:"progress"`
	QualityLabel string               `json:"qualityLabel,omitempty"`
	Message      string               `json:"message,omitempty"`
}
