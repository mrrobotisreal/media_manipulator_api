package telemetry

import (
	"strings"
	"testing"
)

// The liveness rules live in SQL text; pin the clauses that make a link die with
// its job (there is no database in unit tests).
func TestShareQueriesPinSafetyClauses(t *testing.T) {
	for name, q := range map[string]string{"resolve": resolveShareQuery, "retention": shareRetentionQuery} {
		for _, want := range []string{"deleted_at IS NULL", "expires_at > now()", "'output_dir'", "'s3_object'"} {
			if !strings.Contains(q, want) {
				t.Errorf("%s query lost %q", name, want)
			}
		}
		for _, bad := range []string{"metadata_report", "upload_dir"} {
			if strings.Contains(q, bad) {
				t.Errorf("%s query must not count %s rows as the result", name, bad)
			}
		}
	}
	if !strings.Contains(resolveShareQuery, "revoked_at IS NULL") || !strings.Contains(resolveShareQuery, "l.expires_at > now()") {
		t.Error("resolve must filter revoked and expired links")
	}
	if !strings.Contains(revokeSharesQuery, "revoked_at IS NULL") {
		t.Error("revoke must only touch live links")
	}
}
