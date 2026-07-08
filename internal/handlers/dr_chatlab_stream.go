package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// POST /api/dr/chatlab/sessions/:id/messages — the streaming send.
//
// The response is an SSE-formatted body on a POST (Content-Type:
// text/event-stream); the client consumes it via fetch + ReadableStream with a
// Bearer header (EventSource cannot send Authorization, and the DR auth model
// never changes), mirroring lib/dr/useFeedbackEvents.ts.
//
// Sequence: validate → persist the USER message in one transaction (committed
// BEFORE calling upstream, so it is never lost to an upstream failure) →
// assemble the full conversation for OpenRouter (plus, for PROJECT sessions,
// the project system prompt and the read_asset tool) → run a BOUNDED loop of
// upstream rounds (a round ending in tool calls executes them server-side and
// continues; max DR_CHATLAB_TOOL_MAX_ROUNDS), streaming deltas downstream as
// our own SSE events → persist the assistant message EXACTLY ONCE on any
// terminal condition (complete / interrupted / error / round cap) using a
// background context so a client disconnect can't cancel the write.
//
// Sessions WITHOUT a project take exactly one round with no tools and no
// system message — byte-for-byte the original behavior.

// ---- Downstream SSE events (exact wire shapes; unit-tested) -----------------

type chatLabMetaEvent struct {
	Type               string `json:"type"` // "meta"
	UserMessageID      string `json:"userMessageId"`
	AssistantMessageID string `json:"assistantMessageId"`
}

type chatLabReasoningEvent struct {
	Type string `json:"type"` // "reasoning"
	Text string `json:"text"`
}

type chatLabDeltaEvent struct {
	Type string `json:"type"` // "delta"
	Text string `json:"text"`
}

// chatLabToolEvent narrates one read_asset execution: emitted with
// status "running" before the fetch and again with "ok"/"error" after.
type chatLabToolEvent struct {
	Type      string `json:"type"` // "tool"
	Name      string `json:"name"`
	AssetID   string `json:"assetId"`
	AssetName string `json:"assetName"`
	Status    string `json:"status"` // "running" | "ok" | "error"
}

type chatLabUsageEvent struct {
	Type             string  `json:"type"` // "usage"
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	ReasoningTokens  int     `json:"reasoningTokens"`
	CostUsd          float64 `json:"costUsd"`
}

type chatLabDoneEvent struct {
	Type   string `json:"type"` // "done"
	Status string `json:"status"`
}

type chatLabErrorEvent struct {
	Type    string `json:"type"` // "error"
	Message string `json:"message"`
}

// writeChatLabEvent emits one downstream SSE record (`data: {json}\n\n`) and
// flushes. A write error means the client is gone.
func writeChatLabEvent(w gin.ResponseWriter, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	w.Flush()
	return nil
}

// ---- Prompt assembly (pure; unit-tested with a fake fetchBody) ----------------

// storedChatMessage is the minimal view of a persisted message needed to
// rebuild the upstream conversation. Assistant REASONING is deliberately
// absent: stored reasoning is never echoed back upstream.
type storedChatMessage struct {
	ID      string
	Role    string
	Content string
}

// storedChatAttachment is the minimal view of a ready, bound attachment.
type storedChatAttachment struct {
	Kind        string
	FileName    string
	ContentType string
	S3Key       string
}

// storedProjectAsset is the minimal view of a ready project asset used by the
// system-prompt manifest and the read_asset executor.
type storedProjectAsset struct {
	ID          string
	Kind        string
	FileName    string
	ContentType string
	S3Key       string
	SizeBytes   int64
}

// errChatLabInlineBudget is surfaced as a 400 with a clear message.
var errChatLabInlineBudget = errors.New("Attached text files are too large to send (512 KiB total inline limit per request)")

// chatLabTextFileTypes are the attachment content types inlined into the text
// part (everything "file"-kind except PDF).
var chatLabTextFileTypes = map[string]bool{
	"text/plain":       true,
	"text/csv":         true,
	"text/markdown":    true,
	"application/json": true,
}

// buildOpenRouterMessages converts the stored history (every prior message in
// order plus the new one) to upstream messages:
//   - assistant history goes in as plain text content,
//   - user messages with image attachments become multimodal parts — text first
//     (per the multimodal docs), then one image_url part per image using a
//     base64 data URL built from the S3 object bytes (the bucket is private —
//     presigned URLs are never handed upstream),
//   - PDF attachments become file parts (the caller adds the file-parser
//     plugin),
//   - text-kind files are inlined into the text part, under a 512 KiB total
//     budget across the whole request (errChatLabInlineBudget when exceeded).
//
// fetchBody loads an S3 object's bytes; the function is free of gin/context so
// it unit-tests with a fake.
func buildOpenRouterMessages(history []storedChatMessage, attachmentsByMsg map[string][]storedChatAttachment, fetchBody func(s3Key string) ([]byte, error)) ([]openrouter.Message, error) {
	out := make([]openrouter.Message, 0, len(history))
	inlineBudget := drChatLabInlineBudgetBytes
	for _, m := range history {
		if m.Role != "user" {
			out = append(out, openrouter.Message{Role: m.Role, Content: m.Content})
			continue
		}
		atts := attachmentsByMsg[m.ID]
		if len(atts) == 0 {
			out = append(out, openrouter.Message{Role: "user", Content: m.Content})
			continue
		}

		text := m.Content
		var parts []openrouter.ContentPart
		for _, a := range atts {
			switch {
			case a.Kind == "image":
				body, err := fetchBody(a.S3Key)
				if err != nil {
					return nil, fmt.Errorf("load image attachment %s: %w", a.S3Key, err)
				}
				parts = append(parts, openrouter.ContentPart{
					Type:     "image_url",
					ImageURL: &openrouter.ImageURL{URL: dataURL(a.ContentType, body)},
				})
			case a.ContentType == "application/pdf":
				body, err := fetchBody(a.S3Key)
				if err != nil {
					return nil, fmt.Errorf("load pdf attachment %s: %w", a.S3Key, err)
				}
				parts = append(parts, openrouter.ContentPart{
					Type: "file",
					File: &openrouter.FilePart{Filename: a.FileName, FileData: dataURL(a.ContentType, body)},
				})
			case chatLabTextFileTypes[a.ContentType]:
				body, err := fetchBody(a.S3Key)
				if err != nil {
					return nil, fmt.Errorf("load text attachment %s: %w", a.S3Key, err)
				}
				inlineBudget -= len(body)
				if inlineBudget < 0 {
					return nil, errChatLabInlineBudget
				}
				text += fmt.Sprintf("\n\n[Attached file: %s]\n```\n%s\n```", a.FileName, string(body))
			}
		}

		// Text part first, then the media parts (docs recommend the prompt
		// before images).
		content := append([]openrouter.ContentPart{{Type: "text", Text: text}}, parts...)
		out = append(out, openrouter.Message{Role: "user", Content: content})
	}
	return out, nil
}

