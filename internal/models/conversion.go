package models

import (
	"time"
)

type JobStatus string

const (
	StatusPending    JobStatus = "pending"
	StatusProcessing JobStatus = "processing"
	StatusCompleted  JobStatus = "completed"
	StatusFailed     JobStatus = "failed"
)

type FileType string

const (
	FileTypeImage   FileType = "image"
	FileTypeVideo   FileType = "video"
	FileTypeAudio   FileType = "audio"
	FileTypeUnknown FileType = "unknown"
)

type ConversionJob struct {
	ID           string                 `json:"id"`
	Status       JobStatus              `json:"status"`
	Progress     int                    `json:"progress,omitempty"`
	ResultURL    string                 `json:"resultUrl,omitempty"`
	Error        string                 `json:"error,omitempty"`
	OriginalFile OriginalFileInfo       `json:"originalFile"`
	Options      map[string]interface{} `json:"options"`
	CreatedAt    time.Time              `json:"createdAt"`
	CompletedAt  *time.Time             `json:"completedAt,omitempty"`
}

type OriginalFileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Type string `json:"type"`
}

// Crop area for images
type CropArea struct {
	X      int `json:"x" binding:"min=0"`
	Y      int `json:"y" binding:"min=0"`
	Width  int `json:"width" binding:"min=1"`
	Height int `json:"height" binding:"min=1"`
}

// Trim range for video and audio
type TrimRange struct {
	StartTime float64 `json:"startTime" binding:"min=0"`
	EndTime   float64 `json:"endTime" binding:"min=0"`
}

// Video Effects Structures
type VisualEffects struct {
	Brightness    *int         `json:"brightness,omitempty"`
	Contrast      *int         `json:"contrast,omitempty"`
	Saturation    *int         `json:"saturation,omitempty"`
	Hue           *int         `json:"hue,omitempty"`
	Gamma         *float64     `json:"gamma,omitempty"`
	Exposure      *float64     `json:"exposure,omitempty"`
	Shadows       *int         `json:"shadows,omitempty"`
	Highlights    *int         `json:"highlights,omitempty"`
	GaussianBlur  *int         `json:"gaussianBlur,omitempty"`
	MotionBlur    *MotionBlur  `json:"motionBlur,omitempty"`
	UnsharpMask   *UnsharpMask `json:"unsharpMask,omitempty"`
	Artistic      *string      `json:"artistic,omitempty"`
	Noise         *NoiseEffect `json:"noise,omitempty"`
}

type MotionBlur struct {
	Angle    float64 `json:"angle"`
	Distance float64 `json:"distance"`
}

type UnsharpMask struct {
	Radius    float64 `json:"radius"`
	Amount    float64 `json:"amount"`
	Threshold float64 `json:"threshold"`
}

type NoiseEffect struct {
	Type   string  `json:"type"`
	Amount float64 `json:"amount"`
}

type Transform struct {
	Rotation      *float64 `json:"rotation,omitempty"`
	FlipHorizontal *bool    `json:"flipHorizontal,omitempty"`
	FlipVertical   *bool    `json:"flipVertical,omitempty"`
	Crop          *CropArea `json:"crop,omitempty"`
	Padding       *Padding  `json:"padding,omitempty"`
}

type Padding struct {
	Top    int    `json:"top"`
	Bottom int    `json:"bottom"`
	Left   int    `json:"left"`
	Right  int    `json:"right"`
	Color  string `json:"color"`
}

type TemporalEffects struct {
	VariableSpeed   []SpeedPoint     `json:"variableSpeed,omitempty"`
	Reverse         *bool            `json:"reverse,omitempty"`
	PingPong        *bool            `json:"pingPong,omitempty"`
	FrameRate       *FrameRateConfig `json:"frameRate,omitempty"`
	Stabilization   *Stabilization   `json:"stabilization,omitempty"`
}

type SpeedPoint struct {
	Time  float64 `json:"time"`
	Speed float64 `json:"speed"`
}

type FrameRateConfig struct {
	Target        *int  `json:"target,omitempty"`
	Interpolation *bool `json:"interpolation,omitempty"`
}

type Stabilization struct {
	Enabled  bool `json:"enabled"`
	Shakiness int  `json:"shakiness"`
	Accuracy  int  `json:"accuracy"`
}

