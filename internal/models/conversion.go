package models

import (
	"strings"
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
	ID              string                 `json:"id"`
	Status          JobStatus              `json:"status"`
	Progress        int                    `json:"progress,omitempty"`
	ResultURL       string                 `json:"resultUrl,omitempty"`
	Error           string                 `json:"error,omitempty"`
	OriginalFile    OriginalFileInfo       `json:"originalFile"`
	Options         map[string]interface{} `json:"options"`
	CreatedAt       time.Time              `json:"createdAt"`
	CompletedAt     *time.Time             `json:"completedAt,omitempty"`
	Mode            string                 `json:"mode,omitempty"`
	CurrentStage    string                 `json:"currentStage,omitempty"`
	Stages          []TranscodeJobStage    `json:"stages,omitempty"`
	ResultS3Key     string                 `json:"resultS3Key,omitempty"`
	ResultFileName  string                 `json:"resultFileName,omitempty"`
	ExpiresAt       *time.Time             `json:"expiresAt,omitempty"`
	TranscodeReport *VideoProbeResponse    `json:"transcodeReport,omitempty"`
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
	Brightness   *int         `json:"brightness,omitempty"`
	Contrast     *int         `json:"contrast,omitempty"`
	Saturation   *int         `json:"saturation,omitempty"`
	Hue          *int         `json:"hue,omitempty"`
	Gamma        *float64     `json:"gamma,omitempty"`
	Exposure     *float64     `json:"exposure,omitempty"`
	Shadows      *int         `json:"shadows,omitempty"`
	Highlights   *int         `json:"highlights,omitempty"`
	GaussianBlur *int         `json:"gaussianBlur,omitempty"`
	MotionBlur   *MotionBlur  `json:"motionBlur,omitempty"`
	UnsharpMask  *UnsharpMask `json:"unsharpMask,omitempty"`
	Artistic     *string      `json:"artistic,omitempty"`
	Noise        *NoiseEffect `json:"noise,omitempty"`
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
	Rotation       *float64  `json:"rotation,omitempty"`
	FlipHorizontal *bool     `json:"flipHorizontal,omitempty"`
	FlipVertical   *bool     `json:"flipVertical,omitempty"`
	Crop           *CropArea `json:"crop,omitempty"`
	Padding        *Padding  `json:"padding,omitempty"`
}

type Padding struct {
	Top    int    `json:"top"`
	Bottom int    `json:"bottom"`
	Left   int    `json:"left"`
	Right  int    `json:"right"`
	Color  string `json:"color"`
}

type TemporalEffects struct {
	VariableSpeed []SpeedPoint     `json:"variableSpeed,omitempty"`
	Reverse       *bool            `json:"reverse,omitempty"`
	PingPong      *bool            `json:"pingPong,omitempty"`
	FrameRate     *FrameRateConfig `json:"frameRate,omitempty"`
	Stabilization *Stabilization   `json:"stabilization,omitempty"`
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
	Enabled   bool `json:"enabled"`
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
	Normalize    *bool             `json:"normalize,omitempty"`
	Amplify      *float64          `json:"amplify,omitempty"`
	FadeIn       *float64          `json:"fadeIn,omitempty"`
	FadeOut      *float64          `json:"fadeOut,omitempty"`
	DynamicRange *DynamicRange     `json:"dynamicRange,omitempty"`
	Equalizer    *Equalizer        `json:"equalizer,omitempty"`
	Stereo       *StereoProcessing `json:"stereo,omitempty"`
}

type DynamicRange struct {
	Enabled   bool    `json:"enabled"`
	Ratio     float64 `json:"ratio"`
	Threshold float64 `json:"threshold"`
}

type Equalizer struct {
	Enabled  bool     `json:"enabled"`
	Preset   string   `json:"preset"`
	LowPass  *float64 `json:"lowPass,omitempty"`
	HighPass *float64 `json:"highPass,omitempty"`
	Bands    []EQBand `json:"bands,omitempty"`
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
	Format         string               `json:"format" binding:"required,oneof=jpg png webp gif"`
	Width          *int                 `json:"width,omitempty"`
	Height         *int                 `json:"height,omitempty"`
	Quality        int                  `json:"quality" binding:"min=1,max=100"`
	Filter         string               `json:"filter" binding:"oneof=none grayscale sepia blur sharpen"`
	Tint           *string              `json:"tint,omitempty"`
	Crop           *CropArea            `json:"crop,omitempty"`
	TextOverlay    *ImageTextOverlay    `json:"textOverlay,omitempty"`
	RemoveMetadata bool                 `json:"removeMetadata,omitempty"`
	MetadataMode   string               `json:"metadataMode,omitempty"`
	Metadata       *ImageMetadataFields `json:"metadata,omitempty"`
	GPSOptions     *ImageGPSOptions     `json:"gpsOptions,omitempty"`
	AdvancedTags   map[string]string    `json:"advancedTags,omitempty"`
	AI             *AIImageOptions      `json:"ai,omitempty"`
}

