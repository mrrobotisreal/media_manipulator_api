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

// The display-derivative helpers live in image_derivatives.go (shared with the
// Aperture ingest, MM95). These studio-named wrappers keep Darkroom's call sites
// and golden-args tests unchanged.

// studioMagickLimits caps ImageMagick resources on every Darkroom call.
var studioMagickLimits = imageMagickLimits

// buildStudioImageArgs: see buildImageDerivativeArgs.
func buildStudioImageArgs(in, out string, maxDim int, alpha bool) []string {
	return buildImageDerivativeArgs(in, out, maxDim, alpha)
}

// buildStudioImageProxyArgs: 1920px display proxy.
func buildStudioImageProxyArgs(in, out string, alpha bool) []string {
	return buildImageProxyArgs(in, out, alpha)
}

// buildStudioImageThumbArgs: 320px thumbnail.
func buildStudioImageThumbArgs(in, out string, alpha bool) []string {
	return buildImageThumbArgs(in, out, alpha)
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

func studioImageExt(alpha bool) string         { return imageDerivativeExt(alpha) }
func studioImageContentType(alpha bool) string { return imageDerivativeContentType(alpha) }
func studioAlphaFromIdentify(out string) bool  { return alphaFromIdentify(out) }
func studioExtMayHaveAlpha(ext string) bool    { return extMayHaveAlpha(ext) }
func studioMagickBinary() string               { return magickBinary() }

// runStudioMagick runs ImageMagick (magick, else legacy convert).
func runStudioMagick(ctx context.Context, args ...string) error { return runMagick(ctx, args...) }

func studioImageHasAlpha(ctx context.Context, in string) bool { return imageHasAlpha(ctx, in) }

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
