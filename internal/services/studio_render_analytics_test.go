package services

import (
	"errors"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Part 10 server render telemetry (studio_render_completed/failed). Reuses the
// stubAnalytics harness from job_analytics_test.go.

func studioExportFacts() JobFacts {
	return JobFacts{Tool: "studio_export", Mode: "studio_export", MediaKind: "video", TargetFormat: "mp4"}
}

func TestEmitStudioRenderCompleted(t *testing.T) {
	jm, an := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "export.mp4", Size: 0, Type: "video/mp4"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), studioExportFacts())

	stats := StudioRenderStats{Encoder: "nvenc", FilterCount: 12}
	jm.EmitStudioRender(job.ID, stats, 30, 60, nil)

	events := an.named(analyticsemit.StudioRenderCompleted)
	if len(events) != 1 {
		t.Fatalf("studio_render_completed count = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.VisitorID != testVisitor || ev.SessionID != testSession {
		t.Errorf("identity = %q/%q, want the remembered owner", ev.VisitorID, ev.SessionID)
	}
	if ev.Context["tool_slug"] != "studio_export" || ev.Context["job_id"] != job.ID {
		t.Errorf("context = %v, want studio_export/%s", ev.Context, job.ID)
	}
	if ev.Properties["encoder"] != "nvenc" {
		t.Errorf("encoder = %v, want nvenc", ev.Properties["encoder"])
	}
	if ev.Properties["filterCountBucket"] != "11-25" {
		t.Errorf("filterCountBucket = %v, want 11-25", ev.Properties["filterCountBucket"])
	}
	if ev.Properties["durationRatioBucket"] != "0.5-1x" {
		t.Errorf("durationRatioBucket = %v, want 0.5-1x (30s render / 60s timeline)", ev.Properties["durationRatioBucket"])
	}
	if _, present := ev.Properties["error_type"]; present {
		t.Error("error_type must be absent on the completed event")
	}
}

func TestEmitStudioRenderDoesNotConsumeTheTerminalOwner(t *testing.T) {
	jm, an := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "export.mp4", Size: 0, Type: "video/mp4"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), studioExportFacts())

	jm.EmitStudioRender(job.ID, StudioRenderStats{Encoder: "x264", FilterCount: 3}, 10, 60, nil)
	if err := jm.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}

	// The peek must leave the owner in place for the terminal event.
	if got := len(an.named(analyticsemit.ServerJobCompleted)); got != 1 {
		t.Fatalf("server_job_completed count after render emit = %d, want 1", got)
	}
}

func TestEmitStudioRenderFailedCarriesFailureClassNotRawError(t *testing.T) {
	jm, an := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "export.mp4", Size: 0, Type: "video/mp4"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), studioExportFacts())

	rawErr := errors.New("ffmpeg failed on /uploads/studio_export_x_0.mov: no space left on device")
	jm.EmitStudioRender(job.ID, StudioRenderStats{Encoder: "nvenc", FilterCount: 40}, 5, 60, rawErr)

	events := an.named(analyticsemit.StudioRenderFailed)
	if len(events) != 1 {
		t.Fatalf("studio_render_failed count = %d, want 1", len(events))
	}
	ev := events[0]
	class, ok := ev.Properties["error_type"].(string)
	if !ok || class == "" {
		t.Fatalf("error_type = %v, want a non-empty failure class", ev.Properties["error_type"])
	}
	// The coarse class only — never the raw message, which carries a path.
	for k, v := range ev.Properties {
		if s, isStr := v.(string); isStr && strings.Contains(s, "/uploads/") {
			t.Errorf("properties[%q] leaks the raw error text: %q", k, s)
		}
	}
	if ev.Properties["filterCountBucket"] != "26-50" {
		t.Errorf("filterCountBucket = %v, want 26-50", ev.Properties["filterCountBucket"])
	}
}

func TestEmitStudioRenderWithoutOwnerEmitsNothing(t *testing.T) {
	jm, an := analyticsJobManager(t)
	// No RecordJobCreated: no remembered identity, so nothing may go out.
	jm.EmitStudioRender("nonexistent-job", StudioRenderStats{Encoder: "x264", FilterCount: 1}, 1, 10, nil)
	if got := len(an.snapshot()); got != 0 {
		t.Fatalf("event count = %d, want 0 (no identity, no event)", got)
	}
}

func TestEmitStudioRenderZeroTimelineOmitsRatio(t *testing.T) {
	jm, an := analyticsJobManager(t)
	job := jm.CreateJob(models.OriginalFileInfo{Name: "export.mp4", Size: 0, Type: "video/mp4"}, nil)
	jm.RecordJobCreated(t.Context(), job.ID, testOwner(), studioExportFacts())

	jm.EmitStudioRender(job.ID, StudioRenderStats{Encoder: "x264", FilterCount: 1}, 5, 0, nil)
	events := an.named(analyticsemit.StudioRenderCompleted)
	if len(events) != 1 {
		t.Fatalf("studio_render_completed count = %d, want 1", len(events))
	}
	if _, present := events[0].Properties["durationRatioBucket"]; present {
		t.Error("durationRatioBucket must be omitted for a zero-length timeline")
	}
}

func TestCountFilterStatements(t *testing.T) {
	args := []string{"-y", "-i", "a.mov", "-filter_complex", "one;two;three", "-map", "[v]"}
	if got := countFilterStatements(args); got != 3 {
		t.Errorf("countFilterStatements = %d, want 3", got)
	}
	if got := countFilterStatements([]string{"-y", "-i", "a.mov"}); got != 0 {
		t.Errorf("no graph: countFilterStatements = %d, want 0", got)
	}
}

func TestEncoderAnalyticsName(t *testing.T) {
	if got := encoderAnalyticsName("h264_nvenc"); got != "nvenc" {
		t.Errorf("h264_nvenc → %q, want nvenc", got)
	}
	if got := encoderAnalyticsName("libx264"); got != "x264" {
		t.Errorf("libx264 → %q, want x264", got)
	}
}
