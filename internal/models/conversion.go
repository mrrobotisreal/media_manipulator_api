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

// Image conversion options
type ImageConversionOptions struct {
	Format  string  `json:"format" binding:"required,oneof=jpg png webp gif"`
	Width   *int    `json:"width,omitempty"`
	Height  *int    `json:"height,omitempty"`
	Quality int     `json:"quality" binding:"min=1,max=100"`
	Filter  string  `json:"filter" binding:"oneof=none grayscale sepia blur sharpen"`
	Tint    *string `json:"tint,omitempty"`
}

// Video conversion options
type VideoConversionOptions struct {
	Format              string  `json:"format" binding:"required,oneof=mp4 webm avi mov"`
	Width               *int    `json:"width,omitempty"`
	Height              *int    `json:"height,omitempty"`
	PreserveAspectRatio bool    `json:"preserveAspectRatio"`
	Speed               float64 `json:"speed" binding:"min=0.25,max=4"`
	Quality             string  `json:"quality" binding:"oneof=low medium high"`
}

// Audio conversion options
type AudioConversionOptions struct {
	Format  string  `json:"format" binding:"required,oneof=mp3 wav aac ogg"`
	Bitrate string  `json:"bitrate" binding:"oneof=128 192 256 320"`
	Speed   float64 `json:"speed" binding:"min=0.25,max=4"`
	Volume  float64 `json:"volume" binding:"min=0.1,max=2"`
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
