package telemetry

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Accounts and quota persistence.
//
// Unlike the rest of this package these calls are NOT fire-and-forget: a quota
// decision depends on the returned count, so the caller needs the error. What
// the caller does with a failure is a separate decision — RequireQuota fails
// open, because a dead database must never break the free product.

// ErrNoStore is returned when the store has no database pool.
var ErrNoStore = errors.New("telemetry: no database pool")

const userColumns = `id::text, firebase_uid, coalesce(email, ''), email_verified,
	tier, tier_source, tier_expires_at, coalesce(display_name, ''),
	coalesce(signup_source, ''), created_at, updated_at, last_seen_at`

func scanUser(row pgx.Row) (*models.User, error) {
	var u models.User
	err := row.Scan(
		&u.ID, &u.FirebaseUID, &u.Email, &u.EmailVerified,
		&u.Tier, &u.TierSource, &u.TierExpiresAt, &u.DisplayName,
		&u.SignupSource, &u.CreatedAt, &u.UpdatedAt, &u.LastSeenAt,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// scanUserWithCreated scans a row that leads with the `created` flag UpsertUser
// selects. Kept separate from scanUser rather than adding a parameter: the column
// order is the contract between the SQL and the Scan call, and one function per
// column list is how that stays checkable by reading.
func scanUserWithCreated(row pgx.Row) (*models.User, bool, error) {
	var (
		u       models.User
		created bool
	)
	err := row.Scan(
		&created,
		&u.ID, &u.FirebaseUID, &u.Email, &u.EmailVerified,
		&u.Tier, &u.TierSource, &u.TierExpiresAt, &u.DisplayName,
		&u.SignupSource, &u.CreatedAt, &u.UpdatedAt, &u.LastSeenAt,
	)
	if err != nil {
		return nil, false, err
	}
	return &u, created, nil
}

// UserByFirebaseUID returns the account for a verified principal, or
// (nil, nil) when no row exists yet. A signed-in caller with no row is a normal
// state — the sync call may simply not have landed — so it is not an error.
func (s *Store) UserByFirebaseUID(ctx context.Context, firebaseUID string) (*models.User, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	row := s.Pool.QueryRow(ctx, `SELECT `+userColumns+` FROM mm_users WHERE firebase_uid = $1`, firebaseUID)
	user, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return user, err
}

// UserSyncInput is what POST /api/account/sync supplies from a verified token.
type UserSyncInput struct {
	FirebaseUID   string
	Email         string
	EmailVerified bool
	DisplayName   string
	SignupSource  string
}

// UpsertUser creates or refreshes the account for a verified principal and
// returns the resulting row.
//
// It deliberately does NOT touch tier, tier_source or tier_expires_at. Sign-in
// is an identity event, not an entitlement event; only cmd/grant-tier (and,
// later, the billing webhook) may change a tier. A sync that silently reset a
// tier to 'free' on every login would be a very quiet way to lose paying
// customers.
// The second return value reports whether this call CREATED the account, which is
// the only reliable signup signal in the whole system.
//
// The browser cannot answer it. Firebase's Google popup succeeds identically for a
// brand-new account and a returning user, so the UI deliberately under-reports
// signups (it emits `signin_completed` on that path) rather than inflate the most
// important growth number in the product. This statement knows: either the INSERT
// took, or the ON CONFLICT branch did.
//
// `xmax = 0` is how you ask Postgres which branch ran. xmax is the system column
// holding the id of the transaction that deleted or locked a row; on a genuinely
// new row it is 0, and on a row that ON CONFLICT DO UPDATE touched it is the
// current transaction's id. It is the standard trick precisely because the
// alternatives are worse: a preceding SELECT would race two concurrent syncs into
// both reporting "created", and splitting into an explicit INSERT-then-UPDATE would
// need error-code sniffing and two round trips. The cost is one non-obvious
// expression, documented here.
func (s *Store) UpsertUser(ctx context.Context, in UserSyncInput) (*models.User, bool, error) {
	if !s.Enabled() {
		return nil, false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	row := s.Pool.QueryRow(ctx, upsertUserQuery,
		in.FirebaseUID, in.Email, in.EmailVerified, in.DisplayName, in.SignupSource)
	return scanUserWithCreated(row)
}

// upsertUserQuery is package-level so a test can assert the two things about it
// that are easy to break and impossible to notice: that the `created` flag comes
// from `xmax = 0` (not from a racy preceding SELECT), and that it is the FIRST
// returned column, matching scanUserWithCreated's Scan order.
const upsertUserQuery = `
INSERT INTO mm_users (firebase_uid, email, email_verified, display_name, signup_source, last_seen_at)
VALUES ($1, nullif($2, ''), $3, nullif($4, ''), nullif($5, ''), now())
ON CONFLICT (firebase_uid) DO UPDATE SET
	email          = COALESCE(nullif(EXCLUDED.email, ''), mm_users.email),
	email_verified = EXCLUDED.email_verified,
	display_name   = COALESCE(nullif(EXCLUDED.display_name, ''), mm_users.display_name),
	updated_at     = now(),
	last_seen_at   = now()
RETURNING (xmax = 0) AS created, ` + userColumns

// SetUserTier changes an account's tier and records the matching audit row, in
// one transaction — a grant with no audit trail, or an audit row for a grant
// that did not apply, are both worse than failing.
func (s *Store) SetUserTier(ctx context.Context, firebaseUID, tier, source, grantedBy, reason string, expiresAt *time.Time) (*models.User, error) {
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

	const update = `
UPDATE mm_users
SET tier = $2, tier_source = $3, tier_expires_at = $4, updated_at = now()
WHERE firebase_uid = $1
RETURNING ` + userColumns

	user, err := scanUser(tx.QueryRow(ctx, update, firebaseUID, tier, source, expiresAt))
	if err != nil {
		return nil, err
	}

	const audit = `
INSERT INTO mm_tier_grants (user_id, tier, granted_by, reason, expires_at)
VALUES ($1, $2, $3, nullif($4, ''), $5)`
	if _, err := tx.Exec(ctx, audit, user.ID, tier, grantedBy, reason, expiresAt); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return user, nil
}

// IncrementUsage bumps today's counter for (subjectKind, subjectKey, metric)
// and returns the value *after* the increment.
//
// One statement, one round trip, no read-modify-write: the UNIQUE constraint on
// (subject_kind, subject_key, metric, day) is the conflict target, so two
// concurrent requests from the same visitor cannot both see the same pre-value
// and let an extra operation through.
func (s *Store) IncrementUsage(ctx context.Context, subjectKind models.UsageSubjectKind, subjectKey, metric string, day time.Time) (int64, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	const q = `
INSERT INTO mm_usage_counters (subject_kind, subject_key, metric, day, count, updated_at)
VALUES ($1, $2, $3, $4, 1, now())
ON CONFLICT (subject_kind, subject_key, metric, day) DO UPDATE SET
	count = mm_usage_counters.count + 1,
	updated_at = now()
RETURNING count`

	var count int64
	err := s.Pool.QueryRow(ctx, q, string(subjectKind), subjectKey, metric, day.UTC().Format("2006-01-02")).Scan(&count)
	return count, err
}

// UsageForDay returns every metric counted for a subject on one UTC day, as a
// metric → count map. A subject with no activity returns an empty map, not an
// error.
func (s *Store) UsageForDay(ctx context.Context, subjectKind models.UsageSubjectKind, subjectKey string, day time.Time) (map[string]int64, error) {
	out := map[string]int64{}
	if !s.Enabled() {
		return out, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	const q = `SELECT metric, count FROM mm_usage_counters
		WHERE subject_kind = $1 AND subject_key = $2 AND day = $3`
	rows, err := s.Pool.Query(ctx, q, string(subjectKind), subjectKey, day.UTC().Format("2006-01-02"))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var metric string
		var count int64
		if err := rows.Scan(&metric, &count); err != nil {
			return out, err
		}
		out[metric] = count
	}
	return out, rows.Err()
}

// ClaimStudioProjectsForUser attaches the projects a visitor created while
// signed out to the account they have just signed into.
//
// Only unclaimed rows move (user_id IS NULL), so this can never take a project
// from another account, and it is idempotent — safe on every sync, which is
// what makes "sign in on a second device" behave.
func (s *Store) ClaimStudioProjectsForUser(ctx context.Context, userID, sessionID string) (int64, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return 0, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	tag, err := s.Pool.Exec(ctx, `
UPDATE studio_projects SET user_id = $1, updated_at = now()
WHERE session_id = $2 AND user_id IS NULL
`, userID, sessionID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// historyColumns is the projection GET /api/account/history reads. Nullable text
// columns are coalesced so the scan target can stay a plain string.
const historyColumns = `job_id::text, coalesce(tool, ''), coalesce(mode, ''),
	coalesce(media_kind, ''), coalesce(source_format, ''), coalesce(target_format, ''),
	status, coalesce(owner_tier, ''), created_at, completed_at, duration_ms,
	coalesce(result_s3_key, ''), coalesce(result_file_name, ''), result_expires_at`

// ConversionHistoryForUser returns one page of a signed-in user's jobs, newest
// first, plus the unpaginated total so the caller can page honestly.
//
// Keyed on user_id and nothing else: session and visitor ids are client-supplied,
// and history is an account feature precisely because it should follow the person
// rather than the browser. Anonymous jobs (user_id IS NULL) are recorded but are
// not readable here — by design.
func (s *Store) ConversionHistoryForUser(ctx context.Context, userID string, limit, offset int) ([]models.ConversionHistoryEntry, int, error) {
	if !s.Enabled() {
		return nil, 0, ErrNoStore
	}
	if strings.TrimSpace(userID) == "" {
		return nil, 0, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	var total int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM mm_conversion_jobs WHERE user_id = $1`, userID,
	).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := s.Pool.Query(ctx, `SELECT `+historyColumns+`
FROM mm_conversion_jobs
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, total, err
	}
	defer rows.Close()

	out := make([]models.ConversionHistoryEntry, 0, limit)
	for rows.Next() {
		var e models.ConversionHistoryEntry
		if err := rows.Scan(
			&e.JobID, &e.Tool, &e.Mode,
			&e.MediaKind, &e.SourceFormat, &e.TargetFormat,
			&e.Status, &e.OwnerTier, &e.CreatedAt, &e.CompletedAt, &e.DurationMS,
			&e.ResultS3Key, &e.ResultFileName, &e.ResultExpiresAt,
		); err != nil {
			return out, total, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// CountStudioProjectsForUser is the saved-project cap check. Projects created
// while signed out are scoped by session and are not counted here.
func (s *Store) CountStudioProjectsForUser(ctx context.Context, userID string) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM studio_projects WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}
