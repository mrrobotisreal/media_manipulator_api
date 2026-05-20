package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

const (
	transcribeFormatVTT  = "vtt"
	transcribeFormatTXT  = "txt"
	transcribeFormatJSON = "json"

	defaultWhisperCT2Bin         = "/opt/creatv/whisper-ct2/bin/whisper-ctranslate2"
	defaultWhisperCT2Model       = "medium"
	defaultWhisperCT2Device      = "cuda"
	defaultWhisperCT2ComputeType = "float16"
	defaultWhisperCT2OutputDir   = "/tmp/ct2_out"

	transcribeStepStart      = 5
	transcribeStepDetect     = 12
	transcribeStepExtract    = 25
	transcribeStepTranscribe = 70
	transcribeStepFormatted  = 90
	transcribeStepDone       = 100

	defaultTranscribeLanguage = "en"
)

var (
	whisperPermitOnce sync.Once
	whisperPermitPool chan struct{}
)

// TranscriptionService runs the whisper-ctranslate2 transcription pipeline,
// produces transcript artifacts in the requested format, and enqueues a
// follow-up analysis job for downstream summarization and safety review.
type TranscriptionService struct {
	cfg        *config.Config
	inspector  *MediaInspector
	jobManager *JobManager
	analysis   *AnalysisQueue
}

func NewTranscriptionService(cfg *config.Config, inspector *MediaInspector, jm *JobManager, aq *AnalysisQueue) *TranscriptionService {
	return &TranscriptionService{cfg: cfg, inspector: inspector, jobManager: jm, analysis: aq}
}

// TranscribeOptions captures the user-facing controls for a transcription job.
type TranscribeOptions struct {
	Format   string `json:"format"`
	Language string `json:"language,omitempty"`
}

// TranscribeResult is written to transcribe_result.json and surfaced to the UI.
type TranscribeResult struct {
	Format            string                 `json:"format"`
	Language          string                 `json:"language,omitempty"`
	HasAudio          bool                   `json:"hasAudio"`
	HasSpeech         bool                   `json:"hasSpeech"`
	SegmentCount      int                    `json:"segmentCount"`
	DurationSeconds   float64                `json:"durationSeconds,omitempty"`
	TranscriptText    string                 `json:"transcriptText,omitempty"`
	AudioDescription  string                 `json:"audioDescription,omitempty"`
	Message           string                 `json:"message,omitempty"`
	OutputFile        string                 `json:"outputFile"`
	Segments          []TranscribeSegmentDTO `json:"segments,omitempty"`
	StartedAt         time.Time              `json:"startedAt"`
	CompletedAt       time.Time              `json:"completedAt"`
	AudioMetadata     map[string]any         `json:"audioMetadata,omitempty"`
	AnalysisEnqueued  bool                   `json:"analysisEnqueued"`
	AnalysisStatusURL string                 `json:"analysisStatusUrl,omitempty"`
}

// TranscribeSegmentDTO represents a single caption segment in a serializable form.
type TranscribeSegmentDTO struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// WhisperCT2Config mirrors the configuration used by the transcoding project so
// that the same model + GPU server can be reused without further coordination.
type WhisperCT2Config struct {
	Bin         string
	Model       string
	Device      string
	DeviceIndex *int
	ComputeType string
	VADFilter   bool
	Batched     bool
	BatchSize   *int
	Language    string
	OutputDir   string
}

type whisperSegment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type whisperResponse struct {
	Text     string           `json:"text"`
	Language string           `json:"language"`
	Segments []whisperSegment `json:"segments"`
}

type whisperCT2Number float64

func (n *whisperCT2Number) UnmarshalJSON(data []byte) error {
	raw := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if raw == "" || raw == "null" {
		*n = 0
		return nil
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return err
	}
	*n = whisperCT2Number(parsed)
	return nil
}