// dataURL builds a base64 data URL for the given content type + bytes.
func dataURL(contentType string, body []byte) string {
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(body)
}

// hasPDFAttachment reports whether any message carries a PDF (→ the request
// needs the file-parser plugin).
func hasPDFAttachment(attachmentsByMsg map[string][]storedChatAttachment) bool {
	for _, atts := range attachmentsByMsg {
		for _, a := range atts {
			if a.ContentType == "application/pdf" {
				return true
			}
		}
	}
	return false
}

// ---- Project system prompt (pure; unit-tested) ---------------------------------

// projectPromptInput is the project context fed to the system-prompt builder.
type projectPromptInput struct {
	Name         string
	Description  string
	Instructions string
	Memory       string
}

// drChatLabNonToolInlineBudget caps the total text/code asset bytes inlined
// into the system prompt for models WITHOUT tool support.
const drChatLabNonToolInlineBudget = 256 << 10 // 256 KiB

// buildProjectSystemPrompt assembles the system message for a PROJECT session
// (general sessions send no system message at all). Sections are omitted when
// empty.
//
// toolCapable models get an asset MANIFEST plus the read_asset instruction —
// contents are fetched on demand through the tool loop. Non-tool models get
// text/code assets inlined directly (in upload order, stopping once the 256
// KiB budget is hit, with a note listing what was skipped); image/audio/pdf
// assets are listed as unavailable. This manifest+tool design is deliberate
// v1 — if retrieval (vector search / RAG over assets) ever lands, it slots in
// here, replacing the flat manifest with retrieved excerpts.
func buildProjectSystemPrompt(project projectPromptInput, assets []storedProjectAsset, toolCapable bool, fetchBody func(s3Key string) ([]byte, error)) (string, error) {
	var sections []string
	sections = append(sections, fmt.Sprintf("You are assisting inside the project %q.", project.Name))

	if desc := strings.TrimSpace(project.Description); desc != "" {
		sections = append(sections, "## Project description\n"+desc)
	}
	if instr := strings.TrimSpace(project.Instructions); instr != "" {
		sections = append(sections, "## Project instructions — follow these in every response\n"+instr)
	}
	if mem := strings.TrimSpace(project.Memory); mem != "" {
		sections = append(sections,
			"## Project memory\nAccumulated context distilled from earlier chats in this project. Trust it as background, but the current conversation takes precedence if they conflict.\n"+mem)
	}

	if len(assets) > 0 {
		if toolCapable {
			var b strings.Builder
			b.WriteString("## Project assets\n")
			b.WriteString("The following files are available. Use the read_asset tool with an asset's id to retrieve its contents when (and only when) it is relevant to the user's request. Do not guess at file contents — read them.\n")
			for _, a := range assets {
				fmt.Fprintf(&b, "- %s — %s, %s, %s — id: %s\n", a.FileName, a.Kind, a.ContentType, chatLabHumanSize(a.SizeBytes), a.ID)
			}
			sections = append(sections, strings.TrimRight(b.String(), "\n"))
		} else {
			section, err := buildInlineAssetsSection(assets, fetchBody)
			if err != nil {
				return "", err
			}
			sections = append(sections, section)
		}
	}

	return strings.Join(sections, "\n\n"), nil
}

