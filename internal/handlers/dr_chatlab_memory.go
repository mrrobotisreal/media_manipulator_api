package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// Project Memory updater — the living, server-maintained summary of a project.
// It is REGENERATED AND REPLACED (never appended) by a single non-streaming
// completion to DR_CHATLAB_MEMORY_MODEL. Regeneration is HASH-GATED and
// nightly: change events (chats, description/instructions edits, asset
// add/remove, feedback) write cheap content hashes to dr_chat_memory_hashes,
// and the nightly job regenerates only projects whose combined fingerprint
// moved (see dr_chatlab_memory_hashes.go + the scheduler in cmd/api/main.go).
// The manual refresh endpoint still regenerates immediately. Updates are
// single-flighted per project: at most one in-flight OpenRouter call, and the
// final state always reflects the latest trigger.

// ----------------------------------------------------------------------- //
// singleflightLatest (pure concurrency helper; unit-tested)
// ----------------------------------------------------------------------- //

// singleflightLatest coalesces triggers per key to at most ONE running fn at a
// time. A trigger that arrives while a run is in flight marks the flight for
// exactly one rerun after the current run finishes — so N rapid triggers
// collapse to the running call plus one trailing call, and the last write
// always reflects the latest state. The zero value is ready to use.
type singleflightLatest struct {
	mu      sync.Mutex
	flights map[string]*sfFlight
}

type sfFlight struct {
	rerun bool
	// done closes when the flight fully drains (the run plus any coalesced
	// rerun) — DoWait blocks on it.
	done chan struct{}
}

// begin registers a new flight for key, or marks the existing one for a rerun
// and returns it (started=false).
func (s *singleflightLatest) begin(key string) (f *sfFlight, started bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flights == nil {
		s.flights = make(map[string]*sfFlight)
	}
	if f, running := s.flights[key]; running {
		f.rerun = true
		return f, false
	}
	f = &sfFlight{done: make(chan struct{})}
	s.flights[key] = f
	return f, true
}

// drive runs the flight's loop: run, then rerun once per coalesced trigger,
// then unregister and release the waiters.
func (s *singleflightLatest) drive(key string, f *sfFlight, run func()) {
	for {
		run()
		s.mu.Lock()
		if f.rerun {
			f.rerun = false
			s.mu.Unlock()
			continue
		}
		delete(s.flights, key)
		close(f.done)
		s.mu.Unlock()
		return
	}
}

// Do schedules run for key. Non-blocking: the run executes on its own
// goroutine. If a run for key is already in flight, it is marked to rerun once
// after it finishes (multiple marks coalesce; the rerun re-executes the
// FLIGHT's original closure).
func (s *singleflightLatest) Do(key string, run func()) {
	f, started := s.begin(key)
	if !started {
		return
	}
	go s.drive(key, f, run)
}

// DoWait schedules run like Do but BLOCKS until key's flight fully drains
// (the run plus any coalesced rerun). When a flight is already running, run is
// never executed — the flight is marked for a rerun of ITS closure and DoWait
// waits for it to drain (callers detect that via state set inside run). Used
// by the nightly memory job so projects regenerate strictly one at a time
// even when a manual refresh races the sweep.
func (s *singleflightLatest) DoWait(key string, run func()) {
	f, started := s.begin(key)
	if !started {
		<-f.done
		return
	}
	s.drive(key, f, run)
}

// ----------------------------------------------------------------------- //
// Memory prompt (pure builder; unit-tested)
// ----------------------------------------------------------------------- //

// drChatLabMemoryInstruction is the fixed system instruction for the memory
// model; %d is interpolated with DR_CHATLAB_MEMORY_MAX_CHARS. It mandates a
// fixed six-section markdown briefing — always all six sections, in this exact
// order — so every project's memory reads the same way. The former standalone
// feedback-distillation instruction now lives inside the "Key learnings &
// principles" charter.
const drChatLabMemoryInstruction = `You maintain the persistent memory for a project workspace: a concise briefing of the durable facts, decisions, preferences, findings, and open questions a new assistant would need to be immediately effective. Rewrite the memory from scratch every run — a full replacement of the previous memory, never an append.

Your output must be EXACTLY six markdown sections, always all six, in this exact order, each starting with a '##' heading:

## Purpose & context
## Current state
## On the horizon
## Key learnings & principles
## Approach & patterns
## Tools & resources

Section charters:
- Purpose & context — what the project is, who's involved, the stack/domain, non-negotiable ground rules.
- Current state — what has been built, decided, or concluded so far (numbered/bulleted; concrete).
- On the horizon — planned or upcoming work and open questions.
- Key learnings & principles — hard-won specifics: pitfalls hit and their fixes, gotchas, constraints discovered, and durable guidance distilled from response feedback (formatting rules, models that under/over-perform for specific task types, instructions that get ignored).
- Approach & patterns — how work is done in this project: conventions, workflows, recurring structures, preferences.
- Tools & resources — services, libraries, models, key identifiers/links that matter here.

Use markdown bullets or short paragraphs inside each section. A section with nothing durable yet must contain exactly "- Nothing notable yet." Do not include conversational filler, greetings, or transcript quotes. Maximum %d characters total.`

