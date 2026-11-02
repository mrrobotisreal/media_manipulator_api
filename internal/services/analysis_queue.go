package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

	// telemetry is set via SetTelemetry at startup. When nil, persistence
	// hooks become no-ops so unit tests don't need a DB.
	telemetry *analysisTelemetry

	// acquire is the shared GPU scheduler (nil = acquireGPUForJob);
	// injectable for tests. Every GPU step of the analysis (Whisper, each
	// Ollama call) takes a background-priority slot (MM94).
	acquire gpuAcquireFunc
}

type AnalysisJob struct {
	JobID            string          `json:"jobId"`
	InputPath        string          `json:"inputPath"`
	OutputDir        string          `json:"outputDir"`
	FileType         models.FileType `json:"fileType"`
	MimeType         string          `json:"mimeType"`
	Mode             string          `json:"mode,omitempty"`
	Transcript       string          `json:"transcript,omitempty"`
	Language         string          `json:"language,omitempty"`
	AudioDescription string          `json:"audioDescription,omitempty"`
}

type analysisResult struct {
	JobID            string          `json:"jobId"`
	FileType         models.FileType `json:"fileType"`
	Mode             string          `json:"mode,omitempty"`
	Model            string          `json:"model"`
	StartedAt        time.Time       `json:"startedAt"`
	CompletedAt      time.Time       `json:"completedAt"`
	TranscriptPath   string          `json:"transcriptPath,omitempty"`
	FramesDir        string          `json:"framesDir,omitempty"`
	AudioDetected    bool            `json:"audioDetected,omitempty"`
	Summary          any             `json:"summary,omitempty"`
	Batches          []any           `json:"batches,omitempty"`
	TranscriptReview any             `json:"transcriptReview,omitempty"`
	AudioDescription string          `json:"audioDescription,omitempty"`
	Error            string          `json:"error,omitempty"`
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
	result := analysisResult{JobID: job.JobID, FileType: job.FileType, Mode: job.Mode, Model: envOrDefault("OLLAMA_VLM_MODEL", defaultVLMModel), StartedAt: started, AudioDescription: job.AudioDescription}
	defer func() {
		result.CompletedAt = time.Now().UTC()
		_ = writeJSON(filepath.Join(job.OutputDir, "analysis.json"), result)
		// Persist operational telemetry. Safe with nil hooks.
		q.persistAnalysis(ctx, job, result)
	}()

	switch job.Mode {
	case "transcript":
		review, err := q.analyzeTranscript(ctx, job.Transcript, job.Language)
		if err != nil {
			result.Error = err.Error()
			return err
		}
		result.TranscriptReview = review
		return nil
	case "no_speech", "no_audio":
		summary, err := q.describeSilentMedia(ctx, job)
		if err != nil {
			result.Error = err.Error()
			return err
		}
		result.Summary = summary
		if description, ok := summary.(map[string]any); ok {
			if desc, ok := description["audio_description"].(string); ok && desc != "" {
				result.AudioDescription = desc
			}
		}
		return nil
	}

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
	// Resolve through the shared fallback chain so stale WHISPER_CT2_BIN env
	// vars don't silently break this codepath either.
	bin, resolveErr := ResolveWhisperCT2Bin(strings.TrimSpace(os.Getenv("WHISPER_CT2_BIN")))
	if resolveErr != nil {
		return "", resolveErr
	}
	args := []string{
		"--model", envOrDefault("WHISPER_CT2_MODEL", "large-v3"),
		"--device", envOrDefault("WHISPER_CT2_DEVICE", "cuda"),
		"--compute_type", envOrDefault("WHISPER_CT2_COMPUTE_TYPE", "float16"),
		"--output_format", "json",
		"--output_dir", transcriptDir,
	}
	// GPU selection runs through the shared scheduler — same pre-flight
	// nvidia-smi probe + queueing semantics as the captions pipeline. The
	// scheduler may park us briefly if every GPU is saturated; the release
	// function is deferred below so the slot frees the moment we return.
	cfg := WhisperCT2Config{
		Model:       envOrDefault("WHISPER_CT2_MODEL", "large-v3"),
		ComputeType: envOrDefault("WHISPER_CT2_COMPUTE_TYPE", "float16"),
	}
	gpuReq := ResolveWhisperGPURequest(cfg)
	gpuReq.Kind = "whisper-ct2[analysis]"
	// Background work (MM94): never ahead of a user's job, dropped after
	// ANALYSIS_GPU_MAX_WAIT_SECONDS.
	gpuReq.Priority = GPUPriorityBackground
	gpuReq.MaxWait = q.gpuMaxWait()
	acquire := q.acquire
	if acquire == nil {
		acquire = acquireGPUForJob
	}
	gpuIdx, releaseGPU, gpuErr := acquire(ctx, nil, "", gpuReq)
	if gpuErr != nil {
		return "", fmt.Errorf("acquire GPU for whisper: %w", gpuErr)
	}
	defer releaseGPU()
	if gpuIdx >= 0 {
		args = append(args, "--device_index", strconv.Itoa(gpuIdx))
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

	// Use the shared whisper runner so HF_HUB_OFFLINE / TRANSFORMERS_OFFLINE
	// apply here too and we capture both stdout and stderr for diagnostics.
	stdout, stderr, err := runWhisperCommand(ctx, bin, args...)
	if err != nil {
		return "", fmt.Errorf("whisper-ct2: %w\nstderr_tail: %s\nstdout_tail: %s",
			err, tail(stderr, 4000), tail(stdout, 2000))
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
	interval := envInt("OLLAMA_VLM_FRAME_INTERVAL_SECONDS", 10)
	maxWidth := envInt("OLLAMA_VLM_MAX_WIDTH", 768)
	maxFrames := envInt("OLLAMA_VLM_MAX_FRAMES", 24)
	// Shared sampler (vlm.go, MM74); the ffmpeg invocation is unchanged.
	frames, err := extractIntervalFrames(ctx, job.InputPath, framesDir, interval, maxWidth, maxFrames)
	if err != nil {
		return framesDir, nil, err
	}
	return framesDir, FramePaths(frames), nil
}

func (q *AnalysisQueue) analyzeFrameBatches(ctx context.Context, frames []string, transcriptPath string) ([]any, error) {
	batchSize := envInt("OLLAMA_VLM_BATCH_SIZE", 4)
	if batchSize <= 0 {
		batchSize = 4
	}
	transcript := readOptionalText(transcriptPath, 8000)
	var out []any
	n := (len(frames) + batchSize - 1) / batchSize
	// One slot for the whole video: the model stays loaded between batches
	// and is unloaded by the last call (or by the batch on an early return).
	err := q.withOllamaSlot(ctx, "analysis-frames", vlmModel(), func(b *ollamaBatch) error {
		for i := 0; i < n; i++ {
			start := i * batchSize
			end := start + batchSize
			if end > len(frames) {
				end = len(frames)
			}
			prompt := "Analyze these sampled video frames for internal product intelligence. Return concise JSON with scene progression, visual quality, likely editing intent, notable objects/text, and issues."
			if transcript != "" {
				prompt += " Transcript context: " + transcript
			}
			res, err := q.chatImages(ctx, frames[start:end], prompt, b.keepAlive(i, n))
			if err != nil {
				return err
			}
			out = append(out, res)
		}
		return nil
	})
	return out, err
}

// vlmModel is the analysis vision model (OLLAMA_VLM_MODEL).
func vlmModel() string { return envOrDefault("OLLAMA_VLM_MODEL", defaultVLMModel) }

// gpuMaxWait is how long a background analysis step waits for a GPU slot
// before the analysis is dropped (ANALYSIS_GPU_MAX_WAIT_SECONDS, default 120).
func (q *AnalysisQueue) gpuMaxWait() time.Duration {
	if q.cfg != nil && q.cfg.AnalysisGPUMaxWait > 0 {
		return q.cfg.AnalysisGPUMaxWait
	}
	return time.Duration(envInt("ANALYSIS_GPU_MAX_WAIT_SECONDS", 120)) * time.Second
}

// withOllamaSlot runs fn under a background-priority slot on the OLLAMA_URL
// daemon's card (MM94). fn sends keep_alive from the batch; whatever fn does,
// the model is unloaded before the slot is released.
func (q *AnalysisQueue) withOllamaSlot(ctx context.Context, caller, model string, fn func(b *ollamaBatch) error) error {
	client, baseURL := analysisOllamaClient()
	slot := ollamaSlot{
		Caller:   caller,
		BaseURL:  baseURL,
		Model:    model,
		Priority: GPUPriorityBackground,
		MaxWait:  q.gpuMaxWait(),
		Acquire:  q.acquire,
	}
	return withOllamaSlot(ctx, slot, func() error {
		b := &ollamaBatch{client: client, baseURL: baseURL, model: model}
		defer b.finish()
		return fn(b)
	})
}

// analyzeImages is one VLM call under its own GPU slot.
func (q *AnalysisQueue) analyzeImages(ctx context.Context, paths []string, prompt string) (any, error) {
	var out any
	err := q.withOllamaSlot(ctx, "analysis-image", vlmModel(), func(b *ollamaBatch) error {
		var err error
		out, err = q.chatImages(ctx, paths, prompt, b.keepAlive(0, 1))
		return err
	})
	return out, err
}

// chatImages sends one VLM request. The caller holds the GPU slot.
func (q *AnalysisQueue) chatImages(ctx context.Context, paths []string, prompt, keepAlive string) (any, error) {
	images, err := encodeImagesBase64(paths)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model":  vlmModel(),
		"stream": false,
		"messages": []map[string]any{{
			"role":    "user",
			"content": prompt,
			"images":  images,
		}},
		"format":     "json",
		"keep_alive": keepAlive,
	}
	// Shared Ollama call (vlm.go, MM74); the raw response is kept as before.
	client, baseURL := analysisOllamaClient()
	return ollamaChat(ctx, client, baseURL, payload)
}

func (q *AnalysisQueue) analyzeTranscript(ctx context.Context, transcript, language string) (any, error) {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return map[string]any{
			"summary":          "Transcript is empty; no analysis available.",
			"extended_summary": "",
			"main_topics":      []string{},
			"audience_level":   "unspecified",
			"language":         language,
			"sentiment_label":  "neutral",
			"sentiment_score":  0,
			"content_safety": map[string]any{
				"rating":   "safe",
				"labels":   []string{},
				"concerns": "Empty transcript — nothing to assess.",
			},
			"harmful_content":         false,
			"harmful_content_reasons": []string{},
		}, nil
	}
	prompt := buildTranscriptAnalysisPrompt(transcript, language)
	model := envOrDefault("OLLAMA_TEXT_MODEL", vlmModel())
	var response map[string]any
	err := q.withOllamaSlot(ctx, "analysis-transcript", model, func(b *ollamaBatch) error {
		payload := map[string]any{
			"model":      model,
			"stream":     false,
			"messages":   []map[string]any{{"role": "user", "content": prompt}},
			"format":     "json",
			"keep_alive": b.keepAlive(0, 1),
		}
		client, baseURL := analysisOllamaClient()
		var err error
		response, err = ollamaChat(ctx, client, baseURL, payload)
		return err
	})
	if err != nil {
		return nil, err
	}
	if message, ok := response["message"].(map[string]any); ok {
		if content, ok := message["content"].(string); ok && strings.TrimSpace(content) != "" {
			trimmed := strings.TrimSpace(content)
			var parsed map[string]any
			if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
				return parsed, nil
			}
			return map[string]any{"raw": trimmed}, nil
		}
	}
	return response, nil
}

