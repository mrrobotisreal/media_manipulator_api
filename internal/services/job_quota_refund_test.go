package services

import (
	"sync/atomic"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type countingRefunder struct{ n atomic.Int32 }

func (c *countingRefunder) Refund(string) bool { c.n.Add(1); return true }

// A job that fails gives its allowance unit back exactly once, whichever
// failure path marks it: UpdateJobError (handler rejections, ffmpeg errors)
// or UpdateJobStatus(failed) (pipelines that set the status directly).
func TestFailedJobRefundsItsQuotaOnce(t *testing.T) {
	for _, fail := range []struct {
		name string
		do   func(jm *JobManager, id string)
	}{
		{"UpdateJobError", func(jm *JobManager, id string) { _ = jm.UpdateJobError(id, "ffmpeg exited 1") }},
		{"UpdateJobStatus", func(jm *JobManager, id string) { _ = jm.UpdateJobStatus(id, models.StatusFailed) }},
	} {
		t.Run(fail.name, func(t *testing.T) {
			jm := NewJobManager()
			job := jm.CreateJob(models.OriginalFileInfo{Name: "a.mp3"}, nil)
			charge := &countingRefunder{}
			jm.SetJobQuotaCharge(job.ID, charge)

			fail.do(jm, job.ID)
			fail.do(jm, job.ID) // a second terminal write must not refund again
			_ = jm.UpdateJobError(job.ID, "again")

			if got := charge.n.Load(); got != 1 {
				t.Fatalf("refunded %d times, want exactly 1", got)
			}
		})
	}
}

// A job that completes keeps its charge: the unit was earned.
func TestCompletedJobKeepsItsQuota(t *testing.T) {
	jm := NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "a.mp4"}, nil)
	charge := &countingRefunder{}
	jm.SetJobQuotaCharge(job.ID, charge)
	_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	_ = jm.UpdateJobResult(job.ID, "/download/x")
	if got := charge.n.Load(); got != 0 {
		t.Fatalf("a completed job refunded %d units", got)
	}
	if !jm.ForgetJob(job.ID) {
		t.Fatal("completed job should be forgettable")
	}
	jm.emitMu.RLock()
	_, still := jm.quotaByJob[job.ID]
	jm.emitMu.RUnlock()
	if still {
		t.Fatal("ForgetJob must drop the remembered charge")
	}
}

// A job created without a charge (quota off, operator tier) fails harmlessly.
func TestUnchargedJobFailureIsANoop(t *testing.T) {
	jm := NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "a.mp4"}, nil)
	jm.SetJobQuotaCharge(job.ID, nil)
	if err := jm.UpdateJobError(job.ID, "boom"); err != nil {
		t.Fatal(err)
	}
}