// AIImageOptions selects a Phase 1 AI image operation. Only one operation runs
// per job. When Enabled is true and Operation is not empty/"none", the normal
// ImageMagick pipeline is skipped.
type AIImageOptions struct {
	Enabled          bool                     `json:"enabled,omitempty"`
	Operation        string                   `json:"operation,omitempty"`
	FaceMode         string                   `json:"faceMode,omitempty"`
	FaceSelection    *FaceSelectionOptions    `json:"faceSelection,omitempty"`
	BackgroundModel  string                   `json:"backgroundModel,omitempty"`
	UpscaleScale     int                      `json:"upscaleScale,omitempty"`
	UpscaleModel     string                   `json:"upscaleModel,omitempty"`
	TextDetect       string                   `json:"textDetect,omitempty"`
	TextRedaction    string                   `json:"textRedaction,omitempty"`
	RemoveObjectMask *RemoveObjectMaskOptions `json:"removeObjectMask,omitempty"`
}

// RemoveObjectMaskOptions carries the user-drawn rectangles for the
// remove_object AI op. Rectangle fields are normalized to [0,1] relative to
// the original image dimensions so we don't depend on the rendered preview
// size in the UI. The Go side rasterizes a same-sized PNG mask (white inside
// rectangles, black elsewhere) and hands it to remove_object_lama.py.
type RemoveObjectMaskOptions struct {
	Rectangles []NormalizedRect `json:"rectangles,omitempty"`
}

// NormalizedRect is a rectangle in [0,1] normalized image coordinates.
type NormalizedRect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// FaceSelectionMode selects how the stored face boxes are applied during the
// face_privacy AI op. The default ("" or "all") preserves the original behavior
// of obscuring every detected face.
type FaceSelectionMode string

const (
	FaceSelectionAll               FaceSelectionMode = "all"
	FaceSelectionOnlySelected      FaceSelectionMode = "only_selected"
	FaceSelectionAllExceptSelected FaceSelectionMode = "all_except_selected"
)

// FaceSelectionOptions pairs a detect-face session with a user selection so the
// final /api/upload job can reuse the exact face boxes the user saw in the
// preview overlay. SessionID is only required for non-"all" modes.
type FaceSelectionOptions struct {
	SessionID       string   `json:"sessionId,omitempty"`
	SelectionMode   string   `json:"selectionMode,omitempty"`
	SelectedFaceIDs []string `json:"selectedFaceIds,omitempty"`
}

// FaceBox describes one detected face. Normalized fields (x/y/width/height) are
// in [0,1] relative to the original image dimensions so the UI can overlay
// boxes using CSS percentages without knowing the rendered size. PixelBox is
// the absolute box the backend uses when re-applying the effect.
type FaceBox struct {
	ID         string        `json:"id"`
	Index      int           `json:"index,omitempty"`
	Confidence float64       `json:"confidence,omitempty"`
	X          float64       `json:"x"`
	Y          float64       `json:"y"`
	Width      float64       `json:"width"`
	Height     float64       `json:"height"`
	PixelBox   *FacePixelBox `json:"pixelBox,omitempty"`
}

type FacePixelBox struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// FaceDetectionResponse is returned by POST /api/ai/faces/detect. The session
// ID must be echoed back in AIImageOptions.FaceSelection for the final
// /api/upload job to reuse the stored boxes.
type FaceDetectionResponse struct {
	FaceDetectionSessionID string    `json:"faceDetectionSessionId"`
	ImageWidth             int       `json:"imageWidth"`
	ImageHeight            int       `json:"imageHeight"`
	Faces                  []FaceBox `json:"faces"`
	ExpiresAt              time.Time `json:"expiresAt"`
}

