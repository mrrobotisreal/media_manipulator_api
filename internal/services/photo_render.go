package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// MM96: the Aperture export worker. decode (ImageMagick, 16-bit sRGB PNG) ->
// photo_render.py (NumPy/OpenCV, CPU) -> encode (ImageMagick) -> metadata
// (exiftool). Design docs/design/PHOTO_EDITOR.md section 6.3.

// PhotoFormats are the export formats (and result extensions).
var PhotoFormats = map[string]string{
	"jpg": "image/jpeg", "png": "image/png", "webp": "image/webp", "avif": "image/avif", "tiff": "image/tiff",
}

// PhotoExportRequest is one render. Input is the base image (the original, or
// the latest healed base, or an uploaded file); OriginalPath is where the
// metadata is copied from (usually the same as Input).
type PhotoExportRequest struct {
	JobID         string
	Input         string
	OriginalPath  string // metadata source for metadataMode keep
	Doc           []byte // canonical PhotoDoc JSON
	MasksDir      string // <projectDir>/masks (may not exist)
	OutPath       string // final encoded file; its extension is the format
	Format        string // jpg|png|webp|avif|tiff
	Quality       int
	MetadataMode  string // keep|strip
	MaxMegapixels int
	HasAlpha      bool
}

// photoCommandRunner is the slice of *cmdaudit.Runner the renderer uses.
type photoCommandRunner interface {
	Run(ctx context.Context, spec cmdaudit.Spec) (cmdaudit.Result, error)
}

// PhotoRenderer runs exports. Function fields are test seams.
type PhotoRenderer struct {
	cfg      *config.Config
	jobs     *JobManager
	runner   photoCommandRunner
	sem      chan struct{}
	magick   func(ctx context.Context, args ...string) error
	copyMeta func(ctx context.Context, original, out string) error
	strip    func(ctx context.Context, out string) error
	tempDir  string
}

// NewPhotoRenderer builds the production renderer. runner may be nil (an
// unaudited runner is used).
func NewPhotoRenderer(cfg *config.Config, jobs *JobManager, runner *cmdaudit.Runner) *PhotoRenderer {
	n := 2
	if cfg != nil && cfg.PhotoRenderConcurrency > 0 {
		n = cfg.PhotoRenderConcurrency
	}
	if runner == nil {
		runner = cmdaudit.NewRunner(nil, nil)
	}
	return &PhotoRenderer{
		cfg: cfg, jobs: jobs, runner: runner, sem: make(chan struct{}, n),
		magick:   runMagick,
		copyMeta: photoCopyMetadata,
		strip:    func(_ context.Context, out string) error { return stripImageMetadata(out) },
	}
}

// PhotoRenderTimeout is PHOTO_RENDER_TIMEOUT_SECONDS (default 300 s).
func PhotoRenderTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.PhotoRenderTimeoutSeconds > 0 {
		return time.Duration(cfg.PhotoRenderTimeoutSeconds) * time.Second
	}
	return 300 * time.Second
}

// photoDecodeArgs decodes any accepted source to a 16-bit sRGB PNG, EXIF-upright,
// first frame; PNG64 keeps alpha, PNG48 drops it.
func photoDecodeArgs(in, out string, alpha bool) []string {
	args := append([]string{}, imageMagickLimits...)
	args = append(args, in+"[0]", "-auto-orient", "-colorspace", "sRGB", "-depth", "16")
	if alpha {
		return append(args, "PNG64:"+out)
	}
	return append(args, "-alpha", "off", "PNG48:"+out)
}

// photoEncodeArgs encodes the 16-bit render to the requested format. jpg is
// flattened on white; tiff is 8-bit; png keeps alpha; quality applies to
// jpg/webp/avif.
func photoEncodeArgs(in, out, format string, quality int) []string {
	args := append([]string{}, imageMagickLimits...)
	args = append(args, in)
	switch format {
	case "jpg":
		args = append(args, "-background", "white", "-alpha", "remove", "-alpha", "off", "-depth", "8", "-quality", strconv.Itoa(quality))
	case "webp", "avif":
		args = append(args, "-depth", "8", "-quality", strconv.Itoa(quality))
	case "tiff":
		args = append(args, "-depth", "8", "-compress", "zip")
	default: // png
		args = append(args, "-depth", "8")
	}
	return append(args, out)
}

// PhotoRenderArgs is the python argv (without the interpreter): the contract
// pinned by TestPhotoExportArgv.
func PhotoRenderArgs(script, input, doc, masks, out string, maxMP int) []string {
	args := []string{script, "--input", input, "--doc", doc, "--masks", masks, "--out", out}
	if maxMP > 0 {
		args = append(args, "--max-megapixels", strconv.Itoa(maxMP))
	}
	return append(args, "--progress")
}

