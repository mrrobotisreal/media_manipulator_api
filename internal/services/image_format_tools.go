package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// defaultICOSizes is the standard favicon / Windows icon ladder packed into a
// multi-size .ico when the caller does not specify sizes.
var defaultICOSizes = []int{16, 32, 48, 64, 128, 256}

// normalizeICOSizes validates and de-duplicates requested icon sizes, falling
// back to the default favicon ladder when none are usable. Sizes must be in
// 1..256 (ICO's per-dimension ceiling).
func normalizeICOSizes(requested []int) []int {
	if len(requested) == 0 {
		return defaultICOSizes
	}
	filtered := make([]int, 0, len(requested))
	seen := map[int]bool{}
	for _, s := range requested {
		if s >= 1 && s <= 256 && !seen[s] {
			filtered = append(filtered, s)
			seen[s] = true
		}
	}
	if len(filtered) == 0 {
		return defaultICOSizes
	}
	sort.Ints(filtered)
	return filtered
}

// normalizeVectorizeParams clamps the potrace threshold (1..99 percent) and
// turd size (0..1000), applying defaults of 50 and 2.
func normalizeVectorizeParams(opts *models.VectorizeOptions) (threshold, turd int) {
	threshold, turd = 50, 2
	if opts == nil {
		return
	}
	if opts.Threshold >= 1 && opts.Threshold <= 99 {
		threshold = opts.Threshold
	}
	if opts.TurdSize >= 0 && opts.TurdSize <= 1000 {
		turd = opts.TurdSize
	}
	return
}

// isSVGInput reports whether the input file is an SVG, by extension first and
// then by sniffing the leading bytes for an <svg or <?xml marker.
func isSVGInput(path string) bool {
	if strings.EqualFold(filepath.Ext(path), ".svg") {
		return true
	}
	head := strings.ToLower(string(readFilePrefix(path, 512)))
	return strings.Contains(head, "<svg") || (strings.Contains(head, "<?xml") && strings.Contains(head, "svg"))
}

// convertImageToICO produces a real multi-size .ico using ImageMagick's
// icon:auto-resize define. This packs several PNG-encoded icon sizes into one
// container — not a renamed PNG.
func (c *Converter) convertImageToICO(job *models.ConversionJob, options *models.ImageConversionOptions, inputPath, outputPath string) error {
	var requested []int
	if options.ICO != nil {
		requested = options.ICO.Sizes
	}
	sizes := normalizeICOSizes(requested)
	sizeStrs := make([]string, len(sizes))
	for i, s := range sizes {
		sizeStrs[i] = strconv.Itoa(s)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 30)
	}

	// -background none keeps transparency; auto-resize generates each size.
	args := []string{
		inputPath,
		"-auto-orient",
		"-background", "none",
		"-define", "icon:auto-resize=" + strings.Join(sizeStrs, ","),
		outputPath,
	}
	if err := c.runImageMagickWithProgress(job.ID, "convert", args...); err != nil {
		return fmt.Errorf("ICO generation failed: %v", err)
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 100)
	}
	return nil
}

// convertImageToSVG vectorizes a raster image into an SVG with potrace. The
// image is first reduced to a bilevel bitmap with ImageMagick, then traced.
// This is genuine vectorization (best for logos / line art), not a PNG wrapped
// in an <image> element.
func (c *Converter) convertImageToSVG(job *models.ConversionJob, options *models.ImageConversionOptions, inputPath, outputPath string) error {
	if _, err := exec.LookPath("potrace"); err != nil {
		return fmt.Errorf("vectorization is unavailable on this server: the 'potrace' tool is not installed")
	}

	threshold, turd := normalizeVectorizeParams(options.Vectorize)

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}
	workDir, err := os.MkdirTemp(filepath.Dir(outputPath), "trace-")
	if err != nil {
		return fmt.Errorf("create vectorize workdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 25)
	}

	// Step 1: ImageMagick -> bilevel PBM. Flatten alpha onto white, grayscale,
	// then threshold to pure black/white so potrace has clean edges.
	pbm := filepath.Join(workDir, "trace.pbm")
	magickArgs := []string{
		inputPath,
		"-auto-orient",
		"-alpha", "remove",
		"-background", "white",
		"-colorspace", "Gray",
		"-threshold", fmt.Sprintf("%d%%", threshold),
		pbm,
	}
	if err := c.runImageMagickWithProgress(job.ID, "convert", magickArgs...); err != nil {
		return fmt.Errorf("prepare bitmap for vectorization failed: %v", err)
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 60)
	}

	// Step 2: potrace -> SVG.
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.CommandTimeout)
	defer cancel()
	potraceArgs := []string{"-s", "-t", strconv.Itoa(turd), "-o", outputPath, pbm}
	if _, stderr, err := runCommand(ctx, "potrace", potraceArgs...); err != nil {
		return fmt.Errorf("potrace vectorization failed: %w (%s)", err, tail(stderr, 1500))
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 100)
	}
	return nil
}

