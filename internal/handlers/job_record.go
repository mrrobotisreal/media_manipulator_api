package handlers

import (
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// Durable job records.
//
// mm_conversion_jobs is the paper trail behind the in-memory JobManager: what
// ran, for whom, and how it ended. It is written from exactly two places — here,
// once per creation, with the owner facts that only exist while the gin context
// does; and from JobManager itself on lifecycle transitions, with the lifecycle
// facts that only exist inside the job model.
//
// Two boundaries hold:
//
//   - Quota enforcement stays on mm_usage_counters. That ledger is the throttle;
//     these rows are the record. Deriving an allowance decision from job rows
//     would double-charge and race.
//   - JobManager stays the authoritative read path for GET /api/job/:id and its
//     SSE stream. Postgres is written to, never read from, in the status path, so
//     a database outage cannot fail or slow a user's job.

// recordJobCreated writes the initial durable row for a freshly created job.
//
// Call it immediately after CreateJob (and after any SetMode), before the
// pipeline goroutine starts: mm_job_events.job_id references
// mm_conversion_jobs, so the parent row must exist before the first event. Every
// quota-gated creation path goes through this one helper.
func recordJobCreated(c *gin.Context, jm *services.JobManager, job *models.ConversionJob, facts services.JobFacts) {
	if jm == nil || job == nil || c == nil {
		return
	}
	jm.RecordJobCreated(c.Request.Context(), job.ID, jobOwnerFor(c), facts)
}

// jobOwnerFor assembles who is running this job.
//
// The user id comes from the tier resolution, which is the request's one
// authenticated account lookup — never from a client-supplied header. When the
// tier system is switched off entirely (TIER_QUOTA_ENABLED=false) there is no
// resolution and jobs are recorded without an owner, which is the same
// "pre-tier behaviour" that flag buys everywhere else.
func jobOwnerFor(c *gin.Context) services.JobOwner {
	owner := services.JobOwner{
		SessionID: strings.TrimSpace(c.GetHeader("X-MM-Session-ID")),
		VisitorID: strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")),
	}
	if res, ok := middleware.ResolveTier(c); ok {
		owner.Tier = string(res.Tier)
		if res.User != nil {
			owner.UserID = res.User.ID
		}
	}
	return owner
}

// mediaKindFor maps a detected file type onto the mm_conversion_jobs.media_kind
// CHECK domain (image|video|audio|unknown).
//
// Documents are deliberately absent from that domain, so a PDF job records NULL
// rather than violating the constraint — which, for a fire-and-forget writer,
// would silently cost the whole row.
func mediaKindFor(fileType models.FileType) string {
	switch fileType {
	case models.FileTypeImage, models.FileTypeVideo, models.FileTypeAudio, models.FileTypeUnknown:
		return string(fileType)
	default:
		return ""
	}
}

// formatOf reduces a filename to its extension, lowercased and without the dot.
//
// The extension is the only part of a user's filename that is ever persisted:
// "webp", never "tax_return_2025.webp". Anything that does not look like a plain
// extension is dropped.
func formatOf(name string) string {
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

// optionString reads a trimmed, lowercased string option.
func optionString(options map[string]any, key string) string {
	if options == nil {
		return ""
	}
	v, _ := options[key].(string)
	return strings.ToLower(strings.TrimSpace(v))
}

// jobMode is the job's high-level workflow, preferring the value the handler
// already published on the job over the raw client option.
func jobMode(job *models.ConversionJob) string {
	if job == nil {
		return ""
	}
	if mode := strings.TrimSpace(job.Mode); mode != "" {
		return strings.ToLower(mode)
	}
	return optionString(job.Options, "mode")
}

// conversionJobFacts describes a /upload-style job (the multipart path and its
// large-video presign twin, which share every option and every output rule).
// The target format comes from the same getOutputExtension the download filename
// uses, so history cannot disagree with what the visitor received.
func (h *ConversionHandler) conversionJobFacts(job *models.ConversionJob, fileType models.FileType) services.JobFacts {
	if job == nil {
		return services.JobFacts{}
	}
	return services.JobFacts{
		Tool:         conversionToolFor(job),
		Mode:         jobMode(job),
		MediaKind:    mediaKindFor(fileType),
		SourceFormat: formatOf(job.OriginalFile.Name),
		TargetFormat: strings.TrimPrefix(h.getOutputExtension(job), "."),
	}
}

// conversionToolFor names the tool behind a /upload-style job, reusing the
// options.mode vocabulary so a history row reads like the tool page the visitor
// actually used.
func conversionToolFor(job *models.ConversionJob) string {
	if job == nil {
		return ""
	}
	if isTranscribeMode(job) {
		return "transcribe"
	}
	if mode := specializedMode(job); mode != "" {
		return mode
	}
	if mode := jobMode(job); mode != "" {
		return mode
	}
	if op, ok := aiOperation(job); ok {
		return "ai_" + op
	}
	return "convert"
}
