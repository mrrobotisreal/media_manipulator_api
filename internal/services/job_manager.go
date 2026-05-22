package services

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type JobManager struct {
	jobs       map[string]*models.ConversionJob
	mu         sync.RWMutex
	progressCh chan models.ProgressUpdate

	// Subscribers receive a snapshot of the job after every mutating call.
	// Keyed by jobID; values are buffered channels owned by the SSE handler.
	// A separate mutex avoids reentrancy with the main jobs lock.
	subMu       sync.Mutex
	subscribers map[string][]chan *models.ConversionJob
}

func NewJobManager() *JobManager {
	jm := &JobManager{
		jobs:        make(map[string]*models.ConversionJob),
		progressCh:  make(chan models.ProgressUpdate, 100),
		subscribers: make(map[string][]chan *models.ConversionJob),
	}
	go jm.handleProgressUpdates()
	return jm
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
	return nil
}

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