func (q *AnalysisQueue) describeSilentMedia(ctx context.Context, job AnalysisJob) (any, error) {
	switch job.FileType {
	case models.FileTypeVideo:
		framesDir, frames, err := q.extractFrames(ctx, job)
		if err != nil {
			return map[string]any{
				"audio_description": job.AudioDescription,
				"frames_error":      err.Error(),
			}, nil
		}
		if len(frames) == 0 {
			return map[string]any{"audio_description": job.AudioDescription, "frames_dir": framesDir}, nil
		}
		prompt := "This media has no recognizable speech. Describe what is shown across these frames in 2-3 concise sentences. Return strict JSON with fields: visual_description, likely_content_type (music_video|silent_video|ambient|other), notable_objects, harmful_content (boolean), harmful_content_reasons."
		batch, err := q.analyzeImages(ctx, frames, prompt)
		if err != nil {
			return map[string]any{"audio_description": job.AudioDescription, "frames_dir": framesDir, "ollama_error": err.Error()}, nil
		}
		return map[string]any{
			"audio_description": job.AudioDescription,
			"frames_dir":        framesDir,
			"visual_summary":    batch,
		}, nil
	case models.FileTypeAudio:
		return map[string]any{
			"audio_description":   job.AudioDescription,
			"likely_content_type": "music_or_ambient_or_silent",
		}, nil
	default:
		return map[string]any{"audio_description": job.AudioDescription}, nil
	}
}

