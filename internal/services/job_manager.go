package services

import (
	"fmt"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"

	"github.com/google/uuid"
)

type JobManager struct {
	jobs       map[string]*models.ConversionJob
	mu         sync.RWMutex
	progressCh chan models.ProgressUpdate
}

func NewJobManager() *JobManager {
	jm := &JobManager{
		jobs:       make(map[string]*models.ConversionJob),
		progressCh: make(chan models.ProgressUpdate, 100),
	}

	// Start progress updater goroutine
	go jm.handleProgressUpdates()

	return jm
}

func (jm *JobManager) CreateJob(originalFile models.OriginalFileInfo, options map[string]interface{}) *models.ConversionJob {
	jm.mu.Lock()
	defer jm.mu.Unlock()

	job := &models.ConversionJob{
		ID:           uuid.New().String(),
		Status:       models.StatusPending,
		Progress:     0,
		OriginalFile: originalFile,
		Options:      options,
		CreatedAt:    time.Now(),
	}

	jm.jobs[job.ID] = job
	return job
}

func (jm *JobManager) GetJob(jobID string) (*models.ConversionJob, error) {
	fmt.Printf("[DEBUG] GetJob called: jobID=%s\n", jobID)
	jm.mu.RLock()
	defer jm.mu.RUnlock()

	job, exists := jm.jobs[jobID]
	if !exists {
		fmt.Printf("[DEBUG] Job not found in GetJob: %s\n", jobID)
		fmt.Printf("[DEBUG] Available jobs: %v\n", func() []string {
			var keys []string
			for k := range jm.jobs {
				keys = append(keys, k)
			}
			return keys
		}())
		return nil, fmt.Errorf("job not found")
	}

	fmt.Printf("[DEBUG] Job found: %s, status=%s, progress=%d%%\n", jobID, job.Status, job.Progress)
	return job, nil
}

func (jm *JobManager) UpdateJobStatus(jobID string, status models.JobStatus) error {
	fmt.Printf("[DEBUG] UpdateJobStatus called: jobID=%s, status=%s\n", jobID, status)
	jm.mu.Lock()
	defer jm.mu.Unlock()

	job, exists := jm.jobs[jobID]
	if !exists {
		fmt.Printf("[DEBUG] Job not found in UpdateJobStatus: %s\n", jobID)
		return fmt.Errorf("job not found")
	}

	job.Status = status
	if status == models.StatusCompleted || status == models.StatusFailed {
		now := time.Now()
		job.CompletedAt = &now
		if status == models.StatusCompleted {
			job.Progress = 100
			fmt.Printf("[DEBUG] Job marked as completed with 100%% progress: %s\n", jobID)
		}
	}

	fmt.Printf("[DEBUG] Job status updated: %s -> %s (progress: %d%%)\n", jobID, status, job.Progress)
	return nil
}

func (jm *JobManager) UpdateJobProgress(jobID string, progress int) error {
	fmt.Printf("[DEBUG] UpdateJobProgress called: jobID=%s, progress=%d\n", jobID, progress)
	jm.mu.Lock()
	defer jm.mu.Unlock()

	job, exists := jm.jobs[jobID]
	if !exists {
		fmt.Printf("[DEBUG] Job not found: %s\n", jobID)
		return fmt.Errorf("job not found")
	}

	// Don't update progress if job is already completed
	if job.Status == models.StatusCompleted || job.Status == models.StatusFailed {
		fmt.Printf("[DEBUG] Job already completed/failed, skipping progress update: %s\n", jobID)
		return nil
	}

	job.Progress = progress
	fmt.Printf("[DEBUG] Job progress updated: %s -> %d%%\n", jobID, progress)
	return nil
}

func (jm *JobManager) UpdateJobResult(jobID string, resultURL string) error {
	jm.mu.Lock()
	defer jm.mu.Unlock()

	job, exists := jm.jobs[jobID]
	if !exists {
		return fmt.Errorf("job not found")
	}

	job.ResultURL = resultURL
	return nil
}

func (jm *JobManager) UpdateJobError(jobID string, errorMsg string) error {
	jm.mu.Lock()
	defer jm.mu.Unlock()

	job, exists := jm.jobs[jobID]
	if !exists {
		return fmt.Errorf("job not found")
	}

	job.Error = errorMsg
	job.Status = models.StatusFailed
	now := time.Now()
	job.CompletedAt = &now
	return nil
}

func (jm *JobManager) SendProgressUpdate(jobID string, progress int) {
	fmt.Printf("[DEBUG] SendProgressUpdate called: jobID=%s, progress=%d\n", jobID, progress)
	select {
	case jm.progressCh <- models.ProgressUpdate{JobID: jobID, Progress: progress}:
		fmt.Printf("[DEBUG] Progress update sent to channel\n")
	default:
		// Channel is full, skip this update
		fmt.Printf("[DEBUG] Progress channel full, skipping update\n")
	}
}

func (jm *JobManager) handleProgressUpdates() {
	fmt.Printf("[DEBUG] Progress update handler started\n")
	for update := range jm.progressCh {
		fmt.Printf("[DEBUG] Processing progress update: jobID=%s, progress=%d\n", update.JobID, update.Progress)
		jm.UpdateJobProgress(update.JobID, update.Progress)
	}
}

// Cleanup old jobs (call this periodically)
func (jm *JobManager) CleanupOldJobs(maxAge time.Duration) {
	jm.mu.Lock()
	defer jm.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	for jobID, job := range jm.jobs {
		if job.CompletedAt != nil && job.CompletedAt.Before(cutoff) {
			delete(jm.jobs, jobID)
		}
	}
}
