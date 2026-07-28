package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// JobRecorder persists a durable record of a job. *telemetry.Store satisfies it
// as-is; tests supply an in-memory stub.
//
// The interface exists so the in-memory job model has no hard dependency on a
// database being present: a nil recorder is a fully supported configuration and
// every record call becomes a no-op.
type JobRecorder interface {
	UpsertJob(ctx context.Context, j telemetry.JobUpsert)
}

// JobOwner is who started a job, as far as the request layer could tell.
// Assembled by the handler layer (the only place a gin context exists) and
// passed in once at creation.
type JobOwner struct {
	// UserID is an mm_users id — server-resolved, unforgeable, and the key the
	// conversion-history query uses. Empty for anonymous callers.
	UserID string
	// SessionID and VisitorID are client-supplied correlation ids.
	SessionID string
	VisitorID string
	// Tier is the caller's tier at creation time ("anonymous"/"free"/"premium").
	Tier string
}

// JobFacts is what the creating handler knows about a job that the in-memory
// model does not carry: which tool ran it and between which formats.
//
// Note what is absent: the raw client options map, and the source filename. The
// options are arbitrary client JSON and the filename is the user's, so neither is
// persisted — SourceFormat carries the extension and nothing else.
type JobFacts struct {
	Tool         string
	Mode         string
	MediaKind    string
	SourceFormat string
	TargetFormat string
}

type JobManager struct {
	jobs       map[string]*models.ConversionJob
	mu         sync.RWMutex
	progressCh chan models.ProgressUpdate

	// Subscribers receive a snapshot of the job after every mutating call.
	// Keyed by jobID; values are buffered channels owned by the SSE handler.
	// A separate mutex avoids reentrancy with the main jobs lock.
	subMu       sync.Mutex
	subscribers map[string][]chan *models.ConversionJob

	// recordCh serialises durable job writes through a single goroutine, so two
	// rapid transitions can never land out of order (a "completed" overtaken by
	// a "processing" would leave a finished job looking stuck). Buffered and
	// non-blocking: a wedged database slows nothing down, it only drops records.
	recMu     sync.RWMutex
	recorder  JobRecorder
	recordCh  chan telemetry.JobUpsert
	recFlush  chan chan struct{}
	recopened bool
}

func NewJobManager() *JobManager {
	jm := &JobManager{
		jobs:        make(map[string]*models.ConversionJob),
		progressCh:  make(chan models.ProgressUpdate, 100),
		subscribers: make(map[string][]chan *models.ConversionJob),
		recordCh:    make(chan telemetry.JobUpsert, 256),
		recFlush:    make(chan chan struct{}, 4),
	}
	go jm.handleProgressUpdates()
	return jm
}

// SetRecorder attaches the durable job writer. Called once at boot, before the
// HTTP server accepts a request; matches the SetJobManager/SetFaceDetectionStore
// wiring style used elsewhere in this package.
func (jm *JobManager) SetRecorder(r JobRecorder) {
	if r == nil {
		return
	}
	jm.recMu.Lock()
	jm.recorder = r
	opened := jm.recopened
	jm.recopened = true
	jm.recMu.Unlock()
	if !opened {
		go jm.drainRecords()
	}
}

// drainRecords is the single consumer of recordCh.
func (jm *JobManager) drainRecords() {
	write := func(rec telemetry.JobUpsert) {
		jm.recMu.RLock()
		recorder := jm.recorder
		jm.recMu.RUnlock()
		if recorder != nil {
			recorder.UpsertJob(context.Background(), rec)
		}
	}
	for {
		select {
		case rec := <-jm.recordCh:
			write(rec)
		case done := <-jm.recFlush:
			// Write everything already queued before signalling, so a flush
			// really means "all prior records are persisted".
			for {
				select {
				case rec := <-jm.recordCh:
					write(rec)
					continue
				default:
				}
				break
			}
			close(done)
		}
	}
}

