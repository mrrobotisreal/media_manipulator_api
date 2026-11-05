package flywheel

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// SiteOrigin is the only origin a quoted example may keep (its query string and
// fragment are still dropped).
const SiteOrigin = "https://media-manipulator.com"

// ExampleMax is the longest quote a report carries (runes, after scrubbing).
const ExampleMax = 200

const fileExts = `mp4|mov|mkv|avi|webm|m4v|wmv|flv|mpg|mpeg|3gp|mp3|wav|flac|aac|ogg|opus|m4a|wma|aiff|` +
	`jpg|jpeg|png|gif|webp|heic|heif|avif|bmp|tiff?|svg|psd|raw|cr2|nef|arw|dng|ico|` +
	`pdf|docx?|xlsx?|pptx?|txt|rtf|odt|csv|json|xml|srt|vtt|ass|zip|rar|7z|tar|gz|psb|ai|eps|mm\w*`

var (
	urlRe       = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"')\]]+`)
	emailRe     = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+`)
	quotedFile  = regexp.MustCompile(`(?i)["'“‘«][^"'”’»\n]{1,120}\.(?:` + fileExts + `)["'”’»]`)
	winPathRe   = regexp.MustCompile(`(?i)\b[A-Z]:\\[^\s"']*`)
	unixPathRe  = regexp.MustCompile(`(?:^|[\s("'])(~?/(?:[\w.\-]+/)*[\w.\-]*[\w\-])`)
	fileRe      = regexp.MustCompile(`(?i)[^\s/\\"'<>|]+\.(?:` + fileExts + `)\b`)
	ipRe        = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	phoneRe     = regexp.MustCompile(`(?:\+|\b)\(?\d[\d\s().\-]{5,}\d\b`)
	longDigitRe = regexp.MustCompile(`\d{6,}`)
	spaceRe     = regexp.MustCompile(`\s+`)
)

// Scrub removes personal data from s: emails, file names and paths, URLs other
// than the site origin, phone numbers and long digit strings. It collapses
// whitespace. It does not truncate (use Example); names of people cannot be
// detected, which is why every quote is also cut to ExampleMax.
func Scrub(s string) string {
	s = urlRe.ReplaceAllStringFunc(s, func(u string) string {
		trimmed := strings.TrimRight(u, ".,;:!?")
		tail := u[len(trimmed):]
		if strings.HasPrefix(strings.ToLower(trimmed), strings.ToLower(SiteOrigin)) {
			rest := trimmed[len(SiteOrigin):]
			if rest != "" && rest[0] != '/' && rest[0] != '?' && rest[0] != '#' {
				return "[url]" + tail
			}
			if i := strings.IndexAny(rest, "?#"); i >= 0 {
				rest = rest[:i]
			}
			return SiteOrigin + rest + tail
		}
		return "[url]" + tail
	})
	s = emailRe.ReplaceAllString(s, "[email]")
	s = quotedFile.ReplaceAllString(s, "[file]")
	s = winPathRe.ReplaceAllString(s, "[path]")
	s = unixPathRe.ReplaceAllStringFunc(s, func(m string) string {
		if keepSitePath(m) {
			return m
		}
		lead := ""
		if m != "" && (m[0] == ' ' || m[0] == '\t' || m[0] == '\n' || m[0] == '(' || m[0] == '"' || m[0] == '\'') {
			lead = m[:1]
		}
		return lead + "[path]"
	})
	s = fileRe.ReplaceAllStringFunc(s, func(m string) string {
		if strings.Contains(m, "[") { // inside an earlier placeholder
			return m
		}
		return "[file]"
	})
	s = ipRe.ReplaceAllString(s, "[ip]")
	s = phoneRe.ReplaceAllString(s, "[phone]")
	s = longDigitRe.ReplaceAllString(s, "[number]")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// keepSitePath keeps a site-relative route a visitor typed ("/pricing",
// "/tools/compress-video"): one or two short lowercase segments and no dot.
func keepSitePath(m string) bool {
	p := strings.TrimLeft(m, " \t\n(\"'")
	if strings.HasPrefix(p, "~") || strings.Contains(p, ".") {
		return false
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) > 2 {
		return false
	}
	for _, s := range segs {
		if s == "" || len(s) > 40 || strings.ToLower(s) != s {
			return false
		}
	}
	switch segs[0] {
	case "tools", "pricing", "tutorials", "studio", "vector", "photo", "medi", "blog", "account", "help", "faq", "contact":
		return true
	}
	return false
}

// Example is the quote form: scrubbed first, then cut to ExampleMax runes with
// an ellipsis, so a cut can never expose half of a removed value.
func Example(s string) string {
	return Truncate(Scrub(s), ExampleMax)
}

// Truncate cuts s to at most n runes, appending an ellipsis when it cut.
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
