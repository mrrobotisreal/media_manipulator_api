package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	studioImageProxyMaxDim = 1920
	studioImageThumbMaxDim = 320
	studioImageJPEGQuality = "92"
)

// studioMagickLimits caps ImageMagick resources on every Darkroom call.
var studioMagickLimits = []string{"-limit", "memory", "512MiB", "-limit", "map", "1GiB", "-limit", "disk", "2GiB"}

// buildStudioImageArgs builds the (pure) ImageMagick argument list for a
// display derivative: first frame only, EXIF-upright, sRGB, downscale-only,
// metadata stripped. JPEG output gets quality 92; PNG (alpha) keeps alpha.
func buildStudioImageArgs(in, out string, maxDim int, alpha bool) []string {
	args := append([]string{}, studioMagickLimits...)
	args = append(args, in+"[0]", "-auto-orient", "-colorspace", "sRGB",
		"-resize", fmt.Sprintf("%dx%d>", maxDim, maxDim), "-strip")
	if !alpha {
		args = append(args, "-quality", studioImageJPEGQuality)
	}
	return append(args, out)
}

// buildStudioImageProxyArgs: 1920px display proxy.
func buildStudioImageProxyArgs(in, out string, alpha bool) []string {
	return buildStudioImageArgs(in, out, studioImageProxyMaxDim, alpha)
}

// buildStudioImageThumbArgs: 320px thumbnail.
func buildStudioImageThumbArgs(in, out string, alpha bool) []string {
	return buildStudioImageArgs(in, out, studioImageThumbMaxDim, alpha)
}

// buildStudioSVGRsvgArgs rasterizes an SVG to a PNG that fits 1920x1920.
func buildStudioSVGRsvgArgs(in, out string) []string {
	return []string{"-w", "1920", "-h", "1920", "-a", "-f", "png", "-o", out, in}
}

// buildStudioSVGMagickArgs is the fallback SVG rasterization via ImageMagick.
func buildStudioSVGMagickArgs(in, out string) []string {
	args := append([]string{}, studioMagickLimits...)
	return append(args, "-density", "192", "-background", "none", in+"[0]", out)
}

// studioImageExt is the derivative extension for the alpha decision.
func studioImageExt(alpha bool) string {
	if alpha {
		return "png"
	}
	return "jpg"
}

func studioImageContentType(alpha bool) string {
	if alpha {
		return "image/png"
	}
	return "image/jpeg"
}

// studioAlphaFromIdentify interprets `identify -format %A` output.
func studioAlphaFromIdentify(out string) bool {
	v := strings.ToLower(strings.TrimSpace(out))
	return v == "true" || v == "blend" || v == "activate" || v == "associate" || v == "on" || v == "set" || v == "copy" || v == "transparent"
}

// studioExtMayHaveAlpha is the fallback when identify cannot answer.
func studioExtMayHaveAlpha(ext string) bool {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "png", "webp", "gif", "avif", "svg", "tif", "tiff":
		return true
	}
	return false
}

func studioMagickBinary() string {
	if _, err := exec.LookPath("magick"); err == nil {
		return "magick"
	}
	return "convert"
}

// runStudioMagick runs ImageMagick (magick, else legacy convert).
func runStudioMagick(ctx context.Context, args ...string) error {
	bin := studioMagickBinary()
	_, stderr, err := runCommand(ctx, bin, args...)
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", bin, err, commandTail(stderr, 600))
	}
	return nil
}

func studioImageHasAlpha(ctx context.Context, in string) bool {
	ext := filepath.Ext(in)
	bin := "identify"
	var args []string
	if _, err := exec.LookPath("magick"); err == nil {
		bin, args = "magick", []string{"identify"}
	}
	args = append(args, "-format", "%A", in+"[0]")
	stdout, _, err := runCommand(ctx, bin, args...)
	if err != nil {
		return studioExtMayHaveAlpha(ext)
	}
	return studioAlphaFromIdentify(stdout)
}

// generateImage produces proxy.(jpg|png) and a 320px thumbnail. A proxy
// failure fails the job; a thumbnail failure is logged and leaves SpriteKey
// empty.
func (s *StudioIngestService) generateImage(ctx context.Context, jobID, originalKey, assetID, inputPath, workDir string) (StudioIngestResult, error) {
	keyDir := s3KeyDir(originalKey)
	src := inputPath
	alpha := false

	if strings.EqualFold(filepath.Ext(inputPath), ".svg") {
		raster := filepath.Join(workDir, "svg_raster.png")
		if _, err := exec.LookPath("rsvg-convert"); err == nil {
			if _, stderr, err := runCommand(ctx, "rsvg-convert", buildStudioSVGRsvgArgs(inputPath, raster)...); err != nil {
				return StudioIngestResult{}, fmt.Errorf("svg rasterization: %w (%s)", err, commandTail(stderr, 600))
			}
		} else if err := runStudioMagick(ctx, buildStudioSVGMagickArgs(inputPath, raster)...); err != nil {
			return StudioIngestResult{}, fmt.Errorf("svg rasterization: %w", err)
		}
		src, alpha = raster, true
	} else {
		alpha = studioImageHasAlpha(ctx, inputPath)
	}

	ext := studioImageExt(alpha)
	proxyLocal := filepath.Join(workDir, "proxy."+ext)
	if err := runStudioMagick(ctx, buildStudioImageProxyArgs(src, proxyLocal, alpha)...); err != nil {
		return StudioIngestResult{}, fmt.Errorf("image proxy (is ImageMagick with the source's delegate installed?): %w", err)
	}
	var res StudioIngestResult
	res.ProxyKey = keyDir + "/" + assetID + "_proxy." + ext
	if err := s.uploadFile(ctx, proxyLocal, res.ProxyKey, originalKey, studioImageContentType(alpha)); err != nil {
		return StudioIngestResult{}, fmt.Errorf("upload image proxy: %w", err)
	}
	if s.jobManager != nil {
		s.jobManager.SendProgressUpdate(jobID, 50)
	}

	thumbLocal := filepath.Join(workDir, "thumb."+ext)
	if err := runStudioMagick(ctx, buildStudioImageThumbArgs(src, thumbLocal, alpha)...); err != nil {
		log.Printf("studio ingest: thumbnail failed for asset %s: %v", assetID, err)
	} else if st, statErr := os.Stat(thumbLocal); statErr == nil && st.Size() > 0 {
		key := keyDir + "/" + assetID + "_thumb." + ext
		if upErr := s.uploadFile(ctx, thumbLocal, key, originalKey, studioImageContentType(alpha)); upErr != nil {
			log.Printf("studio ingest: thumbnail upload failed for asset %s: %v", assetID, upErr)
		} else {
			res.SpriteKey = key
		}
	}
	if s.jobManager != nil {
		s.jobManager.SendProgressUpdate(jobID, 100)
	}
	return res, nil
}
