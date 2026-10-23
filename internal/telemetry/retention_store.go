package telemetry

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM22: per-user retention settings and the retained-objects ledger.
//
// Nothing here deletes anything. mm_retained_objects.expires_at is written by
// producers (internal/retention.Register) and re-stamped by the settings API;
// the sweeper that acts on it is MM23.

// Settings enum domains. Mirrored by CHECK constraints in 20261023004.
const (
	RetentionScopeAll      = "all"
	HistoryVisibilityFull  = "full"
	HistoryVisibilityNames = "names_only"
)

// Retained object kinds. Mirrored by the CHECK in 20261023004.
const (
	RetainedUploadDir     = "upload_dir"
	RetainedOutputDir     = "output_dir"
	RetainedS3Object      = "s3_object"
	RetainedStudioAsset   = "studio_asset"
	RetainedStudioProject = "studio_project"
	RetainedMetadata      = "metadata_report"
	RetainedJobRow        = "job_row"
)

// UserSettings mirrors mm_user_settings.
type UserSettings struct {
	UserID                string
	RetentionHours        int
	RetentionScope        string
	DeleteMetadataReports bool
	HistoryVisibility     string
	UpdatedAt             time.Time
}

// SettingsPatch is a partial update: nil fields are left alone.
type SettingsPatch struct {
	RetentionHours        *int
	DeleteMetadataReports *bool
	HistoryVisibility     *string
}