// enqueueRecord hands one upsert to the writer goroutine. Never blocks.
func (jm *JobManager) enqueueRecord(rec telemetry.JobUpsert) {
	if rec.JobID == "" {
		return
	}
	jm.recMu.RLock()
	recorder := jm.recorder
	jm.recMu.RUnlock()
	if recorder == nil {
		return
	}
	select {
	case jm.recordCh <- rec:
	default:
		// Queue full: drop. The row is a record, not the source of truth.
	}
}

// RecordJobCreated writes the initial durable row for a job, carrying the owner
// facts that exist only in the request. Synchronous on purpose:
// mm_job_events.job_id references mm_conversion_jobs, so the parent row has to
// exist before a pipeline emits its first event. One small INSERT on a request
// that is about to run ffmpeg is free.
func (jm *JobManager) RecordJobCreated(ctx context.Context, jobID string, owner JobOwner, facts JobFacts) {
	jm.recMu.RLock()
	recorder := jm.recorder
	jm.recMu.RUnlock()
	if recorder == nil || jobID == "" {
		return
	}

	status := string(models.StatusPending)
	var startedAt *time.Time
	if job, err := jm.GetJob(jobID); err == nil {
		status = string(job.Status)
		created := job.CreatedAt
		startedAt = &created
	}

	recorder.UpsertJob(ctx, telemetry.JobUpsert{
		JobID:        jobID,
		UserID:       owner.UserID,
		SessionID:    owner.SessionID,
		VisitorID:    owner.VisitorID,
		OwnerTier:    owner.Tier,
		Status:       status,
		Tool:         facts.Tool,
		Mode:         facts.Mode,
		MediaKind:    facts.MediaKind,
		SourceFormat: facts.SourceFormat,
		TargetFormat: facts.TargetFormat,
		StartedAt:    startedAt,
	})
}

// recordTransition queues a durable write for a job's current lifecycle state.
//
// It carries lifecycle fields only — the owner columns are set once at creation
// and preserved by UpsertJob's COALESCE clauses. Callers must have released
// jm.mu first, exactly like notifySubscribers.
func (jm *JobManager) recordTransition(jobID string) {
	jm.recMu.RLock()
	recorder := jm.recorder
	jm.recMu.RUnlock()
	if recorder == nil {
		return
	}
	jm.mu.RLock()
	job, ok := jm.jobs[jobID]
	if !ok {
		jm.mu.RUnlock()
		return
	}
	rec := telemetry.JobUpsert{
		JobID:          jobID,
		Status:         string(job.Status),
		Mode:           job.Mode,
		CompletedAt:    job.CompletedAt,
		ResultS3Key:    job.ResultS3Key,
		ResultFileName: job.ResultFileName,
		ResultExpires:  job.ExpiresAt,
		ErrorMessage:   job.Error,
	}
	if job.CompletedAt != nil && !job.CreatedAt.IsZero() {
		ms := int(job.CompletedAt.Sub(job.CreatedAt).Milliseconds())
		if ms < 0 {
			ms = 0
		}
		rec.DurationMS = &ms
	}
	jm.mu.RUnlock()
	jm.enqueueRecord(rec)
}

