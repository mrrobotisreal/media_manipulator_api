package models

import "encoding/json"

// Double Raven Communication/Feedback (Slack-style messaging) DTOs. Request /
// response contracts for the endpoints in internal/handlers/dr_feedback.go.
// Authorship (author_uid/author_email) is always taken from the verified
// Firebase claims in the gin context — never from these request bodies. Response
// JSON keys are camelCase, mirroring media-manipulator-ui/schemas/drFeedback.ts.
// Message content is the restricted dr-blocks/v1 subset (paragraph, code, list,
// blockquote) — the same canonical rich-text format as docs/comments, passed
// through as opaque json.RawMessage.

// ---- Request bodies --------------------------------------------------------

// DrCreateConversationRequest creates a channel (kind="channel", name required,
// topic optional) or a direct message (kind="dm", participantEmail required).
type DrCreateConversationRequest struct {
	Kind             string `json:"kind"`
	Name             string `json:"name"`
	Topic            string `json:"topic"`
	ParticipantEmail string `json:"participantEmail"`
}

// DrSendMessageRequest sends a message (parentId set = a thread reply). Content
// is opaque dr-blocks/v1 JSON validated structurally by validateDrMessageJSON.
// attachmentIds reference already-uploaded, unbound attachments in the same
// conversation; they are bound to the message inside the send transaction.
type DrSendMessageRequest struct {
	Content       json.RawMessage `json:"content"`
	AttachmentIDs []string        `json:"attachmentIds"`
	ParentID      *string         `json:"parentId"`
}

// DrFeedbackPresignAttachmentRequest requests an S3 upload URL for one message
// attachment. Kind and ContentType are cross-checked by docAssetExt (the same
// allowlist + per-kind size caps as document assets, by design).
type DrFeedbackPresignAttachmentRequest struct {
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Kind        string `json:"kind"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
}

// ---- Response DTOs ---------------------------------------------------------

// DrFeedbackUser is one portal user from the allowlist (isMe flags the caller).
type DrFeedbackUser struct {
	Email string `json:"email"`
	IsMe  bool   `json:"isMe"`
}

// DrFeedbackUsersResponse is the /feedback/users payload.
type DrFeedbackUsersResponse struct {
	Users []DrFeedbackUser `json:"users"`
}

// DrConversationSummary is one sidebar row. Name/topic are channel-only;
// dmPartnerEmail is the OTHER participant for dms. lastMessageAt is nil when the
// conversation has no messages yet.
type DrConversationSummary struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	Name               *string  `json:"name"`
	Topic              *string  `json:"topic"`
	DMPartnerEmail     *string  `json:"dmPartnerEmail"`
	LastMessageAt      *UTCTime `json:"lastMessageAt"`
	LastMessageSnippet string   `json:"lastMessageSnippet"`
	UnreadCount        int      `json:"unreadCount"`
}

// DrConversationsResponse is the sidebar payload (one round trip).
type DrConversationsResponse struct {
	Conversations []DrConversationSummary `json:"conversations"`
}

// DrMessageAttachment is a hydrated attachment (presigned view/download URLs).
type DrMessageAttachment struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
	ViewURL     string `json:"viewUrl"`     // presigned GET, inline disposition
	DownloadURL string `json:"downloadUrl"` // presigned GET, attachment disposition
}

// DrMessage is one hydrated message. ParentID is nil for a top-level message.
// IsMine is server-computed (author_uid == caller uid). ReplyCount/LastReplyAt
// are computed per query (never denormalized) and are meaningful only for
// top-level messages (0 / nil for replies).
type DrMessage struct {
	ID             string                `json:"id"`
	ConversationID string                `json:"conversationId"`
	ParentID       *string               `json:"parentId"`
	AuthorUID      string                `json:"authorUid"`
	AuthorEmail    string                `json:"authorEmail"`
	IsMine         bool                  `json:"isMine"`
	Content        json.RawMessage       `json:"content"`
	CreatedAt      UTCTime               `json:"createdAt"`
	Attachments    []DrMessageAttachment `json:"attachments"`
	ReplyCount     int                   `json:"replyCount"`
	LastReplyAt    *UTCTime              `json:"lastReplyAt"`
}

// DrMessagesPage is a keyset-paginated page of top-level messages, ordered
// oldest→newest within the page. HasMore is true when older messages remain.
type DrMessagesPage struct {
	Messages []DrMessage `json:"messages"`
	HasMore  bool        `json:"hasMore"`
}

// DrRepliesResponse is a thread: the parent message plus its chronological
// replies.
type DrRepliesResponse struct {
	Parent  DrMessage   `json:"parent"`
	Replies []DrMessage `json:"replies"`
}

// DrThreadListItem is one row in the Threads view: the parent message plus its
// conversation label and reply aggregates.
type DrThreadListItem struct {
	Message          DrMessage `json:"message"`
	ConversationID   string    `json:"conversationId"`
	ConversationKind string    `json:"conversationKind"`
	ConversationName *string   `json:"conversationName"`
	DMPartnerEmail   *string   `json:"dmPartnerEmail"`
	LastReplyAt      UTCTime   `json:"lastReplyAt"`
	ReplyCount       int       `json:"replyCount"`
	LastReplySnippet string    `json:"lastReplySnippet"`
}

// DrThreadsPage is a keyset-paginated page of threads, newest-activity-first.
type DrThreadsPage struct {
	Threads []DrThreadListItem `json:"threads"`
	HasMore bool               `json:"hasMore"`
}

// DrFeedbackPresignResponse returns the created attachment id and presigned PUT
// URL the client uploads to directly.
type DrFeedbackPresignResponse struct {
	AttachmentID string `json:"attachmentId"`
	UploadURL    string `json:"uploadUrl"`
}