// Transcribe runs the full transcription pipeline against inputPath and writes
// the user-facing transcript artifact to outputPath (a path that already
// includes the requested .vtt/.txt/.json extension).
func (s *TranscriptionService) Transcribe(ctx context.Context, job *models.ConversionJob, inputPath, outputPath string, opts TranscribeOptions) (*TranscribeResult, error) {
	format := normalizeTranscribeFormat(opts.Format)
	if format == "" {
		return nil, fmt.Errorf("unsupported transcribe format: %s", opts.Format)
	}

	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	result := &TranscribeResult{
		Format:     format,
		OutputFile: filepath.Base(outputPath),
		StartedAt:  time.Now().UTC(),
		Language:   defaultTranscribeLanguage,
	}

	s.progress(job.ID, transcribeStepStart)

	fileType := models.GetFileType(job.OriginalFile.Type)
	duration, _ := probeMediaDurationSeconds(ctx, inputPath)
	result.DurationSeconds = duration
	result.AudioMetadata = collectAudioMetadata(ctx, inputPath)

	hasAudio := true
	if fileType == models.FileTypeVideo {
		hasAudio = s.inspector.HasAudioStream(ctx, inputPath)
	}
	result.HasAudio = hasAudio
	s.progress(job.ID, transcribeStepDetect)

	if !hasAudio {
		result.Message = "No audio stream detected — there is nothing to transcribe."
		result.AudioDescription = describeMediaForUI(fileType, false, duration, result.AudioMetadata)
		if err := writeTranscriptOutput(outputPath, format, result, nil); err != nil {
			return nil, err
		}
		result.CompletedAt = time.Now().UTC()
		s.persistResult(outputDir, result)
		s.scheduleAnalysis(job, inputPath, outputDir, fileType, "", result.Language, "no_audio", result.AudioDescription)
		result.AnalysisEnqueued = s.analysis != nil
		s.progress(job.ID, transcribeStepDone)
		return result, nil
	}

	audioPath, cleanupAudio, err := extractAudioForTranscribe(ctx, inputPath, outputDir)
	if err != nil {
		return nil, fmt.Errorf("extract audio: %w", err)
	}
	defer cleanupAudio()
	s.progress(job.ID, transcribeStepExtract)

	whisperCfg := loadWhisperCT2ConfigFromEnv()
	if strings.TrimSpace(opts.Language) != "" {
		whisperCfg.Language = strings.TrimSpace(opts.Language)
	}

	// Pre-flight GPU selection through the shared scheduler. The scheduler
	// queries nvidia-smi for current free VRAM on every card, picks one that
	// fits this model's estimated need (with safety margin), and parks the
	// goroutine on a condvar if every card is saturated. The chosen index is
	// what we pass to whisper-ctranslate2 via --device_index. Falls through
	// to (-1, no-op release, nil) on CPU-only hosts so the rest of the flow
	// is unchanged there.
	gpuReq := ResolveWhisperGPURequest(whisperCfg)
	gpuIdx, releaseGPU, err := SharedGPUScheduler().Acquire(ctx, gpuReq)
	if err != nil {
		return nil, fmt.Errorf("acquire GPU for whisper: %w", err)
	}
	defer releaseGPU()
	if gpuIdx >= 0 {
		whisperCfg.DeviceIndex = &gpuIdx
	}

	// We also keep the existing global concurrency permit as a belt-and-
	// suspenders cap. With GPU_SCHED_MAX_JOBS_PER_GPU=1 the scheduler already
	// serializes per-card; the permit pool's WHISPER_GPU_CONCURRENCY default
	// of 1 caps total concurrent whisper procs across all GPUs.
	release, err := acquireWhisperPermit(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire whisper permit: %w", err)
	}
	defer release()

	whisperData, err := callWhisperCT2(ctx, whisperCfg, audioPath, outputDir)
	if err != nil {
		return nil, fmt.Errorf("whisper-ct2: %w", err)
	}
	s.progress(job.ID, transcribeStepTranscribe)

	transcript := strings.TrimSpace(whisperData.Text)
	result.Language = normalizeLanguageCode(whisperData.Language)
	result.SegmentCount = len(whisperData.Segments)
	result.TranscriptText = transcript
	result.Segments = buildSegmentDTOs(whisperData.Segments)

	if transcript == "" && len(whisperData.Segments) == 0 {
		result.HasSpeech = false
		result.Message = "Audio was detected but no recognizable speech was found."
		result.AudioDescription = describeMediaForUI(fileType, true, duration, result.AudioMetadata)
	} else {
		result.HasSpeech = true
	}

	if err := writeTranscriptOutput(outputPath, format, result, whisperData.Segments); err != nil {
		return nil, err
	}
	s.progress(job.ID, transcribeStepFormatted)

	result.CompletedAt = time.Now().UTC()
	s.persistResult(outputDir, result)

	mode := "transcript"
	if !result.HasSpeech {
		mode = "no_speech"
	}
	s.scheduleAnalysis(job, inputPath, outputDir, fileType, transcript, result.Language, mode, result.AudioDescription)
	result.AnalysisEnqueued = s.analysis != nil

	s.progress(job.ID, transcribeStepDone)
	return result, nil
}

