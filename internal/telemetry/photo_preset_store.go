package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM103: Aperture user presets (photo_presets). A preset is account-owned
// settings: a name, a category and a partial doc (`layers`, sanitized by
// photo.SanitizeLayers) plus an optional 96 px PNG thumbnail on disk. Every route
// is signed-in only, so every row has an owner. Ids are uuids carried as strings
// and cast in SQL; every write predicates on owner_user_id as defence in depth.

// RetainedPhotoPreset is the ledger kind of a preset (ADR-0010): locator =
// project_id = the preset id, keep_with_account (presets never expire by time).
// Mirrored by the CHECK in 20261103002_photo_presets.
const RetainedPhotoPreset = "photo_preset"

// PhotoPreset is one photo_presets row. Layers is nil on list reads.
type PhotoPreset struct {
	ID           string
	OwnerUserID  string
	Name         string
	Category     string // "" = none
	Layers       []byte
	ThumbnailKey string // "" = none
	Revision     int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// PhotoPresetInsert creates a preset.
type PhotoPresetInsert struct {
	ID          string // uuid
	OwnerUserID string
	Name        string
	Category    string
	Layers      []byte // canonical JSON from photo.SanitizeLayers
}

// ErrPhotoPresetNotFound is a missing (or not-owned, or soft-deleted) preset.
var ErrPhotoPresetNotFound = errors.New("photo preset not found")

// PhotoPresetLimitError is a create refused by the per-tier preset cap.
type PhotoPresetLimitError struct {
	Limit int
	Used  int
}

func (e *PhotoPresetLimitError) Error() string {
	return fmt.Sprintf("photo preset limit reached (%d of %d)", e.Used, e.Limit)
}

// PhotoPresetRevisionConflictError is a stale If-Match.
type PhotoPresetRevisionConflictError struct {
	Revision  int
	UpdatedAt time.Time
}

func (e *PhotoPresetRevisionConflictError) Error() string {
	return fmt.Sprintf("photo preset revision conflict (current revision %d)", e.Revision)
}

// presetListColumns never includes `layers`; presetFullColumns appends it last
// (scanPhotoPreset's order).
const presetListColumns = `id::text, owner_user_id::text, name, coalesce(category, ''), coalesce(thumbnail_key, ''), revision, created_at, updated_at`
const presetFullColumns = presetListColumns + `, layers`

const (
	presetLockSQL   = `SELECT pg_advisory_xact_lock(hashtextextended('photo_presets:' || $1, 0))`
	presetCountSQL  = `SELECT count(*) FROM photo_presets WHERE owner_user_id = $1::uuid AND deleted_at IS NULL`
	presetInsertSQL = `
INSERT INTO photo_presets (id, owner_user_id, name, category, layers, revision)
VALUES ($1::uuid, $2::uuid, $3, nullif($4, ''), $5::jsonb, 1)
RETURNING ` + presetFullColumns

	presetListSQL = `SELECT ` + presetListColumns + `
FROM photo_presets WHERE owner_user_id = $1::uuid AND deleted_at IS NULL
ORDER BY updated_at DESC, id LIMIT $2 OFFSET $3`

	presetListFullSQL = `SELECT ` + presetFullColumns + `
FROM photo_presets WHERE owner_user_id = $1::uuid AND deleted_at IS NULL
ORDER BY updated_at DESC, id LIMIT $2 OFFSET $3`

	presetGetSQL = `SELECT ` + presetFullColumns + ` FROM photo_presets WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL`

	// presetSaveCASSQL only lands when the stored revision still equals If-Match;
	// every successful save bumps revision. $4 (name), $5 (category) and $6
	// (layers) are NULL when unchanged; $7 = '1' clears the category.
	presetSaveCASSQL = `
UPDATE photo_presets
SET name = coalesce($3, name),
    category = CASE WHEN $7::boolean THEN NULL ELSE coalesce($4, category) END,
    layers = coalesce($5::jsonb, layers),
    revision = revision + 1,
    updated_at = now()
WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL AND revision = $6
RETURNING ` + presetFullColumns

	presetProbeSQL = `SELECT revision, updated_at FROM photo_presets WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL`

	presetSetThumbSQL = `UPDATE photo_presets SET thumbnail_key = $3 WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL RETURNING ` + presetListColumns

	presetSoftDeleteSQL = `UPDATE photo_presets SET deleted_at = coalesce(deleted_at, now()) WHERE id = $1::uuid AND owner_user_id = $2::uuid`
	presetHardDeleteSQL = `DELETE FROM photo_presets WHERE id = $1::uuid`

	// userPhotoPresetIDsSQL serves account deletion; presetExportSQL the account
	// data export (presets.json).
	userPhotoPresetIDsSQL = `SELECT id::text FROM photo_presets WHERE owner_user_id = $1::uuid`
	presetExportSQL       = `SELECT ` + presetFullColumns + ` FROM photo_presets WHERE owner_user_id = $1::uuid AND deleted_at IS NULL ORDER BY updated_at DESC`
)

func scanPhotoPreset(row pgx.Row, full bool) (*PhotoPreset, error) {
	var p PhotoPreset
	dest := []any{&p.ID, &p.OwnerUserID, &p.Name, &p.Category, &p.ThumbnailKey, &p.Revision, &p.CreatedAt, &p.UpdatedAt}
	if full {
		dest = append(dest, &p.Layers)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePhotoPreset inserts a preset after checking the owner's cap in the same
// transaction (advisory lock + count). limit < 0 means unlimited.
func (s *Store) CreatePhotoPreset(ctx context.Context, in PhotoPresetInsert, limit int) (*PhotoPreset, error) {
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
	if limit >= 0 {
		if _, err := tx.Exec(ctx, presetLockSQL, in.OwnerUserID); err != nil {
			return nil, err
		}
		var used int
		if err := tx.QueryRow(ctx, presetCountSQL, in.OwnerUserID).Scan(&used); err != nil {
			return nil, err
		}
		if used >= limit {
			return nil, &PhotoPresetLimitError{Limit: limit, Used: used}
		}
	}
	p, err := scanPhotoPreset(tx.QueryRow(ctx, presetInsertSQL, in.ID, in.OwnerUserID, in.Name, in.Category, string(in.Layers)), true)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit(ctx)
}

// CountPhotoPresets counts an owner's live presets.
func (s *Store) CountPhotoPresets(ctx context.Context, ownerUID string) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	err := s.Pool.QueryRow(ctx, presetCountSQL, ownerUID).Scan(&n)
	return n, err
}

// ListPhotoPresets returns an owner's live presets, newest first. full adds the
// layers (?full=1); otherwise Layers is nil.
func (s *Store) ListPhotoPresets(ctx context.Context, ownerUID string, full bool, limit, offset int) ([]PhotoPreset, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	q := presetListSQL
	if full {
		q = presetListFullSQL
	}
	rows, err := s.Pool.Query(ctx, q, ownerUID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PhotoPreset{}
	for rows.Next() {
		p, err := scanPhotoPreset(rows, full)
		if err != nil {
			return out, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetPhotoPreset reads one live preset of the owner with its layers. A
// soft-deleted, missing or not-owned preset is ErrPhotoPresetNotFound.
func (s *Store) GetPhotoPreset(ctx context.Context, id, ownerUID string) (*PhotoPreset, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	p, err := scanPhotoPreset(s.Pool.QueryRow(ctx, presetGetSQL, id, ownerUID), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPhotoPresetNotFound
	}
	return p, err
}

// PhotoPresetPatch is a save: nil fields stay unchanged; ClearCategory sets the
// category to NULL.
type PhotoPresetPatch struct {
	Name          *string
	Category      *string
	ClearCategory bool
	Layers        []byte
}

// SavePhotoPreset is the compare-and-set save behind PUT If-Match. Zero rows is
// disambiguated into ErrPhotoPresetNotFound or *PhotoPresetRevisionConflictError.
func (s *Store) SavePhotoPreset(ctx context.Context, id, ownerUID string, expectedRevision int, patch PhotoPresetPatch) (*PhotoPreset, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var layersArg any
	if patch.Layers != nil {
		layersArg = string(patch.Layers)
	}
	p, err := scanPhotoPreset(s.Pool.QueryRow(ctx, presetSaveCASSQL, id, ownerUID, patch.Name, patch.Category, layersArg, expectedRevision, patch.ClearCategory), true)
	if errors.Is(err, pgx.ErrNoRows) {
		var current int
		var updated time.Time
		perr := s.Pool.QueryRow(ctx, presetProbeSQL, id, ownerUID).Scan(&current, &updated)
		if errors.Is(perr, pgx.ErrNoRows) {
			return nil, ErrPhotoPresetNotFound
		}
		if perr != nil {
			return nil, perr
		}
		return nil, &PhotoPresetRevisionConflictError{Revision: current, UpdatedAt: updated}
	}
	return p, err
}

// SetPhotoPresetThumbnail records the thumbnail path (relative to PHOTO_DIR).
func (s *Store) SetPhotoPresetThumbnail(ctx context.Context, id, ownerUID, key string) (*PhotoPreset, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	p, err := scanPhotoPreset(s.Pool.QueryRow(ctx, presetSetThumbSQL, id, ownerUID, key), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPhotoPresetNotFound
	}
	return p, err
}

// DeletePhotoPreset soft-deletes (deleted_at = now). Idempotent; the preset
// vanishes from every read at once and the ledger sweep removes thumbnail and row.
func (s *Store) DeletePhotoPreset(ctx context.Context, id, ownerUID string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, presetSoftDeleteSQL, id, ownerUID)
	return err
}

// DeletePhotoPresetRow removes the row. Used by the sweeper after the thumbnail
// is gone and by account deletion. Idempotent.
func (s *Store) DeletePhotoPresetRow(ctx context.Context, id string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, presetHardDeleteSQL, id)
	return err
}

// ExportPhotoPresets lists the user's live presets, layers included, for the
// account data export (presets.json).
func (s *Store) ExportPhotoPresets(ctx context.Context, userID string) ([]PhotoPreset, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, presetExportSQL, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PhotoPreset{}
	for rows.Next() {
		p, err := scanPhotoPreset(rows, true)
		if err != nil {
			return out, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// deleteUserPhotoPresets removes every preset row of a user. Their thumbnails
// are ledgered photo_preset rows the account-wide sweep that runs before this
// already removed; the mm_users fk cascade is the last backstop.
func (s *Store) deleteUserPhotoPresets(ctx context.Context, userID string) (int64, error) {
	ids, err := s.idList(ctx, userPhotoPresetIDsSQL, userID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if err := s.DeletePhotoPresetRow(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
