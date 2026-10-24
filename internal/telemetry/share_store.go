package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM32: mm_share_links persistence. Only the sha256 hash of a token is ever
// stored or queried. Liveness is decided at QUERY time against the retention
// ledger, so deleting a job (MM24) or the sweeper expiring its files makes a
// link stop resolving with no write to this table.

// ShareLinkNew is one link to insert.
type ShareLinkNew struct {
	TokenHash string
	JobID     string
	OwnerID   string
	ExpiresAt time.Time
}

// ShareLink is a resolved, currently-valid link.
type ShareLink struct {
	JobID     string
	ExpiresAt time.Time
	// LiveUntil is the latest expiry among the job's live RESULT files in the
	// ledger; the zero time means none are live (deleted or swept).
	LiveUntil time.Time
}

// resultLedgerKinds are the ledger rows that ARE the shareable result files.
// Upload dirs (the input) and parked metadata reports (expires 9999) are
// deliberately excluded: neither may keep a link alive.
const resultLedgerKinds = `('output_dir', 's3_object')`

const shareRetentionQuery = `
SELECT max(expires_at) FROM mm_retained_objects
WHERE job_id = $1::uuid AND kind IN ` + resultLedgerKinds + ` AND deleted_at IS NULL AND expires_at > now()`

// ShareRetentionEnd returns when the job's result files stop being retained.
// ok is false when no live result row exists (deleted, swept, or never ledgered).
func (s *Store) ShareRetentionEnd(ctx context.Context, jobID string) (time.Time, bool, error) {
	if !s.Enabled() {
		return time.Time{}, false, ErrNoStore
	}
	if uuidOrNil(jobID) == nil {
		return time.Time{}, false, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var end *time.Time
	if err := s.Pool.QueryRow(ctx, shareRetentionQuery, jobID).Scan(&end); err != nil {
		return time.Time{}, false, err
	}
	if end == nil {
		return time.Time{}, false, nil
	}
	return end.UTC(), true, nil
}

const revokeSharesQuery = `
UPDATE mm_share_links SET revoked_at = now()
WHERE job_id = $1::uuid AND revoked_at IS NULL AND expires_at > now()`

// ErrShareLimit means the job already has the maximum number of live links.
var ErrShareLimit = errors.New("telemetry: share link limit reached")

// CreateShareLink inserts a link unless the job already has maxLive live ones.
// Links are additive on purpose: the plaintext of an earlier link cannot be
// re-shown, and silently rotating it would kill a link the owner already sent.
func (s *Store) CreateShareLink(ctx context.Context, l ShareLinkNew, maxLive int) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialise concurrent creates for one job so the cap cannot be raced.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "mm_share:"+l.JobID); err != nil {
		return err
	}
	var live int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM mm_share_links WHERE job_id = $1::uuid AND revoked_at IS NULL AND expires_at > now()`, l.JobID).Scan(&live); err != nil {
		return err
	}
	if maxLive > 0 && live >= maxLive {
		return ErrShareLimit
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO mm_share_links (token_hash, job_id, owner_id, expires_at) VALUES ($1, $2::uuid, $3, $4)`,
		l.TokenHash, l.JobID, l.OwnerID, l.ExpiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// resolveShareQuery returns the link only when it is unrevoked and unexpired;
// LiveUntil is the ledger join evaluated now.
const resolveShareQuery = `
SELECT l.job_id::text, l.expires_at,
       (SELECT max(o.expires_at) FROM mm_retained_objects o
         WHERE o.job_id = l.job_id AND o.kind IN ` + resultLedgerKinds + `
           AND o.deleted_at IS NULL AND o.expires_at > now())
FROM mm_share_links l
WHERE l.token_hash = $1 AND l.revoked_at IS NULL AND l.expires_at > now()`

// ResolveShareLink looks a link up by token HASH. found is false for unknown,
// revoked and expired links alike.
func (s *Store) ResolveShareLink(ctx context.Context, tokenHash string) (ShareLink, bool, error) {
	if !s.Enabled() {
		return ShareLink{}, false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var l ShareLink
	var live *time.Time
	err := s.Pool.QueryRow(ctx, resolveShareQuery, tokenHash).Scan(&l.JobID, &l.ExpiresAt, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return ShareLink{}, false, nil
	}
	if err != nil {
		return ShareLink{}, false, err
	}
	l.ExpiresAt = l.ExpiresAt.UTC()
	if live != nil {
		l.LiveUntil = live.UTC()
	}
	return l, true, nil
}

// RevokeShareLinks sets revoked_at on every live link of the job and returns how many.
func (s *Store) RevokeShareLinks(ctx context.Context, jobID string) (int64, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	if uuidOrNil(jobID) == nil {
		return 0, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tag, err := s.Pool.Exec(ctx, revokeSharesQuery, jobID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// TouchShareView increments view_count (best effort; the caller ignores errors).
func (s *Store) TouchShareView(ctx context.Context, tokenHash string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `UPDATE mm_share_links SET view_count = view_count + 1 WHERE token_hash = $1`, tokenHash)
	return err
}