func (s *TranscriptionService) progress(jobID string, percent int) {
	if s.jobManager == nil {
		return
	}
	s.jobManager.SendProgressUpdate(jobID, percent)
}

func (s *TranscriptionService) persistResult(outputDir string, result *TranscribeResult) {
	_ = writeJSON(filepath.Join(outputDir, "transcribe_result.json"), result)
}

func (s *TranscriptionService) scheduleAnalysis(job *models.ConversionJob, inputPath, outputDir string, fileType models.FileType, transcript, language, mode, audioDescription string) {
	if s.analysis == nil {
		return
	}
	s.analysis.Enqueue(AnalysisJob{
		JobID:            job.ID,
		InputPath:        inputPath,
		OutputDir:        outputDir,
		FileType:         fileType,
		MimeType:         job.OriginalFile.Type,
		Mode:             mode,
		Transcript:       transcript,
		Language:         language,
		AudioDescription: audioDescription,
	})
}

func normalizeTranscribeFormat(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case transcribeFormatVTT:
		return transcribeFormatVTT
	case transcribeFormatTXT, "text", "plain":
		return transcribeFormatTXT
	case transcribeFormatJSON:
		return transcribeFormatJSON
	default:
		return ""
	}
}

func buildSegmentDTOs(segments []whisperSegment) []TranscribeSegmentDTO {
	out := make([]TranscribeSegmentDTO, 0, len(segments))
	for idx, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		end := seg.End
		if end < seg.Start {
			end = seg.Start
		}
		out = append(out, TranscribeSegmentDTO{
			ID:    idx,
			Start: roundMilli(seg.Start),
			End:   roundMilli(end),
			Text:  text,
		})
	}
	return out
}

func writeTranscriptOutput(outputPath, format string, result *TranscribeResult, segments []whisperSegment) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return err
	}
	switch format {
	case transcribeFormatVTT:
		return os.WriteFile(outputPath, []byte(buildVTT(result, segments)), 0o644)
	case transcribeFormatTXT:
		text := strings.TrimSpace(result.TranscriptText)
		if text == "" {
			text = fallbackTranscriptMessage(result)
		}
		return os.WriteFile(outputPath, []byte(text+"\n"), 0o644)
	case transcribeFormatJSON:
		payload := transcribeJSONPayload(result, segments)
		body, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(outputPath, body, 0o644)
	}
	return fmt.Errorf("unsupported transcribe format: %s", format)
}

func fallbackTranscriptMessage(result *TranscribeResult) string {
	msg := strings.TrimSpace(result.Message)
	if msg == "" {
		if result.HasAudio {
			msg = "No recognizable speech was found in the audio."
		} else {
			msg = "No audio stream was found in this file."
		}
	}
	if result.AudioDescription != "" {
		msg = msg + "\n\n" + result.AudioDescription
	}
	return msg
}

