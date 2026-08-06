package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Version history (part 09, ADR ws/0001 item 3). studio_project_versions rows
// snapshot a project's tracks + captions sidecar + revision. The repository
// offers only the primitives — CreateVersion / ListVersions / GetVersion —
// and the handler composes restore from them (checkpoint current state first,
// then overwrite via the ordinary SaveProject path).

// studioVersionMetaColumns is the metadata column list (no snapshot bodies) —
// what ListVersions returns and every version INSERT RETURNINGs.
const studioVersionMetaColumns = `id, project_id, kind, name, revision, created_at`

// studioCreateVersionSQL snapshots the project's live tracks + captions
// sidecar + revision in ONE statement (INSERT … SELECT), so the checkpoint can
// never interleave with a concurrent save and capture a torn document. Zero
// rows means the project does not exist.
const studioCreateVersionSQL = `
INSERT INTO studio_project_versions (project_id, kind, name, revision, tracks, captions)
SELECT id, $2, $3, revision, tracks, captions
FROM studio_projects WHERE id = $1
RETURNING ` + studioVersionMetaColumns

// studioPruneAutoVersionsSQL deletes a project's 'auto' rows beyond the newest
// $2. Manual versions are deliberately outside the predicate — they are kept
// indefinitely.
const studioPruneAutoVersionsSQL = `
DELETE FROM studio_project_versions
WHERE project_id = $1 AND kind = 'auto' AND id NOT IN (
  SELECT id FROM studio_project_versions
  WHERE project_id = $1 AND kind = 'auto'
  ORDER BY created_at DESC, id DESC
  LIMIT $2
)`

// studioListVersionsSQL returns metadata only — the JSONB bodies stay on the
// server (a 12-track EDL is hundreds of KB; the panel needs a list, not the
// documents). Manuals are never pruned, so the LIMIT is a sanity backstop.
const studioListVersionsSQL = `
SELECT ` + studioVersionMetaColumns + `
FROM studio_project_versions
WHERE project_id = $1
ORDER BY created_at DESC, id DESC
LIMIT 200`

// studioGetVersionSQL is scoped by project_id as well as id, so a version can
// never be fetched through a project the caller doesn't own (the handler gates
// on the project; this predicate makes a cross-project versionId a miss).
const studioGetVersionSQL = `
SELECT id, project_id, kind, name, revision, tracks, captions, created_at
FROM studio_project_versions
WHERE project_id = $1 AND id = $2`

// StudioVersionSnapshot is a fully loaded version: the metadata plus the
// snapshot bodies parsed back into the same shapes SaveProject accepts, so the
// restore handler can feed them straight into the ordinary save path.
type StudioVersionSnapshot struct {
	Meta            models.StudioProjectVersion
	Tracks          []models.StudioTrack
	Captions        []models.StudioCaptionCue
	CaptionStyle    *models.StudioCaptionStyle
	CaptionsEnabled *bool
	Audio           *models.StudioAudioConfig
	Markers         []models.StudioMarker
}

// CreateVersion checkpoints the project's current state. After inserting an
// 'auto', autos beyond the newest autoKeep are pruned (best-effort — a failed
// prune must not fail the checkpoint that just landed). name is stored NULL
// when empty.
func (r *StudioRepository) CreateVersion(ctx context.Context, projectID, kind, name string, autoKeep int) (*models.StudioProjectVersion, error) {
	row := r.pool.QueryRow(ctx, studioCreateVersionSQL, projectID, kind, nullableStr(name))
	v, err := scanVersionMeta(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudioNotFound
	}
	if err != nil {
		return nil, err
	}
	if kind == models.StudioVersionKindAuto && autoKeep > 0 {
		if _, pruneErr := r.pool.Exec(ctx, studioPruneAutoVersionsSQL, projectID, autoKeep); pruneErr != nil {
			// The checkpoint is in; a temporarily unbounded auto list is the
			// benign failure mode. The next successful prune catches up.
			return v, nil
		}
	}
	return v, nil
}

// ListVersions returns a project's version metadata, newest first.
func (r *StudioRepository) ListVersions(ctx context.Context, projectID string) ([]*models.StudioProjectVersion, error) {
	rows, err := r.pool.Query(ctx, studioListVersionsSQL, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.StudioProjectVersion, 0)
	for rows.Next() {
		v, err := scanVersionMeta(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion loads one version in full, with the snapshot bodies parsed.
func (r *StudioRepository) GetVersion(ctx context.Context, projectID, versionID string) (*StudioVersionSnapshot, error) {
	var (
		snap             StudioVersionSnapshot
		name             *string
		tracks, captions []byte
	)
	err := r.pool.QueryRow(ctx, studioGetVersionSQL, projectID, versionID).Scan(
		&snap.Meta.ID, &snap.Meta.ProjectID, &snap.Meta.Kind, &name, &snap.Meta.Revision,
		&tracks, &captions, &snap.Meta.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudioNotFound
	}
	if err != nil {
		return nil, err
	}
	if name != nil {
		snap.Meta.Name = *name
	}
	if err := parseVersionBodies(&snap, tracks, captions); err != nil {
		return nil, err
	}
	return &snap, nil
}

// parseVersionBodies decodes the snapshot JSONB columns into SaveProject-ready
// fields. Split out (and pure) so the sidecar mapping — the part a refactor of
// studioProjectSidecar could silently break — is unit-testable without
// Postgres.
func parseVersionBodies(snap *StudioVersionSnapshot, tracks, captions []byte) error {
	if len(tracks) > 0 {
		if err := json.Unmarshal(tracks, &snap.Tracks); err != nil {
			return fmt.Errorf("unmarshal version tracks: %w", err)
		}
	}
	if snap.Tracks == nil {
		snap.Tracks = []models.StudioTrack{}
	}
	if len(captions) > 0 {
		var sc studioProjectSidecar
		if err := json.Unmarshal(captions, &sc); err != nil {
			return fmt.Errorf("unmarshal version captions: %w", err)
		}
		snap.Captions = sc.Cues
		snap.CaptionStyle = sc.Style
		snap.CaptionsEnabled = sc.Enabled
		snap.Audio = sc.Audio
		snap.Markers = sc.Markers
	}
	if snap.Captions == nil {
		snap.Captions = []models.StudioCaptionCue{}
	}
	return nil
}

func scanVersionMeta(row scannable) (*models.StudioProjectVersion, error) {
	var v models.StudioProjectVersion
	var name *string
	if err := row.Scan(&v.ID, &v.ProjectID, &v.Kind, &name, &v.Revision, &v.CreatedAt); err != nil {
		return nil, err
	}
	if name != nil {
		v.Name = *name
	}
	return &v, nil
}
