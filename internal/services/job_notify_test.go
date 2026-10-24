package services

import (
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type recNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (n *recNotifier) AIJobFinished(userID, jobID string, failed bool) {
	n.mu.Lock()
	n.calls = append(n.calls, userID+"/"+jobID)
	n.mu.Unlock()
}

func (n *recNotifier) count() int { n.mu.Lock(); defer n.mu.Unlock(); return len(n.calls) }

// finishedJob creates a job that ran `ran`, with an AI tool and an owner, then
// completes it.
func finishedJob(t *testing.T, jm *JobManager, ran time.Duration, owner JobOwner, tool string, finish bool) string {
	t.Helper()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "x.png"}, map[string]interface{}{})
	jm.mu.Lock()
	jm.jobs[job.ID].CreatedAt = time.Now().UTC().Add(-ran)
	jm.mu.Unlock()
	jm.rememberJobOwner(job.ID, owner, JobFacts{Tool: tool})
	if finish {
		if err := jm.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
			t.Fatal(err)
		}
	}
	return job.ID
}

var signedIn = JobOwner{UserID: "user-1", Tier: "free"}

func TestNotifyLongUnwatchedAIJobOnce(t *testing.T) {
	jm := NewJobManager()
	n := &recNotifier{}
	jm.SetJobFinishedNotifier(n)
	id := finishedJob(t, jm, 3*time.Minute, signedIn, "video_restore", true)
	if n.count() != 1 {
		t.Fatalf("notified %d times, want 1", n.count())
	}
	// Later transitions of the same terminal job (SetResultMetadata etc.) must not re-notify.
	_ = jm.UpdateJobStatus(id, models.StatusCompleted)
	jm.recordTransition(id)
	if n.count() != 1 {
		t.Errorf("re-notified: %d", n.count())
	}
}

func TestNoNotifyWhenTabWasPolling(t *testing.T) {
	jm := NewJobManager()
	n := &recNotifier{}
	jm.SetJobFinishedNotifier(n)
	id := finishedJob(t, jm, 3*time.Minute, signedIn, "video_restore", false)
	jm.touchPollAt(id, time.Now().Add(-20*time.Second))
	_ = jm.UpdateJobStatus(id, models.StatusCompleted)
	if n.count() != 0 {
		t.Fatal("notified although a poll was 20 s old")
	}
	if _, ok := jm.LastJobPoll(id); !ok {
		t.Error("poll time not recorded")
	}
	// A poll older than the quiet window means the tab is gone.
	id2 := finishedJob(t, jm, 3*time.Minute, signedIn, "video_restore", false)
	jm.touchPollAt(id2, time.Now().Add(-90*time.Second))
	_ = jm.UpdateJobStatus(id2, models.StatusCompleted)
	if n.count() != 1 {
		t.Errorf("stale poll should notify: %d", n.count())
	}
}

func TestNoNotifyForShortNonAIOrAnonymousJobs(t *testing.T) {
	jm := NewJobManager()
	n := &recNotifier{}
	jm.SetJobFinishedNotifier(n)
	finishedJob(t, jm, 90*time.Second, signedIn, "video_restore", true)                // under 120 s
	finishedJob(t, jm, 5*time.Minute, signedIn, "convert", true)                       // not an AI tool
	finishedJob(t, jm, 5*time.Minute, JobOwner{SessionID: "s"}, "image_restore", true) // anonymous
	if n.count() != 0 {
		t.Fatalf("notified %d, want 0", n.count())
	}
}

func TestNotifyAIOptionsOnConvertJob(t *testing.T) {
	jm := NewJobManager()
	n := &recNotifier{}
	jm.SetJobFinishedNotifier(n)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "a.wav"}, map[string]interface{}{
		"ai": map[string]interface{}{"enabled": true, "operation": "denoise"},
	})
	jm.mu.Lock()
	jm.jobs[job.ID].CreatedAt = time.Now().UTC().Add(-4 * time.Minute)
	jm.mu.Unlock()
	jm.SetJobOwner(job.ID, signedIn)
	_ = jm.UpdateJobStatus(job.ID, models.StatusFailed)
	if n.count() != 1 {
		t.Errorf("AI option job: notified %d", n.count())
	}
}

func TestNoNotifierIsInert(t *testing.T) {
	jm := NewJobManager()
	finishedJob(t, jm, 5*time.Minute, signedIn, "video_restore", true) // must not panic
}