// drChatLabMemoryMsgTruncate caps each transcript message fed to the memory
// model.
const drChatLabMemoryMsgTruncate = 2 << 10 // 2 KiB

// memoryTranscriptEntry is one recent project message (oldest→newest when
// handed to the builder).
type memoryTranscriptEntry struct {
	SessionTitle string
	Role         string
	Content      string
}

// memoryFeedbackEntry is one recent response-feedback row (categories are the
// human LABELS, resolved from the ids before building the prompt).
type memoryFeedbackEntry struct {
	Rating     string // "up" | "down"
	Model      string
	Categories []string
	Comment    string
}

// memoryPromptInput gathers everything the memory model sees.
type memoryPromptInput struct {
	Name          string
	Description   string
	Instructions  string
	CurrentMemory string
	Assets        []storedProjectAsset // manifest only: names/kinds/sizes
	Messages      []memoryTranscriptEntry
	Feedback      []memoryFeedbackEntry
	MaxChars      int
}

// buildMemoryPrompt renders the (system, user) messages for the memory
// completion. Pure — unit-tested for the six-section output contract, input
// sectioning, message truncation/prefixes, the feedback section, and the
// char-cap interpolation.
func buildMemoryPrompt(in memoryPromptInput) (system, user string) {
	system = fmt.Sprintf(drChatLabMemoryInstruction, in.MaxChars)

	var b strings.Builder
	fmt.Fprintf(&b, "# Project: %s\n", in.Name)
	if desc := strings.TrimSpace(in.Description); desc != "" {
		b.WriteString("\n## Description\n" + desc + "\n")
	}
	if instr := strings.TrimSpace(in.Instructions); instr != "" {
		b.WriteString("\n## Instructions\n" + instr + "\n")
	}
	if mem := strings.TrimSpace(in.CurrentMemory); mem != "" {
		b.WriteString("\n## Current memory (rewrite this from scratch — do not append)\n" + mem + "\n")
	}
	if len(in.Assets) > 0 {
		b.WriteString("\n## Assets on file\n")
		for _, a := range in.Assets {
			fmt.Fprintf(&b, "- %s (%s, %s)\n", a.FileName, a.Kind, chatLabHumanSize(a.SizeBytes))
		}
	}
	if len(in.Messages) > 0 {
		b.WriteString("\n## Recent messages (oldest first)\n")
		for _, m := range in.Messages {
			content := m.Content
			if len(content) > drChatLabMemoryMsgTruncate {
				content = strings.ToValidUTF8(content[:drChatLabMemoryMsgTruncate], "") + "…"
			}
			fmt.Fprintf(&b, "[%s] %s: %s\n", m.SessionTitle, m.Role, content)
		}
	}
	if len(in.Feedback) > 0 {
		b.WriteString("\n## Response feedback\n")
		for _, f := range in.Feedback {
			thumb := "👍"
			if f.Rating == "down" {
				thumb = "👎"
			}
			line := fmt.Sprintf("[feedback · %s · %s] %s", thumb, f.Model, strings.Join(f.Categories, "; "))
			if strings.TrimSpace(f.Comment) != "" {
				line += fmt.Sprintf(" — %q", f.Comment)
			}
			b.WriteString(line + "\n")
		}
	}
	return system, b.String()
}

// sanitizeProjectMemory trims and hard-caps the model's replacement memory.
// Markdown (the six '##' headings) passes through untouched; when the cap
// bites, truncation happens at a LINE boundary — never mid-heading or
// mid-bullet — with an appended ellipsis line, keeping the result ≤ maxChars.
func sanitizeProjectMemory(raw string, maxChars int) string {
	s := strings.TrimSpace(raw)
	if maxChars <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	// Reserve two runes for the "\n…" suffix, then back off to the last full
	// line so a heading or bullet is never sliced mid-way.
	reserve := maxChars - 2
	if reserve < 1 {
		reserve = 1
	}
	cut := string(runes[:reserve])
	if i := strings.LastIndex(cut, "\n"); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \t\n") + "\n…"
}

