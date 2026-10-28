package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// AssetID narrows to one Darkroom asset's own ledger row (kind studio_asset,
	// locator = the asset id). Part 21 asset delete.
	AssetID string
	// S3Key narrows to one S3 object's ledger row (kind s3_object, locator =
	// the key). MM71 retires a Darkroom proxy replaced by a rebuild with it;
	// the inline sweep also takes any object derived from that key.
	S3Key string
	// VoiceID narrows to one saved voice (MM77): its own ledger row (kind
	// voice, locator = the id) and the s3_object rows registered with it as
	// parent (reference.wav, preview.mp3).
	VoiceID string
}

// Valid reports whether the filter is safe to run.
func (f LedgerFilter) Valid() bool {
	return strings.TrimSpace(f.UserID) != "" || strings.TrimSpace(f.SessionID) != "" ||
		strings.TrimSpace(f.JobID) != "" || strings.TrimSpace(f.ProjectID) != "" || strings.TrimSpace(f.AssetID) != "" ||
		strings.TrimSpace(f.S3Key) != "" || strings.TrimSpace(f.VoiceID) != ""
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
	if strings.TrimSpace(f.AssetID) != "" {
		conds = append(conds, "o.kind = 'studio_asset'")
		add("o.locator = $%d", f.AssetID)
	}
	if strings.TrimSpace(f.S3Key) != "" {
		conds = append(conds, "o.kind = 's3_object'")
		add("o.locator = $%d", f.S3Key)
	}
	if strings.TrimSpace(f.VoiceID) != "" {
		add("((o.kind = 'voice' AND o.locator = $%[1]d) OR (o.kind = 's3_object' AND o.parent_locator = $%[1]d))", f.VoiceID)
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

// AssetProject returns the project an asset belongs to. found is false when the
// asset does not exist (or the id is not a uuid).
func (s *Store) AssetProject(ctx context.Context, assetID string) (projectID string, found bool, err error) {
	if !s.Enabled() {
		return "", false, ErrNoStore
	}
	if uuidOrNil(assetID) == nil {
		return "", false, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	err = s.Pool.QueryRow(ctx, `SELECT project_id::text FROM studio_assets WHERE id = $1::uuid`, assetID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return projectID, err == nil, err
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

// DetachUserJobs anonymizes every conversion job of a user in one transaction
// (counts and timings stay; result key/name, file-name-like options and the
// media assets' hashes/names go) and clears user_id so the rows leave the
// account. Returns the number of job rows detached. Idempotent.
func (s *Store) DetachUserJobs(ctx context.Context, userID string) (int64, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
UPDATE mm_media_assets SET original_filename_redacted = NULL, sha256 = NULL, perceptual_hash = NULL
WHERE job_id IN (SELECT job_id FROM mm_conversion_jobs WHERE user_id = $1::uuid)`, userID); err != nil {
		return 0, err
	}
	// MM43: counted Motion exports keep their counts but lose the account link.
	if _, err := tx.Exec(ctx, anonymizeUserVectorExportsSQL, userID); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
UPDATE mm_conversion_jobs
SET result_s3_key = NULL, result_file_name = NULL, result_expires_at = NULL,
    options = options - $2::text[], user_id = NULL, updated_at = now()
WHERE user_id = $1::uuid`, userID, anonymizeJobKeys)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// DeleteUserProjects removes every Darkroom project row of a user (versions and
// assets cascade explicitly) and returns the S3 keys the asset rows held, for
// callers that must clean up projects the ledger never knew. Idempotent.
func (s *Store) DeleteUserProjects(ctx context.Context, userID string) (int64, error) {
	ids, err := s.UserProjectIDs(ctx, userID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if _, err := s.DeleteStudioProjectRows(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	// MM43: Motion projects go with the account too (their thumbnails are
	// ledgered s3_object rows the preceding sweep already removed).
	m, err := s.deleteUserVectorProjects(ctx, userID)
	if err != nil {
		return n + m, err
	}
	// MM77: saved voices go with the account too. Their reference and preview
	// objects are ledgered (kind voice + s3_object children) and the preceding
	// account-wide sweep already removed them; this catches any row the ledger
	// never knew. The mm_users FK cascade is the last backstop.
	v, err := s.DeleteUserVoices(ctx, userID)
	return n + m + v, err
}

// DeleteUserSettingsAndGrants removes mm_user_settings and mm_tier_grants rows
// of a user. Returns (settings, grants) row counts. Idempotent.
func (s *Store) DeleteUserSettingsAndGrants(ctx context.Context, userID string) (settings, grants int64, err error) {
	if !s.Enabled() {
		return 0, 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t1, err := tx.Exec(ctx, `DELETE FROM mm_user_settings WHERE user_id = $1::uuid`, userID)
	if err != nil {
		return 0, 0, err
	}
	t2, err := tx.Exec(ctx, `DELETE FROM mm_tier_grants WHERE user_id = $1::uuid`, userID)
	if err != nil {
		return 0, 0, err
	}
	return t1.RowsAffected(), t2.RowsAffected(), tx.Commit(ctx)
}

// DeleteUserRow removes the mm_users row. It fails (FK, by design) while the
// user still has undeleted ledger rows: ledger.owner_user_id has no ON DELETE
// action so a forgotten sweep is a hard error, never orphaned files. Ledger rows
// that are already stamped deleted are detached first (their audit value does
// not need the user id). Returns whether a row was deleted.
func (s *Store) DeleteUserRow(ctx context.Context, userID string) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE mm_retained_objects SET owner_user_id = NULL
		WHERE owner_user_id = $1::uuid AND deleted_at IS NOT NULL AND owner_session_id IS NOT NULL`, userID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mm_retained_objects
		WHERE owner_user_id = $1::uuid AND deleted_at IS NOT NULL`, userID); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM mm_users WHERE id = $1::uuid`, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, tx.Commit(ctx)
}

// LiveLedgerCount counts a user's undeleted ledger rows (the "did the sweep
// finish" check before the mm_users row goes).
func (s *Store) LiveLedgerCount(ctx context.Context, userID string) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM mm_retained_objects
		WHERE owner_user_id = $1::uuid AND deleted_at IS NULL`, userID).Scan(&n)
	return n, err
}

// --- tombstone ---------------------------------------------------------------

// tombstoneSalt is fixed, like the limiter's and the quota's: the goal is that
// the table holds no raw Firebase uid, not secrecy.
const tombstoneSalt = "mm-account-deletion-uid-salt"

// UIDHash is the tombstone key for a Firebase uid.
func UIDHash(firebaseUID string) string {
	sum := sha256.Sum256([]byte(tombstoneSalt + ":" + firebaseUID))
	return hex.EncodeToString(sum[:])
}

// TombstoneWindow is how long a deleted account's tombstone is kept.
const TombstoneWindow = 30 * 24 * time.Hour

// WriteAccountTombstone records the deletion. Idempotent (a retry keeps the first time).
func (s *Store) WriteAccountTombstone(ctx context.Context, firebaseUID string, now time.Time) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `INSERT INTO mm_account_deletions (firebase_uid_hash, deleted_at)
		VALUES ($1, $2) ON CONFLICT (firebase_uid_hash) DO NOTHING`, UIDHash(firebaseUID), now)
	return err
}

// HasAccountTombstone reports whether this uid was deleted inside the window.
func (s *Store) HasAccountTombstone(ctx context.Context, firebaseUID string, now time.Time) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mm_account_deletions
		WHERE firebase_uid_hash = $1 AND deleted_at > $2)`, UIDHash(firebaseUID), now.Add(-TombstoneWindow)).Scan(&ok)
	return ok, err
}
