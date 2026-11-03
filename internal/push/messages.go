package push

import (
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/email"
)

// Push text for the 18 site locales (email.Locales; fallback en-us).
//
// en-us is hand-written. The other 17 (messages_i18n.go) are model-written and
// NEED NATIVE REVIEW; only ru-ru is Mitch's reviewed set elsewhere on the site,
// and these strings are not part of it.
//
// A message NEVER contains a file name: the only variable is the tool's display
// name from the table below.

type localeText struct {
	CompletedTitle, CompletedBody, FailedTitle, FailedBody, GenericTool string
	Tools                                                               map[string]string
}

// ToolIDs are the JobFacts.Tool values that may notify (AI jobs, > 120 s).
var ToolIDs = []string{
	"ai_upscale", "ai_remove_background", "ai_face_privacy", "ai_redact_text", "ai_remove_object",
	"ai_clean_voice", "ai_remove_background_noise", "ai_isolate_vocals", "ai_remove_vocals",
	"ai_frame_interpolation", "transcribe", "image_restore", "video_restore", "document_scan",
	"video_summary", "caption_translator",
}

const toolPlaceholder = "{{tool}}"

var englishText = localeText{
	CompletedTitle: "Your result is ready",
	CompletedBody:  "{{tool}} finished. Tap to open.",
	FailedTitle:    "Your job didn't finish",
	FailedBody:     "{{tool}} failed — the operation was refunded.",
	GenericTool:    "Your job",
	Tools: map[string]string{
		"ai_upscale": "AI Upscale", "ai_remove_background": "Background Remover",
		"ai_face_privacy": "Face Privacy", "ai_redact_text": "Text Redaction",
		"ai_remove_object": "Object Remover", "ai_clean_voice": "Clean Voice",
		"ai_remove_background_noise": "Noise Remover", "ai_isolate_vocals": "Vocal Isolator",
		"ai_remove_vocals": "Vocal Remover", "ai_frame_interpolation": "Frame Interpolation",
		"transcribe": "Transcription", "image_restore": "Photo Restore",
		"video_restore": "Video Restore", "document_scan": "Document Scan",
		"video_summary": "Video Summary", "caption_translator": "Caption Translator",
	},
}

// messageTable returns the text set for a locale ("" or unknown = en-us).
func messageTable(locale string) localeText {
	l := email.LocaleOrDefault(locale)
	if l == email.DefaultLocale {
		return englishText
	}
	if t, ok := translatedMessages[l]; ok {
		return t
	}
	return englishText
}

// Compose builds the notification for a finished job. failed picks the failure
// text. data carries jobId, tool, status and the deep link; no file name.
func Compose(locale, tool, jobID string, failed bool) Message {
	t := messageTable(locale)
	name := t.GenericTool
	if n, ok := t.Tools[tool]; ok && n != "" {
		name = n
	}
	status, title, body := "completed", t.CompletedTitle, t.CompletedBody
	if failed {
		status, title, body = "failed", t.FailedTitle, t.FailedBody
	}
	return Message{
		Title: title,
		Body:  strings.ReplaceAll(body, toolPlaceholder, name),
		Data: map[string]string{
			"jobId": jobID, "tool": tool, "status": status,
			"url": "mediamanipulator://job/" + jobID,
		},
	}
}
