package services

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Shared still-image derivative helpers: the Darkroom still ingest
// (studio_ingest_image.go) and the Aperture ingest (photo_ingest.go, MM95) make
// the same EXIF-upright 1920 px proxy and 320 px thumbnail. The ImageMagick
// argument lists are byte-identical to what Darkroom has always run (pinned by
// studio_ingest_image_test.go).

const (
	studioImageProxyMaxDim = 1920
	studioImageThumbMaxDim = 320
	studioImageJPEGQuality = "92"
)

// imageMagickLimits caps ImageMagick resources on every derivative call.
var imageMagickLimits = []string{"-limit", "memory", "512MiB", "-limit", "map", "1GiB", "-limit", "disk", "2GiB"}

// buildImageDerivativeArgs builds the (pure) ImageMagick argument list for a
// display derivative: first frame only, EXIF-upright, sRGB, downscale-only,
// metadata stripped. JPEG output gets quality 92; PNG (alpha) keeps alpha.
func buildImageDerivativeArgs(in, out string, maxDim int, alpha bool) []string {
	args := append([]string{}, imageMagickLimits...)
	args = append(args, in+"[0]", "-auto-orient", "-colorspace", "sRGB",
		"-resize", fmt.Sprintf("%dx%d>", maxDim, maxDim), "-strip")
	if !alpha {
		args = append(args, "-quality", studioImageJPEGQuality)
	}
	return append(args, out)
}

func buildImageProxyArgs(in, out string, alpha bool) []string {
	return buildImageDerivativeArgs(in, out, studioImageProxyMaxDim, alpha)
}

func buildImageThumbArgs(in, out string, alpha bool) []string {
	return buildImageDerivativeArgs(in, out, studioImageThumbMaxDim, alpha)
}

// imageDerivativeExt is the derivative extension for the alpha decision.
func imageDerivativeExt(alpha bool) string {
	if alpha {
		return "png"
	}
	return "jpg"
}

func imageDerivativeContentType(alpha bool) string {
	if alpha {
		return "image/png"
	}
	return "image/jpeg"
}

// alphaFromIdentify interprets `identify -format %A` output.
func alphaFromIdentify(out string) bool {
	v := strings.ToLower(strings.TrimSpace(out))
	return v == "true" || v == "blend" || v == "activate" || v == "associate" || v == "on" || v == "set" || v == "copy" || v == "transparent"
}

// extMayHaveAlpha is the fallback when identify cannot answer.
func extMayHaveAlpha(ext string) bool {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "png", "webp", "gif", "avif", "svg", "tif", "tiff":
		return true
	}
	return false
}

// magickBinary prefers ImageMagick 7's `magick`, else the legacy `convert`
// (the server runs ImageMagick 6: never hard-code `magick`).
func magickBinary() string {
	if _, err := exec.LookPath("magick"); err == nil {
		return "magick"
	}
	return "convert"
}

// identifyCommand returns the binary and leading args that run `identify`.
func identifyCommand() (string, []string) {
	if _, err := exec.LookPath("magick"); err == nil {
		return "magick", []string{"identify"}
	}
	return "identify", nil
}

// runMagick runs ImageMagick (magick, else legacy convert).
func runMagick(ctx context.Context, args ...string) error {
	bin := magickBinary()
	_, stderr, err := runCommand(ctx, bin, args...)
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", bin, err, commandTail(stderr, 600))
	}
	return nil
}

func imageHasAlpha(ctx context.Context, in string) bool {
	ext := filepath.Ext(in)
	bin, args := identifyCommand()
	args = append(args, "-format", "%A", in+"[0]")
	stdout, _, err := runCommand(ctx, bin, args...)
	if err != nil {
		return extMayHaveAlpha(ext)
	}
	return alphaFromIdentify(stdout)
}
