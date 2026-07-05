package models

import (
	"encoding/json"
	"errors"
)

// Double Raven document comments. Request/response contracts for the endpoints
// in internal/handlers/dr_comments.go. Authorship is always taken from the
// verified Firebase claims in the gin context — never from these request
// bodies. Response JSON keys are camelCase, mirroring
// media-manipulator-ui/schemas/drComments.ts.

// ---- Anchors ---------------------------------------------------------------

// DrCommentAnchor is where a comment attaches in the document. A "text" anchor
// is a character range within a single block's plain text (see blockPlainText
// in the UI schema — the offset model is defined there and mirrored in the DOM
// selection math). A "block" anchor targets a whole media block (right-click).
// The API stores/returns it as opaque jsonb; it is only parsed to validate on
// create. start/end/quote are pointers so a "block" anchor can omit them.
type DrCommentAnchor struct {
	Type       string `json:"type"`
	BlockIndex int    `json:"blockIndex"`
	Start      *int   `json:"start,omitempty"`
	End        *int   `json:"end,omitempty"`
	Quote      string `json:"quote,omitempty"`
}

const drAnchorMaxQuote = 2000

// Validate enforces the anchor invariants (mirrors the zod-equivalent rules):
// known type, non-negative offsets, start < end for text anchors, bounded quote.
func (a DrCommentAnchor) Validate() error {
	if a.BlockIndex < 0 {
		return errors.New("blockIndex must be non-negative")
	}
	switch a.Type {
	case "text":
		if a.Start == nil || a.End == nil {
			return errors.New("text anchor requires start and end")
		}
		if *a.Start < 0 || *a.End < 0 {
			return errors.New("offsets must be non-negative")
		}
		if *a.Start >= *a.End {
			return errors.New("start must be less than end")
		}
		if len(a.Quote) > drAnchorMaxQuote {
			return errors.New("quote is too long")
		}
		return nil
	case "block":
		return nil
	default:
		return errors.New("anchor type must be 'text' or 'block'")
	}
}

// ---- Request bodies --------------------------------------------------------

type DrCreateCommentRequest struct {
	Anchor DrCommentAnchor `json:"anchor"`
}

type DrPresignAttachmentRequest struct {
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
}

type DrPublishRequest struct {
	Body string `json:"body"`
}

const DrCommentMaxBody = 10000

// ---- Response DTOs ---------------------------------------------------------

type DrAttachmentDTO struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
	ViewURL     string `json:"viewUrl"`
	DownloadURL string `json:"downloadUrl"`
}

type DrReplyDTO struct {
	ID          string            `json:"id"`
	AuthorUID   string            `json:"authorUid"`
	AuthorEmail string            `json:"authorEmail"`
	Body        string            `json:"body"`
	CreatedAt   UTCTime           `json:"createdAt"`
	UpdatedAt   UTCTime           `json:"updatedAt"`
	Attachments []DrAttachmentDTO `json:"attachments"`
}

type DrCommentDTO struct {
	ID          string            `json:"id"`
	AuthorUID   string            `json:"authorUid"`
	AuthorEmail string            `json:"authorEmail"`
	Anchor      json.RawMessage   `json:"anchor"`
	Body        string            `json:"body"`
	CreatedAt   UTCTime           `json:"createdAt"`
	UpdatedAt   UTCTime           `json:"updatedAt"`
	Attachments []DrAttachmentDTO `json:"attachments"`
	ReplyCount  int               `json:"replyCount"`
	Replies     []DrReplyDTO      `json:"replies"`
}
