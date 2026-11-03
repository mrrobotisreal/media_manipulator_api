package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The unit suite has no database: pin the SQL properties that matter on the text.

func TestUpsertPushTokenReenablesAndMovesOwner(t *testing.T) {
	q := upsertPushTokenQuery
	if !strings.Contains(q, "ON CONFLICT (token) DO UPDATE") {
		t.Fatal("upsert must conflict on token")
	}
	tail := q[strings.Index(q, "DO UPDATE"):]
	for _, want := range []string{"user_id = EXCLUDED.user_id", "disabled_at = NULL", "disabled_reason = NULL", "last_seen_at = EXCLUDED.last_seen_at"} {
		if !strings.Contains(tail, want) {
			t.Errorf("upsert missing %q", want)
		}
	}
	if strings.Contains(tail, "created_at") {
		t.Error("re-registration must keep created_at")
	}
}

func TestPushTokensCapPerUser(t *testing.T) {
	if MaxActivePushTokensPerUser != 10 {
		t.Errorf("cap = %d, want 10", MaxActivePushTokensPerUser)
	}
	q := supersedePushTokensQuery
	for _, want := range []string{"disabled_reason = 'superseded'", "user_id = $1::uuid", "disabled_at IS NULL", "ORDER BY last_seen_at DESC", "LIMIT $2", "NOT IN"} {
		if !strings.Contains(q, want) {
			t.Errorf("supersede query missing %q", want)
		}
	}
	if !strings.Contains(activePushTokensQuery, "LIMIT 10") || !strings.Contains(activePushTokensQuery, "disabled_at IS NULL") {
		t.Error("active tokens must be capped at 10 and exclude disabled rows")
	}
}

func TestPushDeleteIsScopedToCaller(t *testing.T) {
	if !strings.Contains(deletePushTokenQuery, "user_id = $1::uuid AND token = $2") {
		t.Error("delete must be scoped to the caller's own row")
	}
	if strings.Contains(pushTokenMetaQuery, "SELECT token") {
		t.Error("export metadata must never select the token")
	}
}

func TestPushStoreNoOpWithoutPool(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	if err := s.UpsertPushToken(ctx, "u", "t", "ios", "", "", time.Now()); err != ErrNoStore {
		t.Errorf("upsert: %v", err)
	}
	if _, err := s.DeletePushToken(ctx, "u", "t"); err != ErrNoStore {
		t.Errorf("delete: %v", err)
	}
	if _, err := s.ActivePushTokens(ctx, "u"); err != ErrNoStore {
		t.Errorf("active: %v", err)
	}
	if err := s.DisablePushToken(ctx, "t", "invalid"); err != ErrNoStore {
		t.Errorf("disable: %v", err)
	}
	if _, err := s.PushTokenMetadata(ctx, "u"); err != ErrNoStore {
		t.Errorf("meta: %v", err)
	}
}

func TestSettingsQueriesIncludePushJobFinished(t *testing.T) {
	if !strings.Contains(settingsColumns, "push_job_finished") || !strings.Contains(updateSettingsQuery, "COALESCE($8, push_job_finished)") {
		t.Error("settings columns / update must carry push_job_finished")
	}
}
