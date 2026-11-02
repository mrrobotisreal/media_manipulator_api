package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// AIService runs Phase 1 local AI tools (audio + image). Commands route through
// the binaries configured in cfg; the parent Converter is responsible for
// deciding when to invoke this service.
type AIService struct {
	cfg        *config.Config
	jobManager *JobManager

	// acquire and run are injectable for tests: acquire is the shared GPU
	// scheduler (nil = acquireGPUForJob), run executes one subprocess
	// (nil = runAI).
	acquire gpuAcquireFunc
	run     func(ctx context.Context, label string, env []string, name string, args ...string) error
}

func NewAIService(cfg *config.Config) *AIService {
	return &AIService{cfg: cfg}
}

func (a *AIService) SetJobManager(jm *JobManager) {
	a.jobManager = jm
}

// Image operation identifiers.
const (
	AIImageOpFacePrivacy      = "face_privacy"
	AIImageOpRemoveBackground = "remove_background"
	AIImageOpUpscale          = "ai_upscale"
	AIImageOpRedactText       = "redact_text"
	AIImageOpRemoveObject     = "remove_object"

	AIAudioOpCleanVoice      = "clean_voice"
	AIAudioOpRemoveNoise     = "remove_background_noise"
	AIAudioOpIsolateVocals   = "isolate_vocals"
	AIAudioOpRemoveVocals    = "remove_vocals"
	AIAudioDemucsTrackVocals = "vocals.wav"
	AIAudioDemucsTrackNoVox  = "no_vocals.wav"

	AIVideoOpFrameInterpolation = "frame_interpolation"
)

// AIRIFEModels enumerates the rife-ncnn-vulkan model directories we support.
// The first entry is the default when the caller doesn't specify a model.
var validRIFEModels = map[string]bool{
	"rife-v4.6": true,
	"rife-v4":   true,
	"rife-v2.3": true,
}

// validFrameInterpolationQuality enumerates accepted encode quality presets
// the runtime script can pass through to ffmpeg.
var validFrameInterpolationQuality = map[string]bool{
	"low": true, "medium": true, "high": true,
}

// validFrameInterpolationTargetFPS enumerates the FPS targets the v1 UI
// exposes. Other targets would still work in the script but the API rejects
// them so the UI and API stay aligned.
var validFrameInterpolationTargetFPS = map[int]bool{
	48: true, 60: true, 120: true,
}

var validFaceModes = map[string]bool{
	"blur": true, "pixelate": true, "blackbox": true,
}

var validBackgroundModels = map[string]bool{
	"birefnet-general":      true,
	"birefnet-general-lite": true,
	"isnet-general-use":     true,
	"u2net":                 true,
	"u2netp":                true,
	"u2net_human_seg":       true,
	"birefnet-portrait":     true,
}

var validUpscaleModels = map[string]bool{
	"realesrgan-x4plus":       true,
	"realesrgan-x4plus-anime": true,
	"realesr-animevideov3":    true,
}

var validTextDetect = map[string]bool{
	"pii":      true,
	"all-text": true,
}

var validTextRedaction = map[string]bool{
	"blackbox": true,
	"blur":     true,
	"pixelate": true,
}

// AI tool keys for the GPU scheduler (MM94): the slot Kind and the
// AI_VRAM_MIB_<KEY> entry in cfg.AIToolVRAMMiB.
const (
	aiGPUKeyRemoveBackground   = "remove_background"
	aiGPUKeyUpscale            = "ai_upscale"
	aiGPUKeyFacePrivacy        = "face_privacy"
	aiGPUKeyRedactText         = "redact_text"
	aiGPUKeyRemoveObject       = "remove_object"
	aiGPUKeyDeepFilter         = "deepfilter"
	aiGPUKeyDemucs             = "demucs"
	aiGPUKeyFrameInterpolation = "frame_interpolation"
)

// defaultAIToolVRAMMiB is the request for a tool with no configured figure.
const defaultAIToolVRAMMiB = 4096

