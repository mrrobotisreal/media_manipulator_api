package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

const defaultVLMModel = "qwen3-vl:8b-instruct-q8_0"

type AnalysisQueue struct {
	cfg       *config.Config
	inspector *MediaInspector
	jobs      chan AnalysisJob
}

type AnalysisJob struct {
	JobID     string          `json:"jobId"`
	InputPath string          `json:"inputPath"`
	OutputDir string          `json:"outputDir"`
	FileType  models.FileType `json:"fileType"`
	MimeType  string          `json:"mimeType"`
}

type analysisResult struct {
	JobID          string          `json:"jobId"`
	FileType       models.FileType `json:"fileType"`
	Model          string          `json:"model"`
	StartedAt      time.Time       `json:"startedAt"`
	CompletedAt    time.Time       `json:"completedAt"`
	TranscriptPath string          `json:"transcriptPath,omitempty"`
	FramesDir      string          `json:"framesDir,omitempty"`
	AudioDetected  bool            `json:"audioDetected,omitempty"`
	Summary        any             `json:"summary,omitempty"`
	Batches        []any           `json:"batches,omitempty"`
	Error          string          `json:"error,omitempty"`
}

func NewAnalysisQueue(cfg *config.Config, inspector *MediaInspector) *AnalysisQueue {
	workers := cfg.AnalysisWorkers
	if workers <= 0 {
		workers = 1
	}
	return &AnalysisQueue{cfg: cfg, inspector: inspector, jobs: make(chan AnalysisJob, workers*8)}
}

func (q *AnalysisQueue) Start() {
	for i := 0; i < q.cfg.AnalysisWorkers; i++ {
		go q.worker()
	}
}

func (q *AnalysisQueue) Enqueue(job AnalysisJob) {
	select {
	case q.jobs <- job:
	default:
		go func() { q.jobs <- job }()
	}
}

func (q *AnalysisQueue) worker() {
	for job := range q.jobs {
		ctx, cancel := context.WithTimeout(context.Background(), q.cfg.CommandTimeout)
		if err := q.run(ctx, job); err != nil {
			log.Printf("analysis job %s failed: %v", job.JobID, err)
		}
		cancel()
	}
}

func (q *AnalysisQueue) run(ctx context.Context, job AnalysisJob) error {
	started := time.Now().UTC()
	result := analysisResult{JobID: job.JobID, FileType: job.FileType, Model: envOrDefault("OLLAMA_VLM_MODEL", defaultVLMModel), StartedAt: started}
	defer func() {
		result.CompletedAt = time.Now().UTC()
		_ = writeJSON(filepath.Join(job.OutputDir, "analysis.json"), result)
	}()

	switch job.FileType {
	case models.FileTypeImage:
		summary, err := q.analyzeImages(ctx, []string{job.InputPath}, "Analyze this uploaded image for internal product intelligence. Return concise JSON with visible subject, media characteristics, possible editing intent, quality issues, and safety-relevant observations.")
		if err != nil {
			result.Error = err.Error()
			return err
		}
		result.Summary = summary
	case models.FileTypeVideo:
		result.AudioDetected = q.inspector.HasAudioStream(ctx, job.InputPath)
		if result.AudioDetected {
			transcriptPath, err := q.transcribeVideo(ctx, job)
			if err != nil {
				log.Printf("transcription job %s failed: %v", job.JobID, err)
			} else {
				result.TranscriptPath = transcriptPath
			}
		}
		framesDir, frames, err := q.extractFrames(ctx, job)
		if err != nil {
			result.Error = err.Error()
			return err
		}
		result.FramesDir = framesDir
		batches, err := q.analyzeFrameBatches(ctx, frames, result.TranscriptPath)
		if err != nil {
			result.Error = err.Error()
			return err
		}
		result.Batches = batches
	case models.FileTypeAudio:
		transcriptPath, err := q.transcribeVideo(ctx, job)
		if err != nil {
			result.Error = err.Error()
			return err
		}
		result.AudioDetected = true
		result.TranscriptPath = transcriptPath
	default:
		result.Error = "unsupported analysis file type"
	}
	return nil
}

