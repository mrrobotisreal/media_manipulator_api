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
}

func NewJobManager() *JobManager {
	jm := &JobManager{jobs: make(map[string]*models.ConversionJob), progressCh: make(chan models.ProgressUpdate, 100)}
	go jm.handleProgressUpdates()
	return jm
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
	defer jm.mu.Unlock()
	job, exists := jm.jobs[jobID]
	if !exists {
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
	return nil
}

func (jm *JobManager) UpdateJobProgress(jobID string, progress int) error {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	job, exists := jm.jobs[jobID]
	if !exists {
		return fmt.Errorf("job not found")
	}
	if job.Status == models.StatusCompleted || job.Status == models.StatusFailed {
		return nil
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
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
	now := time.Now().UTC()
	job.CompletedAt = &now
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
