package telemetry

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM23: the queries behind the ledger-driven retention sweeper
// (internal/retention/sweeper.go). Every statement that changes data is
// written so that running it twice, or from two API processes at once, is
// harmless: rows are claimed with FOR UPDATE SKIP LOCKED and a lease
// (next_attempt_at), and every "mark" statement is a no-op on a row that is
// already marked.

// ParkedExpiry is the expires_at sentinel for rows kept "until the account is
// deleted" (metadata reports, ADR-0010). It is a real timestamp, not NULL, so
// the NOT NULL column and the expires_at index keep working; the sweeper never
// sees it as due.
var ParkedExpiry = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// LedgerRow is one mm_retained_objects row as the sweeper reads it.
type LedgerRow struct {
	ID              string
	Kind            string
	Locator         string
	ParentLocator   string
	OwnerUserID     string
	OwnerSession    string
	JobID           string
	ProjectID       string
	SizeBytes       int64
	CreatedAt       time.Time
	ExpiresAt       time.Time
	Attempts        int
	KeepWithAccount bool
}

// Backlog is what is overdue right now.
type Backlog struct {
	Due    int64 // expired, not deleted, still retryable
	GaveUp int64 // expired, not deleted, attempts exhausted (needs a human)
}

const ledgerRowColumns = `o.id::text, o.kind, o.locator, coalesce(o.parent_locator, ''),
	coalesce(o.owner_user_id::text, ''), coalesce(o.owner_session_id, ''),
	coalesce(o.job_id::text, ''), coalesce(o.project_id::text, ''),
	o.size_bytes, o.created_at, o.expires_at, o.attempts, o.keep_with_account`

func scanLedgerRows(rows pgx.Rows) ([]LedgerRow, error) {
	defer rows.Close()
	var out []LedgerRow
	for rows.Next() {
		var r LedgerRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Locator, &r.ParentLocator, &r.OwnerUserID, &r.OwnerSession,
			&r.JobID, &r.ProjectID, &r.SizeBytes, &r.CreatedAt, &r.ExpiresAt, &r.Attempts, &r.KeepWithAccount); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// claimDueQuery leases up to $2 overdue rows. The lease ($3) is written to
// next_attempt_at, so a crashed sweeper's rows come back after it expires and a
// second process skips them meanwhile (SKIP LOCKED covers the instant between
// select and update). Rows past max attempts ($4) are never claimed again.
const claimDueQuery = `
UPDATE mm_retained_objects o
SET next_attempt_at = $3
FROM (
	SELECT id FROM mm_retained_objects
	WHERE deleted_at IS NULL
	  AND expires_at < $1
	  AND attempts < $4
	  AND (next_attempt_at IS NULL OR next_attempt_at < $1)
	ORDER BY expires_at
	LIMIT $2
	FOR UPDATE SKIP LOCKED
) due
WHERE o.id = due.id
RETURNING ` + ledgerRowColumns

// ClaimDue leases the next batch of overdue ledger rows.
func (s *Store) ClaimDue(ctx context.Context, now time.Time, limit int, lease time.Duration, maxAttempts int) ([]LedgerRow, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, claimDueQuery, now, limit, now.Add(lease), maxAttempts)
	if err != nil {
		return nil, err
	}
	return scanLedgerRows(rows)
}

// peekDueQuery is the dry-run twin of claimDueQuery: read-only, keyset-paged on
// (expires_at, id) so a report can walk the whole backlog without leasing or
// changing anything.
const peekDueQuery = `
SELECT ` + ledgerRowColumns + `
FROM mm_retained_objects o
WHERE o.deleted_at IS NULL
  AND o.expires_at < $1
  AND o.attempts < $4
  AND (o.next_attempt_at IS NULL OR o.next_attempt_at < $1)
  AND (o.expires_at, o.id) > ($5::timestamptz, $6::uuid)
ORDER BY o.expires_at, o.id
LIMIT $2`

