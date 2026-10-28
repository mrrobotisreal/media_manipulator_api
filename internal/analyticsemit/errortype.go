package analyticsemit

import "strings"

// Error-type vocabulary for `server_job_failed`. Small and closed on purpose:
// `error_type` is a GROUP BY dimension, and a dimension whose cardinality grows
// with the number of distinct error strings is not a dimension, it is a log.
const (
	ErrorTypeTimeout      = "timeout"
	ErrorTypeUnsupported  = "unsupported_input"
	ErrorTypeCorruptInput = "corrupt_input"
	ErrorTypeOOM          = "out_of_memory"
	ErrorTypeDisk         = "disk"
	ErrorTypeGPU          = "gpu"
	ErrorTypeUpstream     = "upstream"
	ErrorTypeCancelled    = "cancelled"
	ErrorTypeTooLarge     = "too_large"
	ErrorTypeToolFailure  = "tool_failure"
	ErrorTypeUnknown      = "unknown"
)

// ErrorType classifies a job's error message into one of a fixed set of buckets.
//
// THE RAW MESSAGE MUST NEVER BE EMITTED. Job errors are assembled from ffmpeg and
// ImageMagick output and from our own wrapping, and they routinely contain absolute
// paths — which contain the upload directory, which contains a filename the user
// chose. `no such file or directory: /srv/uploads/<id>/tax_return_2025.pdf` is PII
// in an analytics property, and the analytics service's scrubber cannot recognise
// it as one because it looks like an ordinary sentence. So the message is read here,
// reduced to a category, and discarded. The full text stays in
// `mm_conversion_jobs.error_message`, which is the operational record and has an
// access story.
//
// Substring matching rather than error sentinels because most of these strings come
// from a subprocess's stderr, not from a Go error we constructed. Order matters:
// the more specific patterns are checked first.
func ErrorType(msg string) string {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return ErrorTypeUnknown
	}

	switch {
	case containsAny(m, "context deadline exceeded", "timed out", "timeout", "deadline exceeded", "signal: killed", "took too long"):
		// signal: killed lands here rather than under OOM: our own command timeout
		// kills subprocesses that way, and a timeout is the far more common cause.
		return ErrorTypeTimeout
	case containsAny(m, "context canceled", "context cancelled", "cancelled", "canceled"):
		return ErrorTypeCancelled
	case containsAny(m, "out of memory", "cannot allocate memory", "oom", "std::bad_alloc"):
		return ErrorTypeOOM
	case containsAny(m, "no space left", "disk quota", "write error", "input/output error"):
		return ErrorTypeDisk
	case containsAny(m, "cuda", "nvenc", "vulkan", "gpu", "no device", "hwaccel"):
		return ErrorTypeGPU
	case containsAny(m, "too large", "exceeds", "file size limit", "maximum size"):
		return ErrorTypeTooLarge
	case containsAny(m, "invalid data found", "moov atom not found", "malformed", "corrupt", "truncated", "premature end"):
		return ErrorTypeCorruptInput
	case containsAny(m, "unsupported", "unknown encoder", "unknown decoder", "no such codec",
		"decoder not found", "encoder not found", "does not contain any stream", "unrecognized"):
		return ErrorTypeUnsupported
	case containsAny(m, "connection refused", "connection reset", "no such host", "eof",
		"503", "502", "upstream", "sidecar", "dial tcp", "engine is unavailable"):
		return ErrorTypeUpstream
	default:
		// A real failure we have not categorised. `tool_failure` rather than
		// `unknown`: we know a tool ran and returned non-zero, which is more than
		// nothing, and it keeps `unknown` meaning "we had no message at all" so the
		// two are distinguishable in a report.
		return ErrorTypeToolFailure
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
