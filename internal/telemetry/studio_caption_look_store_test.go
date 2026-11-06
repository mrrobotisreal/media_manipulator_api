package telemetry

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MM122: the caption-look SQL pins (owner-scoped live reads, compare-and-set,
// transactional cap) and a real round trip when MM_CAPTION_LOOKS_TEST_DSN
// points at a throwaway, migrated database (its name must end in _tmp or _it:
// the test inserts and deletes rows).
func TestStudioCaptionLookSQLPins(t *testing.T) {
	for _, q := range []string{captionLookListSQL, captionLookGetSQL, captionLookProbeSQL, captionLookCountSQL} {
		if !strings.Contains(q, "owner_user_id = $") || !strings.Contains(q, "deleted_at IS NULL") {
			t.Errorf("reads are owner-scoped and live-only: %s", q)
		}
	}
	for _, want := range []string{"revision = revision + 1", "AND revision = $5", "owner_user_id = $2::uuid", "deleted_at IS NULL", "updated_at = now()"} {
		if !strings.Contains(captionLookSaveCASSQL, want) {
			t.Errorf("save statement is missing %q", want)
		}
	}
	if !strings.Contains(captionLookLockSQL, "pg_advisory_xact_lock") || RetainedStudioCaptionLook != "studio_caption_look" {
		t.Error("cap check needs the advisory lock; the ledger kind mirrors the migration CHECK")
	}
	s := &Store{}
	if _, _, err := s.CreateStudioCaptionLook(context.Background(), StudioCaptionLookInsert{}, 1); err != ErrNoStore {
		t.Errorf("create without a pool: %v", err)
	}
	if err := s.DeleteStudioCaptionLookRow(context.Background(), "x"); err != ErrNoStore {
		t.Errorf("delete without a pool: %v", err)
	}
}

func TestStudioCaptionLookStorePostgres(t *testing.T) {
	dsn := os.Getenv("MM_CAPTION_LOOKS_TEST_DSN")
	if dsn == "" {
		t.Skip("MM_CAPTION_LOOKS_TEST_DSN not set; the real SQL is not exercised")
	}
	if !strings.Contains(dsn, "_tmp") && !strings.Contains(dsn, "_it") {
		t.Fatal("refusing a DSN that is not a throwaway (_tmp / _it) database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{Pool: pool, WriteTimeout: 5e9}
	owner, other := uuid.NewString(), uuid.NewString()
	for _, u := range []string{owner, other} {
		if _, err := pool.Exec(ctx, `INSERT INTO mm_users (id, firebase_uid) VALUES ($1::uuid, $2)`, u, "fb-"+u); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM mm_users WHERE id = ANY($1::uuid[])`, []string{owner, other})
	}()

	var ids []string
	for i := 0; i < 2; i++ {
		l, used, err := s.CreateStudioCaptionLook(ctx, StudioCaptionLookInsert{ID: uuid.NewString(), OwnerUserID: owner, Name: "L", Style: []byte(`{"animation":"glow"}`)}, 2)
		if err != nil || used != i+1 || l.Revision != 1 {
			t.Fatalf("create %d: %v used %d", i, err, used)
		}
		ids = append(ids, l.ID)
	}
	var capErr *StudioCaptionLookLimitError
	if _, _, err := s.CreateStudioCaptionLook(ctx, StudioCaptionLookInsert{ID: uuid.NewString(), OwnerUserID: owner, Name: "3", Style: []byte(`{}`)}, 2); !errors.As(err, &capErr) || capErr.Used != 2 {
		t.Fatalf("cap: %v", err)
	}
	if _, err := s.GetStudioCaptionLook(ctx, ids[0], other); !errors.Is(err, ErrStudioCaptionLookNotFound) {
		t.Errorf("a stranger's read: %v", err)
	}
	name := "Renamed"
	l, err := s.SaveStudioCaptionLook(ctx, ids[0], owner, 1, StudioCaptionLookPatch{Name: &name})
	if err != nil || l.Revision != 2 || l.Name != name || !strings.Contains(string(l.Style), "glow") {
		t.Fatalf("save: %v %+v", err, l)
	}
	var conflict *StudioCaptionLookRevisionConflictError
	if _, err := s.SaveStudioCaptionLook(ctx, ids[0], owner, 1, StudioCaptionLookPatch{Name: &name}); !errors.As(err, &conflict) || conflict.Revision != 2 {
		t.Errorf("stale save: %v", err)
	}
	if err := s.DeleteStudioCaptionLook(ctx, ids[1], owner); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListStudioCaptionLooks(ctx, owner, 10, 0)
	exp, _ := s.ExportStudioCaptionLooks(ctx, owner)
	if n, _ := s.CountStudioCaptionLooks(ctx, owner); n != 1 || len(list) != 1 || len(exp) != 1 {
		t.Errorf("after soft delete: count %d list %d export %d", n, len(list), len(exp))
	}
	if n, err := s.deleteUserStudioCaptionLooks(ctx, owner); err != nil || n != 2 {
		t.Errorf("account delete removed %d (%v), want both rows", n, err)
	}
	// The ledger CHECK accepts the new kind.
	if _, err := pool.Exec(ctx, `INSERT INTO mm_retained_objects (owner_user_id, kind, locator, project_id, size_bytes, created_at, expires_at, keep_with_account)
VALUES ($1::uuid, 'studio_caption_look', $2::text, $2::uuid, 0, now(), $3, true)`, owner, ids[0], ParkedExpiry); err != nil {
		t.Errorf("ledger kind rejected: %v", err)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM mm_retained_objects WHERE owner_user_id = $1::uuid`, owner)
}
