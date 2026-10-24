package telemetry

import (
	"strings"
	"testing"
)

// No DB in tests: pin the semantics of the un-park SQL. Owners with no settings
// row or delete_metadata_reports = true are released; explicit false is not.
func TestUnparkQueryReleasesDefaultAndOptInOwners(t *testing.T) {
	q := unparkMetadataQuery
	for _, want := range []string{
		"COALESCE((SELECT s.delete_metadata_reports FROM mm_user_settings s WHERE s.user_id = o.owner_user_id), true)",
		"o.kind = 'metadata_report' AND o.keep_with_account AND o.deleted_at IS NULL",
		"o.owner_user_id IS NOT NULL",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lost %q", want)
		}
	}
	if strings.Contains(q, "JOIN") || strings.Contains(q, "FROM mm_user_settings s\nWHERE") {
		t.Error("an inner join would skip owners with no settings row")
	}
}