// convertSVGToRaster renders an SVG to a raster format. rsvg-convert is
// preferred (librsvg does not fetch remote resources by default and gives crisp
// output at the requested size); ImageMagick is the fallback. Non-PNG targets
// are produced by rendering to PNG first, then converting.
func (c *Converter) convertSVGToRaster(job *models.ConversionJob, options *models.ImageConversionOptions, inputPath, outputPath string) error {
	format := strings.ToLower(strings.TrimSpace(options.Format))
	if format == "" {
		format = "png"
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 25)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.CommandTimeout)
	defer cancel()

	if _, err := exec.LookPath("rsvg-convert"); err == nil {
		workDir, err := os.MkdirTemp(filepath.Dir(outputPath), "svg-")
		if err != nil {
			return fmt.Errorf("create svg workdir: %w", err)
		}
		defer os.RemoveAll(workDir)

		pngPath := outputPath
		if format != "png" {
			pngPath = filepath.Join(workDir, "render.png")
		}
		args := []string{"-f", "png", "-o", pngPath}
		if options.Width != nil && *options.Width > 0 {
			args = append(args, "-w", strconv.Itoa(*options.Width))
		}
		if options.Height != nil && *options.Height > 0 {
			args = append(args, "-h", strconv.Itoa(*options.Height))
		}
		args = append(args, inputPath)
		if _, stderr, err := runCommand(ctx, "rsvg-convert", args...); err != nil {
			return fmt.Errorf("SVG rasterization failed: %w (%s)", err, tail(stderr, 1500))
		}
		if c.jobManager != nil {
			c.jobManager.SendProgressUpdate(job.ID, 70)
		}
		if format != "png" {
			mArgs := []string{pngPath}
			if format == "jpg" || format == "jpeg" || format == "webp" {
				q := options.Quality
				if q < 1 || q > 100 {
					q = 90
				}
				mArgs = append(mArgs, "-quality", strconv.Itoa(q))
			}
			mArgs = append(mArgs, outputPath)
			if err := c.runImageMagickWithProgress(job.ID, "convert", mArgs...); err != nil {
				return fmt.Errorf("convert rendered PNG to %s failed: %v", format, err)
			}
		}
		if c.jobManager != nil {
			c.jobManager.SendProgressUpdate(job.ID, 100)
		}
		return nil
	}

	// Fallback: ImageMagick. -density 150 gives a reasonable raster size; the
	// deployment's ImageMagick policy is expected to restrict SVG fetches.
	args := []string{"-density", "150", "-background", "none", inputPath, "-auto-orient"}
	if options.Width != nil && *options.Width > 0 && options.Height != nil && *options.Height > 0 {
		args = append(args, "-resize", fmt.Sprintf("%dx%d", *options.Width, *options.Height))
	} else if options.Width != nil && *options.Width > 0 {
		args = append(args, "-resize", strconv.Itoa(*options.Width))
	}
	if format == "jpg" || format == "jpeg" || format == "webp" {
		q := options.Quality
		if q < 1 || q > 100 {
			q = 90
		}
		args = append(args, "-quality", strconv.Itoa(q))
	}
	args = append(args, outputPath)
	if err := c.runImageMagickWithProgress(job.ID, "convert", args...); err != nil {
		return fmt.Errorf("SVG rasterization failed: %v", err)
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 100)
	}
	return nil
}