// ----------------------------------------------------------------------- //
// Triggers + the run
// ----------------------------------------------------------------------- //

// triggerMemoryUpdate fires a (single-flighted) memory regeneration for the
// project, attributed to the acting user for usage accounting. Fire-and-forget:
// safe to call from request handlers; never blocks. Since the nightly
// hash-gated scheme landed, the ONLY caller is the manual refresh endpoint
// (the nightly job reuses the same machinery via runMemoryUpdateBlocking). A
// coalesced rerun keeps the FIRST trigger's actor — an acceptable attribution
// approximation for back-to-back triggers in a two-person lab.
func (h *DrChatLabHandler) triggerMemoryUpdate(projectID, actorUID, actorEmail string) {
	if h.pool == nil || projectID == "" {
		return
	}
	h.memoryFlights.Do(projectID, func() { _ = h.runMemoryUpdateStamped(projectID, actorUID, actorEmail) })
}

// runMemoryUpdateStamped wraps runMemoryUpdate with fingerprint bookkeeping:
// it snapshots the project's hash-row fingerprint BEFORE generating (changes
// that land mid-generation stay dirty for the next nightly run) and stores it
// as memory_source_hash only on SUCCESS — so both the manual refresh and the
// nightly job leave the same "this state has been summarized" marker, and a
// failed run leaves the old fingerprint in place for the next night's retry.
func (h *DrChatLabHandler) runMemoryUpdateStamped(projectID, actorUID, actorEmail string) error {
	fpCtx, fpCancel := context.WithTimeout(context.Background(), 10*time.Second)
	hashRows, fpErr := h.loadMemoryHashRows(fpCtx, projectID)
	fpCancel()

	if err := h.runMemoryUpdate(projectID, actorUID, actorEmail); err != nil {
		return err
	}
	if fpErr != nil {
		// Generation succeeded but the pre-run snapshot failed: leave the old
		// fingerprint (worst case the nightly job redoes one project).
		log.Printf("dr chatlab memory: fingerprint snapshot for %s: %v", projectID, fpErr)
		return nil
	}
	fingerprint := projectFingerprint(hashRows)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(ctx, `UPDATE dr_chat_projects SET memory_source_hash = $1 WHERE id = $2`, fingerprint, projectID); err != nil {
		log.Printf("dr chatlab memory: store fingerprint for %s: %v", projectID, err)
	}
	return nil
}

