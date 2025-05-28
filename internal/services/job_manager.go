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
	defer jm.mu.Unlock()

	job, exists := jm.jobs[jobID]
	if !exists {
		return fmt.Errorf("job not found")
	}

	job.Status = status
	if status == models.StatusCompleted || status == models.StatusFailed {
		now := time.Now()
		job.CompletedAt = &now
		if status == models.StatusCompleted {
			job.Progress = 100
		}
	}

	return nil
}

func (jm *JobManager) UpdateJobProgress(jobID string, progress int) error {
	jm.mu.Lock()
	defer jm.mu.Unlock()

	job, exists := jm.jobs[jobID]
	if !exists {
		return fmt.Errorf("job not found")
	}

	job.Progress = progress
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
	select {
	case jm.progressCh <- models.ProgressUpdate{JobID: jobID, Progress: progress}:
	default:
		// Channel is full, skip this update
	}
}

func (jm *JobManager) handleProgressUpdates() {
	for update := range jm.progressCh {
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