type AdvancedProcessing struct {
	Deinterlace *bool       `json:"deinterlace,omitempty"`
	HDR         *HDRConfig  `json:"hdr,omitempty"`
	ColorSpace  *ColorSpace `json:"colorSpace,omitempty"`
}

type HDRConfig struct {
	Enabled     bool   `json:"enabled"`
	ToneMapping string `json:"toneMapping"`
}

type ColorSpace struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

// Audio Effects Structures
type BasicProcessing struct {
	Normalize      *bool           `json:"normalize,omitempty"`
	Amplify        *float64        `json:"amplify,omitempty"`
	FadeIn         *float64        `json:"fadeIn,omitempty"`
	FadeOut        *float64        `json:"fadeOut,omitempty"`
	DynamicRange   *DynamicRange   `json:"dynamicRange,omitempty"`
	Equalizer      *Equalizer      `json:"equalizer,omitempty"`
	Stereo         *StereoProcessing `json:"stereo,omitempty"`
}

type DynamicRange struct {
	Enabled   bool    `json:"enabled"`
	Ratio     float64 `json:"ratio"`
	Threshold float64 `json:"threshold"`
}

type Equalizer struct {
	Enabled  bool       `json:"enabled"`
	Preset   string     `json:"preset"`
	LowPass  *float64   `json:"lowPass,omitempty"`
	HighPass *float64   `json:"highPass,omitempty"`
	Bands    []EQBand   `json:"bands,omitempty"`
}

type EQBand struct {
	Frequency float64 `json:"frequency"`
	Gain      float64 `json:"gain"`
	Q         float64 `json:"q"`
}

type StereoProcessing struct {
	Pan            *float64 `json:"pan,omitempty"`
	Balance        *float64 `json:"balance,omitempty"`
	Width          *float64 `json:"width,omitempty"`
	MonoConversion *bool    `json:"monoConversion,omitempty"`
	ChannelSwap    *bool    `json:"channelSwap,omitempty"`
}

type TimeBasedEffects struct {
	Reverb     *Reverb     `json:"reverb,omitempty"`
	Delay      *Delay      `json:"delay,omitempty"`
	Modulation *Modulation `json:"modulation,omitempty"`
}

type Reverb struct {
	Enabled  bool    `json:"enabled"`
	Type     string  `json:"type"`
	RoomSize float64 `json:"roomSize"`
	Damping  float64 `json:"damping"`
	WetLevel float64 `json:"wetLevel"`
	DryLevel float64 `json:"dryLevel"`
}

type Delay struct {
	Enabled  bool    `json:"enabled"`
	Type     string  `json:"type"`
	Time     float64 `json:"time"`
	Feedback float64 `json:"feedback"`
	WetLevel float64 `json:"wetLevel"`
}

type Modulation struct {
	Enabled  bool    `json:"enabled"`
	Type     string  `json:"type"`
	Rate     float64 `json:"rate"`
	Depth    float64 `json:"depth"`
	Feedback float64 `json:"feedback"`
}

type Restoration struct {
	NoiseReduction   *NoiseReduction   `json:"noiseReduction,omitempty"`
	DeHum            *DeHum            `json:"deHum,omitempty"`
	Declip           *Declip           `json:"declip,omitempty"`
	SilenceDetection *SilenceDetection `json:"silenceDetection,omitempty"`
}

type NoiseReduction struct {
	Enabled     bool    `json:"enabled"`
	Type        string  `json:"type"`
	Strength    float64 `json:"strength"`
	Sensitivity float64 `json:"sensitivity"`
}

type DeHum struct {
	Enabled   bool   `json:"enabled"`
	Frequency string `json:"frequency"`
	Harmonics int    `json:"harmonics"`
}

type Declip struct {
	Enabled   bool    `json:"enabled"`
	Threshold float64 `json:"threshold"`
	Strength  float64 `json:"strength"`
}

type SilenceDetection struct {
	Enabled     bool    `json:"enabled"`
	Threshold   float64 `json:"threshold"`
	MinDuration float64 `json:"minDuration"`
	Action      string  `json:"action"`
}

type AdvancedAudio struct {
	PitchShift   *PitchShift   `json:"pitchShift,omitempty"`
	TimeStretch  *TimeStretch  `json:"timeStretch,omitempty"`
	SpatialAudio *SpatialAudio `json:"spatialAudio,omitempty"`
	Spectral     *Spectral     `json:"spectral,omitempty"`
}

