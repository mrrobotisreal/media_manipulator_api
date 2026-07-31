package services

import (
	"sync"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// stubAnalytics collects emitted events in memory. Enabled() always true, so the
// tests exercise the real path rather than the no-op one.
type stubAnalytics struct {
	mu     sync.Mutex
	events []analyticsemit.Event
}

func (s *stubAnalytics) Enabled() bool { return true }

func (s *stubAnalytics) Emit(ev analyticsemit.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *stubAnalytics) snapshot() []analyticsemit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]analyticsemit.Event, len(s.events))
	copy(out, s.events)
	return out
}

func (s *stubAnalytics) named(name string) []analyticsemit.Event {
	var out []analyticsemit.Event
	for _, ev := range s.snapshot() {
		if ev.EventName == name {
			out = append(out, ev)
		}
	}
	return out
}

const (
	testVisitor = "11111111-1111-4111-8111-111111111111"
	testSession = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func analyticsJobManager(t *testing.T) (*JobManager, *stubAnalytics) {
	t.Helper()
	jm := NewJobManager()
	// Deliberately NO recorder: the analytics feed and the durable recorder are
	// independent switches, and a box with no telemetry database must still report
	// product analytics.
	an := &stubAnalytics{}
	jm.SetAnalyticsEmitter(an)
	return jm, an
}

func testOwner() JobOwner {
	return JobOwner{UserID: "u1", VisitorID: testVisitor, SessionID: testSession, Tier: "free"}
}

func testFacts() JobFacts {
	return JobFacts{Tool: "compress", Mode: "compress", MediaKind: "video", SourceFormat: "mov", TargetFormat: "mp4"}
}

func TestEmitsServerJobCreatedWithIdentityAndContext(t *testing.T) {
	jm, an := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "clip.mov", Size: 10, Type: "video/quicktime"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), testFacts())

	created := an.named(analyticsemit.ServerJobCreated)
	if len(created) != 1 {
		t.Fatalf("server_job_created count = %d, want 1", len(created))
	}
	ev := created[0]
	if ev.VisitorID != testVisitor || ev.SessionID != testSession {
		t.Errorf("identity = %q/%q, want %q/%q", ev.VisitorID, ev.SessionID, testVisitor, testSession)
	}
	if ev.Context["tool_slug"] != "compress" {
		t.Errorf("context tool_slug = %v, want the API's tool name", ev.Context["tool_slug"])
	}
	if ev.Context["media_kind"] != "video" {
		t.Errorf("context media_kind = %v, want video", ev.Context["media_kind"])
	}
	if ev.Context["job_id"] != job.ID {
		t.Errorf("context job_id = %v, want %q", ev.Context["job_id"], job.ID)
	}
	if ev.Context["tier"] != "free" {
		t.Errorf("context tier = %v, want free (tier at event time is not reconstructable later)", ev.Context["tier"])
	}
	if ev.Properties["source_format"] != "mov" || ev.Properties["target_format"] != "mp4" {
		t.Errorf("properties = %v, want the source/target formats", ev.Properties)
	}
	// The filename must never appear anywhere in the event.
	for k, v := range ev.Properties {
		if s, ok := v.(string); ok && s == "clip.mov" {
			t.Errorf("properties[%q] carries the raw filename", k)
		}
	}
}

func TestEmitsServerJobCompletedOnceWithTheRememberedOwner(t *testing.T) {
	jm, an := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "clip.mov", Size: 10, Type: "video/quicktime"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), testFacts())

	if err := jm.SetResultSize(job.ID, 4242); err != nil {
		t.Fatalf("SetResultSize: %v", err)
	}
	if err := jm.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	// A second terminal transition — SetResultMetadata and UpdateJobResult both
	// record one on an already-completed job — must NOT produce a second event.
	if err := jm.UpdateJobResult(job.ID, "/api/download/"+job.ID); err != nil {
		t.Fatalf("UpdateJobResult: %v", err)
	}

	completed := an.named(analyticsemit.ServerJobCompleted)
	if len(completed) != 1 {
		t.Fatalf("server_job_completed count = %d, want exactly 1 "+
			"(takeJobOwner deletes on read, which is what makes it at-most-once)", len(completed))
	}
	ev := completed[0]
	// The owner is not on the transition record at all — it is remembered from
	// creation. If that memory broke, this is the assertion that catches it.
	if ev.VisitorID != testVisitor || ev.SessionID != testSession {
		t.Errorf("terminal event identity = %q/%q, want the owner remembered at creation", ev.VisitorID, ev.SessionID)
	}
	if ev.Context["tier"] != "free" {
		t.Errorf("terminal event tier = %v, want free", ev.Context["tier"])
	}
	if ev.Properties["job_id"] != job.ID {
		t.Errorf("job_id = %v, want %q", ev.Properties["job_id"], job.ID)
	}
	if _, ok := ev.Properties["duration_ms"]; !ok {
		t.Error("a completed job must carry duration_ms — server-measured duration is the point of this event")
	}
	if ev.Properties["output_bytes"] != int64(4242) {
		t.Errorf("output_bytes = %v, want 4242", ev.Properties["output_bytes"])
	}
}