// PeekDue returns the next page of overdue rows strictly after the cursor
// without modifying anything. Pass the zero time and "" for the first page.
func (s *Store) PeekDue(ctx context.Context, now time.Time, limit, maxAttempts int, afterExpires time.Time, afterID string) ([]LedgerRow, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	if strings.TrimSpace(afterID) == "" {
		afterID = "00000000-0000-0000-0000-000000000000"
	}
	if afterExpires.IsZero() {
		afterExpires = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, peekDueQuery, now, limit, now, maxAttempts, afterExpires, afterID)
	if err != nil {
		return nil, err
	}
	return scanLedgerRows(rows)
}

// LedgerBacklog counts overdue rows for the retention_backlog gauge.
func (s *Store) LedgerBacklog(ctx context.Context, now time.Time, maxAttempts int) (Backlog, error) {
	var b Backlog
	if !s.Enabled() {
		return b, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	err := s.Pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE attempts < $2), count(*) FILTER (WHERE attempts >= $2)
FROM mm_retained_objects WHERE deleted_at IS NULL AND expires_at < $1`, now, maxAttempts).Scan(&b.Due, &b.GaveUp)
	return b, err
}

// DerivedS3Rows returns the live s3_object rows whose parent_locator is one of
// parents: the proxy / sprite / peaks objects registered with their original.
func (s *Store) DerivedS3Rows(ctx context.Context, parents []string) ([]LedgerRow, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	if len(parents) == 0 {
		return nil, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `SELECT `+ledgerRowColumns+`
FROM mm_retained_objects o
WHERE o.kind = 's3_object' AND o.deleted_at IS NULL AND o.parent_locator = ANY($1)`, parents)
	if err != nil {
		return nil, err
	}
	return scanLedgerRows(rows)
}

// ProjectS3Rows returns the live s3_object rows attributed to a Darkroom project.
// This is how a project's media is found AFTER its asset rows are gone: the
// cascade deletes rows first and S3 second, and a retry must still know the keys.
func (s *Store) ProjectS3Rows(ctx context.Context, projectID string) ([]LedgerRow, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `SELECT `+ledgerRowColumns+`
FROM mm_retained_objects o
WHERE o.kind = 's3_object' AND o.deleted_at IS NULL AND o.project_id = $1::uuid`, projectID)
	if err != nil {
		return nil, err
	}
	return scanLedgerRows(rows)
}

// MarkLedgerDeleted stamps rows as deleted (kept 30 days for audit, then purged).
func (s *Store) MarkLedgerDeleted(ctx context.Context, ids []string, now time.Time) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
UPDATE mm_retained_objects
SET deleted_at = $2, delete_error = NULL, next_attempt_at = NULL
WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`, ids, now)
	return err
}

// MarkLedgerDeletedByLocator stamps live rows of one kind by locator (the
// derived S3 keys and the assets a project cascade removed).
func (s *Store) MarkLedgerDeletedByLocator(ctx context.Context, kind string, locators []string, now time.Time) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	if len(locators) == 0 {
		return nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
UPDATE mm_retained_objects
SET deleted_at = $3, delete_error = NULL, next_attempt_at = NULL
WHERE kind = $1 AND locator = ANY($2) AND deleted_at IS NULL`, kind, locators, now)
	return err
}

// MarkLedgerDeletedByProject stamps every live studio_asset row of a project.
func (s *Store) MarkLedgerDeletedByProject(ctx context.Context, kind, projectID string, now time.Time) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
UPDATE mm_retained_objects
SET deleted_at = $3, delete_error = NULL, next_attempt_at = NULL
WHERE kind = $1 AND project_id = $2::uuid AND deleted_at IS NULL`, kind, projectID, now)
	return err
}

