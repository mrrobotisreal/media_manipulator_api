package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM122: Darkroom saved caption looks (studio_caption_looks). A look is
// account-owned settings: a name and a complete caption style (sanitized by
// models.SanitizeCaptionStyle). Every route is signed-in only, so every row has
// an owner. Ids are uuids carried as strings and cast in SQL; every write
// predicates on owner_user_id as defence in depth. Modelled on the Aperture
// presets (photo_preset_store.go), without a thumbnail.

// RetainedStudioCaptionLook is the ledger kind of a saved caption look
// (ADR-0010): locator = project_id = the look id, keep_with_account (looks
// never expire by time). Mirrored by the CHECK in 20261106122_studio_caption_looks.
const RetainedStudioCaptionLook = "studio_caption_look"

// StudioCaptionLook is one studio_caption_looks row.
type StudioCaptionLook struct {
	ID          string
	OwnerUserID string
	Name        string
	Style       []byte // canonical JSON of the sanitized style
	Revision    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// StudioCaptionLookInsert creates a look.
type StudioCaptionLookInsert struct {
	ID          string // uuid
	OwnerUserID string
	Name        string
	Style       []byte
}

// ErrStudioCaptionLookNotFound is a missing (or not-owned, or soft-deleted) look.
var ErrStudioCaptionLookNotFound = errors.New("caption look not found")

// StudioCaptionLookLimitError is a create refused by the per-tier look cap.
type StudioCaptionLookLimitError struct {
	Limit int
	Used  int
}

func (e *StudioCaptionLookLimitError) Error() string {
	return fmt.Sprintf("caption look limit reached (%d of %d)", e.Used, e.Limit)
}

// StudioCaptionLookRevisionConflictError is a stale If-Match.
type StudioCaptionLookRevisionConflictError struct {
	Revision  int
	UpdatedAt time.Time
}

func (e *StudioCaptionLookRevisionConflictError) Error() string {
	return fmt.Sprintf("caption look revision conflict (current revision %d)", e.Revision)
}

const captionLookColumns = `id::text, owner_user_id::text, name, style, revision, created_at, updated_at`

const (
	captionLookLockSQL   = `SELECT pg_advisory_xact_lock(hashtextextended('studio_caption_looks:' || $1, 0))`
	captionLookCountSQL  = `SELECT count(*) FROM studio_caption_looks WHERE owner_user_id = $1::uuid AND deleted_at IS NULL`
	captionLookInsertSQL = `
INSERT INTO studio_caption_looks (id, owner_user_id, name, style, revision)
VALUES ($1::uuid, $2::uuid, $3, $4::jsonb, 1)
RETURNING ` + captionLookColumns

	captionLookListSQL = `SELECT ` + captionLookColumns + `
FROM studio_caption_looks WHERE owner_user_id = $1::uuid AND deleted_at IS NULL
ORDER BY updated_at DESC, id LIMIT $2 OFFSET $3`

	captionLookGetSQL = `SELECT ` + captionLookColumns + ` FROM studio_caption_looks WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL`

	// captionLookSaveCASSQL only lands when the stored revision still equals
	// If-Match; every successful save bumps revision. $3 (name) and $4 (style)
	// are NULL when unchanged.
	captionLookSaveCASSQL = `
UPDATE studio_caption_looks
SET name = coalesce($3, name),
    style = coalesce($4::jsonb, style),
    revision = revision + 1,
    updated_at = now()
WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL AND revision = $5
RETURNING ` + captionLookColumns

	captionLookProbeSQL      = `SELECT revision, updated_at FROM studio_caption_looks WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL`
	captionLookSoftDeleteSQL = `UPDATE studio_caption_looks SET deleted_at = coalesce(deleted_at, now()) WHERE id = $1::uuid AND owner_user_id = $2::uuid`
	captionLookHardDeleteSQL = `DELETE FROM studio_caption_looks WHERE id = $1::uuid`

	// userCaptionLookIDsSQL serves account deletion; captionLookExportSQL the
	// account data export (caption-looks.json).
	userCaptionLookIDsSQL = `SELECT id::text FROM studio_caption_looks WHERE owner_user_id = $1::uuid`
	captionLookExportSQL  = `SELECT ` + captionLookColumns + ` FROM studio_caption_looks WHERE owner_user_id = $1::uuid AND deleted_at IS NULL ORDER BY updated_at DESC`
)

func scanCaptionLook(row pgx.Row) (*StudioCaptionLook, error) {
	var l StudioCaptionLook
	if err := row.Scan(&l.ID, &l.OwnerUserID, &l.Name, &l.Style, &l.Revision, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return nil, err
	}
	return &l, nil
}

func (s *Store) captionLookRows(ctx context.Context, q string, args ...any) ([]StudioCaptionLook, error) {
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StudioCaptionLook{}
	for rows.Next() {
		l, err := scanCaptionLook(rows)
		if err != nil {
			return out, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// CreateStudioCaptionLook inserts a look after checking the owner's cap in the
// same transaction (advisory lock + count). limit < 0 means unlimited.
func (s *Store) CreateStudioCaptionLook(ctx context.Context, in StudioCaptionLookInsert, limit int) (*StudioCaptionLook, int, error) {
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
	if _, err := tx.Exec(ctx, captionLookLockSQL, in.OwnerUserID); err != nil {
		return nil, 0, err
	}
	var used int
	if err := tx.QueryRow(ctx, captionLookCountSQL, in.OwnerUserID).Scan(&used); err != nil {
		return nil, 0, err
	}
	if limit >= 0 && used >= limit {
		return nil, used, &StudioCaptionLookLimitError{Limit: limit, Used: used}
	}
	l, err := scanCaptionLook(tx.QueryRow(ctx, captionLookInsertSQL, in.ID, in.OwnerUserID, in.Name, string(in.Style)))
	if err != nil {
		return nil, used, err
	}
	return l, used + 1, tx.Commit(ctx)
}

// ListStudioCaptionLooks returns an owner's live looks, newest first.
func (s *Store) ListStudioCaptionLooks(ctx context.Context, ownerUID string, limit, offset int) ([]StudioCaptionLook, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.captionLookRows(ctx, captionLookListSQL, ownerUID, limit, offset)
}

// CountStudioCaptionLooks counts an owner's live looks.
func (s *Store) CountStudioCaptionLooks(ctx context.Context, ownerUID string) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	err := s.Pool.QueryRow(ctx, captionLookCountSQL, ownerUID).Scan(&n)
	return n, err
}

// GetStudioCaptionLook reads one live look of the owner. A soft-deleted,
// missing or not-owned look is ErrStudioCaptionLookNotFound.
func (s *Store) GetStudioCaptionLook(ctx context.Context, id, ownerUID string) (*StudioCaptionLook, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	l, err := scanCaptionLook(s.Pool.QueryRow(ctx, captionLookGetSQL, id, ownerUID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudioCaptionLookNotFound
	}
	return l, err
}

// StudioCaptionLookPatch is a save: nil fields stay unchanged.
type StudioCaptionLookPatch struct {
	Name  *string
	Style []byte
}

// SaveStudioCaptionLook is the compare-and-set save behind PUT If-Match. Zero
// rows is disambiguated into ErrStudioCaptionLookNotFound or
// *StudioCaptionLookRevisionConflictError.
func (s *Store) SaveStudioCaptionLook(ctx context.Context, id, ownerUID string, expectedRevision int, patch StudioCaptionLookPatch) (*StudioCaptionLook, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var styleArg any
	if patch.Style != nil {
		styleArg = string(patch.Style)
	}
	l, err := scanCaptionLook(s.Pool.QueryRow(ctx, captionLookSaveCASSQL, id, ownerUID, patch.Name, styleArg, expectedRevision))
	if errors.Is(err, pgx.ErrNoRows) {
		var current int
		var updated time.Time
		perr := s.Pool.QueryRow(ctx, captionLookProbeSQL, id, ownerUID).Scan(&current, &updated)
		if errors.Is(perr, pgx.ErrNoRows) {
			return nil, ErrStudioCaptionLookNotFound
		}
		if perr != nil {
			return nil, perr
		}
		return nil, &StudioCaptionLookRevisionConflictError{Revision: current, UpdatedAt: updated}
	}
	return l, err
}

// DeleteStudioCaptionLook soft-deletes (deleted_at = now). Idempotent; the look
// vanishes from every read at once and the ledger sweep removes the row.
func (s *Store) DeleteStudioCaptionLook(ctx context.Context, id, ownerUID string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, captionLookSoftDeleteSQL, id, ownerUID)
	return err
}

// DeleteStudioCaptionLookRow removes the row. Used by the sweeper and by
// account deletion. Idempotent.
func (s *Store) DeleteStudioCaptionLookRow(ctx context.Context, id string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, captionLookHardDeleteSQL, id)
	return err
}

// ExportStudioCaptionLooks lists the user's live looks, styles included, for
// the account data export (caption-looks.json).
func (s *Store) ExportStudioCaptionLooks(ctx context.Context, userID string) ([]StudioCaptionLook, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.captionLookRows(ctx, captionLookExportSQL, userID)
}

// deleteUserStudioCaptionLooks removes every look row of a user (the
// account-wide ledger sweep that runs first already removed the ledgered
// ones; the mm_users fk cascade is the last backstop).
func (s *Store) deleteUserStudioCaptionLooks(ctx context.Context, userID string) (int64, error) {
	ids, err := s.idList(ctx, userCaptionLookIDsSQL, userID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if err := s.DeleteStudioCaptionLookRow(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
