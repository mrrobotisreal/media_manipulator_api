package services

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	// Registered so image.Decode can handle these containers in the
	// raster -> PDF path. JPEG is imported non-blank below (DecodeConfig).
	_ "image/gif"
	_ "image/png"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// maxPDFPages caps how many pages we will rasterize from a single PDF. PDFs are
// untrusted input; an attacker could upload a thousand-page document to exhaust
// CPU/disk. 500 is comfortably above any realistic document a user converts.
const maxPDFPages = 500

// imageToPDFDPI is the assumed pixel density when sizing a single image's PDF
// page. 96 DPI yields a sensible physical page size without resampling pixels.
const imageToPDFDPI = 96.0

// ---------------------------------------------------------------------------
// image -> PDF (pure Go, no ImageMagick PDF coder / Ghostscript)
// ---------------------------------------------------------------------------

// convertImageToPDF wraps a single raster image in a one-page PDF. JPEG inputs
// are embedded losslessly via /DCTDecode (the JPEG bytes pass through
// untouched); everything else is decoded, flattened onto white, and embedded
// as a zlib-compressed /FlateDecode RGB image. WebP and other containers Go
// cannot decode natively fall back to an ImageMagick rasterization to PNG.
func (c *Converter) convertImageToPDF(job *models.ConversionJob, options *models.ImageConversionOptions, inputPath, outputPath string) error {
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 20)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.CommandTimeout)
	defer cancel()

	pdfBytes, err := imageToPDFBytes(ctx, inputPath, options.Quality)
	if err != nil {
		return fmt.Errorf("image to PDF failed: %w", err)
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 80)
	}
	if err := os.WriteFile(outputPath, pdfBytes, 0o644); err != nil {
		return fmt.Errorf("failed to write PDF: %w", err)
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 100)
	}
	return nil
}

// imageToPDFBytes builds the PDF document bytes for a single image file.
func imageToPDFBytes(ctx context.Context, inputPath string, quality int) ([]byte, error) {
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("input image is empty")
	}

	// JPEG fast path: embed the original JPEG stream directly with /DCTDecode.
	if isJPEG(raw) {
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw)); err == nil {
			if cs := jpegColorSpace(cfg.ColorModel); cs != "" {
				return buildImagePDF(cfg.Width, cfg.Height, cs, "/DCTDecode", raw, imageToPDFDPI)
			}
			// CMYK or unknown model -> fall through to the raster path, which
			// converts to DeviceRGB safely.
		}
	}

	img, _, decErr := image.Decode(bytes.NewReader(raw))
	if decErr != nil {
		// Go can't decode this container natively (e.g. WebP). Rasterize to
		// PNG with ImageMagick (which has no PDF policy concerns) and retry.
		pngBytes, mErr := imageMagickToPNG(ctx, inputPath)
		if mErr != nil {
			return nil, fmt.Errorf("decode image: %v (imagemagick fallback failed: %v)", decErr, mErr)
		}
		img, _, decErr = image.Decode(bytes.NewReader(pngBytes))
		if decErr != nil {
			return nil, fmt.Errorf("decode rasterized image: %w", decErr)
		}
	}

	rgb, w, h := flattenToRGB(img)
	compressed, err := zlibCompress(rgb)
	if err != nil {
		return nil, fmt.Errorf("compress image data: %w", err)
	}
	return buildImagePDF(w, h, "/DeviceRGB", "/FlateDecode", compressed, imageToPDFDPI)
}