// MarkLedgerFailed records a failed attempt: attempts+1, the error text and the
// earliest time the row may be tried again.
func (s *Store) MarkLedgerFailed(ctx context.Context, id, errMsg string, next time.Time) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
UPDATE mm_retained_objects
SET attempts = attempts + 1, delete_error = $2, next_attempt_at = $3
WHERE id = $1::uuid AND deleted_at IS NULL`, id, errMsg, next)
	return err
}

// DeferLedgerRow postpones a row without counting an attempt (the job is still
// running).
func (s *Store) DeferLedgerRow(ctx context.Context, id string, until time.Time) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `UPDATE mm_retained_objects SET next_attempt_at = $2 WHERE id = $1::uuid AND deleted_at IS NULL`, id, until)
	return err
}

// ParkLedgerRow re-stamps a metadata_report row to account lifetime.
func (s *Store) ParkLedgerRow(ctx context.Context, id string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `
UPDATE mm_retained_objects
SET expires_at = $2, keep_with_account = true, next_attempt_at = NULL, delete_error = NULL
WHERE id = $1::uuid AND deleted_at IS NULL`, id, ParkedExpiry)
	return err
}

// upsertParkedMetadataQuery records that a job's metadata documents outlive the
// job directory. Idempotent; a row that already exists is left alone EXCEPT that
// a previously deleted one is not resurrected (deleted_at stays set).
const upsertParkedMetadataQuery = `
INSERT INTO mm_retained_objects
	(owner_user_id, owner_session_id, kind, locator, job_id, size_bytes, created_at, expires_at, keep_with_account)
VALUES (nullif($1, '')::uuid, nullif($2, ''), 'metadata_report', $3, nullif($4, '')::uuid, $5, $6, $7, true)
ON CONFLICT (kind, locator) DO NOTHING`

// ParkMetadataReport inserts the metadata_report row that keeps a job's
// metadata*.json after its output directory is swept.
func (s *Store) ParkMetadataReport(ctx context.Context, owner LedgerRow, size int64, createdAt time.Time) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, upsertParkedMetadataQuery,
		owner.OwnerUserID, owner.OwnerSession, owner.Locator, owner.JobID, size, createdAt, ParkedExpiry)
	return err
}

// unparkMetadataQuery makes parked reports due again once their owner has
// turned "delete metadata reports" on. Runs hourly; the sweeper then deletes them.
const unparkMetadataQuery = `
UPDATE mm_retained_objects o
SET expires_at = $1, next_attempt_at = NULL
FROM mm_user_settings s
WHERE o.owner_user_id = s.user_id
  AND o.kind = 'metadata_report' AND o.keep_with_account AND o.deleted_at IS NULL
  AND o.expires_at > $1
  AND s.delete_metadata_reports`

// UnparkMetadataReports expires parked reports of users who opted to delete them.
func (s *Store) UnparkMetadataReports(ctx context.Context, now time.Time) (int64, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tag, err := s.Pool.Exec(ctx, unparkMetadataQuery, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// OwnerKeepsMetadataReports reports whether the user keeps metadata reports past
// file expiry. No settings row means the default (keep: delete_metadata_reports
// defaults to false).
func (s *Store) OwnerKeepsMetadataReports(ctx context.Context, userID string) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoStore
	}
	if strings.TrimSpace(userID) == "" {
		return false, nil // guests keep nothing
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var del bool
	err := s.Pool.QueryRow(ctx, `SELECT delete_metadata_reports FROM mm_user_settings WHERE user_id = $1::uuid`, userID).Scan(&del)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !del, nil
}

// StudioAssetKeys returns every S3 key a Darkroom asset row points at.
func (s *Store) StudioAssetKeys(ctx context.Context, assetID string) ([]string, bool, error) {
	if !s.Enabled() {
		return nil, false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var orig, proxy, sprite, peaks string
	err := s.Pool.QueryRow(ctx, `
