package services

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// stubRecorder collects upserts in memory so the recorder wiring can be
// exercised without Postgres.
type stubRecorder struct {
	mu   sync.Mutex
	rows []telemetry.JobUpsert
}

func (s *stubRecorder) UpsertJob(_ context.Context, j telemetry.JobUpsert) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, j)
}

func (s *stubRecorder) snapshot() []telemetry.JobUpsert {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]telemetry.JobUpsert, len(s.rows))
	copy(out, s.rows)
	return out
}

func (s *stubRecorder) last() (telemetry.JobUpsert, bool) {
	rows := s.snapshot()
	if len(rows) == 0 {
		return telemetry.JobUpsert{}, false
	}
	return rows[len(rows)-1], true
}

// recordingJobManager returns a manager wired to a stub recorder.
func recordingJobManager(t *testing.T) (*JobManager, *stubRecorder) {
	t.Helper()
	jm := NewJobManager()
	rec := &stubRecorder{}
	jm.SetRecorder(rec)
	return jm, rec
}

func flush(t *testing.T, jm *JobManager) {
	t.Helper()
	if !jm.flushRecords(2 * time.Second) {
		t.Fatal("timed out waiting for queued job records to be written")
	}
}

func TestRecordJobCreatedCarriesOwnerFacts(t *testing.T) {
	jm, rec := recordingJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "clip.mov", Size: 10, Type: "video/quicktime"}, nil)

	jm.RecordJobCreated(context.Background(), job.ID,
		JobOwner{UserID: "user-1", SessionID: "session-1", VisitorID: "visitor-1", Tier: "premium"},
		JobFacts{Tool: "convert", Mode: "", MediaKind: "video", SourceFormat: "mov", TargetFormat: "mp4"},
	)

	rows := rec.snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected exactly one row for a creation, got %d", len(rows))
	}
	got := rows[0]
	if got.JobID != job.ID {
		t.Errorf("job id: got %q want %q", got.JobID, job.ID)
	}
	if got.UserID != "user-1" || got.SessionID != "session-1" || got.VisitorID != "visitor-1" {
		t.Errorf("owner ids not carried through: %+v", got)
	}
	if got.OwnerTier != "premium" {
		t.Errorf("owner tier: got %q want premium", got.OwnerTier)
	}
	if got.Status != string(models.StatusPending) {
		t.Errorf("status: got %q want pending", got.Status)
	}
	if got.Tool != "convert" || got.MediaKind != "video" || got.SourceFormat != "mov" || got.TargetFormat != "mp4" {
		t.Errorf("facts not carried through: %+v", got)
	}
	// The parent row has to exist before a pipeline's first job event, which is
	// why creation is synchronous: nothing was queued.
	if got.StartedAt == nil {
		t.Error("expected started_at to be set from the job's creation time")
	}
}

func TestCompletionRecordsTimingAndTransitions(t *testing.T) {
	jm, rec := recordingJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "a.png"}, nil)

	if err := jm.UpdateJobStatus(job.ID, models.StatusProcessing); err != nil {
		t.Fatalf("UpdateJobStatus(processing): %v", err)
	}
	if err := jm.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		t.Fatalf("UpdateJobStatus(completed): %v", err)
	}
	flush(t, jm)

	rows := rec.snapshot()
	if len(rows) != 2 {
		t.Fatalf("expected one row per transition, got %d: %+v", len(rows), rows)
	}
	if rows[0].Status != string(models.StatusProcessing) {
		t.Errorf("first transition: got %q want processing", rows[0].Status)
	}
	final := rows[1]
	if final.Status != string(models.StatusCompleted) {
		t.Errorf("second transition: got %q want completed", final.Status)
	}
	if final.CompletedAt == nil {
		t.Error("expected completed_at on completion")
	}
	if final.DurationMS == nil {
		t.Fatal("expected duration_ms on completion")
	}
	if *final.DurationMS < 0 {
		t.Errorf("duration_ms must never be negative, got %d", *final.DurationMS)
	}
	// Owner columns are written once at creation and preserved by UpsertJob's
	// COALESCE clauses, so a transition deliberately carries none.
	if final.UserID != "" || final.OwnerTier != "" {
		t.Errorf("transition should not restate owner facts: %+v", final)
	}
}

