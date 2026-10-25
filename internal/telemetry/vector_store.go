package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM43: Motion (code name `vector`) persistence. vector_projects holds saved
// documents (sanitized VectorDoc v1 in a jsonb column); vector_exports is the
// counted-export ledger, written in the SAME transaction as the
// mm_usage_counters increment so the counter and the rows never disagree.
//
// owner_uid is the mm_users id. anon_owner is reserved (not written in v1).
// Every write predicates on owner_uid as defence in depth; the handler has
// already proven ownership through requireVectorOwner.

// Retained kind for a vector project row (ADR-0010). Mirrored by the CHECK in
// 20261025002_vector_projects.
const RetainedVectorProject = "vector_project"

// VectorProject is one vector_projects row. Document is nil on list reads.
type VectorProject struct {
	ID            string
	OwnerUID      string
	Name          string
	Document      []byte
	DocumentBytes int
	Revision      int
	ThumbnailKey  string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// VectorProjectInsert creates a project.
type VectorProjectInsert struct {
	ID       string
	OwnerUID string
	Name     string
	Document []byte // canonical JSON from vector.Doc.Canonical
}

// VectorProjectUpdate is a compare-and-set save. Nil fields are unchanged;
// ThumbnailKey "" clears the key.
type VectorProjectUpdate struct {
	ID               string
	OwnerUID         string
	ExpectedRevision int
	Name             *string
	Document         []byte // nil = unchanged
	ThumbnailKey     *string
}

// ErrVectorNotFound is a missing (or not-owned) vector project.
var ErrVectorNotFound = errors.New("vector project not found")

// VectorProjectLimitError is a create refused by the saved-project cap.
type VectorProjectLimitError struct {
	Limit int
	Used  int
}

func (e *VectorProjectLimitError) Error() string {
	return fmt.Sprintf("vector project limit reached (%d of %d)", e.Used, e.Limit)
}

// VectorRevisionConflictError is a stale If-Match: another tab or device saved
// first. Same semantics as the Studio conflict (single writer, never merge).
type VectorRevisionConflictError struct {
	CurrentRevision int
	UpdatedAt       time.Time
}

func (e *VectorRevisionConflictError) Error() string {
	return fmt.Sprintf("vector project revision conflict (current revision %d)", e.CurrentRevision)
}

// vectorProjectListColumns never includes `document`: a row can carry up to
// VECTOR_MAX_DOC_BYTES, and a list must stay cheap.
const vectorProjectListColumns = `id, coalesce(owner_uid, ''), name, octet_length(document::text), revision, coalesce(thumbnail_key, ''), created_at, updated_at`

// vectorProjectFullColumns is the single-project read: the list columns plus
// the document, in scanVectorProject's order.
const vectorProjectFullColumns = vectorProjectListColumns + `, document`

// The SQL, as constants so its semantics are unit-testable without Postgres.
const (
	// vectorProjectLockSQL serialises creates per owner for the transaction,
	// so two parallel POSTs cannot both pass the cap check.
	vectorProjectLockSQL   = `SELECT pg_advisory_xact_lock(hashtextextended('vector_projects:' || $1, 0))`
	vectorProjectCountSQL  = `SELECT count(*) FROM vector_projects WHERE owner_uid = $1`
	vectorProjectInsertSQL = `
INSERT INTO vector_projects (id, owner_uid, name, document, revision)
VALUES ($1, $2, $3, $4::jsonb, 1)
RETURNING ` + vectorProjectFullColumns

	vectorProjectListSQL = `SELECT ` + vectorProjectListColumns + `
FROM vector_projects WHERE owner_uid = $1 ORDER BY updated_at DESC LIMIT $2`

	vectorProjectGetSQL = `SELECT ` + vectorProjectFullColumns + ` FROM vector_projects WHERE id = $1`

	vectorProjectOwnerSQL = `SELECT coalesce(owner_uid, ''), coalesce(anon_owner, '') FROM vector_projects WHERE id = $1`

	// vectorProjectSaveCASSQL only lands when the stored revision still equals
	// If-Match; every successful save bumps revision. $4 (document) and $2
	// (name) are NULL when unchanged; $5 says whether $6 (thumbnail key)
	// applies, '' clearing it.
	vectorProjectSaveCASSQL = `
UPDATE vector_projects
SET name = coalesce($2, name),
    document = coalesce($4::jsonb, document),
    thumbnail_key = CASE WHEN $5 THEN nullif($6, '') ELSE thumbnail_key END,
    revision = revision + 1,
    updated_at = now()
WHERE id = $1 AND owner_uid = $3 AND revision = $7
RETURNING ` + vectorProjectFullColumns

	vectorProjectProbeSQL = `SELECT revision, updated_at FROM vector_projects WHERE id = $1 AND owner_uid = $2`

	vectorProjectDeleteSQL = `DELETE FROM vector_projects WHERE id = $1 RETURNING coalesce(thumbnail_key, '')`

	// vectorExportIncrementSQL is the same upsert RequireQuota uses: the row
	// lock it takes serialises concurrent exports of one subject.
	vectorExportIncrementSQL = `
INSERT INTO mm_usage_counters (subject_kind, subject_key, metric, day, count, updated_at)
VALUES ($1, $2, 'vector_export', $3, 1, now())
ON CONFLICT (subject_kind, subject_key, metric, day) DO UPDATE SET
	count = mm_usage_counters.count + 1,
	updated_at = now()
RETURNING count`

	vectorExportInsertSQL = `
INSERT INTO vector_exports (id, project_id, owner, tier, format, height, fps, duration_ms, created_at)
VALUES ($1, nullif($2, ''), $3, $4, $5, $6, $7, $8, $9)`
)

func scanVectorProject(row pgx.Row, full bool) (*VectorProject, error) {
	var p VectorProject
	dest := []any{&p.ID, &p.OwnerUID, &p.Name, &p.DocumentBytes, &p.Revision, &p.ThumbnailKey, &p.CreatedAt, &p.UpdatedAt}
	if full {
		dest = append(dest, &p.Document)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreateVectorProject inserts a project after checking the owner's cap inside
// the same transaction (advisory lock + count). limit < 0 means unlimited.
func (s *Store) CreateVectorProject(ctx context.Context, in VectorProjectInsert, limit int) (*VectorProject, error) {
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
		if _, err := tx.Exec(ctx, vectorProjectLockSQL, in.OwnerUID); err != nil {
			return nil, err
		}
		var used int
		if err := tx.QueryRow(ctx, vectorProjectCountSQL, in.OwnerUID).Scan(&used); err != nil {
			return nil, err
		}
		if used >= limit {
			return nil, &VectorProjectLimitError{Limit: limit, Used: used}
		}
	}
	p, err := scanVectorProject(tx.QueryRow(ctx, vectorProjectInsertSQL, in.ID, in.OwnerUID, in.Name, string(in.Document)), true)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit(ctx)
}

// ListVectorProjects returns an owner's projects, newest first, WITHOUT documents.
func (s *Store) ListVectorProjects(ctx context.Context, ownerUID string, limit int) ([]VectorProject, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, vectorProjectListSQL, ownerUID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VectorProject{}
	for rows.Next() {
		p, err := scanVectorProject(rows, false)
		if err != nil {
			return out, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetVectorProject reads one project with its document.
func (s *Store) GetVectorProject(ctx context.Context, id string) (*VectorProject, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	p, err := scanVectorProject(s.Pool.QueryRow(ctx, vectorProjectGetSQL, id), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrVectorNotFound
	}
	return p, err
}

// VectorProjectOwner reads a project's owner columns for requireVectorOwner.
func (s *Store) VectorProjectOwner(ctx context.Context, id string) (ownerUID, anonOwner string, found bool, err error) {
	if !s.Enabled() {
		return "", "", false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	err = s.Pool.QueryRow(ctx, vectorProjectOwnerSQL, id).Scan(&ownerUID, &anonOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return ownerUID, anonOwner, err == nil, err
}

// SaveVectorProject is the compare-and-set save behind PUT If-Match. Zero rows
// is disambiguated into ErrVectorNotFound or *VectorRevisionConflictError.
func (s *Store) SaveVectorProject(ctx context.Context, in VectorProjectUpdate) (*VectorProject, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var doc any
	if in.Document != nil {
		doc = string(in.Document)
	}
	thumbSet, thumb := in.ThumbnailKey != nil, ""
	if thumbSet {
		thumb = *in.ThumbnailKey
	}
	p, err := scanVectorProject(s.Pool.QueryRow(ctx, vectorProjectSaveCASSQL,
		in.ID, in.Name, in.OwnerUID, doc, thumbSet, thumb, in.ExpectedRevision), true)
	if errors.Is(err, pgx.ErrNoRows) {
		var current int
		var updated time.Time
		perr := s.Pool.QueryRow(ctx, vectorProjectProbeSQL, in.ID, in.OwnerUID).Scan(&current, &updated)
		if errors.Is(perr, pgx.ErrNoRows) {
			return nil, ErrVectorNotFound
		}
		if perr != nil {
			return nil, perr
		}
		return nil, &VectorRevisionConflictError{CurrentRevision: current, UpdatedAt: updated}
	}
	return p, err
}

// DeleteVectorProjectRows removes one project row and returns its thumbnail
// key(s) so the caller (the sweeper) can delete the S3 object. Idempotent.
func (s *Store) DeleteVectorProjectRows(ctx context.Context, id string) ([]string, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var key string
	err := s.Pool.QueryRow(ctx, vectorProjectDeleteSQL, id).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return nonEmpty(key), nil
}

// VectorExportRecord is one counted export about to be written.
type VectorExportRecord struct {
	ID          string
	ProjectID   string
	SubjectKind string
	SubjectKey  string
	Tier        string
	Format      string
	Height      int
	Fps         int
	DurationMs  int
	At          time.Time
}

// Owner is the vector_exports.owner value: "<subject kind>:<subject key>".
func (r VectorExportRecord) Owner() string { return r.SubjectKind + ":" + r.SubjectKey }

// CountVectorExport spends one unit of the vector_export allowance and writes
// the vector_exports row, in ONE transaction. When the increment takes the
// count past limit the transaction rolls back: a refused call writes no row and
// leaves the counter where it was. used is the count INCLUDING this call when
// allowed, and the count before it when refused. limit <= 0 is unlimited: the
// row is written and nothing is counted (operator testing never lands in the
// usage ledger, same rule as RequireQuota).
func (s *Store) CountVectorExport(ctx context.Context, rec VectorExportRecord, limit int64) (used int64, allowed bool, err error) {
	if !s.Enabled() {
		return 0, false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if limit > 0 {
		if err := tx.QueryRow(ctx, vectorExportIncrementSQL, rec.SubjectKind, rec.SubjectKey,
			rec.At.UTC().Format("2006-01-02")).Scan(&used); err != nil {
			return 0, false, err
		}
		if used > limit {
			return used - 1, false, nil // deferred Rollback undoes the increment
		}
	}
	if _, err := tx.Exec(ctx, vectorExportInsertSQL, rec.ID, rec.ProjectID, rec.Owner(), rec.Tier,
		rec.Format, rec.Height, rec.Fps, rec.DurationMs, rec.At.UTC()); err != nil {
		return 0, false, err
	}
	return used, true, tx.Commit(ctx)
}

// userVectorProjectIDsSQL and anonymizeUserVectorExportsSQL serve account
// deletion and delete-all (DeleteUserProjects / DetachUserJobs).
const (
	userVectorProjectIDsSQL       = `SELECT id FROM vector_projects WHERE owner_uid = $1`
	anonymizeUserVectorExportsSQL = `UPDATE vector_exports SET owner = 'deleted', project_id = NULL WHERE owner = 'user:' || $1`
)

// deleteUserVectorProjects removes every Motion project of a user. Thumbnail
// objects are ledgered s3_object rows owned by the user, so the account-wide
// sweep that runs before this deletes them.
func (s *Store) deleteUserVectorProjects(ctx context.Context, userID string) (int64, error) {
	ids, err := s.idList(ctx, userVectorProjectIDsSQL, userID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if _, err := s.DeleteVectorProjectRows(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// vectorExportProjectsSQL lists a user's Motion projects for the account data
// export: duration (ms in the document) is reported in seconds like Studio.
const vectorExportProjectsSQL = `
SELECT id, name, created_at, updated_at,
       coalesce((document->>'duration')::double precision, 0) / 1000.0, 0
FROM vector_projects WHERE owner_uid = $1 ORDER BY updated_at DESC`
