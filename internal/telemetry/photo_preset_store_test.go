package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// MM103: the preset SQL runs on the server only; these pin the semantics a
// careless edit would silently break.
func TestPhotoPresetListNeverSelectsLayers(t *testing.T) {
	if strings.Contains(presetListColumns, "layers") || !strings.HasSuffix(presetFullColumns, ", layers") {
		t.Fatal("list columns exclude layers; full columns end with them")
	}
	for _, q := range []string{presetListSQL, presetGetSQL, presetListFullSQL} {
		if !strings.Contains(q, "owner_user_id = $") || !strings.Contains(q, "deleted_at IS NULL") {
			t.Errorf("reads are owner-scoped and live-only: %s", q)
		}
	}
}

func TestPhotoPresetSaveIsCompareAndSet(t *testing.T) {
	for _, want := range []string{"revision = revision + 1", "AND revision = $6", "owner_user_id = $2::uuid", "deleted_at IS NULL", "updated_at = now()"} {
		if !strings.Contains(presetSaveCASSQL, want) {
			t.Errorf("save statement is missing %q", want)
		}
	}
	if !strings.Contains(presetProbeSQL, "owner_user_id = $2::uuid") {
		t.Error("the conflict probe must stay owner-scoped")
	}
}

func TestPhotoPresetCreateCapIsTransactional(t *testing.T) {
	if !strings.Contains(presetLockSQL, "pg_advisory_xact_lock") || !strings.Contains(presetCountSQL, "deleted_at IS NULL") {
		t.Error("cap check needs the advisory lock and a live-only count")
	}
	if RetainedPhotoPreset != "photo_preset" {
		t.Error("ledger kind mirrors the migration CHECK")
	}
	var e error = &PhotoPresetLimitError{Limit: 10, Used: 10}
	var le *PhotoPresetLimitError
	if !errors.As(e, &le) {
		t.Fatal("limit error type")
	}
}

func TestPhotoPresetStoreNoOpWithoutPool(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	if _, err := s.CreatePhotoPreset(ctx, PhotoPresetInsert{}, 1); err != ErrNoStore {
		t.Errorf("create: %v", err)
	}
	if _, err := s.ListPhotoPresets(ctx, "u", false, 1, 0); err != ErrNoStore {
		t.Errorf("list: %v", err)
	}
	if _, err := s.ExportPhotoPresets(ctx, "u"); err != ErrNoStore {
		t.Errorf("export: %v", err)
	}
	if err := s.DeletePhotoPresetRow(ctx, "x"); err != ErrNoStore {
		t.Errorf("delete: %v", err)
	}
}

func TestUpsertRetainedWritesKeepWithAccount(t *testing.T) {
	if !strings.Contains(upsertRetainedQuery, "keep_with_account") {
		t.Error("the ledger upsert must carry keep_with_account")
	}
	if strings.Contains(upsertRetainedQuery[strings.Index(upsertRetainedQuery, "DO UPDATE"):], "keep_with_account") {
		t.Error("re-registration must not flip keep_with_account")
	}
}
