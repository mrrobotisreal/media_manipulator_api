package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MM95: Aperture (photo) ingest. The handler saves the upload as
// <PHOTO_DIR>/<projectId>/original.<ext>; PhotoIngest then reads the
// EXIF-upright pixel size (cheap header read), enforces the megapixel ceiling,
// and makes proxy.(jpg|png) (1920 px) and thumb.(jpg|png) (320 px) beside it
// with the same ImageMagick argument lists Darkroom's still ingest uses
// (image_derivatives.go). masks/ and bases/ are created for MM96.

// PhotoIngestResult is what a successful ingest learned.
type PhotoIngestResult struct {
	Width    int // EXIF-upright pixel size of the original
	Height   int
	HasAlpha bool
	Ext      string // proxy/thumb extension: "png" with alpha, else "jpg"
}

// PhotoTooManyPixelsError is a source over the megapixel ceiling.
type PhotoTooManyPixelsError struct {
	Width, Height, MaxMegapixels int
}

func (e *PhotoTooManyPixelsError) Error() string {
	return fmt.Sprintf("image is %dx%d, over the %d megapixel limit", e.Width, e.Height, e.MaxMegapixels)
}

// PhotoIngester runs the ingest. The production implementation is
// *PhotoIngest; handler tests use a fake that writes placeholder files.
type PhotoIngester interface {
	Ingest(ctx context.Context, dir, originalPath string, maxMegapixels int) (PhotoIngestResult, error)
}

// PhotoIngest is the ImageMagick-backed ingest. The function fields are test
// seams; the zero value after NewPhotoIngest uses the real tools.
type PhotoIngest struct {
	Timeout  time.Duration
	probe    func(ctx context.Context, path string) (w, h int, err error)
	hasAlpha func(ctx context.Context, path string) bool
	magick   func(ctx context.Context, args ...string) error
}

// NewPhotoIngest builds the production ingest; timeout <= 0 means 60 s.
func NewPhotoIngest(timeout time.Duration) *PhotoIngest {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &PhotoIngest{Timeout: timeout, probe: probePhotoSize, hasAlpha: imageHasAlpha, magick: runMagick}
}

// Ingest implements PhotoIngester. dir must exist; originalPath is inside it.
func (p *PhotoIngest) Ingest(ctx context.Context, dir, originalPath string, maxMegapixels int) (PhotoIngestResult, error) {
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}
	w, h, err := p.probe(ctx, originalPath)
	if err != nil {
		return PhotoIngestResult{}, fmt.Errorf("read image size: %w", err)
	}
	if w <= 0 || h <= 0 {
		return PhotoIngestResult{}, errors.New("image has no readable size")
	}
	if maxMegapixels > 0 && int64(w)*int64(h) > int64(maxMegapixels)*1_000_000 {
		return PhotoIngestResult{}, &PhotoTooManyPixelsError{Width: w, Height: h, MaxMegapixels: maxMegapixels}
	}
	alpha := p.hasAlpha(ctx, originalPath)
	ext := imageDerivativeExt(alpha)
	for _, sub := range []string{"masks", "bases"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return PhotoIngestResult{}, err
		}
	}
	proxy := filepath.Join(dir, "proxy."+ext)
	if err := p.magick(ctx, buildImageProxyArgs(originalPath, proxy, alpha)...); err != nil {
		return PhotoIngestResult{}, fmt.Errorf("image proxy (is ImageMagick with the source's delegate installed?): %w", err)
	}
	thumb := filepath.Join(dir, "thumb."+ext)
	if err := p.magick(ctx, buildImageThumbArgs(originalPath, thumb, alpha)...); err != nil {
		return PhotoIngestResult{}, fmt.Errorf("image thumbnail: %w", err)
	}
	return PhotoIngestResult{Width: w, Height: h, HasAlpha: alpha, Ext: ext}, nil
}

// probePhotoSize reads the displayed (EXIF-upright) pixel size of the first
// frame from the header via identify, without decoding the pixels.
func probePhotoSize(ctx context.Context, path string) (int, int, error) {
	bin, args := identifyCommand()
	args = append(args, "-format", "%w %h %[EXIF:Orientation]", path+"[0]")
	stdout, stderr, err := runCommand(ctx, bin, args...)
	if err != nil {
		return 0, 0, fmt.Errorf("identify: %w (%s)", err, commandTail(stderr, 300))
	}
	return parsePhotoGeometry(stdout)
}

// parsePhotoGeometry parses `identify -format "%w %h %[EXIF:Orientation]"`
// output; orientations 5-8 swap width and height.
func parsePhotoGeometry(out string) (int, int, error) {
	f := strings.Fields(out)
	if len(f) < 2 {
		return 0, 0, fmt.Errorf("unexpected identify output %q", commandTail(out, 80))
	}
	w, err1 := strconv.Atoi(f[0])
	h, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("unexpected identify output %q", commandTail(out, 80))
	}
	if len(f) >= 3 {
		if o, err := strconv.Atoi(f[2]); err == nil && o >= 5 && o <= 8 {
			w, h = h, w
		}
	}
	return w, h, nil
}
