package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type MediaInspector struct {
	commandTimeout time.Duration
}

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

func (m *MediaInspector) ProbeFile(ctx context.Context, path string, fileType models.FileType) (*MediaMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, m.commandTimeout)
	defer cancel()

	metadata := &MediaMetadata{FileType: fileType, Details: map[string]any{}}
	_, mimeType := m.DetectFile(ctx, path, "")
	metadata.MimeType = mimeType

	switch fileType {
	case models.FileTypeImage:
		tool, args := imageMagickIdentifyCommand(path)
		stdout, stderr, err := runCommand(ctx, tool, args...)
		metadata.Tool = strings.Join(append([]string{tool}, args[:len(args)-1]...), " ")
		metadata.Raw = stdout
		if err != nil {
			metadata.Error = strings.TrimSpace(stderr)
			return metadata, fmt.Errorf("identify image: %w", err)
		}
		container := parseIdentifyVerbose(stdout)
		metadata.Details = cloneAnyMap(container)
		metadata.ImageMetadata = &models.StructuredImageMetadata{Container: cloneAnyMap(container)}
		if exifMetadata, raw, exifErr := probeExiftool(ctx, path, container); exifErr != nil {
			metadata.Details["exiftool_error"] = strings.TrimSpace(exifErr.Error())
		} else {
			metadata.Tool += " + exiftool"
			metadata.Raw = strings.TrimSpace(metadata.Raw) + "\n\n--- exiftool ---\n" + raw
			metadata.ImageMetadata = exifMetadata
			metadata.Details["container"] = exifMetadata.Container
			metadata.Details["exifTiff"] = exifMetadata.ExifTiff
			metadata.Details["gpsLocation"] = exifMetadata.GPSLocation
			metadata.Details["advancedDeviceMetadata"] = exifMetadata.AdvancedDeviceMetadata
		}
	case models.FileTypeVideo, models.FileTypeAudio:
		stdout, stderr, err := runCommand(ctx, "ffprobe", "-v", "error", "-print_format", "json", "-show_streams", "-show_format", path)
		metadata.Tool = "ffprobe"
		metadata.Raw = stdout
		if err != nil {
			metadata.Error = strings.TrimSpace(stderr)
			return metadata, fmt.Errorf("ffprobe: %w", err)
		}
		var details map[string]any
		if err := json.Unmarshal([]byte(stdout), &details); err != nil {
			return metadata, fmt.Errorf("parse ffprobe json: %w", err)
		}
		metadata.Details = details
	case models.FileTypeDocument:
		// PDFs are inspected with pdfinfo (poppler-utils) when available. We
		// never feed PDFs to ImageMagick's identify probe — the deployment's
		// ImageMagick policy commonly blocks the PDF coder, and treating
		// untrusted PDFs as images via Ghostscript is a needless risk. If
		// pdfinfo is missing we fall back to the `file` command, which only
		// reads the header.
		if _, lookErr := exec.LookPath("pdfinfo"); lookErr == nil {
			stdout, stderr, err := runCommand(ctx, "pdfinfo", path)
			metadata.Tool = "pdfinfo"
			metadata.Raw = stdout
			if err != nil {
				metadata.Error = strings.TrimSpace(stderr)
				// Non-fatal: fall through to file-based detection below.
			} else {
				metadata.Details = parsePdfInfo(stdout)
			}
		}
		if metadata.Tool == "" || metadata.Error != "" {
			stdout, stderr, err := runCommand(ctx, "file", "-b", "--mime-all", path)
			if err == nil {
				if metadata.Tool == "" {
					metadata.Tool = "file"
				} else {
					metadata.Tool += " + file"
				}
				if strings.TrimSpace(metadata.Raw) == "" {
					metadata.Raw = stdout
				}
				metadata.Details["file_command_output"] = strings.TrimSpace(stdout)
				metadata.Error = ""
			} else if metadata.Error == "" {
				metadata.Error = strings.TrimSpace(stderr)
			}
		}
	default:
		stdout, stderr, err := runCommand(ctx, "file", "-b", "--mime-all", path)
		metadata.Tool = "file"
		metadata.Raw = stdout
		if err != nil {
			metadata.Error = strings.TrimSpace(stderr)
			return metadata, fmt.Errorf("file: %w", err)
		}
		metadata.Details["file_command_output"] = strings.TrimSpace(stdout)
	}

	if stat, err := os.Stat(path); err == nil {
		metadata.Details["size_bytes"] = stat.Size()
		metadata.Details["modification_time"] = stat.ModTime().UTC()
	}
	return metadata, nil
}

