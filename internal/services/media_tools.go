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
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type MediaInspector struct {
	commandTimeout time.Duration
}

type MediaMetadata struct {
	FileType models.FileType `json:"fileType"`
	MimeType string          `json:"mimeType"`
	Tool     string          `json:"tool"`
	Details  map[string]any  `json:"details"`
	Raw      string          `json:"raw,omitempty"`
	Error    string          `json:"error,omitempty"`
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
		metadata.Details = parseIdentifyVerbose(stdout)
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

func detectTypeByExtension(path string) models.FileType {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "tiff", "heic", "avif":
		return models.FileTypeImage
	case "mp4", "mov", "m4v", "webm", "mkv", "avi", "flv", "wmv", "mpeg", "mpg":
		return models.FileTypeVideo
	case "mp3", "wav", "aac", "ogg", "flac", "m4a", "opus", "ac3", "dts", "alac":
		return models.FileTypeAudio
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
