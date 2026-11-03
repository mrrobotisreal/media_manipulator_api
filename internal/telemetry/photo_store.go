package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM95: Aperture (code name `photo`) persistence. photo_projects holds the
// saved PhotoDoc (sanitized, jsonb) plus the facts about the original file kept
// on disk under PHOTO_DIR/<id>/; photo_derivatives caches server-made masks and
// heal bases. Every route is signed-in only, so every row has an owner.
//
// Ids are uuids; the Go side carries them as strings and casts in SQL. Every
// write predicates on owner_user_id as defence in depth; the handler has
// already proven ownership.

// RetainedPhotoProject is the ledger kind of a project directory (ADR-0010).
// locator = project id. Mirrored by the CHECK in 20261103001_photo_projects.
const RetainedPhotoProject = "photo_project"

// Photo project statuses (CHECK in the migration).
const (
	PhotoStatusIngesting = "ingesting"
	PhotoStatusReady     = "ready"
	PhotoStatusFailed    = "failed"
)

// Photo derivative kinds (CHECK in the migration).
const (
	PhotoDerivativeSubjectMask = "subject_mask"
	PhotoDerivativeHealBase    = "heal_base"
)

// PhotoProject is one photo_projects row. Doc is nil on list reads.
type PhotoProject struct {
	ID             string
	OwnerUserID    string
	Name           string
	Doc            []byte
	Revision       int
	OriginalExt    string
	OriginalWidth  int
	OriginalHeight int
	OriginalBytes  int64
	HasAlpha       bool
	Status         string
	Error          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PhotoProjectInsert creates a project (status 'ingesting' unless set).
type PhotoProjectInsert struct {
	ID             string // uuid
	OwnerUserID    string
	Name           string
	Doc            []byte // canonical JSON from photo.Sanitize
	OriginalExt    string
	OriginalWidth  int
	OriginalHeight int
	OriginalBytes  int64
	HasAlpha       bool
	Status         string
}

// PhotoDerivative is one photo_derivatives row.
type PhotoDerivative struct {
	ID         string
	ProjectID  string
	Kind       string
	Relpath    string
	ParamsHash string
	Width      int
	Height     int
	Bytes      int64
	CreatedAt  time.Time
}

// ErrPhotoNotFound is a missing (or not-owned, or soft-deleted) photo project.
var ErrPhotoNotFound = errors.New("photo project not found")

// PhotoProjectLimitError is a create refused by the per-tier project cap.
type PhotoProjectLimitError struct {
	Limit int
	Used  int
}

func (e *PhotoProjectLimitError) Error() string {
	return fmt.Sprintf("photo project limit reached (%d of %d)", e.Used, e.Limit)
}

// PhotoRevisionConflictError is a stale If-Match.
type PhotoRevisionConflictError struct {
	Revision  int
	UpdatedAt time.Time
}

func (e *PhotoRevisionConflictError) Error() string {
	return fmt.Sprintf("photo project revision conflict (current revision %d)", e.Revision)
}

// photoListColumns never includes `doc`; photoFullColumns appends it last
// (scanPhotoProject's order).
const photoListColumns = `id::text, owner_user_id::text, name, revision, original_ext, original_width, original_height, original_bytes, has_alpha, status, coalesce(error, ''), created_at, updated_at`
const photoFullColumns = photoListColumns + `, doc`

const (
	photoLockSQL   = `SELECT pg_advisory_xact_lock(hashtextextended('photo_projects:' || $1, 0))`
	photoCountSQL  = `SELECT count(*) FROM photo_projects WHERE owner_user_id = $1::uuid AND deleted_at IS NULL`
	photoInsertSQL = `
INSERT INTO photo_projects (id, owner_user_id, name, doc, revision, original_ext, original_width, original_height, original_bytes, has_alpha, status)
VALUES ($1::uuid, $2::uuid, $3, $4::jsonb, 1, $5, $6, $7, $8, $9, $10)
RETURNING ` + photoFullColumns

	photoListSQL = `SELECT ` + photoListColumns + `
FROM photo_projects WHERE owner_user_id = $1::uuid AND deleted_at IS NULL
ORDER BY updated_at DESC LIMIT $2 OFFSET $3`

	photoGetSQL = `SELECT ` + photoFullColumns + ` FROM photo_projects WHERE id = $1::uuid AND deleted_at IS NULL`

	// photoSaveCASSQL only lands when the stored revision still equals If-Match;
	// every successful save bumps revision. $3 (name) and $4 (doc) are NULL when
	// unchanged.
	photoSaveCASSQL = `
UPDATE photo_projects
SET name = coalesce($3, name),
    doc = coalesce($4::jsonb, doc),
    revision = revision + 1,
    updated_at = now()
WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL AND revision = $5
RETURNING ` + photoFullColumns

	photoProbeSQL = `SELECT revision, updated_at FROM photo_projects WHERE id = $1::uuid AND owner_user_id = $2::uuid AND deleted_at IS NULL`

	photoSoftDeleteSQL = `UPDATE photo_projects SET deleted_at = coalesce(deleted_at, now()) WHERE id = $1::uuid AND owner_user_id = $2::uuid`
	photoHardDeleteSQL = `DELETE FROM photo_projects WHERE id = $1::uuid`

	photoSetStatusSQL = `
UPDATE photo_projects
SET status = $2, error = nullif($3, ''), updated_at = now()
WHERE id = $1::uuid
RETURNING ` + photoFullColumns

	photoDerivativeUpsertSQL = `
INSERT INTO photo_derivatives (project_id, kind, relpath, params_hash, width, height, bytes)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
ON CONFLICT (project_id, kind, params_hash) DO UPDATE SET
    relpath = EXCLUDED.relpath, width = EXCLUDED.width, height = EXCLUDED.height, bytes = EXCLUDED.bytes
RETURNING id::text, project_id::text, kind, relpath, params_hash, width, height, bytes, created_at`

	photoDerivativeFindSQL = `
SELECT id::text, project_id::text, kind, relpath, params_hash, width, height, bytes, created_at
FROM photo_derivatives WHERE project_id = $1::uuid AND kind = $2 AND params_hash = $3`

	// userPhotoProjectIDsSQL serves account deletion; photoExportProjectsSQL the
	// account data export (kind photo).
	userPhotoProjectIDsSQL = `SELECT id::text FROM photo_projects WHERE owner_user_id = $1::uuid`
	photoExportProjectsSQL = `SELECT id::text, name, created_at, updated_at, 0.0, 0 FROM photo_projects WHERE owner_user_id = $1::uuid AND deleted_at IS NULL ORDER BY updated_at DESC`
)

func scanPhotoProject(row pgx.Row, full bool) (*PhotoProject, error) {
	var p PhotoProject
	dest := []any{&p.ID, &p.OwnerUserID, &p.Name, &p.Revision, &p.OriginalExt, &p.OriginalWidth, &p.OriginalHeight,
		&p.OriginalBytes, &p.HasAlpha, &p.Status, &p.Error, &p.CreatedAt, &p.UpdatedAt}
	if full {
		dest = append(dest, &p.Doc)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePhotoProject inserts a project after checking the owner's cap in the
// same transaction (advisory lock + count). limit < 0 means unlimited.
func (s *Store) CreatePhotoProject(ctx context.Context, in PhotoProjectInsert, limit int) (*PhotoProject, error) {
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
		if _, err := tx.Exec(ctx, photoLockSQL, in.OwnerUserID); err != nil {
			return nil, err
		}
		var used int
		if err := tx.QueryRow(ctx, photoCountSQL, in.OwnerUserID).Scan(&used); err != nil {
			return nil, err
		}
		if used >= limit {
			return nil, &PhotoProjectLimitError{Limit: limit, Used: used}
		}
	}
	status := in.Status
	if status == "" {
		status = PhotoStatusIngesting
	}
	p, err := scanPhotoProject(tx.QueryRow(ctx, photoInsertSQL, in.ID, in.OwnerUserID, in.Name, string(in.Doc),
		in.OriginalExt, in.OriginalWidth, in.OriginalHeight, in.OriginalBytes, in.HasAlpha, status), true)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit(ctx)
}

// CountPhotoProjects counts an owner's live projects.
func (s *Store) CountPhotoProjects(ctx context.Context, ownerUID string) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	err := s.Pool.QueryRow(ctx, photoCountSQL, ownerUID).Scan(&n)
	return n, err
}

// ListPhotoProjects returns an owner's live projects, newest first, WITHOUT docs.
func (s *Store) ListPhotoProjects(ctx context.Context, ownerUID string, limit, offset int) ([]PhotoProject, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, photoListSQL, ownerUID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PhotoProject{}
	for rows.Next() {
		p, err := scanPhotoProject(rows, false)
		if err != nil {
			return out, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetPhotoProject reads one live project with its doc. A soft-deleted or
// missing project is ErrPhotoNotFound.
func (s *Store) GetPhotoProject(ctx context.Context, id string) (*PhotoProject, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	p, err := scanPhotoProject(s.Pool.QueryRow(ctx, photoGetSQL, id), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPhotoNotFound
	}
	return p, err
}

// SavePhotoProject is the compare-and-set save behind PUT If-Match. name == nil
// and doc == nil leave those columns alone. Zero rows is disambiguated into
// ErrPhotoNotFound or *PhotoRevisionConflictError.
func (s *Store) SavePhotoProject(ctx context.Context, id, ownerUID string, expectedRevision int, name *string, doc []byte) (*PhotoProject, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var docArg any
	if doc != nil {
		docArg = string(doc)
	}
	p, err := scanPhotoProject(s.Pool.QueryRow(ctx, photoSaveCASSQL, id, ownerUID, name, docArg, expectedRevision), true)
	if errors.Is(err, pgx.ErrNoRows) {
		var current int
		var updated time.Time
		perr := s.Pool.QueryRow(ctx, photoProbeSQL, id, ownerUID).Scan(&current, &updated)
		if errors.Is(perr, pgx.ErrNoRows) {
			return nil, ErrPhotoNotFound
		}
		if perr != nil {
			return nil, perr
		}
		return nil, &PhotoRevisionConflictError{Revision: current, UpdatedAt: updated}
	}
	return p, err
}

// SetPhotoProjectStatus records the end of ingest (ready / failed + error).
func (s *Store) SetPhotoProjectStatus(ctx context.Context, id, status, errMsg string) (*PhotoProject, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	p, err := scanPhotoProject(s.Pool.QueryRow(ctx, photoSetStatusSQL, id, status, errMsg), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPhotoNotFound
	}
	return p, err
}

// DeletePhotoProject soft-deletes (deleted_at = now). Idempotent; the project
// vanishes from every read at once and the ledger sweep removes directory and row.
func (s *Store) DeletePhotoProject(ctx context.Context, id, ownerUID string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, photoSoftDeleteSQL, id, ownerUID)
	return err
}

// DeletePhotoProjectRows removes the row (derivatives cascade). Used by the
// sweeper after the directory is gone, and by account deletion. Idempotent.
func (s *Store) DeletePhotoProjectRows(ctx context.Context, id string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, photoHardDeleteSQL, id)
	return err
}

// PhotoProjectOwner reads a live project's owner for the ownership check.
func (s *Store) PhotoProjectOwner(ctx context.Context, id string) (ownerUID string, found bool, err error) {
	if !s.Enabled() {
		return "", false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	err = s.Pool.QueryRow(ctx, `SELECT owner_user_id::text FROM photo_projects WHERE id = $1::uuid AND deleted_at IS NULL`, id).Scan(&ownerUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return ownerUID, err == nil, err
}

// UpsertPhotoDerivative records (or refreshes) a cached derivative.
func (s *Store) UpsertPhotoDerivative(ctx context.Context, d PhotoDerivative) (*PhotoDerivative, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var out PhotoDerivative
	err := s.Pool.QueryRow(ctx, photoDerivativeUpsertSQL, d.ProjectID, d.Kind, d.Relpath, d.ParamsHash, d.Width, d.Height, d.Bytes).
		Scan(&out.ID, &out.ProjectID, &out.Kind, &out.Relpath, &out.ParamsHash, &out.Width, &out.Height, &out.Bytes, &out.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FindPhotoDerivative returns the cached derivative, or (nil, nil) when none.
func (s *Store) FindPhotoDerivative(ctx context.Context, projectID, kind, hash string) (*PhotoDerivative, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var out PhotoDerivative
	err := s.Pool.QueryRow(ctx, photoDerivativeFindSQL, projectID, kind, hash).
		Scan(&out.ID, &out.ProjectID, &out.Kind, &out.Relpath, &out.ParamsHash, &out.Width, &out.Height, &out.Bytes, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// deleteUserPhotoProjects removes every Aperture project row of a user. Their
// directories are ledgered photo_project rows owned by the user, so the
// account-wide sweep that runs before this removes the files; the mm_users fk
// cascade is the last backstop.
func (s *Store) deleteUserPhotoProjects(ctx context.Context, userID string) (int64, error) {
	ids, err := s.idList(ctx, userPhotoProjectIDsSQL, userID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if err := s.DeletePhotoProjectRows(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