func (m *MediaInspector) HasAudioStream(ctx context.Context, path string) bool {
	stdout, _, err := runCommand(ctx, "ffprobe", "-v", "error", "-select_streams", "a", "-show_entries", "stream=index", "-of", "csv=p=0", path)
	return err == nil && strings.TrimSpace(stdout) != ""
}

func WriteMetadata(path string, metadata *MediaMetadata) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0644)
}

func imageMagickIdentifyCommand(path string) (string, []string) {
	if _, err := exec.LookPath("magick"); err == nil {
		return "magick", []string{"identify", "-verbose", path}
	}
	return "identify", []string{"-verbose", path}
}

func parseIdentifyVerbose(raw string) map[string]any {
	details := make(map[string]any)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key != "" {
			details[key] = value
		}
	}
	return details
}

func probeExiftool(ctx context.Context, path string, container map[string]any) (*models.StructuredImageMetadata, string, error) {
	stdout, stderr, err := runCommand(ctx, "exiftool", "-json", "-G1", "-s", "-a", path)
	if err != nil {
		if strings.TrimSpace(stderr) != "" {
			return nil, stdout, fmt.Errorf("%s", strings.TrimSpace(stderr))
		}
		return nil, stdout, err
	}
	var records []map[string]any
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		return nil, stdout, fmt.Errorf("parse exiftool json: %w", err)
	}
	metadata := &models.StructuredImageMetadata{
		Container:              cloneAnyMap(container),
		ExifTiff:               map[string]any{},
		GPSLocation:            map[string]any{},
		AdvancedDeviceMetadata: map[string]map[string]any{},
	}
	if len(records) == 0 {
		return metadata, stdout, nil
	}

	for rawKey, value := range records[0] {
		if rawKey == "SourceFile" || value == nil || value == "" {
			continue
		}
		group, tag := splitExiftoolKey(rawKey)
		if exifTiffTags()[tag] {
			metadata.ExifTiff[tag] = value
			continue
		}
		if gpsLocationTags()[tag] {
			metadata.GPSLocation[tag] = value
			continue
		}
		if isContainerExifGroup(group) {
			if _, exists := metadata.Container[tag]; !exists {
				metadata.Container[tag] = value
			}
			continue
		}
		if isAdvancedMetadataGroup(group) {
			if metadata.AdvancedDeviceMetadata[group] == nil {
				metadata.AdvancedDeviceMetadata[group] = map[string]any{}
			}
			metadata.AdvancedDeviceMetadata[group][tag] = value
		}
	}
	return metadata, stdout, nil
}

func splitExiftoolKey(key string) (string, string) {
	parts := strings.Split(key, ":")
	if len(parts) < 2 {
		return "Unknown", key
	}
	group := strings.TrimSpace(parts[0])
	tag := strings.TrimSpace(parts[len(parts)-1])
	if group == "" {
		group = "Unknown"
	}
	return group, tag
}

func cloneAnyMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func isContainerExifGroup(group string) bool {
	switch strings.ToUpper(strings.TrimSpace(group)) {
	case "FILE", "JFIF", "PNG", "RIFF", "WEBP", "ICC_PROFILE", "COMPOSITE":
		return true
	default:
		return false
	}
}

func isAdvancedMetadataGroup(group string) bool {
	group = strings.ToUpper(strings.TrimSpace(group))
	if group == "" || group == "FILE" || group == "EXIF" || group == "GPS" || group == "COMPOSITE" {
		return false
	}
	return true
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
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "tiff", "heic", "avif":
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

// parsePdfInfo turns `pdfinfo` key:value output into a details map. pdfinfo
// prints lines like "Pages:          3" and "Page size:      612 x 792 pts".
func parsePdfInfo(raw string) map[string]any {
	details := make(map[string]any)
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}
		if key == "Pages" {
			if n, err := strconv.Atoi(value); err == nil {
				details["pages"] = n
				continue
			}
		}
		details[key] = value
	}
	return details
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
