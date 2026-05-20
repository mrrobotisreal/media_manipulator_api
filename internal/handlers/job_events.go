package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// StreamJobEvents serves Server-Sent Events for a single job. The client gets
// one `update` event per state change containing the full job JSON snapshot.
//
// We always send an initial snapshot on connect so the client doesn't have to
// race-condition between Subscribe and the first state mutation. After that
// we just relay whatever notifySubscribers emits, plus a `:keepalive` comment
// every 25 s so middlewares (nginx, AWS ALB) don't terminate an idle stream.
//
// The handler exits when:
//   - the job reaches a terminal status (completed | failed) and the final
//     snapshot has been flushed, OR
//   - the client disconnects (ctx done), OR
//   - the subscriber channel closes for any reason.
func (h *ConversionHandler) StreamJobEvents(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job ID is required"})
		return
	}
	if _, err := h.jobManager.GetJob(jobID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		return
	}

	// SSE response headers. Disable any buffering at the proxy layer.
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	ch := h.jobManager.Subscribe(jobID)
	defer h.jobManager.Unsubscribe(jobID, ch)

	// Send the current snapshot immediately so the client renders without
	// waiting for the next pipeline tick.
	if snap, err := h.jobManager.GetJob(jobID); err == nil {
		_ = writeSSEEvent(c.Writer, "update", snap)
		c.Writer.Flush()
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := c.Request.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := io.WriteString(c.Writer, ": keepalive\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case snapshot, ok := <-ch:
			if !ok {
				return
			}
			if snapshot == nil {
				continue
			}
			if err := writeSSEEvent(c.Writer, "update", snapshot); err != nil {
				return
			}
			c.Writer.Flush()
			// Once the job hits a terminal state, push one final snapshot and
			// close the stream. The client treats stream close as "done".
			if snapshot.Status == models.StatusCompleted || snapshot.Status == models.StatusFailed {
				return
			}
		}
	}
}

// writeSSEEvent emits a single SSE record in the format:
//
//	event: <name>
//	data: <json>
//	\n
//
// Multi-line data is split into multiple `data:` lines per the spec.
func writeSSEEvent(w io.Writer, event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, "\n")
	return err
}
