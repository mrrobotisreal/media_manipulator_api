package handlers

import (
	"errors"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// Restore is checkpoint-first BY CONSTRUCTION: the apply step may not run
// until the "Before restore" checkpoint has landed, because overwriting the
// working document without a recoverable snapshot of it is exactly the data
// loss version history exists to prevent.
func TestRestoreStudioVersionCheckpointsFirst(t *testing.T) {
	var order []string
	want := &models.StudioProject{ID: "p1"}

	got, err := restoreStudioVersion(
		func() error {
			order = append(order, "checkpoint")
			return nil
		},
		func() (*models.StudioProject, error) {
			order = append(order, "apply")
			return want, nil
		},
	)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got != want {
		t.Error("restore must return the applied project")
	}
	if len(order) != 2 || order[0] != "checkpoint" || order[1] != "apply" {
		t.Fatalf("order = %v, want [checkpoint apply]", order)
	}
}

// A failed checkpoint must abort the restore entirely — apply may not run —
// and the underlying error must survive wrapping so the handler can still
// branch on ErrStudioNotFound.
func TestRestoreStudioVersionAbortsOnCheckpointFailure(t *testing.T) {
	applied := false
	_, err := restoreStudioVersion(
		func() error { return services.ErrStudioNotFound },
		func() (*models.StudioProject, error) {
			applied = true
			return &models.StudioProject{}, nil
		},
	)
	if applied {
		t.Fatal("apply must not run when the checkpoint failed")
	}
	if err == nil || !errors.Is(err, services.ErrStudioNotFound) {
		t.Fatalf("checkpoint error must propagate (wrapped): %v", err)
	}
}

// Version names: trimmed, capped at 80 runes (not bytes — a byte cut could
// split a multi-byte character and Postgres rejects invalid UTF-8).
func TestClampVersionName(t *testing.T) {
	if got := clampVersionName("  My cut  "); got != "My cut" {
		t.Errorf("trim: got %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := clampVersionName(long); len(got) != studioVersionNameMaxRunes {
		t.Errorf("ASCII clamp: got len %d, want %d", len(got), studioVersionNameMaxRunes)
	}
	multibyte := strings.Repeat("é", 100) // 2 bytes per rune
	got := clampVersionName(multibyte)
	if want := strings.Repeat("é", studioVersionNameMaxRunes); got != want {
		t.Errorf("rune clamp: got %d runes / %d bytes", len([]rune(got)), len(got))
	}
	if got := clampVersionName("   "); got != "" {
		t.Errorf("whitespace-only must clamp to empty, got %q", got)
	}
}
