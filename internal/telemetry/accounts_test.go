package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The `created` flag out of UpsertUser is the exact signup number — the one thing
// the browser cannot report, because Firebase's Google popup resolves identically
// for a new account and a returning user. These tests hold the two properties that
// make it trustworthy and that a well-meaning refactor could silently break. They
// are static, not integration: this repo's unit suite needs no database.

func TestUpsertUserQueryDerivesCreatedFromXmax(t *testing.T) {
	// `xmax = 0` is Postgres telling us which branch of the upsert ran: 0 on a
	// genuinely inserted row, the current transaction id on one that ON CONFLICT DO
	// UPDATE touched. The tempting alternatives are both wrong — a preceding SELECT
	// races two concurrent syncs into both reporting "created", and an explicit
	// INSERT-then-UPDATE needs error-code sniffing and two round trips.
	if !strings.Contains(upsertUserQuery, "xmax = 0") {
		t.Error("UpsertUser must derive `created` from `xmax = 0`; a SELECT-then-upsert races")
	}
	if !strings.Contains(upsertUserQuery, "ON CONFLICT (firebase_uid) DO UPDATE") {
		t.Error("UpsertUser must stay a single-statement upsert")
	}
}

func TestUpsertUserQueryReturnsCreatedFirst(t *testing.T) {
	// scanUserWithCreated scans `created` before the user columns. Column order is
	// the whole contract between the SQL and the Scan call, and getting it wrong
	// produces a type error at runtime on a path that only executes for real
	// signups — the least-tested path with the highest cost of being broken.
	idx := strings.Index(upsertUserQuery, "RETURNING ")
	if idx < 0 {
		t.Fatal("UpsertUser query has no RETURNING clause")
	}
	returning := strings.TrimSpace(upsertUserQuery[idx+len("RETURNING "):])
	if !strings.HasPrefix(returning, "(xmax = 0) AS created") {
		t.Errorf("RETURNING starts with %q; `created` must be the FIRST column to match scanUserWithCreated",
			firstLine(returning))
	}
}

func TestUpsertUserQueryDoesNotTouchTier(t *testing.T) {
	// Pre-existing invariant, now guarded: sign-in is an identity event, not an
	// entitlement event. A sync that reset tier to 'free' on every login would be a
	// very quiet way to lose paying customers.
	// Only the WRITING half of the statement. The RETURNING clause legitimately
	// reads the tier columns back — userColumns includes them.
	writes := upsertUserQuery
	if idx := strings.Index(writes, "RETURNING "); idx >= 0 {
		writes = writes[:idx]
	}
	for _, forbidden := range []string{"tier =", "tier_source", "tier_expires_at"} {
		if strings.Contains(writes, forbidden) {
			t.Errorf("UpsertUser must not write %q — only cmd/grant-tier and billing may change a tier", forbidden)
		}
	}
}

func TestUpsertUserWithNoStoreReportsNotCreated(t *testing.T) {
	// The disabled-store path must never claim a signup happened.
	var s *Store
	user, created, err := s.UpsertUser(context.Background(), UserSyncInput{FirebaseUID: "uid"})
	if !errors.Is(err, ErrNoStore) {
		t.Errorf("err = %v, want ErrNoStore", err)
	}
	if created {
		t.Error("created must be false when there is no store")
	}
	if user != nil {
		t.Error("user must be nil when there is no store")
	}
}

func TestScanUserWithCreatedPropagatesScanErrors(t *testing.T) {
	user, created, err := scanUserWithCreated(errRow{err: pgx.ErrNoRows})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("err = %v, want pgx.ErrNoRows", err)
	}
	if created || user != nil {
		t.Error("a failed scan must report (nil, false, err)")
	}
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
