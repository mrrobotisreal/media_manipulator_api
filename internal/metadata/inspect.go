package metadata

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Kinds understood by Inspect (matches models.FileType values).
const (
	KindImage    = "image"
	KindVideo    = "video"
	KindAudio    = "audio"
	KindDocument = "document"
)

// ToolTimeout is the per-invocation cap for exiftool and ffprobe.
const ToolTimeout = 20 * time.Second

// Warning templates. "<tool>" is "exiftool" or "ffprobe".
const (
	warnUnavailable   = "%s unavailable"                   // binary missing from PATH
	warnTimeout       = "%s timed out after 20s"           // ToolTimeout elapsed
	warnFailed        = "%s failed: %s"                    // non-zero exit; first stderr line
	warnUnparseable   = "%s output could not be parsed"    // JSON decode error
	warnNumericFailed = "exiftool numeric pass failed: %s" // -n pass failed; only RawValue is lost
)

// Inspector extracts MetadataDocuments through a Runner.
type Inspector struct {
	Runner  Runner
	Timeout time.Duration // 0 means ToolTimeout
}

var defaultInspector = &Inspector{Runner: execRunner{}}

// Inspect runs the default (real binaries) inspector.
func Inspect(ctx context.Context, path, kind string) *MetadataDocument {
	return defaultInspector.Inspect(ctx, path, kind)
}

func (i *Inspector) timeout() time.Duration {
	if i.Timeout > 0 {
		return i.Timeout
	}
	return ToolTimeout
}

// Inspect builds the document for the file at path. It never returns nil and
// never fails: tool problems become Warnings. File.Name, SizeBytes and
// ModificationTime come from the filesystem; callers overwrite Name, Mime and
// SHA256 with upload-time values.
func (i *Inspector) Inspect(ctx context.Context, path, kind string) *MetadataDocument {
	doc := &MetadataDocument{
		SchemaVersion: SchemaVersion,
		File:          FileSummary{Name: filepath.Base(path), Kind: kind},
		Tools:         []string{},
		Entries:       []MetadataEntry{},
		Warnings:      []string{},
	}
	if st, err := os.Stat(path); err == nil {
		doc.File.SizeBytes = st.Size()
		doc.File.ModificationTime = st.ModTime().UTC().Format(time.RFC3339)
	}

	// Pass 1: print-converted.
	stdout, stderr, err := i.run(ctx, "exiftool", exiftoolPrintArgs(path)...)
	pass1OK := false
	if w := toolWarning("exiftool", stderr, err); w != "" {
		doc.Warnings = append(doc.Warnings, w)
	}
	if err == nil || strings.TrimSpace(stdout) != "" {
		pairs, perr := decodeExiftoolJSON(stdout)
		switch {
		case perr != nil:
			if err == nil {
				doc.Warnings = append(doc.Warnings, fmt.Sprintf(warnUnparseable, "exiftool"))
			}
		default:
			pass1OK = true
			ex := buildExiftoolEntries(pairs)
			doc.Entries = ex.entries
			if ex.version != "" {
				doc.Tools = append(doc.Tools, "exiftool "+ex.version)
			} else {
				doc.Tools = append(doc.Tools, "exiftool")
			}
		}
	}

	// Pass 2: -n values for GPS decimals and rationals. Skipped when pass 1
	// produced nothing; failure only costs RawValue.
	if pass1OK && len(doc.Entries) > 0 {
		out2, err2Out, err2 := i.run(ctx, "exiftool", exiftoolNumericArgs(path)...)
		if err2 != nil && strings.TrimSpace(out2) == "" {
			doc.Warnings = append(doc.Warnings, fmt.Sprintf(warnNumericFailed, reason(err2Out, err2)))
		} else if pairs2, perr := decodeExiftoolJSON(out2); perr != nil {
			doc.Warnings = append(doc.Warnings, fmt.Sprintf(warnNumericFailed, "output could not be parsed"))
		} else {
			mergeRaw(doc.Entries, buildExiftoolEntries(pairs2).entries)
		}
	}

	if kind == KindVideo || kind == KindAudio {
		i.foldFFprobe(ctx, path, doc)
	}
	return doc
}

func (i *Inspector) run(ctx context.Context, name string, args ...string) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, i.timeout())
	defer cancel()
	return i.Runner.Run(cctx, name, args...)
}

// toolWarning maps a Runner error to a warning string ("" when err is nil).
func toolWarning(tool, stderr string, err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf(warnTimeout, tool)
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return fmt.Sprintf(warnUnavailable, tool)
	}
	return fmt.Sprintf(warnFailed, tool, reason(stderr, err))
}

// reason is the first non-empty stderr line, else the error text.
func reason(stderr string, err error) string {
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out after 20s"
	}
	if err != nil {
		return err.Error()
	}
	return "unknown error"
}

// NewInspector returns an Inspector backed by r (a fake in tests).
func NewInspector(r Runner) *Inspector { return &Inspector{Runner: r} }
