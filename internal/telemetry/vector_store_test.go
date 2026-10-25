package telemetry

import (
	"strings"
	"testing"
)

// MM43: the vector SQL runs on the server only; these pin the semantics a
// careless edit would silently break (same approach as the Studio revision
// statements in services/studio_repository_revision_test.go).

func TestVectorListNeverSelectsDocument(t *testing.T) {
	if strings.Contains(vectorProjectListColumns, "document,") || strings.HasSuffix(vectorProjectListColumns, "document") {
		t.Fatalf("list columns must not select the document: %s", vectorProjectListColumns)
	}
	if !strings.Contains(vectorProjectListSQL, "SELECT "+vectorProjectListColumns) {
		t.Fatal("the list query must use the list column set")
	}
	if !strings.Contains(vectorProjectListSQL, "owner_uid = $1") {
		t.Fatal("the list is owner-scoped")
	}
	if !strings.HasSuffix(vectorProjectFullColumns, ", document") {
		t.Fatal("single reads carry the document last (scan order)")
	}
}

func TestVectorSaveIsCompareAndSet(t *testing.T) {
	q := vectorProjectSaveCASSQL
	for _, want := range []string{"revision = revision + 1", "AND revision = $7", "owner_uid = $3", "RETURNING " + vectorProjectFullColumns, "updated_at = now()"} {
		if !strings.Contains(q, want) {
			t.Errorf("save statement is missing %q", want)
		}
	}
	if !strings.Contains(vectorProjectProbeSQL, "owner_uid = $2") {
		t.Error("the conflict probe must stay owner-scoped")
	}
}

func TestVectorCreateCapIsTransactional(t *testing.T) {
	if !strings.Contains(vectorProjectLockSQL, "pg_advisory_xact_lock") {
		t.Error("creates must serialise per owner with a transaction-scoped lock")
	}
	if !strings.Contains(vectorProjectCountSQL, "owner_uid = $1") {
		t.Error("the cap counts the owner's rows")
	}
	if !strings.Contains(vectorProjectInsertSQL, "revision") || !strings.Contains(vectorProjectInsertSQL, "1)") {
		t.Error("a new project starts at revision 1")
	}
}

func TestVectorExportCounterSQL(t *testing.T) {
	if !strings.Contains(vectorExportIncrementSQL, "'vector_export'") || !strings.Contains(vectorExportIncrementSQL, "ON CONFLICT (subject_kind, subject_key, metric, day)") {
		t.Errorf("increment must be the mm_usage_counters upsert on vector_export: %s", vectorExportIncrementSQL)
	}
	for _, col := range []string{"project_id", "owner", "tier", "format", "height", "fps", "duration_ms", "created_at"} {
		if !strings.Contains(vectorExportInsertSQL, col) {
			t.Errorf("vector_exports insert is missing %s", col)
		}
	}
	rec := VectorExportRecord{SubjectKind: "user", SubjectKey: "u1"}
	if rec.Owner() != "user:u1" {
		t.Fatalf("owner = %q", rec.Owner())
	}
	// Account deletion anonymises exactly the user's subject.
	if !strings.Contains(anonymizeUserVectorExportsSQL, "owner = 'user:' || $1") {
		t.Error("account deletion must anonymise the user's vector_exports rows")
	}
}

func TestVectorAccountQueries(t *testing.T) {
	if !strings.Contains(userVectorProjectIDsSQL, "owner_uid = $1") {
		t.Error("account deletion lists the user's Motion projects by owner_uid")
	}
	if strings.Contains(vectorExportProjectsSQL, "document,") || !strings.Contains(vectorExportProjectsSQL, "document->>'duration'") {
		t.Error("the account export reads only the duration out of the document")
	}
	if !strings.Contains(previewShrinkQuery, "'vector_project'") {
		t.Error("the retention preview counts Motion projects as projects")
	}
}
