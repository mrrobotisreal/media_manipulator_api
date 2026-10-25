package telemetry

import (
	"context"
	"time"
)

// MM24: read-only queries behind GET /api/account/data/export. Every statement is
// keyed on the caller's mm_users id and selects only metadata, never media bytes,
// storage keys or secrets.

// ExportJob is one mm_conversion_jobs row of the user.
type ExportJob struct {
	JobID        string
	Tool         string
	Mode         string
	MediaKind    string
	SourceFormat string
	TargetFormat string
	Status       string
	CreatedAt    time.Time
	CompletedAt  *time.Time
	DurationMS   *int
	// DeletesOn is the earliest live-ledger expiry for the job, nil when nothing
	// of it is stored any more.
	DeletesOn *time.Time
}

// ExportProject is one Darkroom project of the user.
type ExportProject struct {
	// Kind is "studio" (Darkroom) or "vector" (Motion, MM43).
	Kind            string
	ID              string
	Name            string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DurationSeconds float64
	AssetCount      int
}

// ExportFile is one LIVE ledger row of the user (deleted rows are excluded).
type ExportFile struct {
	Kind       string
	JobID      string
	ProjectID  string
	SizeBytes  int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	KeptLonger bool
	// Parked marks a metadata report kept until the account is deleted.
	Parked bool
}

// ExportJobs lists every job row of the user, newest first.
func (s *Store) ExportJobs(ctx context.Context, userID string) ([]ExportJob, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `
SELECT j.job_id::text, coalesce(j.tool, ''), coalesce(j.mode, ''), coalesce(j.media_kind, ''),
       coalesce(j.source_format, ''), coalesce(j.target_format, ''), j.status,
       j.created_at, j.completed_at, j.duration_ms,
       (SELECT min(o.expires_at) FROM mm_retained_objects o
         WHERE o.job_id = j.job_id AND o.deleted_at IS NULL AND NOT o.keep_with_account)
FROM mm_conversion_jobs j WHERE j.user_id = $1::uuid ORDER BY j.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportJob
	for rows.Next() {
		var e ExportJob
		if err := rows.Scan(&e.JobID, &e.Tool, &e.Mode, &e.MediaKind, &e.SourceFormat, &e.TargetFormat, &e.Status,
			&e.CreatedAt, &e.CompletedAt, &e.DurationMS, &e.DeletesOn); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExportProjects lists the user's Darkroom projects.
func (s *Store) ExportProjects(ctx context.Context, userID string) ([]ExportProject, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `
SELECT p.id::text, p.name, p.created_at, p.updated_at, p.duration_seconds,
       (SELECT count(*) FROM studio_assets a WHERE a.project_id = p.id)
FROM studio_projects p WHERE p.user_id = $1::uuid ORDER BY p.updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportProject
	for rows.Next() {
		e := ExportProject{Kind: "studio"}
		if err := rows.Scan(&e.ID, &e.Name, &e.CreatedAt, &e.UpdatedAt, &e.DurationSeconds, &e.AssetCount); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	// MM43: Motion projects (no assets; duration from the document).
	vrows, err := s.Pool.Query(ctx, vectorExportProjectsSQL, userID)
	if err != nil {
		return out, err
	}
	defer vrows.Close()
	for vrows.Next() {
		e := ExportProject{Kind: "vector"}
		if err := vrows.Scan(&e.ID, &e.Name, &e.CreatedAt, &e.UpdatedAt, &e.DurationSeconds, &e.AssetCount); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, vrows.Err()
}

// ExportFiles lists the user's live ledger rows (expired-but-unswept rows are
// excluded too: they are already gone as far as the user is concerned).
func (s *Store) ExportFiles(ctx context.Context, userID string, now time.Time) ([]ExportFile, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `
SELECT kind, coalesce(job_id::text, ''), coalesce(project_id::text, ''), size_bytes,
       created_at, expires_at, kept_longer, keep_with_account
FROM mm_retained_objects
WHERE owner_user_id = $1::uuid AND deleted_at IS NULL AND (expires_at > $2 OR keep_with_account)
ORDER BY created_at DESC`, userID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportFile
	for rows.Next() {
		var e ExportFile
		if err := rows.Scan(&e.Kind, &e.JobID, &e.ProjectID, &e.SizeBytes, &e.CreatedAt, &e.ExpiresAt, &e.KeptLonger, &e.Parked); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExportMetadataJobs lists the job ids whose metadata reports are parked for the user.
func (s *Store) ExportMetadataJobs(ctx context.Context, userID string) ([]string, error) {
	return s.idList(ctx, `SELECT DISTINCT job_id::text FROM mm_retained_objects
		WHERE owner_user_id = $1::uuid AND kind = 'metadata_report' AND deleted_at IS NULL AND job_id IS NOT NULL`, userID)
}