// withToolGPU runs fn under a GPU slot pinned to card (MM94): the queue waits
// for the card the tool is configured for instead of moving it somewhere its
// environment does not expect. fn receives the card to put in the tool's env
// or argv — card itself, also when the scheduler is disabled — so the slot and
// the subprocess can never disagree. The slot is held only while fn runs.
func (a *AIService) withToolGPU(ctx context.Context, jobID, tool string, card int, fn func(card int) error) error {
	force := card
	req := GPURequest{
		Kind:            tool,
		VRAMRequiredMiB: a.toolVRAM(tool),
		ForceIndex:      &force,
		Priority:        a.jobManager.GPUPriorityOf(jobID),
	}
	return withGPUVia(ctx, a.acquire, a.jobManager, jobID, req, func(idx int) error {
		if idx >= 0 {
			return fn(idx)
		}
		return fn(card)
	})
}

// toolVRAM is the VRAM a tool's slot asks for (AI_VRAM_MIB_<KEY>).
func (a *AIService) toolVRAM(tool string) int64 {
	if a.cfg != nil {
		if v, ok := a.cfg.AIToolVRAMMiB[tool]; ok && v > 0 {
			return v
		}
	}
	return defaultAIToolVRAMMiB
}

// exec runs one subprocess through the injectable runner.
func (a *AIService) exec(ctx context.Context, label string, env []string, name string, args ...string) error {
	if a.run != nil {
		return a.run(ctx, label, env, name, args...)
	}
	return a.runAI(ctx, label, env, name, args...)
}

func (a *AIService) sendProgress(jobID string, progress int) {
	if a.jobManager != nil {
		a.jobManager.SendProgressUpdate(jobID, progress)
	}
}

// runAI executes a command with optional extra env vars merged onto the parent
// environment. Combined stdout/stderr are returned; the error wraps a tail of
// that output so callers surface useful diagnostics to the job log.
func (a *AIService) runAI(ctx context.Context, label string, env []string, name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		// Allow absolute paths that exist but aren't on PATH.
		if _, statErr := os.Stat(name); statErr != nil {
			return fmt.Errorf("%s: %s not found: %v", label, name, err)
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s timed out: %w", label, ctx.Err())
		}
		return fmt.Errorf("%s failed: %v\n%s", label, err, commandTail(combined.String(), 4000))
	}
	return nil
}

// shellQuote single-quotes value safely for inclusion in a bash command line.
// Single quotes are escaped using the '\” pattern so file paths with quotes
// or spaces remain intact.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// cudaEnv pins a CUDA subprocess to one physical card. CUDA_DEVICE_ORDER
// makes the index the nvidia-smi index, which is what the scheduler probes.
func cudaEnv(card int) []string {
	return []string{
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		fmt.Sprintf("CUDA_VISIBLE_DEVICES=%d", card),
		"PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True",
	}
}

// CleanVoice runs DeepFilterNet voice cleanup. When polish is true the output
// is post-processed through a presence/loudness/limiter chain ideal for spoken
// voice; otherwise the denoised signal is re-encoded directly. outputPath
// extension drives the encoder choice.
func (a *AIService) CleanVoice(ctx context.Context, jobID, inputPath, outputPath string, polish bool) error {
	return a.runDeepFilter(ctx, jobID, inputPath, outputPath, polish)
}

// RemoveBackgroundNoise is CleanVoice without the broadcast polish chain. It
// is preferred when callers want raw denoise without color/level changes.
func (a *AIService) RemoveBackgroundNoise(ctx context.Context, jobID, inputPath, outputPath string) error {
	return a.runDeepFilter(ctx, jobID, inputPath, outputPath, false)
}

