package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/email"
)

// MM31: mm_email_log persistence and the expiry-digest query. No email body is
// ever stored; the table holds ids, status and a short error class.

// reserveEmailQuery claims (user, kind, dedupe_key). The unique constraint makes
// the INSERT the race-free gate: exactly one concurrent caller gets a row back.
// A FAILED row with attempts left, or a PENDING row abandoned by a crashed
// process (10 minutes), is re-claimed; a sent or in-flight row returns no rows.
const reserveEmailQuery = `
INSERT INTO mm_email_log (user_id, kind, dedupe_key, locale, status)
VALUES ($1::uuid, $2, $3, $4, 'pending')
ON CONFLICT ON CONSTRAINT mm_email_log_dedupe_key DO UPDATE
SET status = 'pending', updated_at = now()
WHERE (mm_email_log.status = 'failed' AND mm_email_log.attempts < $5)
   OR (mm_email_log.status = 'pending' AND mm_email_log.updated_at < now() - interval '10 minutes')
RETURNING id::text, attempts`

// ReserveEmail implements email.Store.
func (s *Store) ReserveEmail(ctx context.Context, userID string, kind email.Kind, dedupeKey, locale string, maxAttempts int) (email.Reservation, bool, error) {
	if !s.Enabled() {
		return email.Reservation{}, false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var r email.Reservation
	err := s.Pool.QueryRow(ctx, reserveEmailQuery, userID, string(kind), dedupeKey, locale, maxAttempts).Scan(&r.ID, &r.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return email.Reservation{}, false, nil
	}
	if err != nil {
		return email.Reservation{}, false, err
	}
	return r, true, nil
}

const finishEmailQuery = `
UPDATE mm_email_log
SET status = $2, provider_id = nullif($3, ''), error = nullif($4, ''), attempts = $5, updated_at = now()
WHERE id = $1::uuid`

// FinishEmail implements email.Store.
func (s *Store) FinishEmail(ctx context.Context, id, status, providerID, errText string, attempts int) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, finishEmailQuery, id, status, providerID, errText, attempts)
	return err
}

// EmailPrefs implements email.Store. No settings row = the defaults.
func (s *Store) EmailPrefs(ctx context.Context, userID string) (bool, bool, error) {
	if !s.Enabled() {
		return false, false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var ai, digest bool
	err := s.Pool.QueryRow(ctx, `SELECT email_ai_job_finished, email_expiry_digest FROM mm_user_settings WHERE user_id = $1::uuid`, userID).Scan(&ai, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, true, nil
	}
	return ai, digest, err
}

// digestRowsQuery lists user-visible live items (job results and Darkroom
// projects) expiring in (now, now+window] for verified, opted-in accounts.
// Parked metadata reports (keep_with_account, expires 9999) and anything outside
// the window never match, so a long retention choice is never nagged.
const digestRowsQuery = `
SELECT u.id::text, u.email, coalesce(u.display_name, ''), coalesce(u.locale, ''),
       coalesce(nullif(j.result_file_name, ''), p.name, ''), o.expires_at
FROM mm_retained_objects o
JOIN mm_users u ON u.id = o.owner_user_id
LEFT JOIN mm_user_settings s ON s.user_id = u.id
LEFT JOIN mm_conversion_jobs j ON o.kind = 'output_dir' AND j.job_id::text = o.locator
LEFT JOIN studio_projects p ON o.kind = 'studio_project' AND p.id::text = o.locator
WHERE o.deleted_at IS NULL
  AND NOT o.keep_with_account
  AND o.kind IN ('output_dir', 'studio_project')
  AND o.expires_at > $1::timestamptz
  AND o.expires_at <= $1::timestamptz + make_interval(secs => $2)
  AND u.email IS NOT NULL AND u.email <> '' AND u.email_verified
  AND coalesce(s.email_expiry_digest, true)
ORDER BY u.id, o.expires_at`

// DigestRows implements email.Store.
func (s *Store) DigestRows(ctx context.Context, now time.Time, within time.Duration) ([]email.DigestRow, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, digestRowsQuery, now, within.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []email.DigestRow
	for rows.Next() {
		var r email.DigestRow
		if err := rows.Scan(&r.UserID, &r.Email, &r.DisplayName, &r.Locale, &r.Name, &r.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
