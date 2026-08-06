package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// The 409 payload is a contract with the client's save-state machine
// (lib/studio/saveState.ts): {error, currentRevision, updatedAt}. "Take over"
// re-sends with expectedRevision = currentRevision, so the value must be the
// live server revision, verbatim.
func TestStudioRevisionConflictBody(t *testing.T) {
	updated := time.Date(2026, 8, 5, 9, 30, 15, 123456789, time.FixedZone("PDT", -7*3600))
	body := studioRevisionConflictBody(&services.StudioRevisionConflictError{
		CurrentRevision: 42,
		UpdatedAt:       updated,
	})

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Error           string `json:"error"`
		CurrentRevision int64  `json:"currentRevision"`
		UpdatedAt       string `json:"updatedAt"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Error != "revision_conflict" {
		t.Errorf("error = %q, want revision_conflict", got.Error)
	}
	if got.CurrentRevision != 42 {
		t.Errorf("currentRevision = %d, want 42", got.CurrentRevision)
	}
	parsed, err := time.Parse(time.RFC3339Nano, got.UpdatedAt)
	if err != nil {
		t.Fatalf("updatedAt %q is not RFC3339: %v", got.UpdatedAt, err)
	}
	if !parsed.Equal(updated) {
		t.Errorf("updatedAt = %v, want %v", parsed, updated)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("updatedAt must be emitted in UTC, got %v", parsed.Location())
	}
}
