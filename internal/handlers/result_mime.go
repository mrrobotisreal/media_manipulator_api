package handlers

import (
	"mime"
	"strings"

	"github.com/gabriel-vasile/mimetype"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// resultExtMIME is the extension fallback for formats content sniffing cannot
// tell apart from generic text or bytes.
var resultExtMIME = []struct{ suffix, mime string }{
	{".tar.gz", "application/gzip"},
	{".srt", "application/x-subrip"},
	{".vtt", "text/vtt"},
	{".m3u8", "application/vnd.apple.mpegurl"},
	{".mpd", "application/dash+xml"},
	{".zip", "application/zip"},
}

// sniffResultMime reports the real MIME type of the file at path, without
// parameters. Content sniffing wins; the extension table only fills in when the
// sniff is generic (plain text / octet-stream) or the file is a subtitle format.
func sniffResultMime(path string) string {
	sniffed := ""
	if detected, err := mimetype.DetectFile(path); err == nil && detected != nil {
		sniffed = normalizeMime(detected.String())
	}
	lower := strings.ToLower(path)
	for _, e := range resultExtMIME {
		if !strings.HasSuffix(lower, e.suffix) {
			continue
		}
		switch sniffed {
		case "", "text/plain", "application/octet-stream", "text/vtt", "application/x-subrip":
			return e.mime
		}
		break
	}
	if sniffed == "" {
		return "application/octet-stream"
	}
	return sniffed
}

func normalizeMime(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if mt, _, err := mime.ParseMediaType(m); err == nil {
		return mt
	}
	if i := strings.Index(m, ";"); i >= 0 {
		return strings.TrimSpace(m[:i])
	}
	return m
}

// inlineSafeMime reports whether a result of this MIME type may be rendered in
// the browser. Everything else is served as an octet-stream attachment.
func inlineSafeMime(m string) bool {
	m = normalizeMime(m)
	switch {
	case m == "image/svg+xml":
		return false
	case strings.HasPrefix(m, "image/"), strings.HasPrefix(m, "video/"), strings.HasPrefix(m, "audio/"):
		return true
	}
	switch m {
	case "application/pdf", "text/plain", "application/json", "text/vtt", "application/x-subrip":
		return true
	}
	return false
}

// ensureResultMime stores the sniffed MIME on the job when it has none, and
// returns the effective value. Safe to call repeatedly.
func (h *ConversionHandler) ensureResultMime(job *models.ConversionJob, path string) string {
	if job == nil {
		return ""
	}
	if job.ResultMime != "" {
		return job.ResultMime
	}
	m := sniffResultMime(path)
	if h != nil && h.jobManager != nil {
		_ = h.jobManager.SetResultMime(job.ID, m)
	} else {
		job.ResultMime = m
	}
	return m
}