func buildTranscriptAnalysisPrompt(transcript, language string) string {
	if language == "" {
		language = "unknown"
	}
	var b strings.Builder
	b.WriteString("You are analyzing a media transcript for internal product review. ")
	b.WriteString("Return STRICT JSON only, with the following fields: ")
	b.WriteString("summary (1-2 sentence summary), ")
	b.WriteString("extended_summary (3-5 sentence summary), ")
	b.WriteString("main_topics (string array, up to 5 items), ")
	b.WriteString("keywords (string array, up to 10 items), ")
	b.WriteString("audience_level (one of: beginner, intermediate, advanced, expert, all, unspecified), ")
	b.WriteString("language (BCP-47 language code, prefer the transcript's actual language), ")
	b.WriteString("sentiment_label (positive | negative | neutral | mixed), ")
	b.WriteString("sentiment_score (number between -1 and 1), ")
	b.WriteString("content_safety { rating (safe|moderate|unsafe), labels (string array), concerns (string) }, ")
	b.WriteString("harmful_content (boolean), ")
	b.WriteString("harmful_content_reasons (string array). ")
	b.WriteString("Detected language: ")
	b.WriteString(language)
	b.WriteString(". Transcript:\n\n")
	if len(transcript) > 16000 {
		transcript = transcript[:16000] + "\n...[truncated]"
	}
	b.WriteString(transcript)
	return b.String()
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
