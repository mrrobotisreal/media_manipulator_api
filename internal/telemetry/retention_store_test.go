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
	if strings.Contains(q, "UPDATE ") || strings.Contains(q, "DELETE FROM") || strings.Contains(q, "INSERT ") {
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

func TestClaimStatementsOnlyTakeUnclaimedRows(t *testing.T) {
	for name, q := range map[string]string{
		"projects": claimStudioProjectsQuery, "jobs": claimJobsQuery,
	} {
		if !strings.Contains(q, "user_id IS NULL") || !strings.Contains(q, "session_id") {
			t.Errorf("%s claim must be scoped to the session and unclaimed rows: %s", name, q)
		}
	}
	for _, want := range []string{"owner_user_id IS NULL", "owner_session_id = $2", "deleted_at IS NULL", "GREATEST(", "make_interval(hours => $3)"} {
		if !strings.Contains(claimObjectsQuery, want) {
			t.Errorf("ledger claim missing %q", want)
		}
	}
	if _, err := (&Store{}).ClaimSessionForUser(context.Background(), "u", "s", 4320, time.Now()); err != ErrNoStore {
		t.Errorf("no pool: %v", err)
	}
}

// MM23: the sweeper's SQL. No database in the unit suite, so the properties that
// keep it safe are pinned on the text.

func TestClaimDueLeasesWithSkipLockedAndRespectsAttempts(t *testing.T) {
	for _, want := range []string{
		"FOR UPDATE SKIP LOCKED", "deleted_at IS NULL", "expires_at < $1", "attempts < $4",
		"next_attempt_at IS NULL OR next_attempt_at < $1", "ORDER BY expires_at", "LIMIT $2",
		"SET next_attempt_at = $3",
	} {
		if !strings.Contains(claimDueQuery, want) {
			t.Errorf("claim query missing %q", want)
		}
	}
}

func TestPeekDueIsReadOnly(t *testing.T) {
	q := strings.ToUpper(peekDueQuery)
	for _, bad := range []string{"UPDATE ", "DELETE ", "INSERT ", "FOR UPDATE"} {
		if strings.Contains(q, bad) {
			t.Errorf("dry-run peek must not contain %q", bad)
		}
	}
	if !strings.Contains(peekDueQuery, "(o.expires_at, o.id) >") {
		t.Error("peek must keyset-page so a dry run can walk the whole backlog")
	}
}

func TestRestampAndClaimSkipParkedMetadata(t *testing.T) {
	for name, q := range map[string]string{"restamp": restampOwnerQuery, "claim": claimObjectsQuery} {
		if !strings.Contains(q, "NOT keep_with_account") {
			t.Errorf("%s must leave parked metadata reports alone", name)
		}
	}
}

func TestParkedExpiryIsFarFuture(t *testing.T) {
	if ParkedExpiry.Year() != 9999 {
		t.Errorf("parked sentinel = %v", ParkedExpiry)
	}
}

func TestSweeperStoreNoOpWithoutPool(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	now := time.Now()
	if _, err := s.ClaimDue(ctx, now, 10, time.Minute, 10); err != ErrNoStore {
		t.Errorf("claim: %v", err)
	}
	if _, err := s.PeekDue(ctx, now, 10, 10, time.Time{}, ""); err != ErrNoStore {
		t.Errorf("peek: %v", err)
	}
	if err := s.MarkLedgerFailed(ctx, "x", "e", now); err != ErrNoStore {
		t.Errorf("fail: %v", err)
	}
	if err := s.AnonymizeJob(ctx, "x"); err != ErrNoStore {
		t.Errorf("anonymize: %v", err)
	}
	if _, err := s.DeleteStudioProjectRows(ctx, "x"); err != ErrNoStore {
		t.Errorf("project: %v", err)
	}
}

func TestNonEmptyDropsBlanks(t *testing.T) {
	got := nonEmpty("a", "", " ", "b")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %v", got)
	}
}