func transcribeJSONPayload(result *TranscribeResult, segments []whisperSegment) map[string]any {
	out := make([]map[string]any, 0, len(segments))
	for idx, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		out = append(out, map[string]any{
			"id":    idx,
			"start": roundMilli(seg.Start),
			"end":   roundMilli(seg.End),
			"text":  text,
		})
	}
	return map[string]any{
		"language":         result.Language,
		"hasAudio":         result.HasAudio,
		"hasSpeech":        result.HasSpeech,
		"transcript":       result.TranscriptText,
		"segments":         out,
		"segmentCount":     len(out),
		"durationSeconds":  result.DurationSeconds,
		"message":          result.Message,
		"audioDescription": result.AudioDescription,
	}
}

func buildVTT(result *TranscribeResult, segments []whisperSegment) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	if len(segments) == 0 {
		message := strings.TrimSpace(result.TranscriptText)
		if message == "" {
			message = strings.TrimSpace(result.Message)
		}
		if message == "" {
			message = strings.TrimSpace(result.AudioDescription)
		}
		if message == "" {
			return b.String()
		}
		end := result.DurationSeconds
		if end <= 0 {
			end = math.Max(5, float64(len(message))/15)
		}
		b.WriteString("1\n")
		b.WriteString(fmt.Sprintf("%s --> %s\n", formatVTTStamp(0), formatVTTStamp(end)))
		b.WriteString(message)
		b.WriteString("\n\n")
		return b.String()
	}
	sortable := append([]whisperSegment{}, segments...)
	sort.SliceStable(sortable, func(i, j int) bool { return sortable[i].Start < sortable[j].Start })
	cueIndex := 1
	for _, seg := range sortable {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		end := seg.End
		if end <= seg.Start {
			end = seg.Start + 0.5
		}
		b.WriteString(strconv.Itoa(cueIndex))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("%s --> %s\n", formatVTTStamp(seg.Start), formatVTTStamp(end)))
		b.WriteString(text)
		b.WriteString("\n\n")
		cueIndex++
	}
	return b.String()
}

func formatVTTStamp(seconds float64) string {
	if seconds < 0 || math.IsNaN(seconds) {
		seconds = 0
	}
	totalMS := int64(math.Round(seconds * 1000))
	hours := totalMS / 3600000
	minutes := (totalMS % 3600000) / 60000
	secs := (totalMS % 60000) / 1000
	millis := totalMS % 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, secs, millis)
}

func roundMilli(seconds float64) float64 {
	if math.IsNaN(seconds) {
		return 0
	}
	return math.Round(seconds*1000) / 1000
}

func normalizeLanguageCode(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return defaultTranscribeLanguage
	}
	if len(raw) > 10 {
		raw = raw[:10]
	}
	return raw
}

// whisperEnvLogOnce ensures we only print the resolved env summary once per
// process lifetime, no matter how many transcription / caption jobs run.
var whisperEnvLogOnce sync.Once