type ImageTextOverlay struct {
	Text        string `json:"text,omitempty"`
	Size        int    `json:"size,omitempty"`
	Color       string `json:"color,omitempty"`
	StrokeColor string `json:"strokeColor,omitempty"`
	StrokeWidth int    `json:"strokeWidth,omitempty"`
	Gravity     string `json:"gravity,omitempty"`
	Font        string `json:"font,omitempty"`
	X           int    `json:"x,omitempty"`
	Y           int    `json:"y,omitempty"`
}

type ImageMetadataFields struct {
	Title       string            `json:"title,omitempty"`
	Author      string            `json:"author,omitempty"`
	Description string            `json:"description,omitempty"`
	Copyright   string            `json:"copyright,omitempty"`
	Comment     string            `json:"comment,omitempty"`
	Keywords    string            `json:"keywords,omitempty"`
	ExifTiff    map[string]string `json:"exifTiff,omitempty"`
	GPSLocation map[string]string `json:"gpsLocation,omitempty"`
	IPTC        map[string]string `json:"iptc,omitempty"`
	Advanced    map[string]string `json:"advanced,omitempty"`
}

type ImageGPSOptions struct {
	RemoveLocationData      bool     `json:"removeLocationData,omitempty"`
	ReplaceLocationData     bool     `json:"replaceLocationData,omitempty"`
	Latitude                *float64 `json:"latitude,omitempty"`
	Longitude               *float64 `json:"longitude,omitempty"`
	Altitude                *float64 `json:"altitude,omitempty"`
	RoundLocationPrecision  bool     `json:"roundLocationPrecision,omitempty"`
	PrecisionDecimals       *int     `json:"precisionDecimals,omitempty"`
	RemoveCaptureDirection  bool     `json:"removeCaptureDirection,omitempty"`
	RemoveGPSTimestamp      bool     `json:"removeGpsTimestamp,omitempty"`
	RemoveAltitude          bool     `json:"removeAltitude,omitempty"`
	RemoveDestinationFields bool     `json:"removeDestinationFields,omitempty"`
}

// Video conversion options
type VideoConversionOptions struct {
	Format              string              `json:"format" binding:"required,oneof=mp4 webm avi mov mkv flv wmv prores dnxhd gif"`
	Width               *int                `json:"width,omitempty"`
	Height              *int                `json:"height,omitempty"`
	PreserveAspectRatio bool                `json:"preserveAspectRatio"`
	Speed               float64             `json:"speed" binding:"min=0.25,max=4"`
	Quality             string              `json:"quality" binding:"oneof=low medium high"`
	Trim                *TrimRange          `json:"trim,omitempty"`
	VisualEffects       *VisualEffects      `json:"visualEffects,omitempty"`
	Transform           *Transform          `json:"transform,omitempty"`
	Temporal            *TemporalEffects    `json:"temporal,omitempty"`
	Advanced            *AdvancedProcessing `json:"advanced,omitempty"`
	GIF                 *GIFOptions         `json:"gif,omitempty"`
	AI                  *AIVideoOptions     `json:"ai,omitempty"`
}

// AIVideoOptions selects a Phase 1 AI video operation. Only one operation runs
// per job. When Enabled is true and Operation is not empty/"none", the normal
// FFmpeg filter chain is skipped and the AI helper script owns the output.
type AIVideoOptions struct {
	Enabled            bool                         `json:"enabled,omitempty"`
	Operation          string                       `json:"operation,omitempty"`
	FrameInterpolation *AIFrameInterpolationOptions `json:"frameInterpolation,omitempty"`
}

// AIFrameInterpolationOptions tunes rife-ncnn-vulkan invocation via the
// frame_interpolate_rife.py runtime script. Defaults are applied server-side
// when fields are empty/zero.
type AIFrameInterpolationOptions struct {
	TargetFPS     int    `json:"targetFps,omitempty"`
	Model         string `json:"model,omitempty"`
	Quality       string `json:"quality,omitempty"`
	MaxHeight     int    `json:"maxHeight,omitempty"`
	PreserveAudio bool   `json:"preserveAudio,omitempty"`
}

