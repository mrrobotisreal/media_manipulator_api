package services

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type MediaInspector struct {
	commandTimeout time.Duration
}

// MediaMetadata is the pre-MM08 metadata.json shape. Nothing writes it any
// more; it is kept so old job files still decode and migrate on read.
type MediaMetadata struct {
	FileType      models.FileType                 `json:"fileType"`
	MimeType      string                          `json:"mimeType"`
	Tool          string                          `json:"tool"`
	Details       map[string]any                  `json:"details"`
	ImageMetadata *models.StructuredImageMetadata `json:"imageMetadata,omitempty"`
	Raw           string                          `json:"raw,omitempty"`
	Error         string                          `json:"error,omitempty"`
}

func NewMediaInspector(commandTimeout time.Duration) *MediaInspector {
	if commandTimeout <= 0 {
		commandTimeout = 6 * time.Hour
	}
	return &MediaInspector{commandTimeout: commandTimeout}
}

func (m *MediaInspector) DetectFile(ctx context.Context, path string, declaredMime string) (models.FileType, string) {
	mimeType := strings.TrimSpace(declaredMime)
	if detected, err := mimetype.DetectFile(path); err == nil && detected != nil {
		mimeType = detected.String()
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(readFilePrefix(path, 512))
	}
	fileType := models.GetFileType(mimeType)
	if fileType != models.FileTypeUnknown {
		return fileType, mimeType
	}
	return detectTypeByExtension(path), mimeType
}

func (m *MediaInspector) HasAudioStream(ctx context.Context, path string) bool {
	stdout, _, err := runCommand(ctx, "ffprobe", "-v", "error", "-select_streams", "a", "-show_entries", "stream=index", "-of", "csv=p=0", path)
	return err == nil && strings.TrimSpace(stdout) != ""
}

func exifTiffTags() map[string]bool {
	return map[string]bool{
		"ImageDescription": true, "Make": true, "Model": true, "Software": true,
		"Artist": true, "Copyright": true, "Orientation": true, "DateTime": true,
		"DateTimeOriginal": true, "CreateDate": true, "ModifyDate": true, "SubSecTime": true,
		"SubSecTimeOriginal": true, "SubSecTimeDigitized": true, "OffsetTime": true,
		"OffsetTimeOriginal": true, "OffsetTimeDigitized": true, "ExposureTime": true,
		"FNumber": true, "ExposureProgram": true, "ISO": true, "SensitivityType": true,
		"RecommendedExposureIndex": true, "ExifVersion": true, "ShutterSpeedValue": true,
		"ApertureValue": true, "BrightnessValue": true, "ExposureCompensation": true,
		"MaxApertureValue": true, "SubjectDistance": true, "MeteringMode": true,
		"LightSource": true, "Flash": true, "FocalLength": true,
		"FocalLengthIn35mmFormat": true, "SubjectArea": true, "MakerNote": true,
		"UserComment": true, "FlashpixVersion": true, "ColorSpace": true,
		"ExifImageWidth": true, "ExifImageHeight": true, "SensingMethod": true,
		"FileSource": true, "SceneType": true, "CustomRendered": true,
		"ExposureMode": true, "WhiteBalance": true, "DigitalZoomRatio": true,
		"SceneCaptureType": true, "GainControl": true, "Contrast": true,
		"Saturation": true, "Sharpness": true, "SubjectDistanceRange": true,
		"LensMake": true, "LensModel": true, "LensSerialNumber": true,
		"CameraOwnerName": true, "BodySerialNumber": true,
	}
}

func gpsLocationTags() map[string]bool {
	return map[string]bool{
		"GPSVersionID": true, "GPSLatitudeRef": true, "GPSLatitude": true,
		"GPSLongitudeRef": true, "GPSLongitude": true, "GPSAltitudeRef": true,
		"GPSAltitude": true, "GPSTimeStamp": true, "GPSSatellites": true,
		"GPSStatus": true, "GPSMeasureMode": true, "GPSDOP": true,
		"GPSSpeedRef": true, "GPSSpeed": true, "GPSTrackRef": true,
		"GPSTrack": true, "GPSImgDirectionRef": true, "GPSImgDirection": true,
		"GPSMapDatum": true, "GPSDestLatitudeRef": true, "GPSDestLatitude": true,
		"GPSDestLongitudeRef": true, "GPSDestLongitude": true,
		"GPSDestBearingRef": true, "GPSDestBearing": true,
		"GPSDestDistanceRef": true, "GPSDestDistance": true,
		"GPSProcessingMethod": true, "GPSAreaInformation": true,
		"GPSDateStamp": true, "GPSDifferential": true,
		"GPSHPositioningError": true,
	}
}

func detectTypeByExtension(path string) models.FileType {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "tiff", "heic", "heif", "avif", "svg", "ico":
		return models.FileTypeImage
	case "mp4", "mov", "m4v", "webm", "mkv", "avi", "flv", "wmv", "mpeg", "mpg":
		return models.FileTypeVideo
	case "mp3", "wav", "aac", "ogg", "flac", "m4a", "opus", "ac3", "dts", "alac":
		return models.FileTypeAudio
	case "pdf":
		return models.FileTypeDocument
	default:
		return models.FileTypeUnknown
	}
}

func readFilePrefix(path string, n int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, n)
	read, _ := f.Read(buf)
	return buf[:read]
}

func runCommand(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}
