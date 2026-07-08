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

// DrChatCreateSessionRequest creates a session. ProjectID is optional: set →
// the session lives inside that project (validated to exist); empty/omitted →
// a general chat (existing behavior, unchanged).
type DrChatCreateSessionRequest struct {
	ProjectID string `json:"projectId"`
}

// DrChatCreateProjectRequest creates a project. Name is required (1–120 chars
// after trim); description ≤ 4 KiB, instructions ≤ 16 KiB.
type DrChatCreateProjectRequest struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
}

// DrChatUpdateProjectRequest partially updates a project — only provided keys
// change (pointer fields). Projects are collaboratively editable by any
// allowlisted user.
type DrChatUpdateProjectRequest struct {
	Name         *string `json:"name"`
	Description  *string `json:"description"`
	Instructions *string `json:"instructions"`
}

// DrChatPresignProjectAssetRequest requests an S3 upload URL for one project
// asset. Kind is NOT client-supplied — it is derived server-side by
// projectAssetKind from the file extension (browsers report unreliable MIME
// types for code files).
type DrChatPresignProjectAssetRequest struct {
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
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
	SupportsTools     bool                  `json:"supportsTools"` // can read project assets on demand
	SupportsAudio     bool                  `json:"supportsAudio"` // input_audio content parts
	SupportedEfforts  []string              `json:"supportedEfforts"`
	Pricing           DrChatLabModelPricing `json:"pricing"`
	Created           int64                 `json:"created"`
}

// DrChatLabModelsResponse is the GET /chatlab/models payload.
type DrChatLabModelsResponse struct {
	Models []DrChatLabModel `json:"models"`
}

// ---- Projects ------------------------------------------------------------------

// DrChatProject is one project summary row. Projects are collaboratively
// editable by both portal users; IsMine (caller is the creator) gates only the
// whole-project DELETE affordance client-side — the server re-enforces.
type DrChatProject struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	IsMine          bool     `json:"isMine"`
	CreatedByEmail  string   `json:"createdByEmail"`
	ChatCount       int      `json:"chatCount"`
	AssetCount      int      `json:"assetCount"`
	MemoryUpdatedAt *UTCTime `json:"memoryUpdatedAt"`
	MemoryStatus    string   `json:"memoryStatus"`
	CreatedAt       UTCTime  `json:"createdAt"`
	UpdatedAt       UTCTime  `json:"updatedAt"`
}

// DrChatProjectsResponse is the GET /chatlab/projects payload (recency order,
// capped at 100).
type DrChatProjectsResponse struct {
	Projects []DrChatProject `json:"projects"`
}

// DrChatProjectAsset is one hydrated project asset (ready assets only carry
// presigned URLs).
type DrChatProjectAsset struct {
	ID              string  `json:"id"`
	Kind            string  `json:"kind"`
	FileName        string  `json:"fileName"`
	ContentType     string  `json:"contentType"`
	SizeBytes       int64   `json:"sizeBytes"`
	Width           *int    `json:"width"`
	Height          *int    `json:"height"`
	UploadedByEmail string  `json:"uploadedByEmail"`
	CreatedAt       UTCTime `json:"createdAt"`
	ViewURL         string  `json:"viewUrl"`
	DownloadURL     string  `json:"downloadUrl"`
}

// DrChatProjectDetail is the GET /chatlab/projects/:projectId payload: the
// summary fields plus the full context (instructions + memory), the ready
// asset library, and this project's sessions.
type DrChatProjectDetail struct {
	DrChatProject
	Instructions string               `json:"instructions"`
	Memory       string               `json:"memory"`
	Assets       []DrChatProjectAsset `json:"assets"`
	Sessions     []DrChatSession      `json:"sessions"`
}

// DrChatProjectPresignResponse returns the created asset id, presigned PUT
// URL, and the object key.
type DrChatProjectPresignResponse struct {
	AssetID   string `json:"assetId"`
	UploadURL string `json:"uploadUrl"`
	Key       string `json:"key"`
}

// ---- Sessions ----------------------------------------------------------------

// DrChatSession is one chat session. Sessions are shared workspace state (both
// portal users see all of them); IsMine (caller is the creator) gates
// rename/delete affordances client-side — the server re-enforces. ProjectID is
// nil for a general chat.
type DrChatSession struct {
	ID                  string  `json:"id"`
	Title               string  `json:"title"`
	TitleSource         string  `json:"titleSource"`
	CreatedByEmail      string  `json:"createdByEmail"`
	IsMine              bool    `json:"isMine"`
	ProjectID           *string `json:"projectId"`
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
	ID               string               `json:"id"`
	Role             string               `json:"role"`
	AuthorEmail      *string              `json:"authorEmail"`
	Content          string               `json:"content"`
	Reasoning        *string              `json:"reasoning"`
	Model            *string              `json:"model"`
	ReasoningEffort  *string              `json:"reasoningEffort"`
	Status           string               `json:"status"`
	ErrorMessage     *string              `json:"errorMessage"`
	PromptTokens     *int                 `json:"promptTokens"`
	CompletionTokens *int                 `json:"completionTokens"`
	ReasoningTokens  *int                 `json:"reasoningTokens"`
	TotalCostUsd     *float64             `json:"totalCostUsd"`
	CreatedAt        UTCTime              `json:"createdAt"`
	Attachments      []DrChatAttachment   `json:"attachments"`
	ToolActivity     []DrChatToolActivity `json:"toolActivity"` // null when the turn used no tools
}

// DrChatToolActivity is one in-stream tool execution recorded on an assistant
// message (currently only read_asset).
type DrChatToolActivity struct {
	Name      string `json:"name"`
	AssetID   string `json:"assetId"`
	AssetName string `json:"assetName"`
	Status    string `json:"status"` // "ok" | "error"
}

// DrChatSessionProjectRef is the tiny project reference embedded in the
// session detail so the chat page can render a breadcrumb without a second
// fetch.
type DrChatSessionProjectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DrChatSessionDetailResponse is the GET /chatlab/sessions/:id payload: the
// session plus its full message history ordered (created_at, seq). Project is
// non-nil only for project chats.
type DrChatSessionDetailResponse struct {
	Session  DrChatSession            `json:"session"`
	Project  *DrChatSessionProjectRef `json:"project"`
	Messages []DrChatMessage          `json:"messages"`
}

// DrChatPresignResponse returns the created attachment id, presigned PUT URL,
// and the object key.
type DrChatPresignResponse struct {
	AttachmentID string `json:"attachmentId"`
	UploadURL    string `json:"uploadUrl"`
	Key          string `json:"key"`
}