// whisperSubprocessEnv returns the environment slice we hand the
// whisper-ctranslate2 subprocess.
//
// Two responsibilities:
//
//  1. Make sure HF_HOME points at the operator's actual model cache. The
//     whisper subprocess needs to know where Systran/faster-whisper-<model>/
//     lives on disk. Resolution order:
//
//     a) WHISPER_HF_CACHE_DIR (explicit, takes precedence over everything
//     — set this in the API env when you can't easily change HF_HOME
//     via your launcher),
//     b) HF_HOME inherited from the API process env (works automatically
//     when launched from a shell that has it set, or via systemd
//     Environment=HF_HOME=...),
//     c) nothing — we log a loud warning at first use because the
//     subprocess will fall back to ~/.cache/huggingface, which is
//     almost never where a system daemon's models live.
//
//  2. Disable HuggingFace network calls so the subprocess uses ONLY the
//     local cache. Set HF_HUB_OFFLINE=1 + TRANSFORMERS_OFFLINE=1. Escape
//     hatch: WHISPER_ALLOW_HF_NETWORK=1 in the API env disables the offline
//     flags, useful for the rare case of pulling a new model onto a fresh
//     host.
//
// Duplicate keys in cmd.Env follow last-wins semantics, so appending our
// overrides at the end of os.Environ() correctly supersedes any inherited
// values.
func whisperSubprocessEnv() []string {
	env := os.Environ()

	// HF_HOME resolution.
	inheritedHFHome := strings.TrimSpace(os.Getenv("HF_HOME"))
	overrideHFHome := strings.TrimSpace(os.Getenv("WHISPER_HF_CACHE_DIR"))
	resolvedHFHome := inheritedHFHome
	if overrideHFHome != "" {
		resolvedHFHome = overrideHFHome
		env = append(env, "HF_HOME="+overrideHFHome)
	}

	whisperEnvLogOnce.Do(func() {
		switch {
		case resolvedHFHome == "":
			log.Printf("whisper-ct2: WARNING — neither HF_HOME nor WHISPER_HF_CACHE_DIR is set in the API process environment. The whisper subprocess will fall back to ~/.cache/huggingface, which usually does NOT contain your downloaded models when the API runs as a system daemon. Set HF_HOME (or WHISPER_HF_CACHE_DIR) to the parent of your `hub/` cache directory — e.g. HF_HOME=/var/lib/creatv/hf when models live at /var/lib/creatv/hf/hub/models--Systran--faster-whisper-medium.")
		case overrideHFHome != "" && inheritedHFHome != "" && overrideHFHome != inheritedHFHome:
			log.Printf("whisper-ct2: WHISPER_HF_CACHE_DIR=%q overrides inherited HF_HOME=%q for the subprocess", overrideHFHome, inheritedHFHome)
		default:
			log.Printf("whisper-ct2: model cache HF_HOME=%q", resolvedHFHome)
		}
	})

	// Lock CUDA to PCI bus order. Without this CUDA defaults to
	// FASTEST_FIRST, which on multi-GPU hosts can shuffle device numbers
	// relative to what nvidia-smi shows. We want the device_index we pass
	// to whisper to map to the same physical card every operator sees in
	// `nvidia-smi -L`.
	env = append(env, "CUDA_DEVICE_ORDER=PCI_BUS_ID")

	if envBool("WHISPER_ALLOW_HF_NETWORK", false) {
		return env
	}
	return append(env,
		"HF_HUB_OFFLINE=1",
		"TRANSFORMERS_OFFLINE=1",
		"HF_HUB_DISABLE_TELEMETRY=1",
	)
}

// runWhisperCommand executes the whisper-ctranslate2 binary with the offline
// HF env applied and captures both stdout and stderr separately so callers
// can include both tails in error messages. The previous implementation
// discarded stdout entirely and truncated stderr to 2000 bytes, which made
// real failures invisible when huggingface_hub emitted a long preamble.
func runWhisperCommand(ctx context.Context, bin string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = whisperSubprocessEnv()
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return outBuf.String(), errBuf.String(), ctx.Err()
	}
	return outBuf.String(), errBuf.String(), runErr
}