func (q *AnalysisQueue) transcribeVideo(ctx context.Context, job AnalysisJob) (string, error) {
	audioPath := filepath.Join(job.OutputDir, "analysis_audio.wav")
	_, stderr, err := runCommand(ctx, "ffmpeg", "-y", "-i", job.InputPath, "-vn", "-acodec", "pcm_s16le", "-ar", "16000", "-ac", "1", audioPath)
	if err != nil {
		return "", fmt.Errorf("extract audio: %w: %s", err, tail(stderr, 1200))
	}

	transcriptDir := filepath.Join(job.OutputDir, "transcript")
	if err := os.MkdirAll(transcriptDir, 0755); err != nil {
		return "", err
	}
	bin := envOrDefault("WHISPER_CT2_BIN", "/opt/creatv/whisper-ct2/bin/whisper-ctranslate2")
	args := []string{
		"--model", envOrDefault("WHISPER_CT2_MODEL", "large-v3"),
		"--device", envOrDefault("WHISPER_CT2_DEVICE", "cuda"),
		"--compute_type", envOrDefault("WHISPER_CT2_COMPUTE_TYPE", "float16"),
		"--output_format", "json",
		"--output_dir", transcriptDir,
	}
	if deviceIndex := strings.TrimSpace(os.Getenv("WHISPER_CT2_DEVICE_INDEX")); deviceIndex != "" {
		args = append(args, "--device_index", deviceIndex)
	}
	if language := strings.TrimSpace(os.Getenv("WHISPER_CT2_LANGUAGE")); language != "" {
		args = append(args, "--language", language)
	}
	if envBool("WHISPER_CT2_VAD_FILTER", true) {
		args = append(args, "--vad_filter", "True")
	}
	if envBool("WHISPER_CT2_BATCHED", true) {
		args = append(args, "--batched", "True", "--batch_size", envOrDefault("WHISPER_CT2_BATCH_SIZE", "8"))
	}
	args = append(args, audioPath)

	stdout, stderr, err := runCommand(ctx, bin, args...)
	if err != nil {
		return "", fmt.Errorf("whisper-ct2: %w: %s", err, tail(stderr, 2000))
	}
	matches, _ := filepath.Glob(filepath.Join(transcriptDir, "*.json"))
	if len(matches) > 0 {
		return matches[0], nil
	}
	fallback := filepath.Join(transcriptDir, "transcript.json")
	if err := os.WriteFile(fallback, []byte(stdout), 0644); err != nil {
		return "", err
	}
	return fallback, nil
}

func (q *AnalysisQueue) extractFrames(ctx context.Context, job AnalysisJob) (string, []string, error) {
	framesDir := filepath.Join(job.OutputDir, "frames")
	if err := os.MkdirAll(framesDir, 0755); err != nil {
		return "", nil, err
	}
	interval := envInt("OLLAMA_VLM_FRAME_INTERVAL_SECONDS", 10)
	maxWidth := envInt("OLLAMA_VLM_MAX_WIDTH", 768)
	maxFrames := envInt("OLLAMA_VLM_MAX_FRAMES", 24)
	pattern := filepath.Join(framesDir, "frame_%05d.jpg")
	filter := fmt.Sprintf("fps=1/%d,scale=%d:-2:flags=lanczos", interval, maxWidth)
	_, stderr, err := runCommand(ctx, "ffmpeg", "-y", "-i", job.InputPath, "-vf", filter, "-q:v", "3", "-frames:v", strconv.Itoa(maxFrames), pattern)
	if err != nil {
		return framesDir, nil, fmt.Errorf("extract frames: %w: %s", err, tail(stderr, 1200))
	}
	frames, err := filepath.Glob(filepath.Join(framesDir, "frame_*.jpg"))
	if err != nil {
		return framesDir, nil, err
	}
	return framesDir, frames, nil
}

func (q *AnalysisQueue) analyzeFrameBatches(ctx context.Context, frames []string, transcriptPath string) ([]any, error) {
	batchSize := envInt("OLLAMA_VLM_BATCH_SIZE", 4)
	if batchSize <= 0 {
		batchSize = 4
	}
	transcript := readOptionalText(transcriptPath, 8000)
	var out []any
	for start := 0; start < len(frames); start += batchSize {
		end := start + batchSize
		if end > len(frames) {
			end = len(frames)
		}
		prompt := "Analyze these sampled video frames for internal product intelligence. Return concise JSON with scene progression, visual quality, likely editing intent, notable objects/text, and issues."
		if transcript != "" {
			prompt += " Transcript context: " + transcript
		}
		res, err := q.analyzeImages(ctx, frames[start:end], prompt)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

func (q *AnalysisQueue) analyzeImages(ctx context.Context, paths []string, prompt string) (any, error) {
	images := make([]string, 0, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		images = append(images, base64.StdEncoding.EncodeToString(body))
	}
	payload := map[string]any{
		"model":  envOrDefault("OLLAMA_VLM_MODEL", defaultVLMModel),
		"stream": false,
		"messages": []map[string]any{{
			"role":    "user",
			"content": prompt,
			"images":  images,
		}},
		"format": "json",
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(envOrDefault("OLLAMA_URL", "http://localhost:11434"), "/") + "/api/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Duration(envInt("OLLAMA_TIMEOUT_SECONDS", 300)) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama status %d: %v", resp.StatusCode, response)
	}
	return response, nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0644)
}

func readOptionalText(path string, max int) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(body) > max {
		body = body[:max]
	}
	return string(body)
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func tail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

var _ = exec.ErrNotFound
