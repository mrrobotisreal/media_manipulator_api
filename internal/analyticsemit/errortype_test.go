package analyticsemit

import (
	"strings"
	"testing"
)

func TestErrorType(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{"empty means we had no message", "", ErrorTypeUnknown},
		{"whitespace only", "   ", ErrorTypeUnknown},
		{"go context deadline", "context deadline exceeded", ErrorTypeTimeout},
		{"command timeout kill", "signal: killed", ErrorTypeTimeout},
		{"cancelled", "context canceled", ErrorTypeCancelled},
		{"oom", "fatal error: out of memory", ErrorTypeOOM},
		{"disk full", "write /srv/outputs/x: no space left on device", ErrorTypeDisk},
		{"cuda", "CUDA error: no kernel image is available", ErrorTypeGPU},
		{"nvenc", "nvenc: unsupported device", ErrorTypeGPU},
		{"too large", "file exceeds the maximum size for your tier", ErrorTypeTooLarge},
		{"ffmpeg corrupt input", "Invalid data found when processing input", ErrorTypeCorruptInput},
		{"mp4 without moov", "moov atom not found", ErrorTypeCorruptInput},
		{"unsupported codec", "Unknown encoder 'libaom-av1'", ErrorTypeUnsupported},
		{"sidecar down", "dial tcp 127.0.0.1:8099: connect: connection refused", ErrorTypeUpstream},
		// MM79: the voice job messages (services.VoiceUserMessage).
		{"voice engine down", "The voice engine is unavailable right now. Your operation was not counted; try again shortly.", ErrorTypeUpstream},
		{"voice engine slow", "The voice engine took too long. Your operation was not counted; try a shorter text.", ErrorTypeTimeout},
		{"uncategorised tool failure", "convert: delegate failed at line 12", ErrorTypeToolFailure},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorType(tc.msg); got != tc.want {
				t.Errorf("ErrorType(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

// The point of the classifier is that the raw message NEVER travels. Job errors are
// assembled from subprocess stderr and routinely contain absolute paths, which
// contain the filename the user chose.
func TestErrorTypeNeverLeaksTheMessage(t *testing.T) {
	secrets := []string{
		"no such file or directory: /srv/uploads/9f2/tax_return_2025.pdf",
		"convert: unable to open image '/srv/uploads/a1/passport-scan.jpg'",
		"ffmpeg: /srv/uploads/b7/interview with jane doe.mov: Invalid data found when processing input",
	}
	for _, msg := range secrets {
		got := ErrorType(msg)
		if strings.Contains(got, "/") || strings.Contains(got, ".") || strings.Contains(got, " ") {
			t.Errorf("ErrorType(%q) = %q — the result must be a bare category token, never message text", msg, got)
		}
		// And it must be one of the known constants, not a passthrough.
		if !isKnownErrorType(got) {
			t.Errorf("ErrorType(%q) = %q, which is not one of the declared categories", msg, got)
		}
	}
}

func isKnownErrorType(s string) bool {
	for _, known := range []string{
		ErrorTypeTimeout, ErrorTypeUnsupported, ErrorTypeCorruptInput, ErrorTypeOOM,
		ErrorTypeDisk, ErrorTypeGPU, ErrorTypeUpstream, ErrorTypeCancelled,
		ErrorTypeTooLarge, ErrorTypeToolFailure, ErrorTypeUnknown,
	} {
		if s == known {
			return true
		}
	}
	return false
}
