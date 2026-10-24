package services

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Export history (part 20): studio_exports rows, one per render the editor
// started. The row is inserted before the job runs and settled by the job's
// terminal path, so the Exports panel survives a page leave or API restart.

const studioExportColumns = `id, project_id, job_id, coalesce(file_name, ''), coalesce(preset, ''), format, resolution, captions_mode, coalesce(loudness, ''), status, coalesce(error, ''), created_at, completed_at`

const studioInsertExportSQL = `
INSERT INTO studio_exports (project_id, job_id, file_name, preset, format, resolution, captions_mode, loudness)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING ` + studioExportColumns

// Settling only moves a 'running' row: a late or duplicate callback can never
// reopen or overwrite a finished export.
const studioSettleExportSQL = `
UPDATE studio_exports
SET status = $2, error = $3, completed_at = now()
WHERE job_id = $1 AND status = 'running'`

// The list is scoped by project_id; the handler gates the project first.
const studioListExportsSQL = `
SELECT ` + studioExportColumns + `
FROM studio_exports
WHERE project_id = $1
ORDER BY created_at DESC, id DESC
LIMIT 100`

// studioGetExportSQL is scoped by project_id as well as id, so an export id can
// never be reached through a project the caller doesn't own.
const studioGetExportSQL = `
SELECT ` + studioExportColumns + `
FROM studio_exports
WHERE project_id = $1 AND id = $2`

func scanStudioExport(row scannable) (*models.StudioExport, error) {
	var e models.StudioExport
	if err := row.Scan(&e.ID, &e.ProjectID, &e.JobID, &e.FileName, &e.Preset, &e.Format, &e.Resolution,
		&e.CaptionsMode, &e.Loudness, &e.Status, &e.Error, &e.CreatedAt, &e.CompletedAt); err != nil {
		return nil, err
	}
	return &e, nil
}

// InsertExport records a started export.
func (r *StudioRepository) InsertExport(ctx context.Context, e models.StudioExport) (*models.StudioExport, error) {
	return scanStudioExport(r.pool.QueryRow(ctx, studioInsertExportSQL,
		e.ProjectID, e.JobID, nullableStr(e.FileName), nullableStr(e.Preset), e.Format, e.Resolution,
		e.CaptionsMode, nullableStr(e.Loudness)))
}

// SettleExport marks a running export completed or failed. errClass is a coarse
// class (never raw ffmpeg output) and is ignored for completed.
func (r *StudioRepository) SettleExport(ctx context.Context, jobID string, ok bool, errClass string) error {
	status := models.StudioExportCompleted
	if !ok {
		status = models.StudioExportFailed
	} else {
		errClass = ""
	}
	_, err := r.pool.Exec(ctx, studioSettleExportSQL, jobID, status, nullableStr(errClass))
	return err
}

// ListExports returns a project's export history, newest first.
func (r *StudioRepository) ListExports(ctx context.Context, projectID string) ([]*models.StudioExport, error) {
	rows, err := r.pool.Query(ctx, studioListExportsSQL, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.StudioExport, 0)
	for rows.Next() {
		e, err := scanStudioExport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetExport loads one export through its project.
func (r *StudioRepository) GetExport(ctx context.Context, projectID, exportID string) (*models.StudioExport, error) {
	e, err := scanStudioExport(r.pool.QueryRow(ctx, studioGetExportSQL, projectID, exportID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudioNotFound
	}
	return e, err
}

// StaleExportAfter is how long a 'running' row may sit before a read treats it
// as orphaned (the API restarted mid-render; the job map is in memory). The
// command timeout is the hard ceiling on a live render.
func StaleExportAfter(commandTimeout time.Duration) time.Duration {
	if commandTimeout <= 0 {
		commandTimeout = 30 * time.Minute
	}
	return commandTimeout + 2*time.Minute
}