// ResolveWhisperCT2Bin returns a usable path to the whisper-ctranslate2
// binary. It tries (in order):
//
//  1. `preferred` (typically `cfg.Bin` derived from WHISPER_CT2_BIN env)
//  2. defaultWhisperCT2Bin compiled in at /opt/creatv/whisper-ct2/bin/whisper-ctranslate2
//  3. exec.LookPath("whisper-ctranslate2") — pure $PATH search
//
// The resolver was added after an incident where a stale WHISPER_CT2_BIN
// pointing at /usr/local/bin/whisper-ctranslate2 (left over from a previous
// deployment) silently killed every transcription / caption job even though
// the binary was still installed at the default location. Now the API
// transparently falls back AND prints a one-line warning identifying the
// stale env var so the operator can find and remove it.
//
// Returns an error only when none of the three candidates resolves — at that
// point there is genuinely no whisper binary on the host.
func ResolveWhisperCT2Bin(preferred string) (string, error) {
	preferred = strings.TrimSpace(preferred)
	candidates := []string{}
	if preferred != "" {
		if isExecutableFile(preferred) {
			return preferred, nil
		}
		candidates = append(candidates, preferred)
	}
	if preferred != defaultWhisperCT2Bin {
		if isExecutableFile(defaultWhisperCT2Bin) {
			if preferred != "" {
				log.Printf("whisper-ct2: WHISPER_CT2_BIN=%q is not an executable file; falling back to %q. Fix or unset the env var to silence this warning.",
					preferred, defaultWhisperCT2Bin)
			}
			return defaultWhisperCT2Bin, nil
		}
		candidates = append(candidates, defaultWhisperCT2Bin)
	}
	if path, err := exec.LookPath("whisper-ctranslate2"); err == nil {
		log.Printf("whisper-ct2: configured paths (%s) not present; resolved via $PATH at %q", strings.Join(candidates, ", "), path)
		return path, nil
	}
	return "", fmt.Errorf("whisper-ctranslate2 binary not found (tried: %s; also not on $PATH). Set WHISPER_CT2_BIN to a valid path or install the binary at %s.",
		strings.Join(candidates, ", "), defaultWhisperCT2Bin)
}

// isExecutableFile reports whether the given path refers to a regular file
// (not a directory or device) that has at least one execute bit set. On
// Windows the execute-bit check is moot but a stat hit is still useful.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// loadWhisperCT2ConfigFromEnv mirrors the transcoding project's config loader so
// that the same environment variables continue to control the GPU pipeline.
func loadWhisperCT2ConfigFromEnv() WhisperCT2Config {
	cfg := WhisperCT2Config{
		Bin:         envOrDefault("WHISPER_CT2_BIN", defaultWhisperCT2Bin),
		Model:       envOrDefault("WHISPER_CT2_MODEL", defaultWhisperCT2Model),
		Device:      envOrDefault("WHISPER_CT2_DEVICE", defaultWhisperCT2Device),
		ComputeType: envOrDefault("WHISPER_CT2_COMPUTE_TYPE", defaultWhisperCT2ComputeType),
		VADFilter:   envBool("WHISPER_CT2_VAD_FILTER", true),
		Batched:     envBool("WHISPER_CT2_BATCHED", false),
		Language:    strings.TrimSpace(os.Getenv("WHISPER_CT2_LANGUAGE")),
		OutputDir:   envOrDefault("WHISPER_CT2_OUTPUT_DIR", defaultWhisperCT2OutputDir),
	}
	// NOTE: cfg.DeviceIndex is intentionally NOT set here. The shared GPU
	// scheduler does pre-flight nvidia-smi probing + queueing at job-start
	// time and sets the index based on live free-VRAM, the operator's
	// WHISPER_CT2_DEVICE_INDEX preference list, and queue depth. See
	// SharedGPUScheduler().Acquire in Transcribe (transcribe.go).
	if bs := strings.TrimSpace(os.Getenv("WHISPER_CT2_BATCH_SIZE")); bs != "" {
		if parsed, err := strconv.Atoi(bs); err == nil && parsed > 0 {
			cfg.BatchSize = &parsed
		}
	}
	return cfg
}

func extractAudioForTranscribe(ctx context.Context, inputPath, outputDir string) (string, func(), error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", func() {}, errors.New("ffmpeg not found in PATH")
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", func() {}, err
	}
	audioPath := filepath.Join(outputDir, "transcribe_audio.wav")
	_, stderr, err := runCommand(ctx, "ffmpeg", "-y", "-i", inputPath, "-vn", "-acodec", "pcm_s16le", "-ar", "16000", "-ac", "1", audioPath)
	if err != nil {
		_ = os.Remove(audioPath)
		return "", func() {}, fmt.Errorf("ffmpeg extract audio failed: %w: %s", err, tail(stderr, 1500))
	}
	return audioPath, func() { _ = os.Remove(audioPath) }, nil
}

