package telemetry

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MA23: mm_push_tokens persistence. The token is a bearer secret for the device's
// push channel: it is never logged, never exported and never returned by an API.

// MaxActivePushTokensPerUser is the per-user cap on active tokens; older ones are
// disabled with reason "superseded".
const MaxActivePushTokensPerUser = 10

// PushToken is one active device token.
type PushToken struct {
	Token      string
	Platform   string
	AppVersion string
	Locale     string
	LastSeenAt time.Time
}

// PushTokenMeta is the exportable shape of a row: everything except the token.
type PushTokenMeta struct {
	Platform   string
	AppVersion string
	CreatedAt  time.Time
	LastSeenAt time.Time
	Disabled   bool
}

// upsertPushTokenQuery re-enables a disabled token and moves it to the current
// user (the device changed hands).
const upsertPushTokenQuery = `
INSERT INTO mm_push_tokens (user_id, token, platform, app_version, locale, created_at, last_seen_at)
VALUES ($1::uuid, $2, $3, nullif($4, ''), nullif($5, ''), $6, $6)
ON CONFLICT (token) DO UPDATE SET
	user_id = EXCLUDED.user_id, platform = EXCLUDED.platform,
	app_version = EXCLUDED.app_version, locale = EXCLUDED.locale,
	last_seen_at = EXCLUDED.last_seen_at, disabled_at = NULL, disabled_reason = NULL`

// supersedePushTokensQuery disables every active token of the user except the
// $2 most recently seen.
const supersedePushTokensQuery = `
UPDATE mm_push_tokens SET disabled_at = $3, disabled_reason = 'superseded'
WHERE user_id = $1::uuid AND disabled_at IS NULL AND id NOT IN (
	SELECT id FROM mm_push_tokens WHERE user_id = $1::uuid AND disabled_at IS NULL
	ORDER BY last_seen_at DESC, id DESC LIMIT $2)`

const deletePushTokenQuery = `DELETE FROM mm_push_tokens WHERE user_id = $1::uuid AND token = $2`

const activePushTokensQuery = `
SELECT token, platform, coalesce(app_version, ''), coalesce(locale, ''), last_seen_at
FROM mm_push_tokens WHERE user_id = $1::uuid AND disabled_at IS NULL
ORDER BY last_seen_at DESC, id DESC LIMIT ` + "10"

const disablePushTokenQuery = `
UPDATE mm_push_tokens SET disabled_at = $3, disabled_reason = $2
WHERE token = $1 AND disabled_at IS NULL`

const pushTokenMetaQuery = `
SELECT platform, coalesce(app_version, ''), created_at, last_seen_at, disabled_at IS NOT NULL
FROM mm_push_tokens WHERE user_id = $1::uuid ORDER BY created_at, id`

// UpsertPushToken registers or refreshes a token, then enforces the per-user cap.
func (s *Store) UpsertPushToken(ctx context.Context, userID, token, platform, appVersion, locale string, now time.Time) error {
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
	if _, err := tx.Exec(ctx, upsertPushTokenQuery, userID, token, platform, appVersion, locale, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, supersedePushTokensQuery, userID, MaxActivePushTokensPerUser, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeletePushToken removes the caller's own row; deleted reports whether one existed.
func (s *Store) DeletePushToken(ctx context.Context, userID, token string) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tag, err := s.Pool.Exec(ctx, deletePushTokenQuery, userID, token)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ActivePushTokens lists the user's enabled tokens, newest first (cap 10).
func (s *Store) ActivePushTokens(ctx context.Context, userID string) ([]PushToken, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	if strings.TrimSpace(userID) == "" {
		return nil, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, activePushTokensQuery, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushToken
	for rows.Next() {
		var t PushToken
		if err := rows.Scan(&t.Token, &t.Platform, &t.AppVersion, &t.Locale, &t.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PushJobFinishedPref reads the push_job_finished preference; no settings row = the
// default (on).
func (s *Store) PushJobFinishedPref(ctx context.Context, userID string) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	on := true
	err := s.Pool.QueryRow(ctx, `SELECT push_job_finished FROM mm_user_settings WHERE user_id = $1::uuid`, userID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return on, err
}

// DisablePushToken marks a token unusable (FCM said unregistered/invalid).
func (s *Store) DisablePushToken(ctx context.Context, token, reason string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, disablePushTokenQuery, token, reason, time.Now().UTC())
	return err
}

// PushTokenMetadata lists the user's rows without the token, for the export.
func (s *Store) PushTokenMetadata(ctx context.Context, userID string) ([]PushTokenMeta, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, pushTokenMetaQuery, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushTokenMeta
	for rows.Next() {
		var m PushTokenMeta
		if err := rows.Scan(&m.Platform, &m.AppVersion, &m.CreatedAt, &m.LastSeenAt, &m.Disabled); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
