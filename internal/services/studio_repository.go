package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// StudioRepository persists Content Studio projects + assets in Postgres. It is
// the only place that touches the studio_projects / studio_assets tables; the
// track/clip tree lives as a JSONB column on studio_projects (see the
// init_content_studio migration).
type StudioRepository struct {
	pool *pgxpool.Pool
}

func NewStudioRepository(pool *pgxpool.Pool) *StudioRepository {
	return &StudioRepository{pool: pool}
}

// Enabled reports whether a DB pool is available. Handlers degrade to a 503
// when it isn't, mirroring how the rest of the API treats an offline DB.
func (r *StudioRepository) Enabled() bool { return r != nil && r.pool != nil }

// ErrStudioNotFound is returned when a project/asset row doesn't exist.
var ErrStudioNotFound = errors.New("not found")

// studioProjectSidecar is the JSON envelope persisted in studio_projects.captions
// (the EDL v2 project sidecar). It bundles the caption set with the project-level
// extras so the caption-generate job and the autosave PUT write independent
// columns. `enabled` is a pointer so an absent value means "captions enabled"
// (the default for pre-v2 rows).
type studioProjectSidecar struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Cues          []models.StudioCaptionCue  `json:"cues"`
	Style         *models.StudioCaptionStyle `json:"style,omitempty"`
	Enabled       *bool                      `json:"enabled,omitempty"`
	Audio         *models.StudioAudioConfig  `json:"audio,omitempty"`
}

// StudioOwner identifies who a project belongs to.
//
// FIX B13. Projects used to be scoped by the CLIENT-SUPPLIED X-MM-Session-ID
// alone, and GetProject/SaveProject took only a project id — so guessing either
// value read or overwrote someone else's editor document. Ownership is now
// explicit and checked on every project-scoped call.
//
// A signed-in caller owns by user id, which survives a cleared session and a
// new device. A signed-out caller still owns by session id, because the
// anonymous editor has to keep working; those rows are claimed by the account
// the first time that visitor signs in (ClaimSessionProjects).
type StudioOwner struct {
	// UserID is the mm_users row id, or "" for an anonymous caller.
	UserID string
	// SessionID is always present.
	SessionID string
}

// userParam renders UserID for SQL: a real uuid, or NULL when anonymous.
func (o StudioOwner) userParam() any {
	if strings.TrimSpace(o.UserID) == "" {
		return nil
	}
	return o.UserID
}

func (r *StudioRepository) CreateProject(ctx context.Context, owner StudioOwner, req models.StudioCreateProjectRequest) (*models.StudioProject, error) {
	tracks := []byte("[]")
	row := r.pool.QueryRow(ctx, `
INSERT INTO studio_projects (session_id, user_id, name, fps, width, height, duration_seconds, tracks)
VALUES ($1, $2, $3, $4, $5, $6, 0, $7)
RETURNING id, name, fps, width, height, duration_seconds, tracks, captions, created_at, updated_at
`, owner.SessionID, owner.userParam(), req.Name, req.FPS, req.Width, req.Height, tracks)
	return scanProject(row)
}

