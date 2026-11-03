package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// MM96: the Aperture AI steps. Subject mask = rembg (one model, mask-only
// output); heal = LaMa via the existing remove_object_lama.py (image + mask PNG
// in, inpainted PNG out). Both run under MM94's GPU scheduler with their own
// kind labels ("rembg[photo]", "lama[photo]") and the AI tools' card and VRAM
// figures.

// PhotoAI runs subject masks and heals. One value is shared by every request.
type PhotoAI struct {
	cfg    *config.Config
	ai     *AIService
	magick func(ctx context.Context, args ...string) error
	temp   string
}

// NewPhotoAI builds the production runner.
func NewPhotoAI(cfg *config.Config, jm *JobManager) *PhotoAI {
	ai := NewAIService(cfg)
	ai.SetJobManager(jm)
	return &PhotoAI{cfg: cfg, ai: ai, magick: runMagick}
}

// photoDecodeRGB8Args makes an 8-bit sRGB opaque PNG, EXIF-upright, first
// frame: what rembg and LaMa both read.
func photoDecodeRGB8Args(in, out string) []string {
	args := append([]string{}, imageMagickLimits...)
	args = append(args, in+"[0]", "-auto-orient", "-colorspace", "sRGB", "-alpha", "off", "-depth", "8", "PNG24:"+out)
	return args
}

// photoMaskGrayArgs normalises rembg's mask-only output to an 8-bit grey PNG.
func photoMaskGrayArgs(in, out string) []string {
	args := append([]string{}, imageMagickLimits...)
	args = append(args, in, "-alpha", "off", "-colorspace", "Gray", "-depth", "8", "PNG8:"+out)
	return args
}

func (p *PhotoAI) work() (string, error) {
	root := p.temp
	if root == "" && p.cfg != nil {
		root = p.cfg.TempDir
	}
	if root != "" {
		_ = os.MkdirAll(root, 0o755)
	}
	return os.MkdirTemp(root, "photo_ai_")
}

// SubjectMask writes outMask (8-bit grey PNG, white = subject, same size as the
// EXIF-upright source). jobID may be "" (the stateless route has no job).
func (p *PhotoAI) SubjectMask(ctx context.Context, jobID string, priority int, source, outMask string) error {
	model := strings.TrimSpace(p.cfg.PhotoSubjectModel)
	if model == "" {
		model = "isnet-general-use"
	}
	if !validBackgroundModels[model] {
		return fmt.Errorf("unsupported subject model: %s", model)
	}
	work, err := p.work()
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(work)
	in := filepath.Join(work, "in.png")
	if err := p.magick(ctx, photoDecodeRGB8Args(source, in)...); err != nil {
		return fmt.Errorf("decode source: %w", err)
	}
	raw := filepath.Join(work, "mask_raw.png")
	err = p.ai.withToolGPUKind(ctx, jobID, aiGPUKeyRemoveBackground, "rembg[photo]", p.cfg.AICUDAGPU, priority, func(card int) error {
		script := fmt.Sprintf(
			"set -e; source %s; export CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES=%d; U2NET_HOME=%s %s i -m %s -om %s %s",
			shellQuote(p.cfg.RembgEnvScript), card, shellQuote(p.cfg.RembgModelDir), shellQuote(p.cfg.RembgBin),
			shellQuote(model), shellQuote(in), shellQuote(raw),
		)
		return p.ai.exec(ctx, "rembg[photo]", nil, "bash", "-lc", script)
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outMask), 0o755); err != nil {
		return err
	}
	if err := p.magick(ctx, photoMaskGrayArgs(raw, outMask)...); err != nil {
		return fmt.Errorf("normalise mask: %w", err)
	}
	return nil
}

// Heal inpaints source under mask (white = heal) into out (PNG, same size).
func (p *PhotoAI) Heal(ctx context.Context, jobID string, priority int, source, mask, out string) error {
	python := strings.TrimSpace(p.cfg.LamaPython)
	if python == "" {
		python = p.cfg.VisionPython
	}
	script := strings.TrimSpace(p.cfg.RemoveObjectScript)
	if script == "" || python == "" {
		return errors.New("heal is not configured (AI_LAMA_PYTHON / AI_REMOVE_OBJECT_SCRIPT)")
	}
	work, err := p.work()
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(work)
	in := filepath.Join(work, "in.png")
	if err := p.magick(ctx, photoDecodeRGB8Args(source, in)...); err != nil {
		return fmt.Errorf("decode source: %w", err)
	}
	grey := filepath.Join(work, "mask.png")
	if err := p.magick(ctx, photoMaskGrayArgs(mask, grey)...); err != nil {
		return fmt.Errorf("normalise heal mask: %w", err)
	}
	mask = grey
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return p.ai.withToolGPUKind(ctx, jobID, aiGPUKeyRemoveObject, "lama[photo]", p.cfg.AICUDAGPU, priority, func(card int) error {
		return p.ai.exec(ctx, "remove_object_lama[photo]", cudaEnv(card), python,
			script, "--input", in, "--mask", mask, "--output", out)
	})
}

// PhotoProbe reads the EXIF-upright size and alpha flag of an image header.
func PhotoProbe(ctx context.Context, path string) (w, h int, alpha bool, err error) {
	w, h, err = probePhotoSize(ctx, path)
	if err != nil {
		return 0, 0, false, err
	}
	return w, h, imageHasAlpha(ctx, path), nil
}

// PhotoBaseProxy makes the display proxy (1920 px, opaque JPEG) of a heal base.
func PhotoBaseProxy(ctx context.Context, in, out string) error {
	return runMagick(ctx, buildImageProxyArgs(in, out, false)...)
}

// PhotoSubjectTimeout / PhotoHealTimeout are the env budgets.
func PhotoSubjectTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.PhotoSubjectTimeoutSeconds > 0 {
		return time.Duration(cfg.PhotoSubjectTimeoutSeconds) * time.Second
	}
	return 90 * time.Second
}

func PhotoHealTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.PhotoHealTimeoutSeconds > 0 {
		return time.Duration(cfg.PhotoHealTimeoutSeconds) * time.Second
	}
	return 300 * time.Second
}
