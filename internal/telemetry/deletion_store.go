package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM24: the queries behind user-initiated deletion ("expire now + sweep inline").
// Everything here is idempotent: a repeat finds nothing to change.

// LedgerFilter selects live ledger rows. At least one of UserID, SessionID,
// JobID, ProjectID must be set (an empty filter would be "every row" and is
// refused). UserID selects an account's rows; SessionID alone selects a guest's
// (rows with no user owner); JobID / ProjectID narrow to one job / project. The
// caller proves ownership of the job or project BEFORE building the filter.
type LedgerFilter struct {
	UserID    string
	SessionID string
	JobID     string
	ProjectID string
}

// Valid reports whether the filter is safe to run.
func (f LedgerFilter) Valid() bool {
	return strings.TrimSpace(f.UserID) != "" || strings.TrimSpace(f.SessionID) != "" ||
		strings.TrimSpace(f.JobID) != "" || strings.TrimSpace(f.ProjectID) != ""
}

// where renders the filter as SQL conditions over mm_retained_objects (alias o)
// and its arguments, numbered from start.
func (f LedgerFilter) where(start int) (string, []any) {
	conds := []string{"o.deleted_at IS NULL"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, start+len(args)-1))
	}
	switch {
	case strings.TrimSpace(f.UserID) != "":
		add("o.owner_user_id = $%d::uuid", f.UserID)
	case strings.TrimSpace(f.SessionID) != "" && strings.TrimSpace(f.JobID) == "" && strings.TrimSpace(f.ProjectID) == "":
		conds = append(conds, "o.owner_user_id IS NULL")
		add("o.owner_session_id = $%d", f.SessionID)
	}
	if strings.TrimSpace(f.JobID) != "" {
		add("o.job_id = $%d::uuid", f.JobID)
	}
	if strings.TrimSpace(f.ProjectID) != "" {
		add("o.project_id = $%d::uuid", f.ProjectID)
	}
	return strings.Join(conds, " AND "), args
}

// ExpireLedger makes every live row matching f due now: expires_at one second in
// the past, backoff and attempt counters reset (a user's delete must not wait
// behind a row the sweeper gave up on). Returns the ids it touched.
func (s *Store) ExpireLedger(ctx context.Context, f LedgerFilter, now time.Time) ([]string, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	if !f.Valid() {
		return nil, errors.New("telemetry: refusing an empty ledger filter")
	}
	where, args := f.where(2)
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `
UPDATE mm_retained_objects o
SET expires_at = LEAST(o.expires_at, $1::timestamptz - interval '1 second'),
    next_attempt_at = NULL, attempts = 0, delete_error = NULL
WHERE `+where+` RETURNING o.id::text`, append([]any{now}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClaimByIDs leases up to limit of the given rows that are still live and not
// leased, exactly like ClaimDue but for a known set.
func (s *Store) ClaimByIDs(ctx context.Context, ids []string, now time.Time, limit int, lease time.Duration) ([]LedgerRow, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `
UPDATE mm_retained_objects o SET next_attempt_at = $4
FROM (
	SELECT id FROM mm_retained_objects
	WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL
	  AND (next_attempt_at IS NULL OR next_attempt_at < $2)
	ORDER BY expires_at
	LIMIT $3
	FOR UPDATE SKIP LOCKED
) due
WHERE o.id = due.id
RETURNING `+ledgerRowColumns, ids, now, limit, now.Add(lease))
	if err != nil {
		return nil, err
	}
	return scanLedgerRows(rows)
}

// PendingByIDs counts how many of the rows are still undeleted.
func (s *Store) PendingByIDs(ctx context.Context, ids []string) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	if len(ids) == 0 {
		return 0, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM mm_retained_objects WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`, ids).Scan(&n)
	return n, err
}

// PinJobLedger implements "keep longer": it extends the job's live, non-parked
// ledger rows owned by userID to at least `until` and marks them kept_longer so
// a later window change keeps the pin. Never shortens a row. Returns the rows
// touched and the latest expiry among them.
func (s *Store) PinJobLedger(ctx context.Context, userID, jobID string, until time.Time) (int64, time.Time, error) {
	if !s.Enabled() {
		return 0, time.Time{}, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int64
	var latest *time.Time
	err := s.Pool.QueryRow(ctx, `
WITH upd AS (
	UPDATE mm_retained_objects
	SET expires_at = GREATEST(expires_at, $3::timestamptz), kept_longer = true
	WHERE owner_user_id = $1::uuid AND job_id = $2::uuid AND deleted_at IS NULL
	  AND NOT keep_with_account AND kind <> 'metadata_report'
	RETURNING expires_at)
SELECT count(*), max(expires_at) FROM upd`, userID, jobID, until).Scan(&n, &latest)
	if err != nil || latest == nil {
		return n, time.Time{}, err
	}
	return n, *latest, nil
}

// ProjectOwner reads a Darkroom project's owner columns. found is false when the
// project does not exist (or the id is not a uuid).
func (s *Store) ProjectOwner(ctx context.Context, projectID string) (userID, sessionID string, found bool, err error) {
	if !s.Enabled() {
		return "", "", false, ErrNoStore
	}
	if uuidOrNil(projectID) == nil {
		return "", "", false, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	err = s.Pool.QueryRow(ctx,
		`SELECT coalesce(user_id::text, ''), session_id FROM studio_projects WHERE id = $1::uuid`, projectID).Scan(&userID, &sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return userID, sessionID, err == nil, err
}

// UserProjectIDs lists a user's Darkroom project ids (ledger-less ones included).
func (s *Store) UserProjectIDs(ctx context.Context, userID string) ([]string, error) {
	return s.idList(ctx, `SELECT id::text FROM studio_projects WHERE user_id = $1::uuid`, userID)
}

// UserJobIDs lists a user's conversion job ids.
func (s *Store) UserJobIDs(ctx context.Context, userID string) ([]string, error) {
	return s.idList(ctx, `SELECT job_id::text FROM mm_conversion_jobs WHERE user_id = $1::uuid`, userID)
}

func (s *Store) idList(ctx context.Context, q, arg string) ([]string, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, q, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DetachJob is the job-row half of a user's delete: the job keeps its counts and
// timings (operational record) but loses everything that names the file AND the
// user, so it also leaves the owner's history. Idempotent.
func (s *Store) DetachJob(ctx context.Context, jobID string) error {
	if err := s.AnonymizeJob(ctx, jobID); err != nil {
		return err
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `UPDATE mm_conversion_jobs SET user_id = NULL, updated_at = now() WHERE job_id = $1::uuid`, jobID)
	return err
}