func (a *AIService) runDeepFilter(ctx context.Context, jobID, inputPath, outputPath string, polish bool) error {
	a.sendProgress(jobID, 10)
	workDir, err := os.MkdirTemp(a.cfg.TempDir, "ai_deepfilter_")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	a.sendProgress(jobID, 25)
	normalizedWav := filepath.Join(workDir, "input_48k_mono.wav")
	if err := a.exec(ctx, "ffmpeg normalize", nil, "ffmpeg",
		"-y", "-i", inputPath, "-ac", "1", "-ar", "48000", normalizedWav,
	); err != nil {
		return err
	}

	a.sendProgress(jobID, 55)
	if err := a.withToolGPU(ctx, jobID, aiGPUKeyDeepFilter, a.cfg.AICUDAGPU, func(card int) error {
		return a.exec(ctx, "deepFilter", cudaEnv(card), a.cfg.DeepFilterBin,
			"--output-dir", workDir, normalizedWav,
		)
	}); err != nil {
		return err
	}

	enhanced, err := findEnhancedWav(workDir, normalizedWav)
	if err != nil {
		return err
	}

	a.sendProgress(jobID, 85)
	args := []string{"-y", "-i", enhanced}
	if polish {
		args = append(args,
			"-af",
			"highpass=f=80,lowpass=f=14000,loudnorm=I=-16:TP=-1.5:LRA=11,alimiter=limit=0.95",
		)
	}
	args = append(args, audioEncoderArgs(outputPath)...)
	args = append(args, outputPath)
	if err := a.exec(ctx, "ffmpeg encode", nil, "ffmpeg", args...); err != nil {
		return err
	}
	a.sendProgress(jobID, 95)
	return nil
}

// findEnhancedWav scans workDir for the DeepFilterNet output. It explicitly
// excludes the normalized input WAV so we never re-encode the un-denoised
// source. Preference goes to files matching the DeepFilterNet naming hint;
// otherwise the newest WAV (by mtime) wins.
func findEnhancedWav(workDir, originalWav string) (string, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return "", fmt.Errorf("read deepFilter output dir: %w", err)
	}
	originalBase := filepath.Base(originalWav)
	type candidate struct {
		path  string
		mtime int64
		hint  bool
	}
	var candidates []candidate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.EqualFold(filepath.Ext(name), ".wav") {
			continue
		}
		if name == originalBase {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{
			path:  filepath.Join(workDir, name),
			mtime: info.ModTime().UnixNano(),
			hint:  strings.Contains(strings.ToLower(name), "deepfilter"),
		})
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("deepFilter did not produce an enhanced WAV in %s", workDir)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].hint != candidates[j].hint {
			return candidates[i].hint
		}
		return candidates[i].mtime > candidates[j].mtime
	})
	return candidates[0].path, nil
}

// SeparateVocals runs Demucs htdemucs in two-stems vocals mode. mode picks
// which stem we keep: AIAudioOpIsolateVocals → vocals.wav,
// AIAudioOpRemoveVocals → no_vocals.wav.
func (a *AIService) SeparateVocals(ctx context.Context, jobID, inputPath, outputPath, mode string) error {
	var stemFile string
	switch mode {
	case AIAudioOpIsolateVocals:
		stemFile = AIAudioDemucsTrackVocals
	case AIAudioOpRemoveVocals:
		stemFile = AIAudioDemucsTrackNoVox
	default:
		return fmt.Errorf("unsupported demucs mode: %s", mode)
	}

	a.sendProgress(jobID, 10)
	workDir, err := os.MkdirTemp(a.cfg.TempDir, "ai_demucs_")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	a.sendProgress(jobID, 25)
	if err := a.withToolGPU(ctx, jobID, aiGPUKeyDemucs, a.cfg.AICUDAGPU, func(card int) error {
		return a.exec(ctx, "demucs", cudaEnv(card), a.cfg.DemucsBin,
			"-n", "htdemucs",
			"--two-stems=vocals",
			"--segment", "7",
			"-o", workDir,
			inputPath,
		)
	}); err != nil {
		return err
	}

	a.sendProgress(jobID, 80)
	stemPath, err := findFileByName(workDir, stemFile)
	if err != nil {
		return err
	}

	a.sendProgress(jobID, 90)
	args := []string{"-y", "-i", stemPath}
	// MM81: stems use 320k for MP3/AAC (StemEncoderArgs); WAV/FLAC unchanged.
	args = append(args, StemEncoderArgs(outputPath)...)
	args = append(args, outputPath)
	if err := a.exec(ctx, "ffmpeg encode stem", nil, "ffmpeg", args...); err != nil {
		return err
	}
	a.sendProgress(jobID, 95)
	return nil
}

