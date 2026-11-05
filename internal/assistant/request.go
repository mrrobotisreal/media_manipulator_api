package assistant

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant/analyze"
)

// ChatBody is the POST /api/assistant/chat body (design §10). Exactly one of
// Message and EditResult is set.
type ChatBody struct {
	Surface        string      `json:"surface"`
	ConversationID string      `json:"conversationId,omitempty"`
	TurnID         string      `json:"turnId,omitempty"`
	Locale         string      `json:"locale"`
	Page           PageRef     `json:"page"`
	Message        *MessageIn  `json:"message,omitempty"`
	Context        *ContextIn  `json:"context,omitempty"`
	Frame          *FrameIn    `json:"frame,omitempty"`
	EditResult     *EditResult `json:"editResult,omitempty"`
	Plan           bool        `json:"plan,omitempty"`
	// FileRef names a file the user dropped into the guide (POST /assistant/files, MM116a).
	FileRef string `json:"fileRef,omitempty"`
}

// PageRef is the page the user is on.
type PageRef struct {
	Path  string `json:"path"`
	Title string `json:"title,omitempty"`
}

// MessageIn is the user's text.
type MessageIn struct {
	Text string `json:"text"`
}

// ContextIn is the editor context of a copilot turn.
type ContextIn struct {
	Digest    string `json:"digest,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	// Metrics are the browser's measurements of the current preview (MM115):
	// luma, clipping, white balance. At most 4 KB, copilot surfaces only.
	Metrics analyze.MetricSet `json:"metrics,omitempty"`
}

// FrameIn is a downscaled preview JPEG (request-only, never stored).
type FrameIn struct {
	JPEGBase64 string `json:"jpegBase64"`
}

// EditResult is the client's answer to an apply_edits call (the verify loop).
type EditResult struct {
	CallID     string   `json:"callId"`
	Applied    bool     `json:"applied"`
	Errors     []string `json:"errors,omitempty"`
	FrameAfter *FrameIn `json:"frameAfter,omitempty"`
	// MetricsBefore/MetricsAfter are the browser's measurements of the preview
	// before and after the edit (MM115), 4 KB each.
	MetricsBefore analyze.MetricSet `json:"metricsBefore,omitempty"`
	MetricsAfter  analyze.MetricSet `json:"metricsAfter,omitempty"`
}

// Limits on the body (decision 9).
const (
	MaxBodyBytes    = 512 << 10
	MaxMessageChars = 4000
	MaxDigestBytes  = 24 << 10
	maxPathChars    = 300
	maxTitleChars   = 200
	maxErrors       = 8
	maxErrorChars   = 300
)

var (
	localeRe = regexp.MustCompile(`^[A-Za-z]{2,3}([-_][A-Za-z0-9]{2,8})*$`)
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// BodyError is a 400 with a machine code.
type BodyError struct {
	Code    string
	Message string
}

func (e *BodyError) Error() string { return e.Code + ": " + e.Message }

func bad(code, format string, a ...any) error {
	return &BodyError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// IsUUID reports whether s is a canonical uuid.
func IsUUID(s string) bool { return uuidRe.MatchString(s) }

// Validate checks the body shape and limits and returns the decoded frames
// as data URLs (frame, frameAfter; "" when absent).
func (b *ChatBody) Validate(maxFrameBytes int) (frameURL, frameAfterURL string, err error) {
	b.Surface = strings.TrimSpace(b.Surface)
	if !ValidSurface(b.Surface) {
		return "", "", bad("invalid_surface", "surface must be guide, studio, vector or photo")
	}
	b.Locale = strings.TrimSpace(b.Locale)
	if b.Locale == "" {
		b.Locale = "en-US"
	}
	if len(b.Locale) > 35 || !localeRe.MatchString(b.Locale) {
		return "", "", bad("invalid_locale", "locale is not a language tag")
	}
	if b.Page.Path != "" && (!strings.HasPrefix(b.Page.Path, "/") || utf8.RuneCountInString(b.Page.Path) > maxPathChars) {
		return "", "", bad("invalid_page", "page.path must be a site path")
	}
	b.Page.Title = clip(b.Page.Title, maxTitleChars)
	if (b.Message == nil) == (b.EditResult == nil) {
		return "", "", bad("invalid_body", "send exactly one of message and editResult")
	}
	if b.ConversationID != "" && !IsUUID(b.ConversationID) {
		return "", "", bad("invalid_conversation", "conversationId is not a uuid")
	}
	if b.TurnID != "" && !IsUUID(b.TurnID) {
		return "", "", bad("invalid_turn", "turnId is not a uuid")
	}
	if b.Message != nil {
		text := strings.TrimSpace(b.Message.Text)
		if text == "" {
			return "", "", bad("empty_message", "message.text is empty")
		}
		if utf8.RuneCountInString(text) > MaxMessageChars {
			return "", "", bad("message_too_long", "message.text is longer than %d characters", MaxMessageChars)
		}
		b.Message.Text = text
	}
	if b.EditResult != nil {
		if b.Surface == SurfaceGuide {
			return "", "", bad("invalid_body", "editResult is for copilot surfaces")
		}
		if b.ConversationID == "" || b.TurnID == "" || strings.TrimSpace(b.EditResult.CallID) == "" {
			return "", "", bad("invalid_body", "editResult needs conversationId, turnId and callId")
		}
		if b.Plan {
			return "", "", bad("invalid_body", "plan is set on a new message only")
		}
		if len(b.EditResult.Errors) > maxErrors {
			b.EditResult.Errors = b.EditResult.Errors[:maxErrors]
		}
		for i, e := range b.EditResult.Errors {
			b.EditResult.Errors[i] = clip(e, maxErrorChars)
		}
	}
	if b.FileRef != "" && (b.Surface != SurfaceGuide || b.Message == nil || !IsUUID(b.FileRef)) {
		return "", "", bad("invalid_file_ref", "fileRef is a file id from /assistant/files, on a guide message")
	}
	if b.Plan && b.Surface == SurfaceGuide {
		return "", "", bad("invalid_body", "plan is for copilot surfaces")
	}
	if b.Context != nil && len(b.Context.Digest) > MaxDigestBytes {
		return "", "", bad("digest_too_large", "context.digest is larger than %d bytes", MaxDigestBytes)
	}
	if err := b.validateMetrics(); err != nil {
		return "", "", err
	}
	if b.Frame != nil {
		if b.Surface == SurfaceGuide {
			return "", "", bad("invalid_body", "frames are for copilot surfaces")
		}
		if frameURL, err = decodeFrame(b.Frame.JPEGBase64, maxFrameBytes); err != nil {
			return "", "", err
		}
	}
	if b.EditResult != nil && b.EditResult.FrameAfter != nil {
		if frameAfterURL, err = decodeFrame(b.EditResult.FrameAfter.JPEGBase64, maxFrameBytes); err != nil {
			return "", "", err
		}
	}
	return frameURL, frameAfterURL, nil
}

// validateMetrics enforces the MM115 limits: copilot surfaces only, each set a
// short identifier → bounded number map within 4 KB on the wire.
func (b *ChatBody) validateMetrics() error {
	check := func(field string, m analyze.MetricSet) error {
		if len(m) == 0 {
			return nil
		}
		if b.Surface == SurfaceGuide {
			return bad("invalid_body", "%s is for copilot surfaces", field)
		}
		if raw, err := json.Marshal(m); err != nil || len(raw) > analyze.MaxMetricsBytes {
			return bad("metrics_too_large", "%s is larger than %d bytes", field, analyze.MaxMetricsBytes)
		}
		if err := m.Validate(); err != nil {
			return bad("invalid_metrics", "%s: %v", field, err)
		}
		return nil
	}
	if b.Context != nil {
		if err := check("context.metrics", b.Context.Metrics); err != nil {
			return err
		}
	}
	if b.EditResult != nil {
		if err := check("editResult.metricsBefore", b.EditResult.MetricsBefore); err != nil {
			return err
		}
		return check("editResult.metricsAfter", b.EditResult.MetricsAfter)
	}
	return nil
}

var jpegMagic = []byte{0xFF, 0xD8, 0xFF}

// decodeFrame checks size and JPEG magic and returns a data URL.
func decodeFrame(b64 string, maxBytes int) (string, error) {
	b64 = strings.TrimSpace(strings.TrimPrefix(b64, "data:image/jpeg;base64,"))
	if base64.StdEncoding.DecodedLen(len(b64)) > maxBytes+3 {
		return "", bad("frame_too_large", "frame is larger than %d bytes", maxBytes)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", bad("invalid_frame", "frame is not base64")
	}
	if len(raw) > maxBytes {
		return "", bad("frame_too_large", "frame is larger than %d bytes", maxBytes)
	}
	if !bytes.HasPrefix(raw, jpegMagic) {
		return "", bad("invalid_frame", "frame is not a JPEG")
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// AsBodyError unwraps a *BodyError.
func AsBodyError(err error) (*BodyError, bool) {
	var be *BodyError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}
