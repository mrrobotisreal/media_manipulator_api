package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
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

// JobAnalytics is the slice of *analyticsemit.Emitter this package needs, so the
// job model keeps no hard dependency on analytics being configured. Same posture
// as JobRecorder: nil is fully supported and every call is a no-op.
//
// Job lifecycle emission lives HERE rather than in the handler layer because the
// handlers only see creation. Completion and failure happen inside the pipeline,
// long after the request returned and the gin context died, and this is the one
// place that observes every terminal transition exactly once.
type JobAnalytics interface {
	Enabled() bool
	Emit(ev analyticsemit.Event)
}

// jobEmitContext is the owner and tool facts a job's TERMINAL analytics events
// need, remembered from creation.
//
// WHY THIS HAS TO BE REMEMBERED. recordTransition deliberately writes lifecycle
// fields only; the owner columns are set once at creation and preserved by
// UpsertJob's COALESCE clauses. That is right for Postgres and useless for
// analytics, which has no prior row to COALESCE against — an event with no
// visitor_id is dropped by the emitter (synthesizing one would fabricate a visitor).
// So the owner is held in memory for the life of the job, exactly as the job itself
// is. Entries are removed when the terminal event is emitted, and swept alongside
// jm.jobs by CleanupOldJobs, so the map cannot outgrow the job map.
type jobEmitContext struct {
	owner JobOwner
	facts JobFacts
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

	// Product-analytics emission. A separate mutex from recMu because the two are
	// independent switches: the durable recorder and the analytics feed are
	// configured, and can fail, entirely apart from one another.
	emitMu     sync.RWMutex
	emitter    JobAnalytics
	emitOwners map[string]jobEmitContext

	// ownerByJob is the durable-in-memory owner used for result authorization.
	// Unlike emitOwners it is NOT consumed at the terminal event; it lives
	// exactly as long as the job in jm.jobs (swept by CleanupOldJobs). Guarded
	// by emitMu.
	ownerByJob map[string]JobOwner
}