// RenderExport runs one export end to end. The job's progress is 5..95.
func (r *PhotoRenderer) RenderExport(ctx context.Context, req PhotoExportRequest) error {
	if _, ok := PhotoFormats[req.Format]; !ok {
		return fmt.Errorf("unsupported export format %q", req.Format)
	}
	if req.Quality < 1 || req.Quality > 100 {
		req.Quality = 92
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	tmpRoot := r.tempDir
	if tmpRoot == "" && r.cfg != nil {
		tmpRoot = r.cfg.TempDir
	}
	if tmpRoot != "" {
		_ = os.MkdirAll(tmpRoot, 0o755)
	}
	work, err := os.MkdirTemp(tmpRoot, "photo_render_")
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(work)
	r.progress(req.JobID, 5)

	base := filepath.Join(work, "base.png")
	if err := r.magick(ctx, photoDecodeArgs(req.Input, base, req.HasAlpha)...); err != nil {
		return fmt.Errorf("decode source: %w", err)
	}
	r.progress(req.JobID, 15)
	docPath := filepath.Join(work, "doc.json")
	if err := os.WriteFile(docPath, req.Doc, 0o600); err != nil {
		return fmt.Errorf("write doc: %w", err)
	}
	rendered := filepath.Join(work, "rendered.png")
	python, script := "python3", ""
	if r.cfg != nil {
		python, script = strings.TrimSpace(r.cfg.PhotoPython), strings.TrimSpace(r.cfg.PhotoRenderScript)
	}
	if python == "" || script == "" {
		return errors.New("photo renderer is not configured (PHOTO_PYTHON / PHOTO_RENDER_SCRIPT)")
	}
	lines := &photoProgressWriter{onFrac: func(f float64) { r.progress(req.JobID, 15+int(f*65)) }}
	res, runErr := r.runner.Run(ctx, cmdaudit.Spec{
		Tool:       "photo_render",
		Stage:      "render",
		Executable: python,
		Args:       PhotoRenderArgs(script, base, docPath, req.MasksDir, rendered, req.MaxMegapixels),
		JobID:      req.JobID,
		Stdout:     lines,
	})
	if runErr != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("render timed out: %w", ctx.Err())
		}
		return fmt.Errorf("photo_render.py exit %d: %s", res.ExitCode, photoStderrTail(res.Stderr))
	}
	r.progress(req.JobID, 82)

	if err := os.MkdirAll(filepath.Dir(req.OutPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	if err := r.magick(ctx, photoEncodeArgs(rendered, req.OutPath, req.Format, req.Quality)...); err != nil {
		return fmt.Errorf("encode %s: %w", req.Format, err)
	}
	r.progress(req.JobID, 92)
	switch strings.ToLower(strings.TrimSpace(req.MetadataMode)) {
	case "strip":
		if err := r.strip(ctx, req.OutPath); err != nil {
			return fmt.Errorf("strip metadata: %w", err)
		}
	default: // keep (best effort, like every other image conversion)
		src := req.OriginalPath
		if src == "" {
			src = req.Input
		}
		if err := r.copyMeta(ctx, src, req.OutPath); err != nil {
			log.Printf("photo export %s: metadata copy skipped: %v", req.JobID, err)
		}
	}
	r.progress(req.JobID, 98)
	return nil
}

func (r *PhotoRenderer) progress(jobID string, p int) {
	if r.jobs != nil && jobID != "" {
		r.jobs.SendProgressUpdate(jobID, p)
	}
}

// photoStderrTail is the 500-char stderr tail a failed render reports; the
// cmdaudit runner already keeps only a bounded ring, and paths are redacted at
// the audit sink. Home directories are scrubbed here too.
func photoStderrTail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no output"
	}
	return cmdaudit.NewPathSanitizer("", "", "").RedactArg(commandTail(s, 500))
}

// photoCopyMetadata copies every tag of the original onto the encoded file and
// resets Orientation to 1 (the pixels are already upright).
func photoCopyMetadata(ctx context.Context, original, out string) error {
	if err := copyImageMetadata(original, out); err != nil {
		return err
	}
	_, _, err := runCommand(ctx, "exiftool", "-overwrite_original", "-n", "-Orientation=1", out)
	return err
}

// photoProgressWriter parses "progress <0..1>" lines from the script's stdout.
type photoProgressWriter struct {
	buf    bytes.Buffer
	onFrac func(float64)
}

func (w *photoProgressWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// incomplete line: put it back
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		w.parse(line)
	}
	return len(p), nil
}

func (w *photoProgressWriter) parse(line string) {
	f := strings.Fields(line)
	if len(f) == 2 && f[0] == "progress" {
		if v, err := strconv.ParseFloat(f[1], 64); err == nil && v >= 0 && v <= 1 && w.onFrac != nil {
			w.onFrac(v)
		}
	}
}

var _ io.Writer = (*photoProgressWriter)(nil)