func TestEmitsServerJobFailedWithACategoryNotTheMessage(t *testing.T) {
	jm, an := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "clip.mov", Size: 10, Type: "video/quicktime"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), testFacts())

	// A realistic error: it contains an absolute path, which contains the user's
	// filename. This is exactly the string that must not reach the analytics DB.
	const raw = "ffmpeg: /srv/uploads/b7/interview with jane doe.mov: Invalid data found when processing input"
	if err := jm.UpdateJobError(job.ID, raw); err != nil {
		t.Fatalf("UpdateJobError: %v", err)
	}

	failed := an.named(analyticsemit.ServerJobFailed)
	if len(failed) != 1 {
		t.Fatalf("server_job_failed count = %d, want 1", len(failed))
	}
	ev := failed[0]
	if got := ev.Properties["error_type"]; got != analyticsemit.ErrorTypeCorruptInput {
		t.Errorf("error_type = %v, want %q", got, analyticsemit.ErrorTypeCorruptInput)
	}
	for k, v := range ev.Properties {
		if s, ok := v.(string); ok && (s == raw || len(s) > 40) {
			t.Errorf("properties[%q] = %q looks like a raw error message; only a category may be emitted", k, s)
		}
	}
	if len(an.named(analyticsemit.ServerJobCompleted)) != 0 {
		t.Error("a failed job must not also emit server_job_completed")
	}
}

func TestNoTerminalEventWithoutARememberedOwner(t *testing.T) {
	// A job created on a path that never calls RecordJobCreated has no identity to
	// attach. Emitting anyway would mean either dropping it downstream or
	// synthesizing a visitor, and synthesizing visitors corrupts the headline
	// number the whole system exists to produce.
	jm, an := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "clip.mov", Size: 10, Type: "video/quicktime"}, nil)

	if err := jm.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	if got := len(an.snapshot()); got != 0 {
		t.Errorf("emitted %d events for a job with no recorded owner, want 0", got)
	}
}

func TestNonTerminalTransitionsEmitNothing(t *testing.T) {
	jm, an := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "clip.mov", Size: 10, Type: "video/quicktime"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), testFacts())

	if err := jm.UpdateJobStatus(job.ID, models.StatusProcessing); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	if err := jm.UpdateJobProgress(job.ID, 50); err != nil {
		t.Fatalf("UpdateJobProgress: %v", err)
	}

	// Only the creation event. Progress ticks are the client's business — this feed
	// is outcomes.
	if got := len(an.snapshot()); got != 1 {
		t.Errorf("event count = %d, want 1 (creation only)", got)
	}
}

func TestJobManagerWithoutAnAnalyticsEmitterIsSafe(t *testing.T) {
	// The default configuration. Every path below must be a no-op, not a panic.
	jm := NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "clip.mov", Size: 10, Type: "video/quicktime"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), testFacts())
	if err := jm.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	jm.SetAnalyticsEmitter(nil) // also a no-op
}

func TestCleanupOldJobsSweepsRememberedOwners(t *testing.T) {
	// Without this sweep, a job that never reaches a terminal state leaves its
	// owner in the map for the process's lifetime.
	jm, _ := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "clip.mov", Size: 10, Type: "video/quicktime"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), testFacts())

	jm.emitMu.RLock()
	remembered := len(jm.emitOwners)
	jm.emitMu.RUnlock()
	if remembered != 1 {
		t.Fatalf("remembered owners = %d, want 1", remembered)
	}

	// Force the job out of the map (a completed job older than the cutoff), then
	// sweep. maxAge of 0 makes every completed job eligible.
	if err := jm.UpdateJobStatus(job.ID, models.StatusFailed); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	jm.CleanupOldJobs(0)

	jm.emitMu.RLock()
	left := len(jm.emitOwners)
	jm.emitMu.RUnlock()
	if left != 0 {
		t.Errorf("remembered owners after cleanup = %d, want 0", left)
	}
}

func TestExtensionOfEmitsOnlyASafeExtension(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"converted.mp4", "mp4"},
		{"CONVERTED.MP4", "mp4"},
		{"tax_return_2025.webp", "webp"},
		{"no-extension", ""},
		{"", ""},
		{"weird.na me", ""},
		{"trailing.", ""},
		{"archive.tar.gz", "gz"},
		{"long.extensionthatistoolong", ""},
	}
	for _, tc := range tests {
		if got := extensionOf(tc.in); got != tc.want {
			t.Errorf("extensionOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
