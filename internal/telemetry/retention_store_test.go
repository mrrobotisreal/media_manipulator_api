package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Static guards on the ledger SQL: the unit suite has no database, so the
// properties that matter (scoping, floor, idempotency) are pinned on the text.

func TestRestampQueryIsScopedToOwnerLiveRowsWithFloor(t *testing.T) {
	for _, want := range []string{"owner_user_id = $1", "deleted_at IS NULL", "GREATEST(", "created_at + make_interval(hours => $2)"} {
		if !strings.Contains(restampOwnerQuery, want) {
			t.Errorf("restamp query missing %q", want)
		}
	}
}

func TestPreviewQueryIsReadOnlyAndScoped(t *testing.T) {
	q := strings.ToUpper(previewShrinkQuery)
	if strings.Contains(q, "UPDATE") || strings.Contains(q, "DELETE") || strings.Contains(q, "INSERT") {
		t.Error("preview must not write")
	}
	if !strings.Contains(previewShrinkQuery, "owner_user_id = $1") || !strings.Contains(previewShrinkQuery, "deleted_at IS NULL") {
		t.Error("preview must be scoped to the caller's live rows")
	}
}

func TestUpsertRetainedIsIdempotentAndNeverExtends(t *testing.T) {
	if !strings.Contains(upsertRetainedQuery, "ON CONFLICT (kind, locator) DO UPDATE") {
		t.Error("ledger upsert must conflict on (kind, locator)")
	}
	tail := upsertRetainedQuery[strings.Index(upsertRetainedQuery, "DO UPDATE"):]
	if strings.Contains(tail, "expires_at") || strings.Contains(tail, "created_at") {
		t.Error("re-registration must not move created_at/expires_at")
	}
}

func TestRetentionStoreNoOpWithoutPool(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	if err := s.UpsertRetainedObject(ctx, RetainedObject{}); err != ErrNoStore {
		t.Errorf("upsert: %v", err)
	}
	if _, _, err := s.UserRetentionHours(ctx, "u"); err != ErrNoStore {
		t.Errorf("hours: %v", err)
	}
	if _, err := s.GetOrCreateUserSettings(ctx, "u", 4320); err != ErrNoStore {
		t.Errorf("get: %v", err)
	}
	if _, _, err := s.ApplyUserSettings(ctx, "u", 4320, SettingsPatch{}, time.Now()); err != ErrNoStore {
		t.Errorf("apply: %v", err)
	}
	if _, err := s.PreviewRetentionShrink(ctx, "u", 24, time.Now()); err != ErrNoStore {
		t.Errorf("preview: %v", err)
	}
}
