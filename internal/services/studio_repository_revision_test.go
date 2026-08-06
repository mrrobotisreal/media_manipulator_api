package services

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Part 07 save safety. The compare-and-set itself is SQL and needs Postgres to
// exercise end to end (it runs on the server); what IS unit-testable — and what
// a careless refactor would silently break — is the revision semantics encoded
// in the statements and the typed conflict error the handler branches on.

// Every tracks save must bump revision (even a legacy last-write-wins save, so
// a concurrent tab's CAS still detects it), and only the CAS statement may
// predicate on the expected revision.
func TestStudioSaveProjectRevisionSemantics(t *testing.T) {
	if !strings.Contains(studioSaveProjectSQL, "revision = revision + 1") {
		t.Error("legacy save must still increment revision")
	}
	if strings.Contains(studioSaveProjectSQL, "AND revision =") {
		t.Error("legacy save must not compare-and-set (embed back-compat)")
	}
	if !strings.Contains(studioSaveProjectCASSQL, "revision = revision + 1") {
		t.Error("CAS save must increment revision")
	}
	if !strings.Contains(studioSaveProjectCASSQL, "AND revision = $9") {
		t.Error("CAS save must predicate on the expected revision")
	}
}

// Both save statements return the canonical column list, and that list carries
// revision — so a successful save always hands the client its new revision.
func TestStudioSaveProjectReturnsRevision(t *testing.T) {
	if !strings.Contains(studioProjectColumns, "revision") {
		t.Fatal("studioProjectColumns must include revision")
	}
	for name, sql := range map[string]string{
		"legacy": studioSaveProjectSQL,
		"cas":    studioSaveProjectCASSQL,
	} {
		if !strings.Contains(sql, "RETURNING "+studioProjectColumns) {
			t.Errorf("%s save must RETURNING the canonical project columns", name)
		}
	}
}

// The captions sidecar write deliberately bypasses the tracks document: it must
// never bump (or read) revision, or the caption-generate job would make an open
// editor's next CAS save fail with a phantom conflict.
func TestStudioSaveCaptionsLeavesRevisionUntouched(t *testing.T) {
	if strings.Contains(studioSaveCaptionsSQL, "revision") {
		t.Fatal("SaveCaptions must not touch revision")
	}
}

// The handler unwraps the conflict with errors.As, so the typed error must
// survive wrapping and carry the current server state.
func TestStudioRevisionConflictError(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	base := &StudioRevisionConflictError{CurrentRevision: 7, UpdatedAt: now}
	wrapped := fmt.Errorf("save project: %w", base)

	var conflict *StudioRevisionConflictError
	if !errors.As(wrapped, &conflict) {
		t.Fatal("errors.As must find StudioRevisionConflictError through wrapping")
	}
	if conflict.CurrentRevision != 7 || !conflict.UpdatedAt.Equal(now) {
		t.Fatalf("conflict payload lost in transit: %+v", conflict)
	}
	if !strings.Contains(base.Error(), "7") {
		t.Fatalf("error message should mention the current revision: %q", base.Error())
	}
	if errors.Is(wrapped, ErrStudioNotFound) {
		t.Fatal("a revision conflict must never read as not-found")
	}
}