// flushRecords blocks until every record queued so far has been handed to the
// recorder. Test-only helper: the queue is deliberately asynchronous in
// production and nothing there needs to wait on it.
func (jm *JobManager) flushRecords(timeout time.Duration) bool {
	jm.recMu.RLock()
	opened := jm.recopened
	jm.recMu.RUnlock()
	if !opened {
		return true
	}
	done := make(chan struct{})
	select {
	case jm.recFlush <- done:
	case <-time.After(timeout):
		return false
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Subscribe returns a channel that receives a fresh snapshot of the job after
// every state change. The channel is buffered so slow consumers don't block
// the pipeline; bursts beyond the buffer are dropped (the consumer can always
// re-fetch via GET /api/job/:id). Call Unsubscribe with the same channel when
// the consumer disconnects.
func (jm *JobManager) Subscribe(jobID string) chan *models.ConversionJob {
	ch := make(chan *models.ConversionJob, 16)
	jm.subMu.Lock()
	jm.subscribers[jobID] = append(jm.subscribers[jobID], ch)
	jm.subMu.Unlock()
	return ch
}

// Unsubscribe removes the subscriber channel and closes it.
func (jm *JobManager) Unsubscribe(jobID string, ch chan *models.ConversionJob) {
	jm.subMu.Lock()
	defer jm.subMu.Unlock()
	subs := jm.subscribers[jobID]
	for i, existing := range subs {
		if existing == ch {
			jm.subscribers[jobID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(jm.subscribers[jobID]) == 0 {
		delete(jm.subscribers, jobID)
	}
	// Drain any pending messages so a Close() doesn't block on a full channel.
	select {
	case <-ch:
	default:
	}
	close(ch)
}

// notifySubscribers snapshots the current job state and fans it out to every
// subscribed channel. Non-blocking: if a subscriber's buffer is full we drop
// the update (consumer can always re-fetch via GET).
//
// IMPORTANT: this function must NOT be called while holding jm.mu, because
// some mutating methods would otherwise deadlock if a subscriber goroutine
// is in the middle of reading. We arrange for callers to release jm.mu first.
func (jm *JobManager) notifySubscribers(jobID string) {
	jm.mu.RLock()
	job, ok := jm.jobs[jobID]
	if !ok {
		jm.mu.RUnlock()
		return
	}
	snapshot := *job
	if snapshot.Stages != nil {
		clone := make([]models.TranscodeJobStage, len(snapshot.Stages))
		copy(clone, snapshot.Stages)
		snapshot.Stages = clone
	}
	jm.mu.RUnlock()

	jm.subMu.Lock()
	subs := jm.subscribers[jobID]
	for _, ch := range subs {
		select {
		case ch <- &snapshot:
		default:
			// subscriber is slow; drop this update
		}
	}
	jm.subMu.Unlock()
}

func (jm *JobManager) CreateJob(originalFile models.OriginalFileInfo, options map[string]interface{}) *models.ConversionJob {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if options == nil {
		options = map[string]interface{}{}
	}
	job := &models.ConversionJob{
		ID:           uuid.New().String(),
		Status:       models.StatusPending,
		Progress:     0,
		OriginalFile: originalFile,
		Options:      options,
		CreatedAt:    time.Now().UTC(),
	}
	jm.jobs[job.ID] = job
	return job
}

func (jm *JobManager) GetJob(jobID string) (*models.ConversionJob, error) {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	job, exists := jm.jobs[jobID]
	if !exists {
		return nil, fmt.Errorf("job not found")
	}
	return job, nil
}

func (jm *JobManager) UpdateJobStatus(jobID string, status models.JobStatus) error {
	jm.mu.Lock()
	job, exists := jm.jobs[jobID]
	if !exists {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	job.Status = status
	if status == models.StatusCompleted || status == models.StatusFailed {
		now := time.Now().UTC()
		job.CompletedAt = &now
		if status == models.StatusCompleted {
			job.Progress = 100
		}
	}
	jm.mu.Unlock()
	jm.notifySubscribers(jobID)
	jm.recordTransition(jobID)
	return nil
}

// UpdateJobProgress deliberately does NOT record. Progress ticks arrive many
// times a second from ffmpeg; they belong in memory and on the SSE stream only.
// The durable row tracks lifecycle, not percentages.
func (jm *JobManager) UpdateJobProgress(jobID string, progress int) error {
	jm.mu.Lock()
	job, exists := jm.jobs[jobID]
	if !exists {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	if job.Status == models.StatusCompleted || job.Status == models.StatusFailed {
		jm.mu.Unlock()
		return nil
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	job.Progress = progress
	jm.mu.Unlock()
	jm.notifySubscribers(jobID)
	return nil
}

func (jm *JobManager) UpdateJobResult(jobID string, resultURL string) error {
	jm.mu.Lock()
	job, exists := jm.jobs[jobID]
	if !exists {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	job.ResultURL = resultURL
	jm.mu.Unlock()
	jm.notifySubscribers(jobID)
	jm.recordTransition(jobID)
	return nil
}

// SetMode marks the job's high-level workflow (e.g. "transcode") so the UI
// can branch on job.mode when polling /api/job/:jobId.
func (jm *JobManager) SetMode(jobID, mode string) error {
	jm.mu.Lock()
	job, ok := jm.jobs[jobID]
	if !ok {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	job.Mode = mode
	jm.mu.Unlock()
	jm.notifySubscribers(jobID)
	return nil
}

// ReplaceStages overwrites the entire stages list and updates currentStage.
// Used by the transcode pipeline once a final stage transitions.
func (jm *JobManager) ReplaceStages(jobID string, stages []models.TranscodeJobStage, current string) error {
	jm.mu.Lock()
	job, ok := jm.jobs[jobID]
	if !ok {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	job.Stages = stages
	job.CurrentStage = current
	jm.mu.Unlock()
	jm.notifySubscribers(jobID)
	return nil
}

// SetTranscodeReport attaches the source probe report to the job so the UI can
// render the probe panel even before the package is ready.
func (jm *JobManager) SetTranscodeReport(jobID string, report *models.VideoProbeResponse) error {
	jm.mu.Lock()
	job, ok := jm.jobs[jobID]
	if !ok {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	job.TranscodeReport = report
	jm.mu.Unlock()
	jm.notifySubscribers(jobID)
	return nil
}

// SetResultMetadata records the S3 key + filename + expiry for a transcode
// result. The download URL itself goes through UpdateJobResult.
func (jm *JobManager) SetResultMetadata(jobID, s3Key, fileName string, expiresAt time.Time) error {
	jm.mu.Lock()
	job, ok := jm.jobs[jobID]
	if !ok {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	job.ResultS3Key = s3Key
	job.ResultFileName = fileName
	if !expiresAt.IsZero() {
		exp := expiresAt
		job.ExpiresAt = &exp
	}
	jm.mu.Unlock()
	jm.notifySubscribers(jobID)
	jm.recordTransition(jobID)
	return nil
}

// SetResultSize records the byte size of the packaged result artifact.
func (jm *JobManager) SetResultSize(jobID string, sizeBytes int64) error {
	jm.mu.Lock()
	job, ok := jm.jobs[jobID]
	if !ok {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	job.ResultSizeBytes = sizeBytes
	jm.mu.Unlock()
	jm.notifySubscribers(jobID)
	return nil
}

func (jm *JobManager) UpdateJobError(jobID string, errorMsg string) error {
	jm.mu.Lock()
	job, exists := jm.jobs[jobID]
	if !exists {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	job.Error = errorMsg
	job.Status = models.StatusFailed
	now := time.Now().UTC()
	job.CompletedAt = &now
	jm.mu.Unlock()
	jm.notifySubscribers(jobID)
	jm.recordTransition(jobID)
	return nil
}

func (jm *JobManager) SendProgressUpdate(jobID string, progress int) {
	select {
	case jm.progressCh <- models.ProgressUpdate{JobID: jobID, Progress: progress}:
	default:
	}
}

func (jm *JobManager) handleProgressUpdates() {
	for update := range jm.progressCh {
		_ = jm.UpdateJobProgress(update.JobID, update.Progress)
	}
}

// ActiveJobIDs returns the set of jobs currently in flight so external
// sweepers (cleanup worker) can avoid deleting their files.
func (jm *JobManager) ActiveJobIDs() map[string]struct{} {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	out := make(map[string]struct{}, len(jm.jobs))
	for id, job := range jm.jobs {
		if job.Status != models.StatusCompleted && job.Status != models.StatusFailed {
			out[id] = struct{}{}
		}
	}
	return out
}

// ActiveCount returns the number of in-flight jobs (helper for metrics).
func (jm *JobManager) ActiveCount() int {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	n := 0
	for _, job := range jm.jobs {
		if job.Status != models.StatusCompleted && job.Status != models.StatusFailed {
			n++
		}
	}
	return n
}

func (jm *JobManager) CleanupOldJobs(maxAge time.Duration) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	cutoff := time.Now().UTC().Add(-maxAge)
	for jobID, job := range jm.jobs {
		if job.CompletedAt != nil && job.CompletedAt.Before(cutoff) {
			delete(jm.jobs, jobID)
		}
	}
}