func findFileByName(root, name string) (string, error) {
	var found string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Base(path) == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("expected %s under %s but it was not produced", name, root)
	}
	return found, nil
}

// audioEncoderArgs returns ffmpeg codec/bitrate args inferred from the output
// extension. WAV defaults to pcm_s16le so stems stay lossless.
func audioEncoderArgs(outputPath string) []string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(outputPath), ".")) {
	case "wav":
		return []string{"-c:a", "pcm_s16le"}
	case "flac":
		return []string{"-c:a", "flac"}
	case "mp3":
		return []string{"-c:a", "libmp3lame", "-b:a", "192k"}
	case "aac", "m4a":
		return []string{"-c:a", "aac", "-b:a", "192k"}
	case "ogg":
		return []string{"-c:a", "libvorbis", "-b:a", "192k"}
	case "opus":
		return []string{"-c:a", "libopus", "-b:a", "128k"}
	case "ac3":
		return []string{"-c:a", "ac3", "-b:a", "192k"}
	default:
		return []string{"-c:a", "pcm_s16le"}
	}
}

// FacePrivacy obscures detected faces. mode picks the redaction style.
// selectionJSONPath, when non-empty, is forwarded to the runtime script
// (configured by AI_FACE_PRIVACY_SCRIPT, default
// /opt/media-manipulator-ai/scripts/face_privacy.py) via --selection-json. The
// JSON contains the face boxes from a prior detect session plus the
// selectionMode/selectedFaceIds, so the script reuses the same boxes the user
// saw in the overlay instead of redetecting.
func (a *AIService) FacePrivacy(ctx context.Context, jobID, inputPath, outputPath, mode, selectionJSONPath string) error {
	if mode == "" {
		mode = "blur"
	}
	if !validFaceModes[mode] {
		return fmt.Errorf("unsupported face mode: %s", mode)
	}
	a.sendProgress(jobID, 10)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	a.sendProgress(jobID, 30)
	args := []string{
		a.cfg.FacePrivacyScript,
		"--input", inputPath,
		"--output", outputPath,
		"--mode", mode,
	}
	if strings.TrimSpace(selectionJSONPath) != "" {
		args = append(args, "--selection-json", selectionJSONPath)
	}
	if err := a.withToolGPU(ctx, jobID, aiGPUKeyFacePrivacy, a.cfg.AICUDAGPU, func(card int) error {
		return a.exec(ctx, "face_privacy", cudaEnv(card), a.cfg.VisionPython, args...)
	}); err != nil {
		return err
	}
	a.sendProgress(jobID, 95)
	return nil
}

