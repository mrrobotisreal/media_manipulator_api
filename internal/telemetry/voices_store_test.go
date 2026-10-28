package telemetry

import (
	"strings"
	"testing"
)

// MM77: the voices SQL runs on the server only; these pin the semantics a
// careless edit would silently break.

func TestVoiceReadsAreOwnerScopedAndHideDeleted(t *testing.T) {
	for name, q := range map[string]string{"list": voiceListSQL, "get": voiceGetSQL, "rename": voiceRenameSQL,
		"soft delete": voiceSoftDeleteSQL, "count": voiceCountSQL, "export": voiceExportSQL} {
		if !strings.Contains(q, "user_id = ") {
			t.Errorf("%s must be owner-scoped: %s", name, q)
		}
		if !strings.Contains(q, "deleted_at IS NULL") {
			t.Errorf("%s must ignore soft-deleted voices: %s", name, q)
		}
	}
}

func TestVoiceCreateCapIsTransactional(t *testing.T) {
	if !strings.Contains(voiceLockSQL, "pg_advisory_xact_lock") || !strings.Contains(voiceLockSQL, "'mm_voices:'") {
		t.Fatal("the cap check must hold a per-user transaction lock")
	}
	if !strings.Contains(voiceInsertSQL, "RETURNING "+voiceColumns) {
		t.Fatal("insert returns the full row in scan order")
	}
}

func TestVoiceHardDeleteReturnsKeys(t *testing.T) {
	if !strings.Contains(voiceHardDeleteSQL, "RETURNING coalesce(reference_s3_key, ''), coalesce(preview_s3_key, '')") {
		t.Fatal("the sweeper needs both keys back")
	}
	if strings.Contains(voiceHardDeleteSQL, "deleted_at IS NULL") {
		t.Fatal("the sweeper must also remove soft-deleted rows")
	}
	if !strings.Contains(voiceDeleteUserSQL, "user_id = $1::uuid") {
		t.Fatal("account deletion removes every row of the user")
	}
}

func TestVoiceExportHasNoStorageKeys(t *testing.T) {
	if strings.Contains(voiceExportSQL, "s3_key") {
		t.Fatal("the export must never carry storage keys")
	}
	if RetainedVoice != "voice" {
		t.Fatal("ledger kind must match the migration CHECK")
	}
}

func TestVoiceLimitError(t *testing.T) {
	var err error = &VoiceLimitError{Limit: 1, Used: 1}
	if err.Error() == "" {
		t.Fatal("error text")
	}
}