// RetainedObject is one ledger row to write.
type RetainedObject struct {
	Kind          string
	Locator       string
	ParentLocator string
	OwnerUserID   string
	OwnerSession  string
	JobID         string
	ProjectID     string
	SizeBytes     int64
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// RetentionPreview is what a shrink would delete right away.
type RetentionPreview struct {
	Results  int64
	Projects int64
	Other    int64
	Bytes    int64
}

// ClaimResult counts what a sign-in claim moved to the account.
type ClaimResult struct {
	Projects int64
	Jobs     int64
	Objects  int64
}

const settingsColumns = `user_id::text, retention_hours, retention_scope, delete_metadata_reports,
	history_visibility, updated_at`

func scanSettings(row pgx.Row) (*UserSettings, error) {
	var s UserSettings
	if err := row.Scan(&s.UserID, &s.RetentionHours, &s.RetentionScope,
		&s.DeleteMetadataReports, &s.HistoryVisibility, &s.UpdatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// getOrCreateSettingsQuery inserts the default row if missing and returns the
// row either way. The no-op DO UPDATE is what makes RETURNING work on conflict.
const getOrCreateSettingsQuery = `
INSERT INTO mm_user_settings (user_id, retention_hours) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
RETURNING ` + settingsColumns

// GetOrCreateUserSettings returns the user's settings, creating the row with
// defaultHours on first read.
func (s *Store) GetOrCreateUserSettings(ctx context.Context, userID string, defaultHours int) (*UserSettings, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	return scanSettings(s.Pool.QueryRow(ctx, getOrCreateSettingsQuery, userID, defaultHours))
}

// UserRetentionHours reads the stored window. ok is false when the user has no
// settings row yet (never an error: the caller falls back to the tier default).
func (s *Store) UserRetentionHours(ctx context.Context, userID string) (int, bool, error) {
	if !s.Enabled() {
		return 0, false, ErrNoStore
	}
	if strings.TrimSpace(userID) == "" {
		return 0, false, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var hours int
	err := s.Pool.QueryRow(ctx, `SELECT retention_hours FROM mm_user_settings WHERE user_id = $1`, userID).Scan(&hours)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return hours, true, nil
}

// updateSettingsQuery applies a partial patch. COALESCE keeps untouched fields.
const updateSettingsQuery = `
UPDATE mm_user_settings SET
	retention_hours         = COALESCE($2, retention_hours),
	delete_metadata_reports = COALESCE($3, delete_metadata_reports),
	history_visibility      = COALESCE($4, history_visibility),
	updated_at              = now()
WHERE user_id = $1
RETURNING ` + settingsColumns

// restampOwnerQuery moves the expiry of every LIVE row of one user to
// max(created_at + window, floor). It touches only owner_user_id = $1 and only
// rows with deleted_at IS NULL, so it can never reach another account's rows or
// resurrect a deleted one.
const restampOwnerQuery = `
UPDATE mm_retained_objects
SET expires_at = GREATEST(created_at + make_interval(hours => $2), $3::timestamptz)
WHERE owner_user_id = $1 AND deleted_at IS NULL`

// ApplyUserSettings updates the settings and, when the window changed, re-stamps
// every live ledger row of the user, in one transaction. Returns the new
// settings and the number of rows re-stamped. now is a parameter so the floor
// (now + 1h) is deterministic in tests.
func (s *Store) ApplyUserSettings(ctx context.Context, userID string, defaultHours int, patch SettingsPatch, now time.Time) (*UserSettings, int64, error) {
	if !s.Enabled() {
		return nil, 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `INSERT INTO mm_user_settings (user_id, retention_hours) VALUES ($1, $2)
		ON CONFLICT (user_id) DO NOTHING`, userID, defaultHours); err != nil {
		return nil, 0, err
	}
	var prev int
	if err := tx.QueryRow(ctx, `SELECT retention_hours FROM mm_user_settings WHERE user_id = $1 FOR UPDATE`, userID).Scan(&prev); err != nil {
		return nil, 0, err
	}
	updated, err := scanSettings(tx.QueryRow(ctx, updateSettingsQuery,
		userID, patch.RetentionHours, patch.DeleteMetadataReports, patch.HistoryVisibility))
	if err != nil {
		return nil, 0, err
	}
	var restamped int64
	// Re-stamp even when the number is unchanged only if it was a first write;
	// otherwise skip the table scan.
	if patch.RetentionHours != nil {
		tag, err := tx.Exec(ctx, restampOwnerQuery, userID, updated.RetentionHours, now.Add(time.Hour))
		if err != nil {
			return nil, 0, err
		}
		restamped = tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	return updated, restamped, nil
}

// previewShrinkQuery counts the live rows whose created_at + window is already
// past now. Read-only. Results = job output dirs, Projects = Darkroom projects,
// Other = everything else (uploads, S3 objects, assets, metadata).
const previewShrinkQuery = `
SELECT
	count(*) FILTER (WHERE kind = 'output_dir'),
	count(*) FILTER (WHERE kind = 'studio_project'),
	count(*) FILTER (WHERE kind NOT IN ('output_dir', 'studio_project')),
	coalesce(sum(size_bytes), 0)
FROM mm_retained_objects
WHERE owner_user_id = $1 AND deleted_at IS NULL
  AND created_at + make_interval(hours => $2) < $3::timestamptz`

// PreviewRetentionShrink reports what a window of `hours` would delete.
func (s *Store) PreviewRetentionShrink(ctx context.Context, userID string, hours int, now time.Time) (RetentionPreview, error) {
	var out RetentionPreview
	if !s.Enabled() {
		return out, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	err := s.Pool.QueryRow(ctx, previewShrinkQuery, userID, hours, now).
		Scan(&out.Results, &out.Projects, &out.Other, &out.Bytes)
	return out, err
}

// upsertRetainedQuery is idempotent on (kind, locator). Re-registration never
// resets created_at/expires_at (that would extend retention by re-saving); it
// only grows size_bytes and fills in owner/back-links that were missing.
const upsertRetainedQuery = `
INSERT INTO mm_retained_objects
	(owner_user_id, owner_session_id, kind, locator, parent_locator, job_id, project_id,
	 size_bytes, created_at, expires_at)
VALUES (nullif($1, '')::uuid, nullif($2, ''), $3, $4, nullif($5, ''), nullif($6, '')::uuid,
	nullif($7, '')::uuid, $8, $9, $10)
ON CONFLICT (kind, locator) DO UPDATE SET
	size_bytes     = GREATEST(mm_retained_objects.size_bytes, EXCLUDED.size_bytes),
	owner_user_id  = COALESCE(mm_retained_objects.owner_user_id, EXCLUDED.owner_user_id),
	parent_locator = COALESCE(mm_retained_objects.parent_locator, EXCLUDED.parent_locator),
	job_id         = COALESCE(mm_retained_objects.job_id, EXCLUDED.job_id),
	project_id     = COALESCE(mm_retained_objects.project_id, EXCLUDED.project_id)`

// UpsertRetainedObject writes one ledger row.
func (s *Store) UpsertRetainedObject(ctx context.Context, o RetainedObject) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, upsertRetainedQuery,
		o.OwnerUserID, o.OwnerSession, o.Kind, o.Locator, o.ParentLocator, o.JobID, o.ProjectID,
		o.SizeBytes, o.CreatedAt, o.ExpiresAt)
	return err
}