// DetectFaces runs the runtime script in --detect-only mode. The script
// (configured via AI_FACE_PRIVACY_SCRIPT, defaults to
// /opt/media-manipulator-ai/scripts/face_privacy.py) writes a JSON document
// with image dimensions and normalized face boxes; we parse and return them so
// the handler can persist a session and respond to the UI. The temp working
// directory is removed before returning. priority is the caller's GPU queue
// priority (a GPUPriority* constant; detection has no job id to look it up).
func (a *AIService) DetectFaces(ctx context.Context, inputPath string, priority int) (*models.FaceDetectionResponse, error) {
	workDir, err := os.MkdirTemp(a.cfg.TempDir, "ai_face_detect_")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	jsonOut := filepath.Join(workDir, "faces.json")
	force := a.cfg.AICUDAGPU
	detectReq := GPURequest{
		Kind:            aiGPUKeyFacePrivacy + "[detect]",
		VRAMRequiredMiB: a.toolVRAM(aiGPUKeyFacePrivacy),
		ForceIndex:      &force,
		Priority:        priority,
	}
	if err := withGPUVia(ctx, a.acquire, nil, "", detectReq, func(idx int) error {
		card := a.cfg.AICUDAGPU
		if idx >= 0 {
			card = idx
		}
		return a.exec(ctx, "face_privacy detect", cudaEnv(card), a.cfg.VisionPython,
			a.cfg.FacePrivacyScript,
			"--input", inputPath,
			"--detect-only",
			"--json-out", jsonOut,
		)
	}); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(jsonOut)
	if err != nil {
		return nil, fmt.Errorf("read detect json: %w", err)
	}

	var payload struct {
		ImageWidth  int              `json:"imageWidth"`
		ImageHeight int              `json:"imageHeight"`
		Faces       []models.FaceBox `json:"faces"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parse detect json: %w", err)
	}

	// Clamp normalized fields defensively. The script clamps in pixel space,
	// but rounding could still nudge a value barely past 1.0 — capping here
	// keeps the UI overlay from rendering off-image.
	for i := range payload.Faces {
		f := &payload.Faces[i]
		f.X = clampUnit(f.X)
		f.Y = clampUnit(f.Y)
		f.Width = clampUnit(f.Width)
		f.Height = clampUnit(f.Height)
	}

	return &models.FaceDetectionResponse{
		ImageWidth:  payload.ImageWidth,
		ImageHeight: payload.ImageHeight,
		Faces:       payload.Faces,
	}, nil
}

func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// RemoveBackground runs rembg through the project's CUDA/ONNXRuntime helper
// script. The helper must be sourced in a shell so its environment exports
// reach rembg, so we use bash -lc with explicit shell quoting on user-supplied
// paths. The card is exported AFTER sourcing the helper (which defaults
// CUDA_VISIBLE_DEVICES on its own), so AI_CUDA_GPU alone decides it (MM94).
func (a *AIService) RemoveBackground(ctx context.Context, jobID, inputPath, outputPath, model string) error {
	if model == "" {
		model = "birefnet-general"
	}
	if !validBackgroundModels[model] {
		return fmt.Errorf("unsupported background model: %s", model)
	}
	if !strings.EqualFold(filepath.Ext(outputPath), ".png") {
		return fmt.Errorf("remove_background output must be a .png path, got %s", outputPath)
	}
	a.sendProgress(jobID, 10)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	a.sendProgress(jobID, 30)
	if err := a.withToolGPU(ctx, jobID, aiGPUKeyRemoveBackground, a.cfg.AICUDAGPU, func(card int) error {
		script := fmt.Sprintf(
			"set -e; source %s; export CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES=%d; U2NET_HOME=%s %s i -m %s %s %s",
			shellQuote(a.cfg.RembgEnvScript),
			card,
			shellQuote(a.cfg.RembgModelDir),
			shellQuote(a.cfg.RembgBin),
			shellQuote(model),
			shellQuote(inputPath),
			shellQuote(outputPath),
		)
		return a.exec(ctx, "rembg", nil, "bash", "-lc", script)
	}); err != nil {
		return err
	}
	a.sendProgress(jobID, 95)
	return nil
}

// UpscaleImage runs Real-ESRGAN ncnn Vulkan. The Vulkan loader needs the
// project-pinned ICD JSON so we set env vars directly rather than sourcing the
// helper. NOTE: On this server we observed -s 2 with realesrgan-x4plus
// producing apparent crop/zoom on at least one 640x360 photo even though the
// output dimensions doubled. -t 0 (auto tiling) was required to avoid tile
// seams; we deliberately do NOT secretly run 4x and downscale — keep behavior
// honest and let the UI surface a caveat.
func (a *AIService) UpscaleImage(ctx context.Context, jobID, inputPath, outputPath string, scale int, model string) error {
	if scale == 0 {
		scale = 4
	}
	if scale != 2 && scale != 4 {
		return fmt.Errorf("upscale scale must be 2 or 4, got %d", scale)
	}
	if model == "" {
		model = "realesrgan-x4plus"
	}
	if !validUpscaleModels[model] {
		return fmt.Errorf("unsupported upscale model: %s", model)
	}
	if !strings.EqualFold(filepath.Ext(outputPath), ".png") {
		return fmt.Errorf("ai_upscale output must be a .png path, got %s", outputPath)
	}
	a.sendProgress(jobID, 10)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	a.sendProgress(jobID, 30)
	env := []string{
		"VK_ICD_FILENAMES=/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json",
		"VK_DRIVER_FILES=/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json",
		"VK_LOADER_LAYERS_DISABLE=*",
	}
	if err := a.withToolGPU(ctx, jobID, aiGPUKeyUpscale, a.cfg.AIVulkanGPU, func(card int) error {
		args := []string{
			"-i", inputPath,
			"-o", outputPath,
			"-n", model,
			"-s", strconv.Itoa(scale),
			"-t", "0",
			"-g", strconv.Itoa(card),
			"-f", "png",
		}
		return a.exec(ctx, "realesrgan-ncnn-vulkan", env, a.cfg.RealESRGANBin, args...)
	}); err != nil {
		return err
	}
	a.sendProgress(jobID, 95)
	return nil
}

// RedactText runs the OCR-based PII redactor. detect picks pii vs all-text and
// redaction picks the obscuring style (blackbox/blur/pixelate). The script
// reads --gpu to opt into CUDA.
func (a *AIService) RedactText(ctx context.Context, jobID, inputPath, outputPath, detect, redaction string) error {
	if detect == "" {
		detect = "pii"
	}
	if !validTextDetect[detect] {
		return fmt.Errorf("unsupported text detect mode: %s", detect)
	}
	if redaction == "" {
		redaction = "blackbox"
	}
	if !validTextRedaction[redaction] {
		return fmt.Errorf("unsupported text redaction style: %s", redaction)
	}
	a.sendProgress(jobID, 10)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	a.sendProgress(jobID, 30)
	if err := a.withToolGPU(ctx, jobID, aiGPUKeyRedactText, a.cfg.AICUDAGPU, func(card int) error {
		return a.exec(ctx, "redact_text_pii", cudaEnv(card), a.cfg.VisionPython,
			a.cfg.TextRedactScript,
			"--input", inputPath,
			"--output", outputPath,
			"--detect", detect,
			"--redaction", redaction,
			"--gpu",
		)
	}); err != nil {
		return err
	}
	a.sendProgress(jobID, 95)
	return nil
}

// RemoveObject runs LaMa inpainting via the remove_object_lama.py script. The
// caller supplies normalized rectangles ([0,1]) that the user drew over the
// preview; we rasterize those into a same-size grayscale PNG mask (white inside
// rectangles, black elsewhere) and hand input + mask to the script. The script
// itself only does Image.open + SimpleLama(image, mask) + save, so it does not
// need to know anything about rectangles.
func (a *AIService) RemoveObject(ctx context.Context, jobID, inputPath, outputPath string, rectangles []models.NormalizedRect) error {
	if len(rectangles) == 0 {
		return fmt.Errorf("remove_object requires at least one rectangle covering the object to remove")
	}
	a.sendProgress(jobID, 10)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	width, height, err := imageDimensionsFn(ctx, inputPath)
	if err != nil {
		return fmt.Errorf("inspect image for mask: %w", err)
	}

	a.sendProgress(jobID, 25)
	tempDir := a.cfg.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	maskFile, err := os.CreateTemp(tempDir, "remove_object_mask_*.png")
	if err != nil {
		return fmt.Errorf("create mask file: %w", err)
	}
	maskPath := maskFile.Name()
	_ = maskFile.Close()
	defer os.Remove(maskPath)

	if err := writeRectangleMask(maskPath, width, height, rectangles); err != nil {
		return fmt.Errorf("write mask: %w", err)
	}

	a.sendProgress(jobID, 50)
	python := strings.TrimSpace(a.cfg.LamaPython)
	if python == "" {
		python = a.cfg.VisionPython
	}
	script := strings.TrimSpace(a.cfg.RemoveObjectScript)
	if script == "" {
		return fmt.Errorf("remove_object script path is not configured")
	}
	if err := a.withToolGPU(ctx, jobID, aiGPUKeyRemoveObject, a.cfg.AICUDAGPU, func(card int) error {
		return a.exec(ctx, "remove_object_lama", cudaEnv(card), python,
			script,
			"--input", inputPath,
			"--mask", maskPath,
			"--output", outputPath,
		)
	}); err != nil {
		return err
	}
	a.sendProgress(jobID, 95)
	return nil
}

// imageDimensionsFn is imageDimensions, swappable in tests (no ImageMagick).
var imageDimensionsFn = imageDimensions

// imageDimensions returns the pixel dimensions of inputPath via ImageMagick's
// identify. We use identify rather than decoding in Go because the broader
// pipeline already depends on ImageMagick and it covers WebP/HEIC/TIFF without
// pulling in extra Go image codecs.
func imageDimensions(ctx context.Context, inputPath string) (int, int, error) {
	cmd := exec.CommandContext(ctx, "identify", "-format", "%w %h", inputPath+"[0]")
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if magick, mErr := exec.LookPath("magick"); mErr == nil {
			cmd = exec.CommandContext(ctx, magick, "identify", "-format", "%w %h", inputPath+"[0]")
			out.Reset()
			errBuf.Reset()
			cmd.Stdout = &out
			cmd.Stderr = &errBuf
			if err2 := cmd.Run(); err2 != nil {
				return 0, 0, fmt.Errorf("identify failed: %v\n%s", err2, errBuf.String())
			}
		} else {
			return 0, 0, fmt.Errorf("identify failed: %v\n%s", err, errBuf.String())
		}
	}
	parts := strings.Fields(strings.TrimSpace(out.String()))
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("identify returned unexpected output: %q", out.String())
	}
	w, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse width: %w", err)
	}
	h, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse height: %w", err)
	}
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("identify returned non-positive dimensions: %dx%d", w, h)
	}
	return w, h, nil
}

// writeRectangleMask rasterizes the user's normalized rectangles into a grayscale
// PNG. White (255) marks pixels for inpainting; black (0) marks pixels to keep.
// Rectangles are clamped to image bounds so a slightly-out-of-range value from
// the UI does not crash the encoder.
func writeRectangleMask(path string, width, height int, rectangles []models.NormalizedRect) error {
	mask := image.NewGray(image.Rect(0, 0, width, height))
	white := color.Gray{Y: 255}
	for _, r := range rectangles {
		x1 := int(math.Round(r.X * float64(width)))
		y1 := int(math.Round(r.Y * float64(height)))
		x2 := int(math.Round((r.X + r.Width) * float64(width)))
		y2 := int(math.Round((r.Y + r.Height) * float64(height)))
		if x1 > x2 {
			x1, x2 = x2, x1
		}
		if y1 > y2 {
			y1, y2 = y2, y1
		}
		if x1 < 0 {
			x1 = 0
		}
		if y1 < 0 {
			y1 = 0
		}
		if x2 > width {
			x2 = width
		}
		if y2 > height {
			y2 = height
		}
		if x2-x1 < 1 || y2-y1 < 1 {
			continue
		}
		for y := y1; y < y2; y++ {
			for x := x1; x < x2; x++ {
				mask.SetGray(x, y, white)
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, mask)
}

// resolveRIFEModelPath returns the directory path that rife-ncnn-vulkan should
// load model weights from, given a UI-side model identifier. Empty input maps
// to the configured default (typically rife-v4.6).
func (a *AIService) resolveRIFEModelPath(requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == "rife-v4.6" {
		return a.cfg.AIRIFEModel, nil
	}
	if !validRIFEModels[requested] {
		return "", fmt.Errorf("unsupported RIFE model: %s", requested)
	}
	// For non-default models, derive the path from the configured default by
	// swapping the trailing model directory name. This keeps a single env var
	// (AI_RIFE_MODEL) as the source of truth for the install location.
	base := filepath.Dir(a.cfg.AIRIFEModel)
	if base == "" {
		return "", fmt.Errorf("AI_RIFE_MODEL is not configured")
	}
	return filepath.Join(base, requested), nil
}

// InterpolateFrames runs rife-ncnn-vulkan via the frame_interpolate_rife.py
// helper script. The script does frame extract → RIFE interpolate → encode in
// one process so the Go side just shells out, streams progress, and returns
// any helper errors verbatim.
func (a *AIService) InterpolateFrames(ctx context.Context, jobID, inputPath, outputPath string, opts models.AIFrameInterpolationOptions) error {
	if !a.cfg.AIFrameInterpolationEnabled {
		return fmt.Errorf("AI frame interpolation is disabled on this server")
	}
	script := strings.TrimSpace(a.cfg.AIFrameInterpolationScript)
	if script == "" {
		return fmt.Errorf("AI_FRAME_INTERPOLATION_SCRIPT is not configured")
	}
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("frame interpolation script not found at %s (set AI_FRAME_INTERPOLATION_SCRIPT): %v", script, err)
	}
	rifeBin := strings.TrimSpace(a.cfg.AIRIFEBin)
	if rifeBin == "" {
		return fmt.Errorf("AI_RIFE_BIN is not configured")
	}
	if _, err := os.Stat(rifeBin); err != nil {
		return fmt.Errorf("rife-ncnn-vulkan binary not found at %s (set AI_RIFE_BIN): %v", rifeBin, err)
	}

	targetFPS := opts.TargetFPS
	if targetFPS == 0 {
		targetFPS = 60
	}
	if !validFrameInterpolationTargetFPS[targetFPS] {
		return fmt.Errorf("unsupported target FPS for AI frame interpolation: %d (allowed: 48, 60, 120)", targetFPS)
	}

	quality := strings.ToLower(strings.TrimSpace(opts.Quality))
	if quality == "" {
		quality = "medium"
	}
	if !validFrameInterpolationQuality[quality] {
		return fmt.Errorf("unsupported quality for AI frame interpolation: %s", opts.Quality)
	}

	maxHeight := opts.MaxHeight
	if maxHeight == 0 {
		maxHeight = a.cfg.AIFrameInterpolationMaxHeight
	}
	if maxHeight != 0 && (maxHeight < 144 || maxHeight > 1080) {
		return fmt.Errorf("max height must be 0 or between 144 and 1080, got %d", maxHeight)
	}

	modelPath, err := a.resolveRIFEModelPath(opts.Model)
	if err != nil {
		return err
	}
	if _, err := os.Stat(modelPath); err != nil {
		return fmt.Errorf("RIFE model directory not found at %s (set AI_RIFE_MODEL): %v", modelPath, err)
	}

	a.sendProgress(jobID, 10)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	env := []string{
		"VK_ICD_FILENAMES=/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json",
		"VK_DRIVER_FILES=/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json",
		"VK_LOADER_LAYERS_DISABLE=*",
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
	}

	threads := strings.TrimSpace(a.cfg.AIRIFEThreads)
	if threads == "" {
		threads = "1:2:2"
	}

	a.sendProgress(jobID, 20)
	// Invoke the script directly — it has a shebang and is marked executable
	// by the deployment script. This avoids depending on a specific system
	// python interpreter path.
	if err := a.withToolGPU(ctx, jobID, aiGPUKeyFrameInterpolation, a.cfg.AIRIFEGPU, func(card int) error {
		args := []string{
			"--input", inputPath,
			"--output", outputPath,
			"--target-fps", strconv.Itoa(targetFPS),
			"--rife-bin", rifeBin,
			"--rife-model", modelPath,
			"--gpu", strconv.Itoa(card),
			"--threads", threads,
			"--quality", quality,
			"--max-height", strconv.Itoa(maxHeight),
			"--max-duration-seconds", strconv.Itoa(a.cfg.AIFrameInterpolationMaxDurationSeconds),
			"--temp-root", a.cfg.AIFrameInterpolationTempRoot,
		}
		return a.exec(ctx, "frame_interpolate_rife", env, script, args...)
	}); err != nil {
		a.sendProgress(jobID, 0)
		return err
	}
	a.sendProgress(jobID, 95)
	return nil
}