// buildInlineAssetsSection is the non-tool-capable fallback: text/code assets
// inlined in upload order under the 256 KiB budget; once an asset does not
// fit, inlining STOPS and it plus everything after it is listed as skipped.
// Media assets are listed as tool-only.
func buildInlineAssetsSection(assets []storedProjectAsset, fetchBody func(s3Key string) ([]byte, error)) (string, error) {
	var b strings.Builder
	b.WriteString("## Project assets\n")

	remaining := drChatLabNonToolInlineBudget
	stopped := false
	var skipped []string
	var media []storedProjectAsset

	for _, a := range assets {
		if a.Kind != "text" && a.Kind != "code" {
			media = append(media, a)
			continue
		}
		if stopped {
			skipped = append(skipped, a.FileName)
			continue
		}
		body, err := fetchBody(a.S3Key)
		if err != nil {
			return "", fmt.Errorf("inline project asset %s: %w", a.S3Key, err)
		}
		if len(body) > remaining {
			stopped = true
			skipped = append(skipped, a.FileName)
			continue
		}
		remaining -= len(body)
		fmt.Fprintf(&b, "[Asset: %s]\n```\n%s\n```\n", a.FileName, string(body))
	}

	if len(skipped) > 0 {
		fmt.Fprintf(&b, "The following files were not included — too large for this model's asset budget: %s\n", strings.Join(skipped, ", "))
	}
	for _, a := range media {
		fmt.Fprintf(&b, "- %s — %s — available only when using a tool-capable model\n", a.FileName, a.Kind)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ---- The read_asset tool ----------------------------------------------------------

// readAssetTool is the single tool exposed to project chats (OpenAI function
// format), included in the upstream request only when the session is in a
// project with ≥1 ready asset AND the model supports tools.
var readAssetTool = openrouter.Tool{
	Type: "function",
	Function: openrouter.ToolFunction{
		Name:        "read_asset",
		Description: "Read a project asset. Returns text/code content directly; for images, audio, and PDFs the file is attached to the conversation in the following message.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"asset_id":{"type":"string","description":"The asset id from the Project assets list."}},"required":["asset_id"]}`),
	},
}

// toolCallAccumulator assembles streamed tool-call fragments keyed by index:
// the first chunk for an index carries id/type/function.name; subsequent
// chunks carry only function.arguments fragments to concatenate.
type toolCallAccumulator struct {
	order []int
	calls map[int]*openrouter.ToolCall
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{calls: map[int]*openrouter.ToolCall{}}
}

func (a *toolCallAccumulator) Add(d openrouter.ToolCallDelta) {
	tc, ok := a.calls[d.Index]
	if !ok {
		tc = &openrouter.ToolCall{}
		a.calls[d.Index] = tc
		a.order = append(a.order, d.Index)
	}
	if d.ID != "" {
		tc.ID = d.ID
	}
	if d.Type != "" {
		tc.Type = d.Type
	}
	if d.Function.Name != "" {
		tc.Function.Name = d.Function.Name
	}
	tc.Function.Arguments += d.Function.Arguments
}

func (a *toolCallAccumulator) hasCalls() bool { return len(a.order) > 0 }

// Finalize returns the assembled calls in first-seen index order, defaulting
// Type to "function" when a provider omitted it.
func (a *toolCallAccumulator) Finalize() []openrouter.ToolCall {
	out := make([]openrouter.ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		tc := *a.calls[idx]
		if tc.Type == "" {
			tc.Type = "function"
		}
		out = append(out, tc)
	}
	return out
}

// ---- read_asset execution (pure given fetchBody; unit-tested) --------------------

// readAssetExecution is the outcome of one tool call: the role:"tool" result
// text, an optional media part carried by the synthetic follow-up user
// message, the activity record for persistence, and whether the PDF plugin is
// now needed.
type readAssetExecution struct {
	ResultText string
	MediaPart  *openrouter.ContentPart
	Activity   models.DrChatToolActivity
	IsPDF      bool
}

// readAssetArgs is the tool's argument object.
type readAssetArgs struct {
	AssetID string `json:"asset_id"`
}

// findProjectAsset resolves an asset id against THIS session's project assets
// — the executor can never leak another project's data because it only ever
// sees this slice.
func findProjectAsset(assets []storedProjectAsset, id string) *storedProjectAsset {
	for i := range assets {
		if strings.EqualFold(assets[i].ID, id) {
			return &assets[i]
		}
	}
	return nil
}

// audioInputFormat maps an audio asset to OpenRouter's input_audio format
// ("mp3" | "wav"), falling back to the file extension.
func audioInputFormat(contentType, fileName string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "audio/mpeg", "audio/mp3", "audio/mpeg3":
		return "mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	}
	if strings.HasSuffix(strings.ToLower(fileName), ".wav") {
		return "wav"
	}
	return "mp3"
}

// executeReadAsset runs one read_asset call. It NEVER returns a Go error —
// every failure becomes an error-text tool result so the model can recover,
// and the Activity records ok/error for persistence/UI.
func executeReadAsset(rawArgs string, assets []storedProjectAsset, model *models.DrChatLabModel, readCapBytes int, fetchBody func(s3Key string) ([]byte, error)) readAssetExecution {
	activity := models.DrChatToolActivity{Name: "read_asset", AssetName: "unknown", Status: "error"}

	var args readAssetArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil || strings.TrimSpace(args.AssetID) == "" {
		return readAssetExecution{
			ResultText: `Error: invalid read_asset arguments — expected {"asset_id": "<id from the Project assets list>"}.`,
			Activity:   activity,
		}
	}
	activity.AssetID = args.AssetID

	asset := findProjectAsset(assets, args.AssetID)
	if asset == nil {
		return readAssetExecution{
			ResultText: fmt.Sprintf("Error: no asset with id %q exists in this project.", args.AssetID),
			Activity:   activity,
		}
	}
	activity.AssetName = asset.FileName

	fail := func(msg string) readAssetExecution {
		return readAssetExecution{ResultText: msg, Activity: activity}
	}

	switch asset.Kind {
	case "text", "code":
		body, err := fetchBody(asset.S3Key)
		if err != nil {
			return fail(fmt.Sprintf("Error: failed to read asset %q.", asset.FileName))
		}
		content := string(body)
		if len(body) > readCapBytes {
			content = strings.ToValidUTF8(string(body[:readCapBytes]), "") +
				fmt.Sprintf("\n…[truncated at %d KiB]", readCapBytes/1024)
		}
		activity.Status = "ok"
		return readAssetExecution{ResultText: content, Activity: activity}

	case "image":
		if model == nil || !model.SupportsImages {
			return fail(fmt.Sprintf("Error: asset %q is an image, but the current model does not support image input.", asset.FileName))
		}
		body, err := fetchBody(asset.S3Key)
		if err != nil {
			return fail(fmt.Sprintf("Error: failed to read asset %q.", asset.FileName))
		}
		activity.Status = "ok"
		return readAssetExecution{
			ResultText: fmt.Sprintf("The file '%s' is attached in the next message.", asset.FileName),
			MediaPart:  &openrouter.ContentPart{Type: "image_url", ImageURL: &openrouter.ImageURL{URL: dataURL(asset.ContentType, body)}},
			Activity:   activity,
		}

	case "audio":
		if model == nil || !model.SupportsAudio {
			return fail(fmt.Sprintf("Error: asset %q is audio, but the current model does not support audio input.", asset.FileName))
		}
		body, err := fetchBody(asset.S3Key)
		if err != nil {
			return fail(fmt.Sprintf("Error: failed to read asset %q.", asset.FileName))
		}
		activity.Status = "ok"
		return readAssetExecution{
			ResultText: fmt.Sprintf("The file '%s' is attached in the next message.", asset.FileName),
			// Per the audio docs: base64 data (NOT a data URL) + format.
			MediaPart: &openrouter.ContentPart{Type: "input_audio", InputAudio: &openrouter.InputAudio{
				Data:   base64.StdEncoding.EncodeToString(body),
				Format: audioInputFormat(asset.ContentType, asset.FileName),
			}},
			Activity: activity,
		}

	case "pdf":
		body, err := fetchBody(asset.S3Key)
		if err != nil {
			return fail(fmt.Sprintf("Error: failed to read asset %q.", asset.FileName))
		}
		activity.Status = "ok"
		return readAssetExecution{
			ResultText: fmt.Sprintf("The file '%s' is attached in the next message.", asset.FileName),
			MediaPart:  &openrouter.ContentPart{Type: "file", File: &openrouter.FilePart{Filename: asset.FileName, FileData: dataURL(asset.ContentType, body)}},
			Activity:   activity,
			IsPDF:      true,
		}
	}
	return fail(fmt.Sprintf("Error: asset %q has an unsupported kind.", asset.FileName))
}

// syntheticMediaMessage wraps media parts produced by read_asset calls into
// the ONE follow-up user message appended after the tool results.
func syntheticMediaMessage(parts []openrouter.ContentPart) openrouter.Message {
	content := append([]openrouter.ContentPart{{Type: "text", Text: "Attached asset(s) from read_asset:"}}, parts...)
	return openrouter.Message{Role: "user", Content: content}
}

// ---- Usage accumulation across rounds (unit-tested) --------------------------------

// usageTotals sums usage across all tool rounds of one send — the client gets
// ONE usage event and the persisted row carries the totals.
type usageTotals struct {
	seen             bool
	promptTokens     int
	completionTokens int
	reasoningTokens  int
	cost             float64
}

func (t *usageTotals) add(u *openrouter.Usage) {
	if u == nil {
		return
	}
	t.seen = true
	t.promptTokens += u.PromptTokens
	t.completionTokens += u.CompletionTokens
	t.reasoningTokens += u.ReasoningTokens()
	t.cost += u.Cost
}

// toUsage renders the totals as an openrouter.Usage for persistence (nil when
// no round reported usage).
func (t *usageTotals) toUsage() *openrouter.Usage {
	if !t.seen {
		return nil
	}
	return &openrouter.Usage{
		PromptTokens:            t.promptTokens,
		CompletionTokens:        t.completionTokens,
		Cost:                    t.cost,
		CompletionTokensDetails: &openrouter.CompletionTokensDetails{ReasoningTokens: t.reasoningTokens},
	}
}

// ---- The endpoint --------------------------------------------------------------

const chatLabSafeUpstreamError = "The AI provider request failed"
const chatLabToolLimitError = "Tool call limit reached"

// chatRoundOutcome is how one upstream round ended.
type chatRoundOutcome int

const (
	roundFinal         chatRoundOutcome = iota // finish without tool calls → the answer is done
	roundTools                                 // finished requesting tool calls
	roundClientGone                            // client disconnected / stopped / write failed
	roundShutdown                              // server shutting down
	roundUpstreamError                         // mid-stream upstream/transport failure
)

// chatRoundResult carries one round's accumulation.
type chatRoundResult struct {
	outcome          chatRoundOutcome
	toolCalls        []openrouter.ToolCall
	reasoningDetails []json.RawMessage
	roundText        string
	err              error
}

func (h *DrChatLabHandler) SendChatMessage(c *gin.Context) {
	if !h.dbReady(c) || !h.orReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	sessionID, ok := drChatLabSessionID(c)
	if !ok {
		return
	}
	var req models.DrChatSendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// -- Validation (step 1) --------------------------------------------------
	content := req.Content
	if len(content) > drChatLabMaxContentBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Message is too long (64 KiB max)"})
		return
	}
	if strings.TrimSpace(content) == "" && len(req.AttachmentIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Message is empty"})
		return
	}
	if len(req.AttachmentIDs) > drChatLabMaxAttachments {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("At most %d attachments are allowed", drChatLabMaxAttachments)})
		return
	}
	for _, rawID := range req.AttachmentIDs {
		if _, err := uuid.Parse(strings.TrimSpace(rawID)); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Attachment is not ready"})
			return
		}
	}
	if !drChatLabEfforts[req.ReasoningEffort] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid reasoning effort"})
		return
	}

	setupCtx, setupCancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer setupCancel()

	session, err := h.loadSession(setupCtx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Chat not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	// Model must be in the CURRENT filtered catalog (cache refreshes if stale).
	model, err := h.catalogModel(setupCtx, req.Model)
	if err != nil {
		log.Printf("dr chatlab: catalog lookup for send: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to load model catalog"})
		return
	}
	if model == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown or unsupported model"})
		return
	}
	effort := req.ReasoningEffort
	if !model.SupportsReasoning {
		effort = "" // silently ignore effort for models without reasoning
	}

	// -- Persist the user message (step 2) — committed BEFORE upstream ---------
	tx, err := h.pool.Begin(setupCtx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}
	defer func() { _ = tx.Rollback(setupCtx) }()

	var userMessageID string
	err = tx.QueryRow(setupCtx, `
INSERT INTO dr_chat_messages (session_id, role, author_uid, author_email, content, model, reasoning_effort)
VALUES ($1, 'user', $2, lower($3), $4, $5, $6)
RETURNING id`, sessionID, claims.UID, claims.Email, content, model.ID, effort).Scan(&userMessageID)
	if err != nil {
		log.Printf("dr chatlab: insert user message: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	// Bind each attachment: must be ready, unbound, same session, caller-owned.
	// A zero-row update aborts the whole send.
	for _, rawID := range req.AttachmentIDs {
		aid := strings.TrimSpace(rawID)
		tag, err := tx.Exec(setupCtx, `
UPDATE dr_chat_attachments
SET message_id = $1
WHERE id = $2 AND session_id = $3 AND author_uid = $4 AND status = 'ready' AND message_id IS NULL`,
			userMessageID, aid, sessionID, claims.UID)
		if err != nil {
			log.Printf("dr chatlab: bind attachment: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
			return
		}
		if tag.RowsAffected() == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Attachment is not ready"})
			return
		}
	}

	if _, err := tx.Exec(setupCtx, `
UPDATE dr_chat_sessions
SET updated_at = now(), last_model = $1, last_reasoning_effort = $2
WHERE id = $3`, model.ID, effort, sessionID); err != nil {
		log.Printf("dr chatlab: bump session: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}
	if err := tx.Commit(setupCtx); err != nil {
		log.Printf("dr chatlab: commit user message: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	// Auto-title after the first exchange (fire-and-forget; guarded by
	// title_source='default' so a racing manual rename wins).
	go h.autoTitleSession(sessionID, content)

	// -- Assemble the upstream conversation (step 3) ---------------------------
	history, attachmentsByMsg, err := h.loadConversation(setupCtx, sessionID)
	if err != nil {
		log.Printf("dr chatlab: load conversation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}
	fetchBody := func(s3Key string) ([]byte, error) { return h.fetchObject(setupCtx, s3Key) }
	upstreamMessages, err := buildOpenRouterMessages(history, attachmentsByMsg, fetchBody)
	if errors.Is(err, errChatLabInlineBudget) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errChatLabInlineBudget.Error()})
		return
	}
	if err != nil {
		log.Printf("dr chatlab: build upstream messages: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare attachments"})
		return
	}

	// Project sessions get a system prompt (description/instructions/memory/
	// asset manifest) and, when the model can call tools and assets exist, the
	// read_asset tool. General sessions: no system message, no tools — one
	// round, exactly the original behavior.
	toolsEnabled := false
	var projectAssets []storedProjectAsset
	if session.projectID != nil {
		project, assets, perr := h.loadProjectContext(setupCtx, *session.projectID)
		if perr != nil {
			log.Printf("dr chatlab: load project context: %v", perr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load project context"})
			return
		}
		projectAssets = assets
		toolsEnabled = model.SupportsTools && len(assets) > 0
		systemPrompt, perr := buildProjectSystemPrompt(project, assets, model.SupportsTools, fetchBody)
		if perr != nil {
			log.Printf("dr chatlab: build project system prompt: %v", perr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load project context"})
			return
		}
		upstreamMessages = append([]openrouter.Message{{Role: "system", Content: systemPrompt}}, upstreamMessages...)
	}

	// -- Upstream request (step 4) ---------------------------------------------
	chatReq := openrouter.ChatRequest{
		Model:     model.ID,
		Messages:  upstreamMessages,
		MaxTokens: h.cfg.DRChatLabMaxOutputTokens,
	}
	if effort != "" {
		chatReq.Reasoning = &openrouter.Reasoning{Effort: effort}
	}
	needPDFPlugin := hasPDFAttachment(attachmentsByMsg)
	if needPDFPlugin {
		chatReq.Plugins = []openrouter.Plugin{{ID: "file-parser"}}
	}
	if toolsEnabled {
		chatReq.Tools = []openrouter.Tool{readAssetTool}
	}
	maxRounds := h.cfg.DRChatLabToolMaxRounds
	if maxRounds < 1 {
		maxRounds = 1
	}

	// The upstream stream lives on the REQUEST context so a client disconnect
	// (including the Stop button, which just aborts the fetch) cancels it.
	streamCtx, streamCancel := context.WithCancel(c.Request.Context())
	defer streamCancel()

	// First round starts BEFORE any SSE bytes: a failure here is a plain 502
	// JSON response. Upstream details go to the logs only.
	stream, err := h.or.StreamChat(streamCtx, chatReq)
	if err != nil {
		log.Printf("dr chatlab: upstream request failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": chatLabSafeUpstreamError})
		return
	}

	// -- Downstream SSE (step 5) ------------------------------------------------
	assistantMessageID := uuid.NewString()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")

	var contentB, reasoningB strings.Builder
	totals := usageTotals{}
	var toolActivity []models.DrChatToolActivity

	// persist writes the assistant row EXACTLY ONCE per request — every return
	// path below calls it exactly once — and fires the project-memory updater
	// for project sessions with non-empty content.
	persisted := false
	persist := func(status string, errorMessage ...string) {
		if persisted {
			return
		}
		persisted = true
		msg := ""
		if len(errorMessage) > 0 {
			msg = errorMessage[0]
		}
		h.persistAssistantMessage(assistantMessageID, sessionID, model.ID, effort, contentB.String(), reasoningB.String(), status, msg, totals.toUsage(), toolActivity)
		if session.projectID != nil && contentB.Len() > 0 {
			h.triggerMemoryUpdate(*session.projectID)
		}
	}

	if err := writeChatLabEvent(c.Writer, chatLabMetaEvent{Type: "meta", UserMessageID: userMessageID, AssistantMessageID: assistantMessageID}); err != nil {
		stream.Close()
		streamCancel()
		persist("interrupted")
		return
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	// pumpRound drains one upstream stream, forwarding reasoning/content deltas
	// downstream and accumulating tool-call fragments + raw reasoning_details
	// (passed back VERBATIM on the assistant tool-call message, per the
	// reasoning docs).
	pumpRound := func(stream *openrouter.ChatStream) chatRoundResult {
		defer stream.Close()

		type streamItem struct {
			chunk *openrouter.StreamChunk
			err   error
		}
		items := make(chan streamItem, 8)
		go func() {
			defer close(items)
			for {
				chunk, err := stream.Next()
				select {
				case items <- streamItem{chunk: chunk, err: err}:
				case <-streamCtx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()

		acc := newToolCallAccumulator()
		var roundText strings.Builder
		var details []json.RawMessage
		sawToolFinish := false

		finish := func() chatRoundResult {
			if sawToolFinish || acc.hasCalls() {
				// Some providers misreport finish_reason "stop" on tool-call
				// rounds — the presence of finalized calls wins.
				return chatRoundResult{outcome: roundTools, toolCalls: acc.Finalize(), reasoningDetails: details, roundText: roundText.String()}
			}
			return chatRoundResult{outcome: roundFinal, roundText: roundText.String(), reasoningDetails: details}
		}

		for {
			select {
			case <-c.Request.Context().Done():
				return chatRoundResult{outcome: roundClientGone}
			case <-h.shutdownChan():
				return chatRoundResult{outcome: roundShutdown}
			case <-keepalive.C:
				if _, err := io.WriteString(c.Writer, ": ping\n\n"); err != nil {
					return chatRoundResult{outcome: roundClientGone}
				}
				c.Writer.Flush()
			case item, chOpen := <-items:
				if !chOpen {
					return chatRoundResult{outcome: roundClientGone}
				}
				if item.err != nil {
					if errors.Is(item.err, io.EOF) {
						return finish()
					}
					return chatRoundResult{outcome: roundUpstreamError, err: item.err}
				}
				chunk := item.chunk
				for _, choice := range chunk.Choices {
					if rt := choice.Delta.ReasoningText(); rt != "" {
						reasoningB.WriteString(rt)
						if err := writeChatLabEvent(c.Writer, chatLabReasoningEvent{Type: "reasoning", Text: rt}); err != nil {
							return chatRoundResult{outcome: roundClientGone}
						}
					}
					details = append(details, choice.Delta.ReasoningDetails...)
					if choice.Delta.Content != "" {
						contentB.WriteString(choice.Delta.Content)
						roundText.WriteString(choice.Delta.Content)
						if err := writeChatLabEvent(c.Writer, chatLabDeltaEvent{Type: "delta", Text: choice.Delta.Content}); err != nil {
							return chatRoundResult{outcome: roundClientGone}
						}
					}
					for _, tcd := range choice.Delta.ToolCalls {
						acc.Add(tcd)
					}
					if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
						sawToolFinish = true
					}
				}
				if chunk.Usage != nil {
					totals.add(chunk.Usage)
				}
			}
		}
	}

	// -- The bounded round loop (step 6) ------------------------------------------
	for round := 1; ; round++ {
		if round > 1 {
			var rerr error
			stream, rerr = h.or.StreamChat(streamCtx, chatReq)
			if rerr != nil {
				// The SSE body already started — error event, not a 502.
				log.Printf("dr chatlab: upstream round %d request failed: %v", round, rerr)
				persist("error", chatLabSafeUpstreamError)
				_ = writeChatLabEvent(c.Writer, chatLabErrorEvent{Type: "error", Message: chatLabSafeUpstreamError})
				return
			}
		}
		res := pumpRound(stream)

		switch res.outcome {
		case roundClientGone, roundShutdown:
			// Client disconnected / Stop button / server shutdown: cancel
			// upstream, persist the partial as interrupted.
			streamCancel()
			persist("interrupted")
			return

		case roundUpstreamError:
			log.Printf("dr chatlab: upstream stream error: %v", res.err)
			persist("error", chatLabSafeUpstreamError)
			_ = writeChatLabEvent(c.Writer, chatLabErrorEvent{Type: "error", Message: chatLabSafeUpstreamError})
			return

		case roundFinal:
			if totals.seen {
				_ = writeChatLabEvent(c.Writer, chatLabUsageEvent{
					Type:             "usage",
					PromptTokens:     totals.promptTokens,
					CompletionTokens: totals.completionTokens,
					ReasoningTokens:  totals.reasoningTokens,
					CostUsd:          totals.cost,
				})
			}
			persist("complete", "")
			_ = writeChatLabEvent(c.Writer, chatLabDoneEvent{Type: "done", Status: "complete"})
			return

		case roundTools:
			if !toolsEnabled {
				// No tools were offered (general session or tool-less model) —
				// a misbehaving provider hallucinated a call. Treat the text
				// that streamed as the final answer instead of looping.
				log.Printf("dr chatlab: provider returned tool calls without tools enabled (model %s)", model.ID)
				persist("complete", "")
				_ = writeChatLabEvent(c.Writer, chatLabDoneEvent{Type: "done", Status: "complete"})
				return
			}
			if round >= maxRounds {
				persist("error", chatLabToolLimitError)
				_ = writeChatLabEvent(c.Writer, chatLabErrorEvent{Type: "error", Message: chatLabToolLimitError})
				return
			}

			// Assistant tool-call message: this round's text (null when empty),
			// the finalized calls, and the raw reasoning_details VERBATIM.
			assistantMsg := openrouter.Message{Role: "assistant", ToolCalls: res.toolCalls, ReasoningDetails: res.reasoningDetails}
			if res.roundText != "" {
				assistantMsg.Content = res.roundText
			}
			upstreamMessages = append(upstreamMessages, assistantMsg)

			var mediaParts []openrouter.ContentPart
			for _, call := range res.toolCalls {
				if call.Function.Name != "read_asset" {
					activity := models.DrChatToolActivity{Name: call.Function.Name, AssetName: "unknown", Status: "error"}
					toolActivity = append(toolActivity, activity)
					upstreamMessages = append(upstreamMessages, openrouter.Message{
						Role: "tool", ToolCallID: call.ID,
						Content: fmt.Sprintf("Error: unknown tool %q — only read_asset is available.", call.Function.Name),
					})
					continue
				}
				// Announce the read before executing it.
				var announce readAssetArgs
				_ = json.Unmarshal([]byte(call.Function.Arguments), &announce)
				announceName := "unknown"
				if a := findProjectAsset(projectAssets, announce.AssetID); a != nil {
					announceName = a.FileName
				}
				if err := writeChatLabEvent(c.Writer, chatLabToolEvent{Type: "tool", Name: "read_asset", AssetID: announce.AssetID, AssetName: announceName, Status: "running"}); err != nil {
					streamCancel()
					persist("interrupted")
					return
				}

				exec := executeReadAsset(call.Function.Arguments, projectAssets, model, h.cfg.DRChatLabAssetReadCapBytes, fetchBody)
				toolActivity = append(toolActivity, exec.Activity)
				upstreamMessages = append(upstreamMessages, openrouter.Message{Role: "tool", ToolCallID: call.ID, Content: exec.ResultText})
				if exec.MediaPart != nil {
					mediaParts = append(mediaParts, *exec.MediaPart)
				}
				if exec.IsPDF {
					needPDFPlugin = true
				}

				if err := writeChatLabEvent(c.Writer, chatLabToolEvent{Type: "tool", Name: "read_asset", AssetID: exec.Activity.AssetID, AssetName: exec.Activity.AssetName, Status: exec.Activity.Status}); err != nil {
					streamCancel()
					persist("interrupted")
					return
				}
			}
			if len(mediaParts) > 0 {
				upstreamMessages = append(upstreamMessages, syntheticMediaMessage(mediaParts))
			}

			chatReq.Messages = upstreamMessages
			if needPDFPlugin && len(chatReq.Plugins) == 0 {
				chatReq.Plugins = []openrouter.Plugin{{ID: "file-parser"}}
			}
			// Next round continues with the same model/effort/max_tokens.
		}
	}
}

// shutdownChan returns the app context's Done channel (nil-safe; a nil channel
// blocks forever in select — behaves like "no shutdown signal").
func (h *DrChatLabHandler) shutdownChan() <-chan struct{} {
	if h.appCtx == nil {
		return nil
	}
	return h.appCtx.Done()
}

// fetchObject downloads one S3 object's bytes (attachment/asset bodies for
// prompt assembly + tool reads). Bounded by the largest allowed upload plus
// slack.
func (h *DrChatLabHandler) fetchObject(ctx context.Context, key string) ([]byte, error) {
	if h.s3Client == nil || h.cfg == nil {
		return nil, errors.New("attachment storage unavailable")
	}
	out, err := h.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(io.LimitReader(out.Body, drChatLabMaxPDFBytes+(1<<20)))
}

// loadConversation loads the full ordered message history plus the ready,
// bound attachments of user messages.
func (h *DrChatLabHandler) loadConversation(ctx context.Context, sessionID string) ([]storedChatMessage, map[string][]storedChatAttachment, error) {
	rows, err := h.pool.Query(ctx, `
SELECT id, role, content
FROM dr_chat_messages
WHERE session_id = $1
ORDER BY created_at, seq`, sessionID)
	if err != nil {
		return nil, nil, err
	}
	var history []storedChatMessage
	func() {
		defer rows.Close()
		for rows.Next() {
			var m storedChatMessage
			if err := rows.Scan(&m.ID, &m.Role, &m.Content); err != nil {
				return
			}
			history = append(history, m)
		}
	}()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	attRows, err := h.pool.Query(ctx, `
SELECT message_id, kind, file_name, content_type, s3_key
FROM dr_chat_attachments
WHERE session_id = $1 AND message_id IS NOT NULL AND status = 'ready'
ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, nil, err
	}
	attachmentsByMsg := map[string][]storedChatAttachment{}
	func() {
		defer attRows.Close()
		for attRows.Next() {
			var msgID string
			var a storedChatAttachment
			if err := attRows.Scan(&msgID, &a.Kind, &a.FileName, &a.ContentType, &a.S3Key); err != nil {
				continue
			}
			attachmentsByMsg[msgID] = append(attachmentsByMsg[msgID], a)
		}
	}()
	return history, attachmentsByMsg, nil
}

// loadProjectContext loads a project's prompt fields plus its ready assets in
// upload order (the manifest / inline order).
func (h *DrChatLabHandler) loadProjectContext(ctx context.Context, projectID string) (projectPromptInput, []storedProjectAsset, error) {
	var p projectPromptInput
	err := h.pool.QueryRow(ctx, `
SELECT name, description, instructions, memory FROM dr_chat_projects WHERE id = $1`, projectID).
		Scan(&p.Name, &p.Description, &p.Instructions, &p.Memory)
	if err != nil {
		return p, nil, err
	}
	rows, err := h.pool.Query(ctx, `
SELECT id, kind, file_name, content_type, s3_key, size_bytes
FROM dr_chat_project_assets
WHERE project_id = $1 AND status = 'ready'
ORDER BY created_at, id`, projectID)
	if err != nil {
		return p, nil, err
	}
	var assets []storedProjectAsset
	func() {
		defer rows.Close()
		for rows.Next() {
			var a storedProjectAsset
			if err := rows.Scan(&a.ID, &a.Kind, &a.FileName, &a.ContentType, &a.S3Key, &a.SizeBytes); err != nil {
				continue
			}
			assets = append(assets, a)
		}
	}()
	return p, assets, rows.Err()
}

// persistAssistantMessage inserts the assistant row on a terminal condition
// and bumps the session's updated_at. Runs on context.Background() with a
// short timeout so a client disconnect can't cancel the write.
func (h *DrChatLabHandler) persistAssistantMessage(id, sessionID, model, effort, content, reasoning, status, errorMessage string, usage *openrouter.Usage, toolActivity []models.DrChatToolActivity) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var reasoningPtr, errPtr *string
	if reasoning != "" {
		reasoningPtr = &reasoning
	}
	if errorMessage != "" {
		errPtr = &errorMessage
	}
	var promptTokens, completionTokens, reasoningTokens *int
	var costUsd *float64
	if usage != nil {
		pt, ct, rt := usage.PromptTokens, usage.CompletionTokens, usage.ReasoningTokens()
		cost := usage.Cost
		promptTokens, completionTokens, reasoningTokens, costUsd = &pt, &ct, &rt, &cost
	}
	var toolActivityRaw []byte
	if len(toolActivity) > 0 {
		raw, err := json.Marshal(toolActivity)
		if err != nil {
			log.Printf("dr chatlab: marshal tool activity: %v", err)
		} else {
			toolActivityRaw = raw
		}
	}

	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_chat_messages (id, session_id, role, content, reasoning, model, reasoning_effort,
                              status, error_message, prompt_tokens, completion_tokens, reasoning_tokens, total_cost_usd, tool_activity)
VALUES ($1, $2, 'assistant', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		id, sessionID, content, reasoningPtr, model, effort, status, errPtr, promptTokens, completionTokens, reasoningTokens, costUsd, toolActivityRaw); err != nil {
		log.Printf("dr chatlab: persist assistant message: %v", err)
		return
	}
	if _, err := h.pool.Exec(ctx, `UPDATE dr_chat_sessions SET updated_at = now() WHERE id = $1`, sessionID); err != nil {
		log.Printf("dr chatlab: bump session after assistant message: %v", err)
	}
}

// ---- Auto-title (unchanged from the original build) --------------------------------

// autoTitleSession sets the session title after the first exchange. When
// DR_CHATLAB_TITLE_MODEL is configured it makes ONE non-streaming completion
// call; on any failure — or when unset — it falls back to a truncation of the
// first user message. Guarded by WHERE title_source='default' so a manual
// rename racing this goroutine wins.
func (h *DrChatLabHandler) autoTitleSession(sessionID, firstUserMessage string) {
	if h.pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Cheap pre-check so repeat sends don't burn a title-model call: only a
	// still-default title is eligible.
	var titleSource string
	if err := h.pool.QueryRow(ctx, `SELECT title_source FROM dr_chat_sessions WHERE id = $1`, sessionID).Scan(&titleSource); err != nil || titleSource != "default" {
		return
	}

	title := ""
	source := "derived"
	if h.or != nil && strings.TrimSpace(h.cfg.DRChatLabTitleModel) != "" {
		excerpt := firstUserMessage
		if len(excerpt) > drChatLabTitlePromptTruncate {
			excerpt = strings.ToValidUTF8(excerpt[:drChatLabTitlePromptTruncate], "")
		}
		resp, err := h.or.Complete(ctx, openrouter.ChatRequest{
			Model: h.cfg.DRChatLabTitleModel,
			Messages: []openrouter.Message{
				{Role: "system", Content: "Generate a concise 3–6 word title for this conversation. Reply with the title only."},
				{Role: "user", Content: excerpt},
			},
			MaxTokens: 64,
		})
		if err != nil {
			log.Printf("dr chatlab: title generation failed (falling back to derived): %v", err)
		} else if t := sanitizeGeneratedTitle(resp.FirstText()); t != "" {
			title, source = t, "generated"
		}
	}
	if title == "" {
		title = deriveChatTitle(firstUserMessage)
	}

	if _, err := h.pool.Exec(ctx, `
UPDATE dr_chat_sessions SET title = $1, title_source = $2
WHERE id = $3 AND title_source = 'default'`, title, source, sessionID); err != nil {
		log.Printf("dr chatlab: save auto title: %v", err)
	}
}