// buildImagePDF assembles a minimal, valid single-page PDF that draws one image
// XObject scaled to fill the page. pxW/pxH are the image's pixel dimensions;
// the page is sized at the given DPI so the image renders at native resolution.
func buildImagePDF(pxW, pxH int, colorSpace, filter string, data []byte, dpi float64) ([]byte, error) {
	if pxW <= 0 || pxH <= 0 {
		return nil, fmt.Errorf("invalid image dimensions %dx%d", pxW, pxH)
	}
	if dpi <= 0 {
		dpi = imageToPDFDPI
	}
	pageW := float64(pxW) * 72.0 / dpi
	pageH := float64(pxH) * 72.0 / dpi

	var buf bytes.Buffer
	offsets := make([]int, 0, 5)
	obj := func(body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}

	buf.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>", ftoa(pageW), ftoa(pageH)))

	// Image XObject (object 4) — written manually to carry the binary stream.
	offsets = append(offsets, buf.Len())
	fmt.Fprintf(&buf, "%d 0 obj\n", len(offsets))
	fmt.Fprintf(&buf, "<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Filter %s /Length %d >>\nstream\n", pxW, pxH, colorSpace, filter, len(data))
	buf.Write(data)
	buf.WriteString("\nendstream\nendobj\n")

	// Content stream (object 5): scale the unit image space to the page box.
	content := fmt.Sprintf("q\n%s 0 0 %s 0 0 cm\n/Im0 Do\nQ\n", ftoa(pageW), ftoa(pageH))
	offsets = append(offsets, buf.Len())
	fmt.Fprintf(&buf, "%d 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(offsets), len(content), content)

	xrefStart := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(offsets)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefStart)
	return buf.Bytes(), nil
}

func isJPEG(b []byte) bool {
	return len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
}

func jpegColorSpace(model color.Model) string {
	switch model {
	case color.GrayModel, color.Gray16Model:
		return "/DeviceGray"
	case color.CMYKModel:
		return "" // force the raster path; CMYK DCTDecode handling is fiddly
	default:
		return "/DeviceRGB"
	}
}

// flattenToRGB returns packed 8-bit RGB bytes (3 per pixel), compositing any
// alpha channel over a white background so transparent PNGs become opaque pages.
func flattenToRGB(img image.Image) ([]byte, int, int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, 0, w*h*3)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA() // premultiplied, 0..65535
			inv := 0xffff - a
			rr := clamp16(r + inv)
			gg := clamp16(g + inv)
			bb := clamp16(bl + inv)
			out = append(out, byte(rr>>8), byte(gg>>8), byte(bb>>8))
		}
	}
	return out, w, h
}

func clamp16(v uint32) uint32 {
	if v > 0xffff {
		return 0xffff
	}
	return v
}

func zlibCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ftoa(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// imageMagickToPNG rasterizes any ImageMagick-readable image to PNG bytes on
// stdout. Used as the WebP/exotic-format fallback for image -> PDF.
func imageMagickToPNG(ctx context.Context, inputPath string) ([]byte, error) {
	bin := "magick"
	if _, err := exec.LookPath(bin); err != nil {
		bin = "convert"
	}
	cmd := exec.CommandContext(ctx, bin, inputPath, "-auto-orient", "png:-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("imagemagick rasterize: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("imagemagick produced no output")
	}
	return stdout.Bytes(), nil
}

// ---------------------------------------------------------------------------
// PDF -> image (poppler: pdfinfo + pdftoppm)
// ---------------------------------------------------------------------------

// pdfRenderOptions is the normalized form of the job's PDF options.
type pdfRenderOptions struct {
	Format        string // "jpg" or "png"
	PageSelection string // "first" or "all"
	DPI           int
	Quality       int
}

func parsePDFRenderOptions(raw map[string]interface{}) pdfRenderOptions {
	opts := pdfRenderOptions{Format: "jpg", PageSelection: "all", DPI: 150, Quality: 90}
	if raw == nil {
		return opts
	}
	if v, ok := raw["format"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "png":
			opts.Format = "png"
		case "jpg", "jpeg":
			opts.Format = "jpg"
		}
	}
	if v, ok := raw["pageSelection"].(string); ok {
		if strings.EqualFold(strings.TrimSpace(v), "first") {
			opts.PageSelection = "first"
		}
	}
	if v, ok := raw["dpi"].(float64); ok && v > 0 {
		opts.DPI = int(v)
	}
	if opts.DPI < 50 {
		opts.DPI = 50
	}
	if opts.DPI > 400 {
		opts.DPI = 400
	}
	if v, ok := raw["quality"].(float64); ok && v > 0 {
		opts.Quality = int(v)
	}
	if opts.Quality < 1 {
		opts.Quality = 1
	}
	if opts.Quality > 100 {
		opts.Quality = 100
	}
	return opts
}