// runMemoryUpdate performs one regeneration. Background context with a 120s
// timeout, additionally cancelled on server shutdown. On failure the previous
// memory is left intact (stale memory beats no memory), memory_status is set
// to 'error', and the error is returned so the nightly job can decide to
// retry next night (the manual path ignores it). The completion is recorded
// as a kind='memory' usage event.
func (h *DrChatLabHandler) runMemoryUpdate(projectID, actorUID, actorEmail string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if h.appCtx != nil {
		go func() {
			select {
			case <-h.appCtx.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	setStatus := func(status string) {
		if _, err := h.pool.Exec(ctx, `UPDATE dr_chat_projects SET memory_status = $1 WHERE id = $2`, status, projectID); err != nil {
			log.Printf("dr chatlab memory: set status %s: %v", status, err)
		}
	}

	if h.cfg == nil || strings.TrimSpace(h.cfg.DRChatLabMemoryModel) == "" || h.or == nil {
		setStatus("disabled")
		return errors.New("memory model is not configured")
	}
	setStatus("updating")

	// Gather inputs: project context + asset manifest + the latest messages
	// across ALL of the project's sessions + the latest response feedback.
	project, assets, err := h.loadProjectContext(ctx, projectID)
	if err != nil {
		log.Printf("dr chatlab memory: load project %s: %v", projectID, err)
		setStatus("error")
		return err
	}
	messages, err := h.loadRecentProjectMessages(ctx, projectID, 40)
	if err != nil {
		log.Printf("dr chatlab memory: load messages for %s: %v", projectID, err)
		setStatus("error")
		return err
	}
	feedback, err := h.loadRecentProjectFeedback(ctx, projectID, 20)
	if err != nil {
		log.Printf("dr chatlab memory: load feedback for %s: %v", projectID, err)
		setStatus("error")
		return err
	}

	maxChars := h.cfg.DRChatLabMemoryMaxChars
	if maxChars <= 0 {
		maxChars = 8192
	}
	system, user := buildMemoryPrompt(memoryPromptInput{
		Name:          project.Name,
		Description:   project.Description,
		Instructions:  project.Instructions,
		CurrentMemory: project.Memory,
		Assets:        assets,
		Messages:      messages,
		Feedback:      feedback,
		MaxChars:      maxChars,
	})

	memoryModel := strings.TrimSpace(h.cfg.DRChatLabMemoryModel)
	started := time.Now()
	resp, err := h.or.Complete(ctx, openrouter.ChatRequest{
		Model: memoryModel,
		Messages: []openrouter.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens: 2048,
	})
	elapsedMs := int(time.Since(started).Milliseconds())
	if err != nil {
		log.Printf("dr chatlab memory: completion for %s failed: %v", projectID, err)
		setStatus("error")
		return err
	}

	// The completion happened — record its usage whatever the sanitize
	// outcome below. Background completions are text-in/text-out: they carry a
	// duration and request_type='text', never reasoning/first-token timings.
	{
		nameCopy := project.Name
		requestType := chatLabRequestTypeText
		cost, estimated := estimateCostUSD(resp.Usage, h.cachedCatalogModel(memoryModel))
		h.recordUsageEvent(ctx, usageEventInsert{
			Kind: "memory", Model: memoryModel,
			ProjectID: &projectID, ProjectName: &nameCopy,
			UserUID: actorUID, UserEmail: actorEmail,
			Usage: resp.Usage, CostUSD: cost, CostEstimated: estimated,
			DurationMs: &elapsedMs, RequestType: &requestType,
		})
	}
	memory := sanitizeProjectMemory(resp.FirstText(), maxChars)
	if memory == "" {
		log.Printf("dr chatlab memory: model returned an empty memory for %s", projectID)
		setStatus("error")
		return errors.New("model returned an empty memory")
	}

	// Wholesale REPLACEMENT — never an append.
	if _, err := h.pool.Exec(ctx, `
UPDATE dr_chat_projects
SET memory = $1, memory_updated_at = now(), memory_status = 'idle'
WHERE id = $2`, memory, projectID); err != nil {
		log.Printf("dr chatlab memory: save for %s: %v", projectID, err)
		return err
	}
	return nil
}

// loadRecentProjectMessages returns the latest `limit` messages across the
// project's sessions, oldest→newest.
func (h *DrChatLabHandler) loadRecentProjectMessages(ctx context.Context, projectID string, limit int) ([]memoryTranscriptEntry, error) {
	rows, err := h.pool.Query(ctx, `
SELECT s.title, m.role, m.content
FROM dr_chat_messages m
JOIN dr_chat_sessions s ON s.id = m.session_id
WHERE s.project_id = $1
ORDER BY m.created_at DESC, m.seq DESC
LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	var newestFirst []memoryTranscriptEntry
	func() {
		defer rows.Close()
		for rows.Next() {
			var e memoryTranscriptEntry
			if err := rows.Scan(&e.SessionTitle, &e.Role, &e.Content); err != nil {
				continue
			}
			newestFirst = append(newestFirst, e)
		}
	}()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to chronological order for the prompt.
	out := make([]memoryTranscriptEntry, 0, len(newestFirst))
	for i := len(newestFirst) - 1; i >= 0; i-- {
		out = append(out, newestFirst[i])
	}
	return out, nil
}

// loadRecentProjectFeedback returns the latest `limit` feedback rows for the
// project (via the denormalized project_id), oldest→newest, with category ids
// resolved to their human labels for the prompt.
func (h *DrChatLabHandler) loadRecentProjectFeedback(ctx context.Context, projectID string, limit int) ([]memoryFeedbackEntry, error) {
	rows, err := h.pool.Query(ctx, `
SELECT rating, model, categories, comment
FROM dr_chat_message_feedback
WHERE project_id = $1
ORDER BY updated_at DESC
LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	var newestFirst []memoryFeedbackEntry
	func() {
		defer rows.Close()
		for rows.Next() {
			var e memoryFeedbackEntry
			var categoryIDs []string
			if err := rows.Scan(&e.Rating, &e.Model, &categoryIDs, &e.Comment); err != nil {
				continue
			}
			for _, id := range categoryIDs {
				if label, ok := drChatLabFeedbackCategoryLabels[id]; ok {
					e.Categories = append(e.Categories, label)
				} else {
					e.Categories = append(e.Categories, id)
				}
			}
			newestFirst = append(newestFirst, e)
		}
	}()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]memoryFeedbackEntry, 0, len(newestFirst))
	for i := len(newestFirst) - 1; i >= 0; i-- {
		out = append(out, newestFirst[i])
	}
	return out, nil
}
