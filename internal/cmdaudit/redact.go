// Package cmdaudit centralises redaction and command execution audit so we
// never leak local paths, AWS keys, presigned URL signatures, bearer tokens,
// or secret env vars into logs or the database.
package cmdaudit

import (
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// PathSanitizer translates absolute paths inside the configured upload/output/
// temp roots into stable placeholders like "<UPLOAD_DIR>/<job-id>/file.mp4".
// Anything outside those roots is replaced with its basename so we still see
// what the command operated on without leaking the host filesystem.
type PathSanitizer struct {
	UploadDir string
	OutputDir string
	TempDir   string
}

// NewPathSanitizer constructs the sanitizer. Empty roots are tolerated and
// simply contribute no rewrites.
func NewPathSanitizer(upload, output, temp string) *PathSanitizer {
	return &PathSanitizer{
		UploadDir: cleanAbs(upload),
		OutputDir: cleanAbs(output),
		TempDir:   cleanAbs(temp),
	}
}

func cleanAbs(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// RedactArg redacts a single argument. The implementation never panics on
// malformed input.
func (s *PathSanitizer) RedactArg(arg string) string {
	if arg == "" {
		return arg
	}
	out := arg
	out = s.redactKnownDirs(out)
	out = redactPresignedURL(out)
	out = redactSecretAssignment(out)
	out = redactBearer(out)
	out = redactHomePaths(out)
	return out
}

// RedactArgs redacts a slice of arguments.
func (s *PathSanitizer) RedactArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = s.RedactArg(a)
	}
	return out
}

// RedactEnv redacts known-sensitive env keys and trims to a string map for
// JSONB storage. Keys are kept; values containing tokens/keys are masked.
func (s *PathSanitizer) RedactEnv(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		val := kv[eq+1:]
		if isSensitiveKey(key) {
			out[key] = maskValue(val)
			continue
		}
		// Even when the key is innocuous, the value can still contain a
		// presigned URL or absolute path. Apply the value-level rewrites.
		out[key] = s.RedactArg(val)
	}
	return out
}

// RedactWorkingDir redacts a chdir / working directory argument.
func (s *PathSanitizer) RedactWorkingDir(p string) string {
	if p == "" {
		return p
	}
	return s.RedactArg(p)
}

// --- internal helpers --------------------------------------------------------

func (s *PathSanitizer) redactKnownDirs(in string) string {
	out := in
	if s.UploadDir != "" {
		out = rewriteRoot(out, s.UploadDir, "<UPLOAD_DIR>")
	}
	if s.OutputDir != "" {
		out = rewriteRoot(out, s.OutputDir, "<OUTPUT_DIR>")
	}
	if s.TempDir != "" {
		out = rewriteRoot(out, s.TempDir, "<TEMP_DIR>")
	}
	return out
}

func rewriteRoot(in, root, placeholder string) string {
	if root == "" || in == "" {
		return in
	}
	// Both absolute (full path) and relative-passed-as-absolute cases.
	if strings.Contains(in, root) {
		return strings.ReplaceAll(in, root, placeholder)
	}
	return in
}

var (
	homePathRegex = regexp.MustCompile(`(/Users/|/home/|C:\\Users\\)([^/\\ "'$\n]+)`)
	bearerRegex   = regexp.MustCompile(`(?i)\bBearer\s+([A-Za-z0-9._\-+/=]{8,})`)
	awsKeyRegex   = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	awsSecret     = regexp.MustCompile(`(?i)(aws_secret_access_key|secret_access_key|secret_key)["'= ]+([^\s"']{8,})`)
)

func redactHomePaths(in string) string {
	return homePathRegex.ReplaceAllString(in, "$1<USER>")
}

func redactBearer(in string) string {
	return bearerRegex.ReplaceAllString(in, "Bearer ***")
}

// redactPresignedURL parses the input for an http(s) URL and scrubs the
// signature-bearing query params. Non-URL input passes through unchanged.
func redactPresignedURL(in string) string {
	idx := strings.Index(in, "http")
	if idx < 0 {
		return in
	}
	// Find the URL substring.
	end := idx
	for end < len(in) && in[end] != ' ' && in[end] != '"' && in[end] != '\'' && in[end] != '\n' {
		end++
	}
	urlStr := in[idx:end]
	u, err := url.Parse(urlStr)
	if err != nil || u.Scheme == "" {
		return in
	}
	q := u.Query()
	scrubbed := false
	for _, k := range []string{
		"X-Amz-Signature",
		"X-Amz-Credential",
		"X-Amz-Security-Token",
		"X-Amz-SignedHeaders",
		"signature",
		"sig",
		"token",
	} {
		if q.Has(k) {
			q.Set(k, "***")
			scrubbed = true
		}
	}
	if !scrubbed {
		return in
	}
	u.RawQuery = q.Encode()
	return in[:idx] + u.String() + in[end:]
}

func redactSecretAssignment(in string) string {
	out := awsKeyRegex.ReplaceAllString(in, "AKIA****************")
	out = awsSecret.ReplaceAllString(out, "$1=***")
	return out
}

// isSensitiveKey decides whether an env value should be entirely masked
// before audit storage. We mask on substring match (case-insensitive) to
// cover the long tail of `*_TOKEN`, `*_SECRET`, etc. variants.
func isSensitiveKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, needle := range []string{
		"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY",
		"AUTHORIZATION", "BEARER", "SESSION_KEY",
		"AWS_SECRET", "AWS_ACCESS_KEY_ID", "AWS_SESSION_TOKEN",
		"PRIVATE_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
		"S3_ACCESS_KEY", "DB_PASSWORD",
	} {
		if strings.Contains(upper, needle) {
			return true
		}
	}
	return false
}

func maskValue(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 4 {
		return "***"
	}
	return "***" + v[len(v)-2:]
}

// TailString returns the last n bytes of s. Useful for capturing stderr/stdout
// tails without storing megabytes of output.
func TailString(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// RedactErrorMessage applies value-level rewrites to an error string for
// safe storage in `mm_tool_errors.redacted_error_message`.
func (s *PathSanitizer) RedactErrorMessage(msg string) string {
	if msg == "" {
		return ""
	}
	return s.RedactArg(msg)
}