func TestFailureRecordsErrorMessage(t *testing.T) {
	jm, rec := recordingJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "a.png"}, nil)

	if err := jm.UpdateJobError(job.ID, "ffmpeg exited 1"); err != nil {
		t.Fatalf("UpdateJobError: %v", err)
	}
	flush(t, jm)

	got, ok := rec.last()
	if !ok {
		t.Fatal("expected a row for a failure")
	}
	if got.Status != string(models.StatusFailed) {
		t.Errorf("status: got %q want failed", got.Status)
	}
	if got.ErrorMessage != "ffmpeg exited 1" {
		t.Errorf("error_message: got %q", got.ErrorMessage)
	}
	if got.CompletedAt == nil {
		t.Error("a failed job is a finished job; expected completed_at")
	}
}

func TestResultMetadataIsRecorded(t *testing.T) {
	jm, rec := recordingJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "a.mp4"}, nil)

	expires := time.Now().UTC().Add(2 * time.Hour)
	if err := jm.SetResultMetadata(job.ID, "results/2026/a.tar.gz", "a_hls.tar.gz", expires); err != nil {
		t.Fatalf("SetResultMetadata: %v", err)
	}
	flush(t, jm)

	got, ok := rec.last()
	if !ok {
		t.Fatal("expected a row for result metadata")
	}
	if got.ResultS3Key != "results/2026/a.tar.gz" || got.ResultFileName != "a_hls.tar.gz" {
		t.Errorf("result metadata not recorded: %+v", got)
	}
	if got.ResultExpires == nil || !got.ResultExpires.Equal(expires) {
		t.Errorf("result_expires_at: got %v want %v", got.ResultExpires, expires)
	}
}

func TestProgressTicksAreNeverRecorded(t *testing.T) {
	jm, rec := recordingJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "a.mp4"}, nil)

	for i := 0; i <= 100; i += 10 {
		if err := jm.UpdateJobProgress(job.ID, i); err != nil {
			t.Fatalf("UpdateJobProgress(%d): %v", i, err)
		}
	}
	jm.SendProgressUpdate(job.ID, 55)
	flush(t, jm)

	if rows := rec.snapshot(); len(rows) != 0 {
		t.Fatalf("progress belongs in memory only; got %d recorded rows: %+v", len(rows), rows)
	}
}

func TestRecorderlessManagerIsANoOp(t *testing.T) {
	// A JobManager with no recorder is a supported configuration (no database
	// configured). Every path must stay silent rather than panic.
	jm := NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "a.png"}, nil)

	jm.RecordJobCreated(context.Background(), job.ID, JobOwner{UserID: "user-1"}, JobFacts{Tool: "convert"})
	if err := jm.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	if err := jm.UpdateJobResult(job.ID, "/api/download/"+job.ID); err != nil {
		t.Fatalf("UpdateJobResult: %v", err)
	}
	if err := jm.UpdateJobError(job.ID, "boom"); err != nil {
		t.Fatalf("UpdateJobError: %v", err)
	}
	if !jm.flushRecords(time.Second) {
		t.Fatal("flush on a recorderless manager should return immediately")
	}
}

func TestRecordsForAnUnknownJobAreDropped(t *testing.T) {
	jm, rec := recordingJobManager(t)
	jm.recordTransition("00000000-0000-0000-0000-000000000000")
	flush(t, jm)
	if rows := rec.snapshot(); len(rows) != 0 {
		t.Fatalf("expected nothing recorded for an unknown job, got %+v", rows)
	}
}