func callWhisperCT2(ctx context.Context, cfg WhisperCT2Config, audioPath, parentOutputDir string) (whisperResponse, error) {
	if strings.TrimSpace(audioPath) == "" {
		return whisperResponse{}, errors.New("audio path is required")
	}
	resolvedBin, err := ResolveWhisperCT2Bin(cfg.Bin)
	if err != nil {
		return whisperResponse{}, err
	}
	cfg.Bin = resolvedBin
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return whisperResponse{}, fmt.Errorf("create whisper output dir %s: %w", cfg.OutputDir, err)
	}
	runOutputDir, err := os.MkdirTemp(cfg.OutputDir, "mm-whisper-ct2-")
	if err != nil {
		return whisperResponse{}, fmt.Errorf("create whisper run output dir: %w", err)
	}
	defer os.RemoveAll(runOutputDir)

	args := []string{audioPath, "--model", cfg.Model}
	if strings.TrimSpace(cfg.Device) != "" {
		args = append(args, "--device", cfg.Device)
	}
	if cfg.DeviceIndex != nil {
		args = append(args, "--device_index", strconv.Itoa(*cfg.DeviceIndex))
	}
	if strings.TrimSpace(cfg.ComputeType) != "" {
		args = append(args, "--compute_type", cfg.ComputeType)
	}
	args = append(args, "--vad_filter", whisperCT2Bool(cfg.VADFilter))
	if cfg.Batched {
		args = append(args, "--batched", "True")
	}
	if cfg.BatchSize != nil {
		args = append(args, "--batch_size", strconv.Itoa(*cfg.BatchSize))
	}
	if strings.TrimSpace(cfg.Language) != "" {
		args = append(args, "--language", cfg.Language)
	}
	args = append(args, "--output_format", "json", "--output_dir", runOutputDir)

	stdout, stderr, err := runWhisperCommand(ctx, cfg.Bin, args...)
	if err != nil {
		hfHome := strings.TrimSpace(os.Getenv("WHISPER_HF_CACHE_DIR"))
		if hfHome == "" {
			hfHome = strings.TrimSpace(os.Getenv("HF_HOME"))
		}
		deviceIdx := "(unset)"
		if cfg.DeviceIndex != nil {
			deviceIdx = strconv.Itoa(*cfg.DeviceIndex)
		}
		return whisperResponse{}, fmt.Errorf(
			"whisper-ctranslate2 failed: %w\nHF_HOME(resolved)=%q model=%q device=%s device_index=%s\nstderr_tail: %s\nstdout_tail: %s",
			err, hfHome, cfg.Model, cfg.Device, deviceIdx, tail(stderr, 4000), tail(stdout, 2000),
		)
	}

	jsonPath, err := locateWhisperCT2JSON(runOutputDir, audioPath)
	if err != nil {
		return whisperResponse{}, err
	}
	payload, err := os.ReadFile(jsonPath)
	if err != nil {
		return whisperResponse{}, fmt.Errorf("read whisper output json %s: %w", jsonPath, err)
	}
	return parseWhisperCT2Response(payload)
}