// OwnsProject reports whether owner may read or write this project.
//
// A signed-in caller matches on user_id, or on an unclaimed row from their own
// session (the window between creating a project and the sign-in that claims
// it). An anonymous caller matches only an unclaimed row from their session —
// once a project belongs to an account, a bare session id can never reach it
// again, which is the actual hole B13 describes.
func (r *StudioRepository) OwnsProject(ctx context.Context, projectID string, owner StudioOwner) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM studio_projects
  WHERE id = $1
    AND (
      ($2::uuid IS NOT NULL AND user_id = $2::uuid)
      OR (user_id IS NULL AND session_id = $3)
    )
)
`, projectID, owner.userParam(), owner.SessionID).Scan(&exists)
	return exists, err
}

// ClaimSessionProjects attaches the projects a visitor created while signed out
// to the account they just signed into. Only unclaimed rows move, so it can
// never steal a project from another account, and it is idempotent.
func (r *StudioRepository) ClaimSessionProjects(ctx context.Context, userID, sessionID string) (int64, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
UPDATE studio_projects SET user_id = $1::uuid, updated_at = now()
WHERE session_id = $2 AND user_id IS NULL
`, userID, sessionID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CountProjectsForUser backs the saved-project cap. Anonymous session rows are
// deliberately not counted: the cap is an account entitlement.
func (r *StudioRepository) CountProjectsForUser(ctx context.Context, userID string) (int, error) {
	if strings.TrimSpace(userID) == "" {
		return 0, nil
	}
	var n int
	err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM studio_projects WHERE user_id = $1::uuid`, userID).Scan(&n)
	return n, err
}

func (r *StudioRepository) GetProject(ctx context.Context, id string) (*models.StudioProject, error) {
	row := r.pool.QueryRow(ctx, `
SELECT id, name, fps, width, height, duration_seconds, tracks, captions, created_at, updated_at
FROM studio_projects WHERE id = $1
`, id)
	p, err := scanProject(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudioNotFound
	}
	return p, err
}

func (r *StudioRepository) ListRecentProjects(ctx context.Context, owner StudioOwner, limit int) ([]*models.StudioProject, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := r.pool.Query(ctx, `
SELECT id, name, fps, width, height, duration_seconds, tracks, captions, created_at, updated_at
FROM studio_projects
WHERE ($1::uuid IS NOT NULL AND user_id = $1::uuid)
   OR (user_id IS NULL AND session_id = $2)
ORDER BY updated_at DESC LIMIT $3
`, owner.userParam(), owner.SessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.StudioProject, 0)
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveProject overwrites the editor document. durationSeconds is computed from
// the tracks (end of the last clip) so the field stays authoritative.
func (r *StudioRepository) SaveProject(ctx context.Context, id string, req models.StudioSaveProjectRequest) (*models.StudioProject, error) {
	// Sanitize before persisting so the stored EDL is always trustworthy (the
	// export compiler reads it back without re-validating).
	cleanTracks := models.SanitizeTracks(req.Tracks)
	tracks, err := json.Marshal(cleanTracks)
	if err != nil {
		return nil, fmt.Errorf("marshal tracks: %w", err)
	}
	enabled := true
	if req.CaptionsEnabled != nil {
		enabled = *req.CaptionsEnabled
	}
	sidecar := studioProjectSidecar{
		SchemaVersion: models.StudioSchemaVersionCurrent,
		Cues:          models.SanitizeCaptions(req.Captions),
		Style:         models.SanitizeCaptionStyle(req.CaptionStyle),
		Enabled:       &enabled,
		Audio:         models.SanitizeAudioConfig(req.Audio),
	}
	captions, err := json.Marshal(sidecar)
	if err != nil {
		return nil, fmt.Errorf("marshal captions: %w", err)
	}
	duration := computeProjectDuration(cleanTracks)
	// tracks + captions written in one statement → atomic; the caption-generate
	// job touches only the captions column (see SaveCaptions).
	row := r.pool.QueryRow(ctx, `
UPDATE studio_projects
SET name = $2, fps = $3, width = $4, height = $5, duration_seconds = $6, tracks = $7, captions = $8, updated_at = now()
WHERE id = $1
RETURNING id, name, fps, width, height, duration_seconds, tracks, captions, created_at, updated_at
`, id, req.Name, req.FPS, req.Width, req.Height, duration, tracks, captions)
	p, err := scanProject(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudioNotFound
	}
	return p, err
}

// SaveCaptions writes ONLY the cue list into the captions sidecar (via jsonb_set
// with create_missing), preserving the style/enabled/audio keys. This is used by
// the caption-generate job so it never clobbers the tracks the autosave PUT owns.
func (r *StudioRepository) SaveCaptions(ctx context.Context, id string, cues []models.StudioCaptionCue) error {
	if cues == nil {
		cues = []models.StudioCaptionCue{}
	}
	payload, err := json.Marshal(models.SanitizeCaptions(cues))
	if err != nil {
		return fmt.Errorf("marshal cues: %w", err)
	}
	tag, err := r.pool.Exec(ctx, `
UPDATE studio_projects
SET captions = jsonb_set(coalesce(captions, '{}'::jsonb), '{cues}', $2::jsonb, true), updated_at = now()
WHERE id = $1
`, id, payload)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStudioNotFound
	}
	return nil
}

func (r *StudioRepository) CreateAsset(ctx context.Context, a *models.StudioAsset) (*models.StudioAsset, error) {
	probe, _ := json.Marshal(a.ProbeJSON)
	if len(probe) == 0 {
		probe = []byte("{}")
	}
	row := r.pool.QueryRow(ctx, `
INSERT INTO studio_assets (
  project_id, original_file_name, s3_key_original, media_kind, duration_seconds,
  width, height, fps, video_codec, audio_codec, has_audio, sample_rate, channels, probe_json
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
RETURNING id, project_id, original_file_name, s3_key_original, s3_key_proxy, thumbnail_sprite_url, s3_key_peaks,
          media_kind, duration_seconds, width, height, fps, video_codec, audio_codec,
          has_audio, sample_rate, channels, probe_json, created_at
`,
		a.ProjectID, a.OriginalFileName, a.S3KeyOriginal, string(a.MediaKind), a.DurationSeconds,
		a.Width, a.Height, a.FPS, nullableStr(a.VideoCodec), nullableStr(a.AudioCodec), a.HasAudio,
		a.SampleRate, a.Channels, probe,
	)
	return scanAsset(row)
}

// SetAssetDerived records the proxy + filmstrip S3 keys once the ingest job has
// produced them. spriteKey may be empty for audio-only assets.
func (r *StudioRepository) SetAssetDerived(ctx context.Context, assetID, proxyKey, spriteKey string) error {
	_, err := r.pool.Exec(ctx, `
UPDATE studio_assets SET s3_key_proxy = $2, thumbnail_sprite_url = $3 WHERE id = $1
`, assetID, nullableStr(proxyKey), nullableStr(spriteKey))
	return err
}

// SetAssetPeaks records the waveform peaks S3 key (from ingest or on-demand
// backfill). Empty clears it.
func (r *StudioRepository) SetAssetPeaks(ctx context.Context, assetID, peaksKey string) error {
	_, err := r.pool.Exec(ctx, `
UPDATE studio_assets SET s3_key_peaks = $2 WHERE id = $1
`, assetID, nullableStr(peaksKey))
	return err
}

func (r *StudioRepository) GetAsset(ctx context.Context, id string) (*models.StudioAsset, error) {
	row := r.pool.QueryRow(ctx, `
SELECT id, project_id, original_file_name, s3_key_original, s3_key_proxy, thumbnail_sprite_url, s3_key_peaks,
       media_kind, duration_seconds, width, height, fps, video_codec, audio_codec,
       has_audio, sample_rate, channels, probe_json, created_at
FROM studio_assets WHERE id = $1
`, id)
	a, err := scanAsset(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudioNotFound
	}
	return a, err
}

func (r *StudioRepository) ListAssets(ctx context.Context, projectID string) ([]*models.StudioAsset, error) {
	rows, err := r.pool.Query(ctx, `
SELECT id, project_id, original_file_name, s3_key_original, s3_key_proxy, thumbnail_sprite_url, s3_key_peaks,
       media_kind, duration_seconds, width, height, fps, video_codec, audio_codec,
       has_audio, sample_rate, channels, probe_json, created_at
FROM studio_assets WHERE project_id = $1 ORDER BY created_at
`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.StudioAsset, 0)
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// row abstracts *pgx.Row and pgx.Rows for the shared scan helpers.
type scannable interface {
	Scan(dest ...any) error
}

func scanProject(row scannable) (*models.StudioProject, error) {
	var p models.StudioProject
	var tracks, captions []byte
	if err := row.Scan(&p.ID, &p.Name, &p.FPS, &p.Width, &p.Height, &p.DurationSeconds, &tracks, &captions, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if len(tracks) > 0 {
		if err := json.Unmarshal(tracks, &p.Tracks); err != nil {
			return nil, fmt.Errorf("unmarshal tracks: %w", err)
		}
	}
	if p.Tracks == nil {
		p.Tracks = []models.StudioTrack{}
	}
	// EDL v2 sidecar (captions / style / enabled / audio). The server always
	// reports the current schema version; pre-v2 rows have an empty '{}' sidecar
	// and default to captions-enabled.
	p.SchemaVersion = models.StudioSchemaVersionCurrent
	p.CaptionsEnabled = true
	if len(captions) > 0 {
		var sc studioProjectSidecar
		if err := json.Unmarshal(captions, &sc); err != nil {
			return nil, fmt.Errorf("unmarshal captions: %w", err)
		}
		p.Captions = sc.Cues
		p.CaptionStyle = sc.Style
		p.Audio = sc.Audio
		if sc.Enabled != nil {
			p.CaptionsEnabled = *sc.Enabled
		}
	}
	if p.Captions == nil {
		p.Captions = []models.StudioCaptionCue{}
	}
	return &p, nil
}

func scanAsset(row scannable) (*models.StudioAsset, error) {
	var a models.StudioAsset
	var kind string
	var proxyKey, spriteKey, peaksKey, videoCodec, audioCodec *string
	var probe []byte
	if err := row.Scan(
		&a.ID, &a.ProjectID, &a.OriginalFileName, &a.S3KeyOriginal, &proxyKey, &spriteKey, &peaksKey,
		&kind, &a.DurationSeconds, &a.Width, &a.Height, &a.FPS, &videoCodec, &audioCodec,
		&a.HasAudio, &a.SampleRate, &a.Channels, &probe, &a.CreatedAt,
	); err != nil {
		return nil, err
	}
	a.MediaKind = models.StudioMediaKind(kind)
	if proxyKey != nil {
		a.S3KeyProxy = *proxyKey
	}
	if spriteKey != nil {
		a.ThumbnailSpriteURL = *spriteKey
	}
	if peaksKey != nil {
		a.S3KeyPeaks = *peaksKey
	}
	if videoCodec != nil {
		a.VideoCodec = *videoCodec
	}
	if audioCodec != nil {
		a.AudioCodec = *audioCodec
	}
	if len(probe) > 0 {
		a.ProbeJSON = json.RawMessage(probe)
	}
	return &a, nil
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// computeProjectDuration returns the timeline end (seconds) = the latest clip
// end across all tracks. Used so studio_projects.duration_seconds stays
// authoritative on every save.
func computeProjectDuration(tracks []models.StudioTrack) float64 {
	var end float64
	for _, t := range tracks {
		for _, c := range t.Clips {
			clipEnd := c.TimelineStart + (c.SourceOut - c.SourceIn)
			if clipEnd > end {
				end = clipEnd
			}
		}
	}
	return end
}