SELECT s3_key_original, coalesce(s3_key_proxy, ''), coalesce(thumbnail_sprite_url, ''), coalesce(s3_key_peaks, '')
FROM studio_assets WHERE id = $1::uuid`, assetID).Scan(&orig, &proxy, &sprite, &peaks)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return nonEmpty(orig, proxy, sprite, peaks), true, nil
}

// DeleteStudioAssetRow removes one asset row.
func (s *Store) DeleteStudioAssetRow(ctx context.Context, assetID string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `DELETE FROM studio_assets WHERE id = $1::uuid`, assetID)
	return err
}

// DeleteStudioProjectRows removes a project, its versions and its asset rows in
// one transaction and returns the S3 keys the asset rows held. The caller deletes
// the keys afterwards; a retry finds them again through the ledger
// (ProjectS3Rows), because the rows are gone by then.
func (s *Store) DeleteStudioProjectRows(ctx context.Context, projectID string) ([]string, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
SELECT s3_key_original, coalesce(s3_key_proxy, ''), coalesce(thumbnail_sprite_url, ''), coalesce(s3_key_peaks, '')
FROM studio_assets WHERE project_id = $1::uuid`, projectID)
	if err != nil {
		return nil, err
	}
	var keys []string
	for rows.Next() {
		var a, b, c, d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, nonEmpty(a, b, c, d)...)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, stmt := range []string{
		`DELETE FROM studio_project_versions WHERE project_id = $1::uuid`,
		`DELETE FROM studio_assets WHERE project_id = $1::uuid`,
		`DELETE FROM studio_projects WHERE id = $1::uuid`,
	} {
		if _, err := tx.Exec(ctx, stmt, projectID); err != nil {
			return nil, err
		}
	}
	return keys, tx.Commit(ctx)
}

// anonymizeJobKeys are the option keys that can carry a file name.
var anonymizeJobKeys = []string{"fileName", "filename", "file_name", "originalFileName", "originalName", "outputFileName", "name"}

// AnonymizeJob keeps a job's counts and timings and drops everything that names
// the user's file: the result key and name, any file-name-like option, and the
// hashes / redacted names of the job's media assets. Idempotent.
func (s *Store) AnonymizeJob(ctx context.Context, jobID string) error {
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
	if _, err := tx.Exec(ctx, `
UPDATE mm_conversion_jobs
SET result_s3_key = NULL, result_file_name = NULL, result_expires_at = NULL,
    options = options - $2::text[], updated_at = now()
WHERE job_id = $1::uuid`, jobID, anonymizeJobKeys); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE mm_media_assets
SET original_filename_redacted = NULL, sha256 = NULL, perceptual_hash = NULL
WHERE job_id = $1::uuid`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LiveLedgerLocators returns which of locators have a live (not deleted) ledger
// row of one of kinds. The orphan walker uses it: a path with a live row belongs
// to the sweeper, not to the mtime walker.
func (s *Store) LiveLedgerLocators(ctx context.Context, kinds, locators []string) (map[string]bool, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	out := make(map[string]bool, len(locators))
	if len(locators) == 0 {
		return out, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `
SELECT DISTINCT locator FROM mm_retained_objects
WHERE kind = ANY($1) AND locator = ANY($2) AND deleted_at IS NULL`, kinds, locators)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out[l] = true
	}
	return out, rows.Err()
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

// JobHasLiveRows reports whether any non-metadata ledger row of the job is
// still live. The sweeper anonymizes a job's database row only once every file
// it names is gone.
func (s *Store) JobHasLiveRows(ctx context.Context, jobID string) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoStore
	}
	if strings.TrimSpace(jobID) == "" {
		return false, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var live bool
	err := s.Pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM mm_retained_objects
	WHERE job_id = $1::uuid AND deleted_at IS NULL AND kind <> 'metadata_report')`, jobID).Scan(&live)
	return live, err
}

// JobLedgerEntry is the slice of a ledger row account history needs.
type JobLedgerEntry struct {
	ID              string
	Kind            string
	Locator         string
	JobID           string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	DeletedAt       *time.Time
	KeepWithAccount bool
}

