package services

import (
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MM31: "your AI job finished" email support.
//
// A job qualifies when it is an AI operation, ran longer than EmailJobMinDuration,
// has a signed-in owner, and nobody read its status in the last EmailPollQuietWindow
// (a closed tab). The notifier applies the owner's opt-in and the per-job dedupe;
// this file only decides "was anyone watching".

const (
	// EmailJobMinDuration is how long an AI job must have run to be worth an email.
	EmailJobMinDuration = 120 * time.Second
	// EmailPollQuietWindow is how recently a status poll counts as "tab still open".
	EmailPollQuietWindow = 60 * time.Second
)

// JobFinishedNotifier receives long, unwatched AI jobs. Implemented by
// *email.Service; it must return immediately (it runs its work in a goroutine).
type JobFinishedNotifier interface {
	AIJobFinished(userID, jobID string, failed bool)
}

// SetJobFinishedNotifier attaches the notifier. Nil leaves it off.
func (jm *JobManager) SetJobFinishedNotifier(n JobFinishedNotifier) {
	if jm == nil || n == nil {
		return
	}
	jm.emitMu.Lock()
	jm.notifier = n
	jm.emitMu.Unlock()
}

func (jm *JobManager) hasNotifier() bool {
	jm.emitMu.RLock()
	defer jm.emitMu.RUnlock()
	return jm.notifier != nil
}

// TouchJobPoll records that a client just read this job's status. Cheap: one map
// write under emitMu. Called by the status route and by the SSE stream.
func (jm *JobManager) TouchJobPoll(jobID string) {
	jm.touchPollAt(jobID, time.Now())
}

func (jm *JobManager) touchPollAt(jobID string, at time.Time) {
	if jm == nil || jobID == "" {
		return
	}
	jm.emitMu.Lock()
	if jm.lastPoll == nil {
		jm.lastPoll = make(map[string]time.Time)
	}
	jm.lastPoll[jobID] = at
	jm.emitMu.Unlock()
}

// LastJobPoll returns when the job's status was last read.
func (jm *JobManager) LastJobPoll(jobID string) (time.Time, bool) {
	if jm == nil {
		return time.Time{}, false
	}
	jm.emitMu.RLock()
	defer jm.emitMu.RUnlock()
	t, ok := jm.lastPoll[jobID]
	return t, ok
}

// aiTools are the JobFacts.Tool values of the ai_ops-quota routes.
var aiTools = map[string]struct{}{
	"image_restore": {}, "video_restore": {}, "document_scan": {},
	"studio_captions": {}, "studio_derive": {},
	"video_summary": {}, // MM74
}

func toolIsAI(tool string) bool {
	if _, ok := aiTools[tool]; ok {
		return true
	}
	return strings.HasPrefix(tool, "ai_")
}

// optionsEnableAI reports options.ai.enabled with a real operation.
func optionsEnableAI(options map[string]interface{}) bool {
	ai, ok := options["ai"].(map[string]interface{})
	if !ok {
		return false
	}
	enabled, _ := ai["enabled"].(bool)
	op, _ := ai["operation"].(string)
	op = strings.ToLower(strings.TrimSpace(op))
	return enabled && op != "" && op != "none"
}

// maybeNotifyFinished runs on every terminal transition (recordTransition fires
// several times for one job); the notified set makes the callback once-per-job.
func (jm *JobManager) maybeNotifyFinished(jobID string, status models.JobStatus, optsAI bool) {
	if status != models.StatusCompleted && status != models.StatusFailed {
		return
	}
	now := time.Now().UTC()
	jm.emitMu.Lock()
	n := jm.notifier
	if n == nil {
		jm.emitMu.Unlock()
		return
	}
	owner, hasOwner := jm.ownerByJob[jobID]
	fctx, hasFacts := jm.emitOwners[jobID] // peek: emitTerminal consumes it later
	_, already := jm.notified[jobID]
	last, polled := jm.lastPoll[jobID]
	jm.emitMu.Unlock()

	if already || !hasOwner || owner.UserID == "" {
		return
	}
	if !optsAI && !(hasFacts && toolIsAI(fctx.facts.Tool)) {
		return
	}
	jm.mu.RLock()
	job, ok := jm.jobs[jobID]
	var ran time.Duration
	if ok && job.CompletedAt != nil {
		ran = job.CompletedAt.Sub(job.CreatedAt)
	}
	jm.mu.RUnlock()
	if !ok || ran <= EmailJobMinDuration {
		return
	}
	if polled && now.Sub(last) < EmailPollQuietWindow {
		return // the tab is still open
	}
	jm.emitMu.Lock()
	if _, dup := jm.notified[jobID]; dup {
		jm.emitMu.Unlock()
		return
	}
	jm.notified[jobID] = struct{}{}
	jm.emitMu.Unlock()
	n.AIJobFinished(owner.UserID, jobID, status == models.StatusFailed)
}