type PitchShift struct {
	Enabled          bool `json:"enabled"`
	Semitones        int  `json:"semitones"`
	PreserveFormants bool `json:"preserveFormants"`
}

type TimeStretch struct {
	Enabled   bool    `json:"enabled"`
	Factor    float64 `json:"factor"`
	Algorithm string  `json:"algorithm"`
}

type SpatialAudio struct {
	Enabled  bool      `json:"enabled"`
	Type     string    `json:"type"`
	Position *Position `json:"position,omitempty"`
}

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type Spectral struct {
	Enabled bool   `json:"enabled"`
	Type    string `json:"type"`
	FFTSize int    `json:"fftSize"`
}

// Image conversion options
type ImageConversionOptions struct {
	Format  string     `json:"format" binding:"required,oneof=jpg png webp gif"`
	Width   *int       `json:"width,omitempty"`
	Height  *int       `json:"height,omitempty"`
	Quality int        `json:"quality" binding:"min=1,max=100"`
	Filter  string     `json:"filter" binding:"oneof=none grayscale sepia blur sharpen"`
	Tint    *string    `json:"tint,omitempty"`
	Crop    *CropArea  `json:"crop,omitempty"`
}

// Video conversion options
type VideoConversionOptions struct {
	Format              string               `json:"format" binding:"required,oneof=mp4 webm avi mov mkv flv wmv prores dnxhd"`
	Width               *int                 `json:"width,omitempty"`
	Height              *int                 `json:"height,omitempty"`
	PreserveAspectRatio bool                 `json:"preserveAspectRatio"`
	Speed               float64              `json:"speed" binding:"min=0.25,max=4"`
	Quality             string               `json:"quality" binding:"oneof=low medium high"`
	Trim                *TrimRange           `json:"trim,omitempty"`
	VisualEffects       *VisualEffects       `json:"visualEffects,omitempty"`
	Transform           *Transform           `json:"transform,omitempty"`
	Temporal            *TemporalEffects     `json:"temporal,omitempty"`
	Advanced            *AdvancedProcessing  `json:"advanced,omitempty"`
}

// Audio conversion options
type AudioConversionOptions struct {
	Format           string             `json:"format" binding:"required,oneof=mp3 wav aac ogg flac alac opus ac3 dts"`
	Bitrate          string             `json:"bitrate" binding:"oneof=128 192 256 320 512 1024"`
	SampleRate       string             `json:"sampleRate" binding:"oneof=22050 44100 48000 96000 192000"`
	Channels         string             `json:"channels" binding:"oneof=mono stereo 5.1 7.1"`
	Speed            float64            `json:"speed" binding:"min=0.25,max=4"`
	Volume           float64            `json:"volume" binding:"min=0.1,max=2"`
	Trim             *TrimRange         `json:"trim,omitempty"`
	BasicProcessing  *BasicProcessing   `json:"basicProcessing,omitempty"`
	TimeBasedEffects *TimeBasedEffects  `json:"timeBasedEffects,omitempty"`
	Restoration      *Restoration       `json:"restoration,omitempty"`
	Advanced         *AdvancedAudio     `json:"advanced,omitempty"`
}

// Upload request
type UploadRequest struct {
	Options map[string]interface{} `json:"options"`
}

// Upload response
type UploadResponse struct {
	JobID string `json:"jobId"`
}

// Progress update
type ProgressUpdate struct {
	JobID    string `json:"jobId"`
	Progress int    `json:"progress"`
}

// File identification response
type FileIdentificationResponse struct {
	FileName    string                 `json:"fileName"`
	FileSize    int64                  `json:"fileSize"`
	FileType    FileType               `json:"fileType"`
	MimeType    string                 `json:"mimeType"`
	Details     map[string]interface{} `json:"details"`
	Tool        string                 `json:"tool"`        // Which tool was used for identification
	RawOutput   string                 `json:"rawOutput"`   // Raw command output for debugging
}

// Helper function to determine file type from MIME type
func GetFileType(mimeType string) FileType {
	switch {
	case mimeType[:6] == "image/":
		return FileTypeImage
	case mimeType[:6] == "video/":
		return FileTypeVideo
	case mimeType[:6] == "audio/":
		return FileTypeAudio
	default:
		return FileTypeUnknown
	}
}