// LedgerForJobs returns every result-bearing ledger row (output dir, S3 object,
// parked metadata) for the given jobs, deleted ones included, so history can say
// "expired" truthfully after the sweeper ran.
func (s *Store) LedgerForJobs(ctx context.Context, jobIDs []string) ([]JobLedgerEntry, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	if len(jobIDs) == 0 {
		return nil, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `
SELECT id::text, kind, locator, job_id::text, created_at, expires_at, deleted_at, keep_with_account
FROM mm_retained_objects
WHERE job_id = ANY($1::uuid[]) AND kind IN ('output_dir', 's3_object', 'metadata_report')`, jobIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobLedgerEntry
	for rows.Next() {
		var e JobLedgerEntry
		if err := rows.Scan(&e.ID, &e.Kind, &e.Locator, &e.JobID, &e.CreatedAt, &e.ExpiresAt, &e.DeletedAt, &e.KeepWithAccount); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeLedgerDeleted removes audit rows whose deletion is older than cutoff.
func (s *Store) PurgeLedgerDeleted(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.purge(ctx, `
DELETE FROM mm_retained_objects WHERE ctid IN (
	SELECT ctid FROM mm_retained_objects WHERE deleted_at IS NOT NULL AND deleted_at < $1 LIMIT $2)`, cutoff, limit)
}

func (s *Store) purge(ctx context.Context, sql string, args ...any) (int64, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tag, err := s.Pool.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Postgres purges (MM23 decision 6). Each statement takes one bounded batch and
// the caller loops until it returns fewer rows than the limit. All of them are
// pure functions of a cutoff, so a repeat is a no-op.

// PurgeUsageCounters deletes mm_usage_counters rows older than cutoffDay.
func (s *Store) PurgeUsageCounters(ctx context.Context, cutoffDay time.Time, limit int) (int64, error) {
	return s.purge(ctx, `
DELETE FROM mm_usage_counters WHERE id IN (
	SELECT id FROM mm_usage_counters WHERE day < $1::date LIMIT $2)`, cutoffDay, limit)
}

// PurgeSafetyIncidents deletes incidents past retention_until unless on legal hold.
func (s *Store) PurgeSafetyIncidents(ctx context.Context, now time.Time, limit int) (int64, error) {
	return s.purge(ctx, `
DELETE FROM mm_safety_incidents WHERE safety_incident_id IN (
	SELECT safety_incident_id FROM mm_safety_incidents
	WHERE retention_until IS NOT NULL AND retention_until < $1 AND legal_hold = false LIMIT $2)`, now, limit)
}

// ScrubRequestIPs nulls raw addresses on old mm_api_requests rows.
func (s *Store) ScrubRequestIPs(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.purge(ctx, `
UPDATE mm_api_requests SET ip = NULL, cf_connecting_ip = NULL, x_forwarded_for = NULL WHERE ctid IN (
	SELECT ctid FROM mm_api_requests
	WHERE created_at < $1 AND (ip IS NOT NULL OR cf_connecting_ip IS NOT NULL OR x_forwarded_for IS NOT NULL)
	LIMIT $2)`, cutoff, limit)
}

// ScrubRateLimitIPs nulls raw addresses on old mm_rate_limit_events rows.
func (s *Store) ScrubRateLimitIPs(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.purge(ctx, `
UPDATE mm_rate_limit_events SET ip = NULL WHERE ctid IN (
	SELECT ctid FROM mm_rate_limit_events WHERE created_at < $1 AND ip IS NOT NULL LIMIT $2)`, cutoff, limit)
}

// PurgeCleanupRuns deletes audit runs older than cutoff (paths cascade).
func (s *Store) PurgeCleanupRuns(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.purge(ctx, `
DELETE FROM mm_cleanup_runs WHERE cleanup_run_id IN (
	SELECT cleanup_run_id FROM mm_cleanup_runs WHERE started_at < $1 LIMIT $2)`, cutoff, limit)
}
