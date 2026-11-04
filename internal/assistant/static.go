package assistant

import (
	"fmt"
	"strings"
)

// The guide's answer when the spend cap is reached (decision 8): no model call,
// the top 3 retrieved docs as links, framed by a short localized sentence.
// MM104 ships the frame text in English plus the locale table shape; MM107
// fills the other 17 locales.

// staticFrames maps a locale prefix (lowercase language, or language-region)
// to {intro, none}. intro introduces the links; none is used when retrieval
// found nothing.
var staticFrames = map[string][2]string{
	"en": {
		"I'm taking a short break today, so here are the help pages that match your question:",
		"I'm taking a short break today. You can search the help pages or browse all tools at /tools.",
	},
}

// StaticFrame returns the frame pair for a locale (falls back to English).
func StaticFrame(locale string) (intro, none string) {
	l := strings.ToLower(strings.TrimSpace(locale))
	if f, ok := staticFrames[l]; ok {
		return f[0], f[1]
	}
	if i := strings.IndexAny(l, "-_"); i > 0 {
		if f, ok := staticFrames[l[:i]]; ok {
			return f[0], f[1]
		}
	}
	f := staticFrames["en"]
	return f[0], f[1]
}

// StaticAnswer renders the retrieval-only answer as markdown.
func StaticAnswer(locale string, hits []Hit) string {
	intro, none := StaticFrame(locale)
	var links []string
	for _, h := range hits {
		if h.Doc.URL == "" {
			continue
		}
		links = append(links, fmt.Sprintf("- [%s](%s)", h.Doc.Title, h.Doc.URL))
		if len(links) == 3 {
			break
		}
	}
	if len(links) == 0 {
		return none
	}
	return intro + "\n\n" + strings.Join(links, "\n")
}

// outOfScopeLines is the guide's reply to a clearly off-topic message (MM105
// decision 7), keyed like staticFrames. English here; MM107 adds the other
// 17 locales with the static-answer table.
var outOfScopeLines = map[string]string{
	"en": "I'm Medi, the Media Manipulator guide, so I stick to the site, its tools and media editing (video, photos, audio, animation, formats). Ask me which tool to use or how to edit something and I'll help.",
}

// OutOfScopeLine returns the out-of-scope reply for a locale (English fallback).
func OutOfScopeLine(locale string) string {
	l := strings.ToLower(strings.TrimSpace(locale))
	if s, ok := outOfScopeLines[l]; ok {
		return s
	}
	if i := strings.IndexAny(l, "-_"); i > 0 {
		if s, ok := outOfScopeLines[l[:i]]; ok {
			return s
		}
	}
	return outOfScopeLines["en"]
}