func parseWhisperCT2Response(payload []byte) (whisperResponse, error) {
	var parsed struct {
		Text     string `json:"text"`
		Language string `json:"language"`
		Segments []struct {
			Start whisperCT2Number `json:"start"`
			End   whisperCT2Number `json:"end"`
			Text  string           `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return whisperResponse{}, fmt.Errorf("parse whisper-ctranslate2 json: %w", err)
	}
	resp := whisperResponse{
		Text:     strings.TrimSpace(parsed.Text),
		Language: strings.TrimSpace(parsed.Language),
		Segments: make([]whisperSegment, 0, len(parsed.Segments)),
	}
	parts := make([]string, 0, len(parsed.Segments))
	for idx, seg := range parsed.Segments {
		text := strings.TrimSpace(seg.Text)
		start := float64(seg.Start)
		end := float64(seg.End)
		if end < start {
			end = start
		}
		resp.Segments = append(resp.Segments, whisperSegment{ID: idx, Start: start, End: end, Text: text})
		if text != "" {
			parts = append(parts, text)
		}
	}
	if resp.Text == "" {
		resp.Text = strings.TrimSpace(strings.Join(parts, " "))
	}
	if resp.Language == "" {
		resp.Language = defaultTranscribeLanguage
	}
	return resp, nil
}

func locateWhisperCT2JSON(outputDir, audioPath string) (string, error) {
	audioBase := strings.TrimSuffix(filepath.Base(audioPath), filepath.Ext(audioPath))
	expected := filepath.Join(outputDir, audioBase+".json")
	if stat, err := os.Stat(expected); err == nil && !stat.IsDir() {
		return expected, nil
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return "", fmt.Errorf("read whisper output dir %s: %w", outputDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			return filepath.Join(outputDir, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("whisper-ctranslate2 produced no JSON output in %s", outputDir)
}

func whisperCT2Bool(v bool) string {
	if v {
		return "True"
	}
	return "False"
}

func acquireWhisperPermit(ctx context.Context) (func(), error) {
	whisperPermitOnce.Do(initWhisperPermitPool)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-whisperPermitPool:
	}
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() { whisperPermitPool <- struct{}{} })
	}, nil
}

func initWhisperPermitPool() {
	concurrency := envInt("WHISPER_GPU_CONCURRENCY", 1)
	if concurrency < 1 {
		concurrency = 1
	}
	whisperPermitPool = make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		whisperPermitPool <- struct{}{}
	}
}

func probeMediaDurationSeconds(ctx context.Context, path string) (float64, error) {
	stdout, _, err := runCommand(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(stdout), 64)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func collectAudioMetadata(ctx context.Context, path string) map[string]any {
	stdout, _, err := runCommand(ctx, "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name,sample_rate,channels,channel_layout,bit_rate,duration", "-of", "json", path)
	out := map[string]any{}
	if err != nil || strings.TrimSpace(stdout) == "" {
		return out
	}
	var parsed struct {
		Streams []map[string]any `json:"streams"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return out
	}
	if len(parsed.Streams) > 0 {
		out = parsed.Streams[0]
	}
	return out
}

func describeMediaForUI(fileType models.FileType, hasAudio bool, duration float64, metadata map[string]any) string {
	if !hasAudio {
		if fileType == models.FileTypeVideo {
			return formatMediaDescription("This is a video file with no audio track", duration, nil)
		}
		return formatMediaDescription("This file does not contain an audio stream", duration, nil)
	}
	if fileType == models.FileTypeVideo {
		return formatMediaDescription("This is a video whose audio track contains no recognizable speech (it may be music, ambient sound, or silence)", duration, metadata)
	}
	return formatMediaDescription("This is an audio file with no recognizable speech (it may be music, ambient sound, or silence)", duration, metadata)
}

func formatMediaDescription(prefix string, duration float64, metadata map[string]any) string {
	var b strings.Builder
	b.WriteString(prefix)
	if duration > 0 {
		minutes := int(duration) / 60
		seconds := int(duration) % 60
		b.WriteString(fmt.Sprintf(" (approximately %d:%02d", minutes, seconds))
		if metadata != nil {
			if codec, ok := metadata["codec_name"].(string); ok && codec != "" {
				b.WriteString(", codec: ")
				b.WriteString(codec)
			}
			if sampleRate, ok := metadata["sample_rate"].(string); ok && sampleRate != "" {
				b.WriteString(", sample rate: ")
				b.WriteString(sampleRate)
				b.WriteString(" Hz")
			}
			if channels, ok := metadata["channels"].(float64); ok && channels > 0 {
				b.WriteString(fmt.Sprintf(", channels: %d", int(channels)))
			}
		}
		b.WriteString(")")
	}
	b.WriteString(".")
	return b.String()
}
