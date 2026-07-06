package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Realtime "nudge" stream for the DR Communication/Feedback workspace.
//
// This is a SINGLE-PROCESS, in-memory broadcaster (like the job-events
// subscribers): the API runs as one process on the owner's home server, so
// there is deliberately no Redis / multi-process pub-sub. The stream is a
// cache-invalidation ACCELERANT, never the source of truth — the whole feature
// is fully functional with the stream disconnected because the client polls over
// REST as an always-on fallback (see media-manipulator-ui/lib/dr/useFeedback*).
//
// No message payloads travel over SSE — only tiny nudges telling the client
// which queries to invalidate. Ordering/consistency always comes from Postgres.

// drFeedbackEvent is one nudge. Type is "hello" (sent once on connect),
// "message" (a message was sent — ConversationID + ParentID identify what to
// invalidate) or "conversation" (a conversation was created).
type drFeedbackEvent struct {
	Type           string  `json:"type"`
	ConversationID string  `json:"conversationId,omitempty"`
	ParentID       *string `json:"parentId,omitempty"`
}

// drFeedbackBroadcaster fans a marshaled event out to every connected client.
// Subscriber channels are buffered and sends are non-blocking (drop-on-full), so
// a stalled client can never block a message-send path — the dropped nudge is
// covered by that client's polling fallback.
type drFeedbackBroadcaster struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newDrFeedbackBroadcaster() *drFeedbackBroadcaster {
	return &drFeedbackBroadcaster{subs: make(map[chan []byte]struct{})}
}

// Subscribe registers a new subscriber and returns its channel plus an
// idempotent unsubscribe. The channel is closed by unsubscribe; callers must not
// close it themselves.
func (b *drFeedbackBroadcaster) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			// delete + close under the lock so a concurrent Broadcast (which also
			// holds the lock while sending) can never send on a closed channel.
			b.mu.Lock()
			delete(b.subs, ch)
			close(ch)
			b.mu.Unlock()
		})
	}
	return ch, unsubscribe
}

// Broadcast marshals the event once and non-blocking-sends it to every
// subscriber. A full buffer drops the nudge for that subscriber (drop-on-full).
func (b *drFeedbackBroadcaster) Broadcast(event drFeedbackEvent) {
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- payload:
		default:
			// Buffer full — drop. The client's polling fallback recovers the miss.
		}
	}
}

// subscriberCount is used by tests to assert lifecycle behavior.
func (b *drFeedbackBroadcaster) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// StreamFeedbackEvents serves the per-client SSE nudge stream. Follows
// job_events.go faithfully: SSE headers with proxy-buffering disabled, an
// immediate `hello` snapshot so the client knows the stream is live, a
// `:keepalive` comment every 25s so middlewares don't kill an idle stream, and
// clean exit on client disconnect OR server shutdown. Auth is already enforced
// by the /dr group middleware — nothing token-related goes in the URL (the
// browser's EventSource can't set headers, so the client consumes this via
// fetch + ReadableStream with a Bearer header).
func (h *DrFeedbackHandler) StreamFeedbackEvents(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	ch, unsubscribe := h.broadcaster.Subscribe()
	defer unsubscribe()

	// Immediate hello so the client renders "live" without waiting for a nudge.
	if err := writeSSEData(c.Writer, "hello", []byte(`{"type":"hello"}`)); err != nil {
		return
	}
	c.Writer.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := c.Request.Context()
	shutdown := h.shutdownDone()
	for {
		select {
		case <-ctx.Done():
			return
		case <-shutdown:
			// Server is shutting down — close the stream promptly instead of
			// holding the graceful-shutdown timeout open.
			return
		case <-keepalive.C:
			if _, err := io.WriteString(c.Writer, ": keepalive\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case payload, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSEData(c.Writer, "nudge", payload); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}

// shutdownDone returns the app context's Done channel (or a never-closed channel
// if no app context was wired), so the SSE loop can exit on server shutdown.
func (h *DrFeedbackHandler) shutdownDone() <-chan struct{} {
	if h.appCtx == nil {
		return nil // nil channel blocks forever in select — behaves like "no shutdown signal"
	}
	return h.appCtx.Done()
}

// writeSSEData emits one SSE record with the given event name and pre-marshaled
// JSON data (split across multiple `data:` lines if it ever contains newlines).
func writeSSEData(w io.Writer, event string, data []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}