// convertPDFToImages renders a PDF to one or more raster images using poppler's
// pdftoppm. "first" page selection produces a single image at outputPath;
// "all" produces one image per page packaged into a .zip at outputPath. The
// caller (handler) computes outputPath's extension deterministically from the
// same options, so the produced file always matches what download serves.
func (c *Converter) convertPDFToImages(job *models.ConversionJob, inputPath, outputPath string) error {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		return fmt.Errorf("PDF conversion is unavailable: pdftoppm (poppler-utils) is not installed")
	}
	opts := parsePDFRenderOptions(job.Options)

	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.CommandTimeout)
	defer cancel()

	pageCount, err := pdfPageCount(ctx, inputPath)
	if err != nil {
		return fmt.Errorf("could not read PDF: %w", err)
	}
	if pageCount <= 0 {
		return fmt.Errorf("the PDF has no pages")
	}
	if pageCount > maxPDFPages {
		return fmt.Errorf("this PDF has %d pages, which exceeds the %d-page limit for conversion", pageCount, maxPDFPages)
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 15)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}
	workDir, err := os.MkdirTemp(filepath.Dir(outputPath), "pdf-pages-")
	if err != nil {
		return fmt.Errorf("create pdf workdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	ext := opts.Format
	args := []string{"-r", strconv.Itoa(opts.DPI)}
	if opts.Format == "png" {
		args = append(args, "-png")
	} else {
		args = append(args, "-jpeg", "-jpegopt", "quality="+strconv.Itoa(opts.Quality))
	}
	if opts.PageSelection == "first" {
		args = append(args, "-f", "1", "-l", "1")
	}
	args = append(args, inputPath, filepath.Join(workDir, "page"))

	if _, stderr, err := runCommand(ctx, "pdftoppm", args...); err != nil {
		return fmt.Errorf("pdftoppm failed: %w (%s)", err, tail(stderr, 1500))
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 75)
	}

	// Collect produced pages. pdftoppm zero-pads the page index to a fixed
	// width based on the page count, so a lexical sort is already page order.
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return fmt.Errorf("read pdf workdir: %w", err)
	}
	produced := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), "."+ext) {
			produced = append(produced, filepath.Join(workDir, e.Name()))
		}
	}
	sort.Strings(produced)
	if len(produced) == 0 {
		return fmt.Errorf("no pages were rendered from the PDF")
	}

	if opts.PageSelection == "first" {
		// Single-image output: move the one rendered page to outputPath.
		if err := os.Rename(produced[0], outputPath); err != nil {
			return fmt.Errorf("finalize single page: %w", err)
		}
		if c.jobManager != nil {
			c.jobManager.SendProgressUpdate(job.ID, 100)
		}
		return nil
	}

	// All-pages output: rename to clean, sortable names then zip.
	renamed := make([]string, 0, len(produced))
	for i, p := range produced {
		clean := filepath.Join(workDir, fmt.Sprintf("page-%03d.%s", i+1, ext))
		if clean != p {
			if err := os.Rename(p, clean); err != nil {
				return fmt.Errorf("rename page %d: %w", i+1, err)
			}
		}
		renamed = append(renamed, clean)
	}
	if err := zipFiles(outputPath, renamed); err != nil {
		return fmt.Errorf("package pages zip: %w", err)
	}
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 100)
	}
	return nil
}

// pdfPageCount returns the number of pages in a PDF via pdfinfo.
func pdfPageCount(ctx context.Context, path string) (int, error) {
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		// Without pdfinfo we cannot enforce the page cap safely; refuse rather
		// than risk an unbounded render.
		return 0, fmt.Errorf("pdfinfo (poppler-utils) is not installed")
	}
	stdout, stderr, err := runCommand(ctx, "pdfinfo", path)
	if err != nil {
		return 0, fmt.Errorf("pdfinfo: %w (%s)", err, strings.TrimSpace(stderr))
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "Pages:") {
			n, convErr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			if convErr != nil {
				return 0, fmt.Errorf("parse page count: %w", convErr)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("could not determine page count")
}
