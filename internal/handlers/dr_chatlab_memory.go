package handlers

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/services/openrouter"
)

// Project Memory updater — the living, server-maintained summary of a project.
// It is REGENERATED AND REPLACED (never appended) by a single non-streaming
// completion to DR_CHATLAB_MEMORY_MODEL, fired (fire-and-forget, like
// autoTitleSession) after assistant turns in project chats, after
// description/instructions edits, and on the manual refresh endpoint. Updates
// are single-flighted per project: at most one in-flight OpenRouter call, and
// the final state always reflects the latest trigger.

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
}

// Do schedules run for key. Non-blocking: the run executes on its own
// goroutine. If a run for key is already in flight, it is marked to rerun once
// after it finishes (multiple marks coalesce).
func (s *singleflightLatest) Do(key string, run func()) {
	s.mu.Lock()
	if s.flights == nil {
		s.flights = make(map[string]*sfFlight)
	}
	if f, running := s.flights[key]; running {
		f.rerun = true
		s.mu.Unlock()
		return
	}
	f := &sfFlight{}
	s.flights[key] = f
	s.mu.Unlock()

	go func() {
		for {
			run()
			s.mu.Lock()
			if f.rerun {
				f.rerun = false
				s.mu.Unlock()
				continue
			}
			delete(s.flights, key)
			s.mu.Unlock()
			return
		}
	}()
}

// ----------------------------------------------------------------------- //
// Memory prompt (pure builder; unit-tested)
// ----------------------------------------------------------------------- //

// drChatLabMemoryInstruction is the fixed system instruction for the memory
// model; %d is interpolated with DR_CHATLAB_MEMORY_MAX_CHARS.
const drChatLabMemoryInstruction = "You maintain the persistent memory for a project workspace. Rewrite the memory from scratch as a concise briefing of the most important durable facts, decisions, preferences, findings, and open questions from this project — the context a new assistant would need to be immediately effective. Do not include conversational filler, greetings, or transcript quotes. Plain text or simple markdown lists. Maximum %d characters."

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

// memoryPromptInput gathers everything the memory model sees.
type memoryPromptInput struct {
	Name          string
	Description   string
	Instructions  string
	CurrentMemory string
	Assets        []storedProjectAsset // manifest only: names/kinds/sizes
	Messages      []memoryTranscriptEntry
	MaxChars      int
}

// buildMemoryPrompt renders the (system, user) messages for the memory
// completion. Pure — unit-tested for sectioning, message truncation/prefixes,
// and the char-cap interpolation.
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
	return system, b.String()
}

// sanitizeProjectMemory trims and hard-caps the model's replacement memory.
func sanitizeProjectMemory(raw string, maxChars int) string {
	s := strings.TrimSpace(raw)
	if maxChars > 0 {
		if runes := []rune(s); len(runes) > maxChars {
			s = strings.TrimSpace(string(runes[:maxChars]))
		}
	}
	return s
}

// ----------------------------------------------------------------------- //
// Triggers + the run
// ----------------------------------------------------------------------- //

// triggerMemoryUpdate fires a (single-flighted) memory regeneration for the
// project. Fire-and-forget: safe to call from request handlers and the stream
// loop; never blocks.
func (h *DrChatLabHandler) triggerMemoryUpdate(projectID string) {
	if h.pool == nil || projectID == "" {
		return
	}
	h.memoryFlights.Do(projectID, func() { h.runMemoryUpdate(projectID) })
}

// runMemoryUpdate performs one regeneration. Background context with a 120s
// timeout, additionally cancelled on server shutdown. On failure the previous
// memory is left intact (stale memory beats no memory) and memory_status is
// set to 'error'.
func (h *DrChatLabHandler) runMemoryUpdate(projectID string) {
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
		return
	}
	setStatus("updating")

	// Gather inputs: project context + asset manifest + the latest messages
	// across ALL of the project's sessions.
	project, assets, err := h.loadProjectContext(ctx, projectID)
	if err != nil {
		log.Printf("dr chatlab memory: load project %s: %v", projectID, err)
		setStatus("error")
		return
	}
	messages, err := h.loadRecentProjectMessages(ctx, projectID, 40)
	if err != nil {
		log.Printf("dr chatlab memory: load messages for %s: %v", projectID, err)
		setStatus("error")
		return
	}

	maxChars := h.cfg.DRChatLabMemoryMaxChars
	if maxChars <= 0 {
		maxChars = 4096
	}
	system, user := buildMemoryPrompt(memoryPromptInput{
		Name:          project.Name,
		Description:   project.Description,
		Instructions:  project.Instructions,
		CurrentMemory: project.Memory,
		Assets:        assets,
		Messages:      messages,
		MaxChars:      maxChars,
	})

	resp, err := h.or.Complete(ctx, openrouter.ChatRequest{
		Model: h.cfg.DRChatLabMemoryModel,
		Messages: []openrouter.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens: 2048,
	})
	if err != nil {
		log.Printf("dr chatlab memory: completion for %s failed: %v", projectID, err)
		setStatus("error")
		return
	}
	memory := sanitizeProjectMemory(resp.FirstText(), maxChars)
	if memory == "" {
		log.Printf("dr chatlab memory: model returned an empty memory for %s", projectID)
		setStatus("error")
		return
	}

	// Wholesale REPLACEMENT — never an append.
	if _, err := h.pool.Exec(ctx, `
UPDATE dr_chat_projects
SET memory = $1, memory_updated_at = now(), memory_status = 'idle'
WHERE id = $2`, memory, projectID); err != nil {
		log.Printf("dr chatlab memory: save for %s: %v", projectID, err)
	}
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
