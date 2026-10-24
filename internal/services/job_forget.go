package services

import "github.com/mrrobotisreal/media_manipulator_api/internal/models"

// ForgetJob drops a FINISHED job (and its remembered owner) from the in-memory
// map. A job that is still running is left alone and false is returned: the
// sweeper defers active jobs, and pulling the map entry out from under a running
// pipeline would break it.
func (jm *JobManager) ForgetJob(jobID string) bool {
	if jm == nil || jobID == "" {
		return false
	}
	jm.mu.Lock()
	job, ok := jm.jobs[jobID]
	if ok && job.Status != models.StatusCompleted && job.Status != models.StatusFailed {
		jm.mu.Unlock()
		return false
	}
	delete(jm.jobs, jobID)
	jm.mu.Unlock()

	jm.emitMu.Lock()
	delete(jm.emitOwners, jobID)
	delete(jm.ownerByJob, jobID)
	delete(jm.lastPoll, jobID)
	delete(jm.notified, jobID)
	jm.emitMu.Unlock()
	return true
}