// GIFOptions tunes the animated-GIF pipeline (ffmpeg -> gifsicle). Mirrors the
// quick-gif2.sh script: scale to a width, sample at a frame rate, quantize via
// gifsicle. All fields are optional — sensible defaults are applied server-side
// when Format is "gif".
type GIFOptions struct {
	Width    *int `json:"width,omitempty"`    // output width in px (height auto, multiple of 4). Default 900.
	FPS      *int `json:"fps,omitempty"`      // ffmpeg sampling rate before gifsicle. Default 12.
	Colors   *int `json:"colors,omitempty"`   // gifsicle palette size, 2-256. Default 128.
	Delay    *int `json:"delay,omitempty"`    // gifsicle inter-frame delay in 1/100s. Default 3.
	Optimize *int `json:"optimize,omitempty"` // gifsicle --optimize level 1-3. Default 3.
}

// Audio conversion options
type AudioConversionOptions struct {
	Format           string            `json:"format" binding:"required,oneof=mp3 wav aac ogg flac alac opus ac3 dts"`
	Bitrate          string            `json:"bitrate" binding:"oneof=128 192 256 320 512 1024"`
	SampleRate       string            `json:"sampleRate" binding:"oneof=22050 44100 48000 96000 192000"`
	Channels         string            `json:"channels" binding:"oneof=mono stereo 5.1 7.1"`
	Speed            float64           `json:"speed" binding:"min=0.25,max=4"`
	Volume           float64           `json:"volume" binding:"min=0.1,max=2"`
	Trim             *TrimRange        `json:"trim,omitempty"`
	BasicProcessing  *BasicProcessing  `json:"basicProcessing,omitempty"`
	TimeBasedEffects *TimeBasedEffects `json:"timeBasedEffects,omitempty"`
	Restoration      *Restoration      `json:"restoration,omitempty"`
	Advanced         *AdvancedAudio    `json:"advanced,omitempty"`
	AI               *AIAudioOptions   `json:"ai,omitempty"`
}

// AIAudioOptions selects a Phase 1 AI audio operation. Only one operation runs
// per job. When Enabled is true and Operation is not empty/"none", the normal
// FFmpeg pipeline is skipped.
type AIAudioOptions struct {
	Enabled   bool   `json:"enabled,omitempty"`
	Operation string `json:"operation,omitempty"`
}

// Upload request
type UploadRequest struct {
	Options map[string]interface{} `json:"options"`
}

// Upload response
type UploadResponse struct {
	JobID string `json:"jobId"`
}

type VideoUploadPresignRequest struct {
	FileName      string `json:"fileName"`
	ContentType   string `json:"contentType"`
	FileSizeBytes int64  `json:"fileSizeBytes"`
	SessionID     string `json:"sessionId,omitempty"`
}

type VideoUploadTarget struct {
	UploadURL string `json:"uploadUrl"`
	S3Key     string `json:"s3Key"`
	Bucket    string `json:"bucket"`
	ExpiresAt string `json:"expiresAt"`
}

type VideoUploadCompleteRequest struct {
	S3Key         string                 `json:"s3Key"`
	FileName      string                 `json:"fileName"`
	ContentType   string                 `json:"contentType"`
	FileSizeBytes int64                  `json:"fileSizeBytes"`
	Options       map[string]interface{} `json:"options"`
}

type StructuredImageMetadata struct {
	Container              map[string]interface{}            `json:"container,omitempty"`
	ExifTiff               map[string]interface{}            `json:"exifTiff,omitempty"`
	GPSLocation            map[string]interface{}            `json:"gpsLocation,omitempty"`
	AdvancedDeviceMetadata map[string]map[string]interface{} `json:"advancedDeviceMetadata,omitempty"`
}

// Progress update
type ProgressUpdate struct {
	JobID    string `json:"jobId"`
	Progress int    `json:"progress"`
}

// File identification response
type FileIdentificationResponse struct {
	FileName      string                   `json:"fileName"`
	FileSize      int64                    `json:"fileSize"`
	FileType      FileType                 `json:"fileType"`
	MimeType      string                   `json:"mimeType"`
	Details       map[string]interface{}   `json:"details"`
	ImageMetadata *StructuredImageMetadata `json:"imageMetadata,omitempty"`
	Tool          string                   `json:"tool"`      // Which tool was used for identification
	RawOutput     string                   `json:"rawOutput"` // Raw command output for debugging
}

// Helper function to determine file type from MIME type
func GetFileType(mimeType string) FileType {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return FileTypeImage
	case strings.HasPrefix(mimeType, "video/"):
		return FileTypeVideo
	case strings.HasPrefix(mimeType, "audio/"):
		return FileTypeAudio
	default:
		return FileTypeUnknown
	}
}
