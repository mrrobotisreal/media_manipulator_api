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
)

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

func (a *AIService) cudaEnv() []string {
	return []string{
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		fmt.Sprintf("CUDA_VISIBLE_DEVICES=%d", a.cfg.AICUDAGPU),
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
	if err := a.runAI(ctx, "ffmpeg normalize", nil, "ffmpeg",
		"-y", "-i", inputPath, "-ac", "1", "-ar", "48000", normalizedWav,
	); err != nil {
		return err
	}

	a.sendProgress(jobID, 55)
	if err := a.runAI(ctx, "deepFilter", a.cudaEnv(), a.cfg.DeepFilterBin,
		"--output-dir", workDir, normalizedWav,
	); err != nil {
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
	if err := a.runAI(ctx, "ffmpeg encode", nil, "ffmpeg", args...); err != nil {
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
	if err := a.runAI(ctx, "demucs", a.cudaEnv(), a.cfg.DemucsBin,
		"-n", "htdemucs",
		"--two-stems=vocals",
		"--segment", "7",
		"-o", workDir,
		inputPath,
	); err != nil {
		return err
	}

	a.sendProgress(jobID, 80)
	stemPath, err := findFileByName(workDir, stemFile)
	if err != nil {
		return err
	}

	a.sendProgress(jobID, 90)
	args := []string{"-y", "-i", stemPath}
	args = append(args, audioEncoderArgs(outputPath)...)
	args = append(args, outputPath)
	if err := a.runAI(ctx, "ffmpeg encode stem", nil, "ffmpeg", args...); err != nil {
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
	if err := a.runAI(ctx, "face_privacy", a.cudaEnv(), a.cfg.VisionPython, args...); err != nil {
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
// directory is removed before returning.
func (a *AIService) DetectFaces(ctx context.Context, inputPath string) (*models.FaceDetectionResponse, error) {
	workDir, err := os.MkdirTemp(a.cfg.TempDir, "ai_face_detect_")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	jsonOut := filepath.Join(workDir, "faces.json")
	if err := a.runAI(ctx, "face_privacy detect", a.cudaEnv(), a.cfg.VisionPython,
		a.cfg.FacePrivacyScript,
		"--input", inputPath,
		"--detect-only",
		"--json-out", jsonOut,
	); err != nil {
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
// paths.
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
	script := fmt.Sprintf(
		"set -e; source %s; U2NET_HOME=%s %s i -m %s %s %s",
		shellQuote(a.cfg.RembgEnvScript),
		shellQuote(a.cfg.RembgModelDir),
		shellQuote(a.cfg.RembgBin),
		shellQuote(model),
		shellQuote(inputPath),
		shellQuote(outputPath),
	)
	if err := a.runAI(ctx, "rembg", nil, "bash", "-lc", script); err != nil {
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
	args := []string{
		"-i", inputPath,
		"-o", outputPath,
		"-n", model,
		"-s", strconv.Itoa(scale),
		"-t", "0",
		"-g", strconv.Itoa(a.cfg.AIVulkanGPU),
		"-f", "png",
	}
	if err := a.runAI(ctx, "realesrgan-ncnn-vulkan", env, a.cfg.RealESRGANBin, args...); err != nil {
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
	if err := a.runAI(ctx, "redact_text_pii", a.cudaEnv(), a.cfg.VisionPython,
		a.cfg.TextRedactScript,
		"--input", inputPath,
		"--output", outputPath,
		"--detect", detect,
		"--redaction", redaction,
		"--gpu",
	); err != nil {
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

	width, height, err := imageDimensions(ctx, inputPath)
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
	if err := a.runAI(ctx, "remove_object_lama", a.cudaEnv(), python,
		script,
		"--input", inputPath,
		"--mask", maskPath,
		"--output", outputPath,
	); err != nil {
		return err
	}
	a.sendProgress(jobID, 95)
	return nil
}

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
