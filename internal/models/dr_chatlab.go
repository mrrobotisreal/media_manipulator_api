package models

// DR AI Chat Test Lab DTOs. Request / response contracts for the endpoints in
// internal/handlers/dr_chatlab*.go. Authorship (author_uid/author_email) is
// always taken from the verified Firebase claims in the gin context — never
// from these request bodies. Response JSON keys are camelCase, mirroring
// media-manipulator-ui/schemas/drChatLab.ts. Assistant message content is plain
// markdown text (model output) — NOT dr-blocks.

// ---- Request bodies --------------------------------------------------------

// DrChatRenameSessionRequest renames a session (creator-only). Title is 1–120
// chars after trim.
type DrChatRenameSessionRequest struct {
	Title string `json:"title"`
}

// DrChatPresignAttachmentRequest requests an S3 upload URL for one composing
// attachment. Kind and ContentType are cross-checked by chatLabAttachmentExt
// (a chat-lab-specific allowlist — images plus PDF/text files that become
// model inputs).
type DrChatPresignAttachmentRequest struct {
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Kind        string `json:"kind"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
}

// DrChatSendMessageRequest is the streaming send body. Content is plain text
// (required unless attachments are present, max 64 KiB). Model must be in the
// current filtered catalog. ReasoningEffort is ” (off) or one of
// minimal|low|medium|high|xhigh; it is ignored for models without reasoning
// support. AttachmentIDs reference ready, unbound attachments in this session
// (max 5), bound to the message inside the send transaction.
type DrChatSendMessageRequest struct {
	Content         string   `json:"content"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoningEffort"`
	AttachmentIDs   []string `json:"attachmentIds"`
}

// ---- Model catalog ----------------------------------------------------------

// DrChatLabModelPricing is USD per million tokens (converted from OpenRouter's
// per-token decimal strings), so the UI renders "$3.00 / $15.00 per MTok"
// without further math.
type DrChatLabModelPricing struct {
	PromptUsdPerMTok     float64 `json:"promptUsdPerMTok"`
	CompletionUsdPerMTok float64 `json:"completionUsdPerMTok"`
}

// DrChatLabModel is one allowed model in the picker. Provider is the id
// substring before '/'. SupportsImages comes from
// architecture.input_modalities; SupportsReasoning from supported_parameters
// containing "reasoning". SupportedEfforts is the effort menu for the UI
// (heuristic — see buildChatLabModel).
type DrChatLabModel struct {
	ID                string                `json:"id"`
	Name              string                `json:"name"`
	Description       string                `json:"description"`
	Provider          string                `json:"provider"`
	ContextLength     int64                 `json:"contextLength"`
	SupportsImages    bool                  `json:"supportsImages"`
	SupportsReasoning bool                  `json:"supportsReasoning"`
	SupportedEfforts  []string              `json:"supportedEfforts"`
	Pricing           DrChatLabModelPricing `json:"pricing"`
	Created           int64                 `json:"created"`
}

// DrChatLabModelsResponse is the GET /chatlab/models payload.
type DrChatLabModelsResponse struct {
	Models []DrChatLabModel `json:"models"`
}

// ---- Sessions ----------------------------------------------------------------

// DrChatSession is one chat session. Sessions are shared workspace state (both
// portal users see all of them); IsMine (caller is the creator) gates
// rename/delete affordances client-side — the server re-enforces.
type DrChatSession struct {
	ID                  string  `json:"id"`
	Title               string  `json:"title"`
	TitleSource         string  `json:"titleSource"`
	CreatedByEmail      string  `json:"createdByEmail"`
	IsMine              bool    `json:"isMine"`
	LastModel           *string `json:"lastModel"`
	LastReasoningEffort *string `json:"lastReasoningEffort"`
	CreatedAt           UTCTime `json:"createdAt"`
	UpdatedAt           UTCTime `json:"updatedAt"`
}

// DrChatSessionsResponse is the GET /chatlab/sessions payload (recency order,
// capped at 200).
type DrChatSessionsResponse struct {
	Sessions []DrChatSession `json:"sessions"`
}

// ---- Messages + attachments ----------------------------------------------------

// DrChatAttachment is a hydrated attachment (presigned view/download URLs).
type DrChatAttachment struct {
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

// DrChatMessage is one hydrated chat turn. For role="user": AuthorEmail is set,
// Model/ReasoningEffort record what was requested. For role="assistant":
// AuthorEmail is nil, Model is the producing model, Reasoning is the captured
// reasoning stream (nil when none), Status/ErrorMessage record how the turn
// ended, and the usage/cost fields come from OpenRouter's usage accounting.
type DrChatMessage struct {
	ID               string             `json:"id"`
	Role             string             `json:"role"`
	AuthorEmail      *string            `json:"authorEmail"`
	Content          string             `json:"content"`
	Reasoning        *string            `json:"reasoning"`
	Model            *string            `json:"model"`
	ReasoningEffort  *string            `json:"reasoningEffort"`
	Status           string             `json:"status"`
	ErrorMessage     *string            `json:"errorMessage"`
	PromptTokens     *int               `json:"promptTokens"`
	CompletionTokens *int               `json:"completionTokens"`
	ReasoningTokens  *int               `json:"reasoningTokens"`
	TotalCostUsd     *float64           `json:"totalCostUsd"`
	CreatedAt        UTCTime            `json:"createdAt"`
	Attachments      []DrChatAttachment `json:"attachments"`
}

// DrChatSessionDetailResponse is the GET /chatlab/sessions/:id payload: the
// session plus its full message history ordered (created_at, seq).
type DrChatSessionDetailResponse struct {
	Session  DrChatSession   `json:"session"`
	Messages []DrChatMessage `json:"messages"`
}

// DrChatPresignResponse returns the created attachment id, presigned PUT URL,
// and the object key.
type DrChatPresignResponse struct {
	AttachmentID string `json:"attachmentId"`
	UploadURL    string `json:"uploadUrl"`
	Key          string `json:"key"`
}
