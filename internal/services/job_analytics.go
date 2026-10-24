package services

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Server-truth analytics for the job lifecycle.
//
// Three events, all fire-and-forget, all carrying the owner identity remembered at
// creation: `server_job_created` when the API accepts a job, and exactly one of
// `server_job_completed` / `server_job_failed` on the first terminal transition.
//
// WHAT IS DELIBERATELY ABSENT FROM THESE EVENTS:
//
//   - The raw error message. It routinely contains an absolute path, which contains
//     the user's filename. Only a coarse `error_type` goes out; the full text stays
//     in mm_conversion_jobs.error_message, which is the operational record.
//   - The source filename. `output_format` is derived from the result file's
//     extension and nothing else, the same rule mm_conversion_jobs follows.
//   - The client options map. Arbitrary client JSON with no bounded shape.

// emitJobCreated reports that the API accepted a job.
func (jm *JobManager) emitJobCreated(jobID string, owner JobOwner, facts JobFacts) {
	emitter := jm.analytics()
	if emitter == nil {
		return
	}
	props := map[string]any{"job_id": jobID}
	if facts.SourceFormat != "" {
		props["source_format"] = facts.SourceFormat
	}
	if facts.TargetFormat != "" {
		props["target_format"] = facts.TargetFormat
	}
	if facts.Mode != "" {
		props["mode"] = facts.Mode
	}
	emitter.Emit(analyticsemit.Event{
		EventName:  analyticsemit.ServerJobCreated,
		EventTS:    time.Now().UTC(),
		VisitorID:  owner.VisitorID,
		SessionID:  owner.SessionID,
		Context:    analyticsemit.Context(facts.Tool, facts.MediaKind, jobID, owner.Tier),
		Properties: props,
	})
}

// emitTerminal reports the authoritative outcome of a job, once.
//
// The at-most-once guarantee comes from takeJobOwner deleting the remembered
// context on read: recordTransition runs on several state changes and can run more
// than once with a terminal status (SetResultMetadata and UpdateJobResult both
// transition a completed job), and only the first call finds an owner.
func (jm *JobManager) emitTerminal(
	emitter JobAnalytics,
	jobID string,
	status models.JobStatus,
	durationMS *int,
	resultFileName string,
	resultSizeBytes int64,
	errorMessage string,
) {
	if status != models.StatusCompleted && status != models.StatusFailed {
		return
	}
	ctx, ok := jm.takeJobOwner(jobID)
	if !ok {
		// No remembered owner. Either the terminal event already went out, or the
		// job was created on a path that never called RecordJobCreated. Emitting
		// without identity is not an option — the emitter would drop it anyway, and
		// synthesizing ids would fabricate visitors.
		return
	}

	props := map[string]any{"job_id": jobID}
	if durationMS != nil {
		props["duration_ms"] = *durationMS
	}

	name := analyticsemit.ServerJobCompleted
	if status == models.StatusFailed {
		name = analyticsemit.ServerJobFailed
		props["error_type"] = analyticsemit.ErrorType(errorMessage)
	} else {
		if resultSizeBytes > 0 {
			props["output_bytes"] = resultSizeBytes
		}
		if format := extensionOf(resultFileName); format != "" {
			props["output_format"] = format
		}
	}

	emitter.Emit(analyticsemit.Event{
		EventName:  name,
		EventTS:    time.Now().UTC(),
		VisitorID:  ctx.owner.VisitorID,
		SessionID:  ctx.owner.SessionID,
		Context:    analyticsemit.Context(ctx.facts.Tool, ctx.facts.MediaKind, jobID, ctx.owner.Tier),
		Properties: props,
	})
}

// EmitStudioRender reports the part-10 server render telemetry for a Content
// Studio export: what the render actually cost (duration ratio), what encoded
// it (nvenc/x264), how complex the filter graph was, and — on failure — the
// coarse failure class. Fire-and-forget like its neighbours.
//
// Called by the export job runner BEFORE the terminal status update: the
// terminal transition consumes the remembered owner (takeJobOwner), so this
// peeks. All values are buckets/enums; the raw error text never leaves.
func (jm *JobManager) EmitStudioRender(
	jobID string,
	stats StudioRenderStats,
	renderSeconds, timelineSeconds float64,
	renderErr error,
) {
	emitter := jm.analytics()
	if emitter == nil {
		return
	}
	ctx, ok := jm.peekJobOwner(jobID)
	if !ok {
		// No identity, no event — same rule as emitTerminal: synthesizing ids
		// would fabricate visitors, and the emitter drops them anyway.
		return
	}

	props := map[string]any{
		"job_id":            jobID,
		"encoder":           stats.Encoder,
		"filterCountBucket": analyticsemit.FilterCountBucket(stats.FilterCount),
	}
	// Part 20: the output format (h264|hevc|mp3|wav), a closed enum.
	if stats.Format != "" {
		props["format"] = stats.Format
	}
	if timelineSeconds > 0 && renderSeconds >= 0 {
		props["durationRatioBucket"] = analyticsemit.RatioBucket(renderSeconds / timelineSeconds)
	}

	name := analyticsemit.StudioRenderCompleted
	if renderErr != nil {
		name = analyticsemit.StudioRenderFailed
		props["error_type"] = analyticsemit.ErrorType(renderErr.Error())
	}

	emitter.Emit(analyticsemit.Event{
		EventName:  name,
		EventTS:    time.Now().UTC(),
		VisitorID:  ctx.owner.VisitorID,
		SessionID:  ctx.owner.SessionID,
		Context:    analyticsemit.Context(ctx.facts.Tool, ctx.facts.MediaKind, jobID, ctx.owner.Tier),
		Properties: props,
	})
}

// extensionOf reduces a filename to its extension, lowercased and without the dot.
//
// The extension is the ONLY part of a filename that may ever leave this process for
// analytics: "webp", never "tax_return_2025.webp". Anything that does not look like
// a plain short extension is dropped rather than guessed at. Mirrors
// handlers.formatOf, which applies the same rule to the durable job record; the
// duplication is two lines and beats making the handlers package importable from
// here.
func extensionOf(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(strings.TrimSpace(name)), "."))
	if ext == "" || len(ext) > 12 {
		return ""
	}
	for _, r := range ext {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return ext
}