func NewJobManager() *JobManager {
	jm := &JobManager{
		jobs:        make(map[string]*models.ConversionJob),
		progressCh:  make(chan models.ProgressUpdate, 100),
		subscribers: make(map[string][]chan *models.ConversionJob),
		recordCh:    make(chan telemetry.JobUpsert, 256),
		recFlush:    make(chan chan struct{}, 4),
		emitOwners:  make(map[string]jobEmitContext),
		ownerByJob:  make(map[string]JobOwner),
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

// SetAnalyticsEmitter attaches the server-truth analytics feed. Called once at
// boot, matching SetRecorder's wiring style. A nil emitter leaves emission off.
func (jm *JobManager) SetAnalyticsEmitter(e JobAnalytics) {
	if e == nil {
		return
	}
	jm.emitMu.Lock()
	jm.emitter = e
	jm.emitMu.Unlock()
}

// analytics returns the emitter when it is live, else nil.
func (jm *JobManager) analytics() JobAnalytics {
	if jm == nil {
		return nil
	}
	jm.emitMu.RLock()
	e := jm.emitter
	jm.emitMu.RUnlock()
	if e == nil || !e.Enabled() {
		return nil
	}
	return e
}

// rememberJobOwner stores the identity a job's terminal events will need.
func (jm *JobManager) rememberJobOwner(jobID string, owner JobOwner, facts JobFacts) {
	if jm == nil || jobID == "" {
		return
	}
	jm.emitMu.Lock()
	if jm.emitOwners == nil {
		jm.emitOwners = make(map[string]jobEmitContext)
	}
	jm.emitOwners[jobID] = jobEmitContext{owner: owner, facts: facts}
	jm.emitMu.Unlock()
	jm.SetJobOwner(jobID, owner)
}

// SetJobOwner remembers who owns a job for later result authorization.
func (jm *JobManager) SetJobOwner(jobID string, owner JobOwner) {
	if jm == nil || jobID == "" {
		return
	}
	jm.emitMu.Lock()
	if jm.ownerByJob == nil {
		jm.ownerByJob = make(map[string]JobOwner)
	}
	jm.ownerByJob[jobID] = owner
	jm.emitMu.Unlock()
}

// JobOwnerOf returns the recorded owner of a job; ok is false when none was
// recorded (job created before the tier system, or with it switched off).
func (jm *JobManager) JobOwnerOf(jobID string) (JobOwner, bool) {
	if jm == nil || jobID == "" {
		return JobOwner{}, false
	}
	jm.emitMu.RLock()
	defer jm.emitMu.RUnlock()
	o, ok := jm.ownerByJob[jobID]
	return o, ok
}

// takeJobOwner reads and removes a job's remembered identity. Removing on read is
// what makes the terminal event fire at most once per job even though
// recordTransition runs on several state changes: only the first terminal
// transition finds a context.
func (jm *JobManager) takeJobOwner(jobID string) (jobEmitContext, bool) {
	if jm == nil || jobID == "" {
		return jobEmitContext{}, false
	}
	jm.emitMu.Lock()
	defer jm.emitMu.Unlock()
	ctx, ok := jm.emitOwners[jobID]
	if ok {
		delete(jm.emitOwners, jobID)
	}
	return ctx, ok
}

// peekJobOwner reads a job's remembered identity WITHOUT consuming it. For
// emissions that happen strictly before the terminal transition (the studio
// render telemetry) — the terminal server_job_* event still needs the context,
// so a peek must never disturb takeJobOwner's at-most-once behaviour.
func (jm *JobManager) peekJobOwner(jobID string) (jobEmitContext, bool) {
	if jm == nil || jobID == "" {
		return jobEmitContext{}, false
	}
	jm.emitMu.RLock()
	defer jm.emitMu.RUnlock()
	ctx, ok := jm.emitOwners[jobID]
	return ctx, ok
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
	if jobID == "" {
		return
	}

	// Analytics first, and unconditionally on the recorder: the two feeds are
	// independent, and a box with no telemetry database should still be able to
	// report product analytics.
	jm.rememberJobOwner(jobID, owner, facts)
	jm.emitJobCreated(jobID, owner, facts)

	jm.recMu.RLock()
	recorder := jm.recorder
	jm.recMu.RUnlock()
	if recorder == nil {
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
	emitter := jm.analytics()
	if recorder == nil && emitter == nil {
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
	// Snapshot the analytics inputs while the read lock is held, so nothing below
	// touches the job pointer after the unlock.
	status := job.Status
	resultFileName := job.ResultFileName
	resultSize := job.ResultSizeBytes
	errMsg := job.Error
	jm.mu.RUnlock()

	if recorder != nil {
		jm.enqueueRecord(rec)
	}
	if emitter != nil {
		jm.emitTerminal(emitter, jobID, status, rec.DurationMS, resultFileName, resultSize, errMsg)
	}
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
	return jm.CreateJobWithID(uuid.New().String(), originalFile, options)
}

// CreateJobWithID is CreateJob for a caller that had to pick the id first: the
// upload is saved under uploads/<jobId>/ before the job exists (it needs the
// file to detect its type), so a directory is the unit from the first byte
// (MM22). An empty id falls back to a fresh uuid.
func (jm *JobManager) CreateJobWithID(id string, originalFile models.OriginalFileInfo, options map[string]interface{}) *models.ConversionJob {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if options == nil {
		options = map[string]interface{}{}
	}
	if id == "" {
		id = uuid.New().String()
	}
	job := &models.ConversionJob{
		ID:           id,
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

// SetResultMime records the sniffed MIME type of the job's result file.
func (jm *JobManager) SetResultMime(jobID, mime string) error {
	jm.mu.Lock()
	job, ok := jm.jobs[jobID]
	if !ok {
		jm.mu.Unlock()
		return fmt.Errorf("job not found")
	}
	job.ResultMime = mime
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
	cutoff := time.Now().UTC().Add(-maxAge)
	for jobID, job := range jm.jobs {
		if job.CompletedAt != nil && job.CompletedAt.Before(cutoff) {
			delete(jm.jobs, jobID)
		}
	}
	// Snapshot the surviving ids so the analytics sweep below does not need the
	// jobs lock while it takes the emit lock (two locks, always in this order).
	live := make(map[string]struct{}, len(jm.jobs))
	for jobID := range jm.jobs {
		live[jobID] = struct{}{}
	}
	jm.mu.Unlock()

	// Drop remembered owners for jobs that no longer exist. Normally the terminal
	// emit removes the entry itself; this covers the jobs that never reach a
	// terminal state — without it, an abandoned job's identity would sit in this
	// map for the process's lifetime. Bounding it by jm.jobs means the map can
	// never be larger than the job map it shadows.
	jm.emitMu.Lock()
	for jobID := range jm.emitOwners {
		if _, ok := live[jobID]; !ok {
			delete(jm.emitOwners, jobID)
		}
	}
	for jobID := range jm.ownerByJob {
		if _, ok := live[jobID]; !ok {
			delete(jm.ownerByJob, jobID)
		}
	}
	jm.emitMu.Unlock()
}
