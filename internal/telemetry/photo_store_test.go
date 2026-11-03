package telemetry

import (
	"errors"
	"strings"
	"testing"
)

// MM95: the photo SQL runs on the server only; these pin the semantics a
// careless edit would silently break (same approach as vector_store_test.go).

func TestPhotoListNeverSelectsDoc(t *testing.T) {
	if strings.Contains(photoListColumns, "doc,") || strings.HasSuffix(photoListColumns, "doc") {
		t.Fatalf("list columns must not select the doc: %s", photoListColumns)
	}
	if !strings.Contains(photoListSQL, "SELECT "+photoListColumns) || !strings.Contains(photoListSQL, "owner_user_id = $1::uuid") ||
		!strings.Contains(photoListSQL, "deleted_at IS NULL") || !strings.Contains(photoListSQL, "LIMIT $2 OFFSET $3") {
		t.Fatal("the list is owner-scoped, live-only, paged")
	}
	if !strings.HasSuffix(photoFullColumns, ", doc") {
		t.Fatal("single reads carry the doc last (scan order)")
	}
	if !strings.Contains(photoGetSQL, "deleted_at IS NULL") {
		t.Fatal("a soft-deleted project must not be readable")
	}
}

func TestPhotoSaveIsCompareAndSet(t *testing.T) {
	q := photoSaveCASSQL
	for _, want := range []string{"revision = revision + 1", "AND revision = $5", "owner_user_id = $2::uuid", "deleted_at IS NULL", "RETURNING " + photoFullColumns, "updated_at = now()"} {
		if !strings.Contains(q, want) {
			t.Errorf("save statement is missing %q", want)
		}
	}
	if !strings.Contains(photoProbeSQL, "owner_user_id = $2::uuid") {
		t.Error("the conflict probe must stay owner-scoped")
	}
}

func TestPhotoCreateCapIsTransactional(t *testing.T) {
	if !strings.Contains(photoLockSQL, "pg_advisory_xact_lock") {
		t.Error("creates must serialise per owner with a transaction-scoped lock")
	}
	if !strings.Contains(photoCountSQL, "deleted_at IS NULL") || !strings.Contains(photoCountSQL, "owner_user_id = $1::uuid") {
		t.Error("the cap counts the owner's live rows")
	}
	if !strings.Contains(photoInsertSQL, "1, $5") {
		t.Error("a new project starts at revision 1")
	}
}

func TestPhotoDeleteAndDerivativeSQL(t *testing.T) {
	if !strings.Contains(photoSoftDeleteSQL, "deleted_at = coalesce(deleted_at, now())") || !strings.Contains(photoSoftDeleteSQL, "owner_user_id") {
		t.Error("soft delete is idempotent and owner-scoped")
	}
	if !strings.Contains(photoDerivativeUpsertSQL, "ON CONFLICT (project_id, kind, params_hash)") {
		t.Error("derivatives upsert on the unique key")
	}
	if !strings.Contains(photoExportProjectsSQL, "owner_user_id = $1::uuid") || strings.Contains(photoExportProjectsSQL, "doc") {
		t.Error("the data export is owner-scoped and never selects the doc")
	}
	if !strings.Contains(userPhotoProjectIDsSQL, "owner_user_id = $1::uuid") {
		t.Error("account delete lists the user's projects")
	}
}

func TestPhotoCompleteIngestSQL(t *testing.T) {
	for _, want := range []string{"status = 'ready'", "error = NULL", "original_width = $2", "has_alpha = $4", "doc = $5::jsonb", "deleted_at IS NULL"} {
		if !strings.Contains(photoCompleteSQL, want) {
			t.Errorf("complete statement is missing %q", want)
		}
	}
}

func TestPhotoErrors(t *testing.T) {
	var e error = &PhotoProjectLimitError{Limit: 5, Used: 5}
	var le *PhotoProjectLimitError
	if !errors.As(e, &le) || le.Limit != 5 {
		t.Fatal("limit error")
	}
	var c error = &PhotoRevisionConflictError{Revision: 3}
	var ce *PhotoRevisionConflictError
	if !errors.As(c, &ce) || ce.Revision != 3 || !strings.Contains(c.Error(), "3") {
		t.Fatal("conflict error")
	}
	if RetainedPhotoProject != "photo_project" {
		t.Fatal("ledger kind string is part of the migration CHECK")
	}
}
