package telemetry

import (
	"context"
	"errors"
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
func (s *Store) UpsertUser(ctx context.Context, in UserSyncInput) (*models.User, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	const q = `
INSERT INTO mm_users (firebase_uid, email, email_verified, display_name, signup_source, last_seen_at)
VALUES ($1, nullif($2, ''), $3, nullif($4, ''), nullif($5, ''), now())
ON CONFLICT (firebase_uid) DO UPDATE SET
	email          = COALESCE(nullif(EXCLUDED.email, ''), mm_users.email),
	email_verified = EXCLUDED.email_verified,
	display_name   = COALESCE(nullif(EXCLUDED.display_name, ''), mm_users.display_name),
	updated_at     = now(),
	last_seen_at   = now()
RETURNING ` + userColumns

	row := s.Pool.QueryRow(ctx, q, in.FirebaseUID, in.Email, in.EmailVerified, in.DisplayName, in.SignupSource)
	return scanUser(row)
}

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
