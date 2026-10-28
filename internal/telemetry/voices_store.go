package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM77: saved voices (design docs/design/VOICES.md §4.2, migration
// 20261028001_mm_voices). Every read and write is scoped to the owning
// mm_users id; there is no anonymous voice and no operator bypass.
//
// Life of a voice: CreateVoice inserts it (the saved-voice cap is checked in
// the same transaction); SoftDeleteVoice hides it from every read at once; the
// retention sweeper (ledger kind "voice") then hard-deletes the row with
// DeleteVoiceRow and removes its S3 objects. DeleteUserVoices is the account
// deletion backstop for rows the ledger never knew.

// RetainedVoice is the ledger kind of a voice row (ADR-0010). Its reference and
// preview objects are s3_object rows with parent_locator = the voice id.
const RetainedVoice = "voice"

// Voice kinds (mm_voices.kind CHECK).
const (
	VoiceKindCloned   = "cloned"
	VoiceKindDesigned = "designed"
	VoiceKindPreset   = "preset"
)

// Voice is one mm_voices row.
type Voice struct {
	ID                 string
	UserID             string
	Name               string
	Kind               string
	EngineVersion      string
	LanguageHint       string
	Description        string
	Seed               *int64
	ReferenceKey       string
	PreviewKey         string
	ConsentAt          *time.Time
	ConsentTextVersion string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ErrVoiceNotFound is a missing, deleted or not-owned voice.
var ErrVoiceNotFound = errors.New("voice not found")

// VoiceLimitError is a create refused by the saved-voice cap.
type VoiceLimitError struct {
	Limit int
	Used  int
}

func (e *VoiceLimitError) Error() string { return "saved voice limit reached" }

const voiceColumns = `id::text, user_id::text, name, kind, engine_version, language_hint,
coalesce(description, ''), seed, coalesce(reference_s3_key, ''), coalesce(preview_s3_key, ''),
consent_at, coalesce(consent_text_version, ''), created_at, updated_at`

const (
	voiceLockSQL   = `SELECT pg_advisory_xact_lock(hashtextextended('mm_voices:' || $1, 0))`
	voiceCountSQL  = `SELECT count(*) FROM mm_voices WHERE user_id = $1::uuid AND deleted_at IS NULL`
	voiceInsertSQL = `
INSERT INTO mm_voices (id, user_id, name, kind, engine_version, language_hint, description, seed,
                       reference_s3_key, preview_s3_key, consent_at, consent_text_version)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, nullif($7, ''), $8, nullif($9, ''), nullif($10, ''), $11, nullif($12, ''))
RETURNING ` + voiceColumns
	voiceListSQL = `SELECT ` + voiceColumns + `
FROM mm_voices WHERE user_id = $1::uuid AND deleted_at IS NULL ORDER BY created_at DESC`
	voiceGetSQL = `SELECT ` + voiceColumns + `
FROM mm_voices WHERE id = $1::uuid AND user_id = $2::uuid AND deleted_at IS NULL`
	voiceRenameSQL = `
UPDATE mm_voices SET name = $3, updated_at = now()
WHERE id = $1::uuid AND user_id = $2::uuid AND deleted_at IS NULL
RETURNING ` + voiceColumns
	voiceSoftDeleteSQL = `
UPDATE mm_voices SET deleted_at = now(), updated_at = now()
WHERE id = $1::uuid AND user_id = $2::uuid AND deleted_at IS NULL`
	voiceHardDeleteSQL = `
DELETE FROM mm_voices WHERE id = $1::uuid
RETURNING coalesce(reference_s3_key, ''), coalesce(preview_s3_key, '')`
	voiceDeleteUserSQL = `DELETE FROM mm_voices WHERE user_id = $1::uuid`
	voiceExportSQL     = `
SELECT id::text, name, kind, language_hint, coalesce(description, ''), engine_version,
       consent_at, coalesce(consent_text_version, ''), created_at, updated_at
FROM mm_voices WHERE user_id = $1::uuid AND deleted_at IS NULL ORDER BY created_at DESC`
)

func scanVoice(row pgx.Row) (*Voice, error) {
	var v Voice
	if err := row.Scan(&v.ID, &v.UserID, &v.Name, &v.Kind, &v.EngineVersion, &v.LanguageHint,
		&v.Description, &v.Seed, &v.ReferenceKey, &v.PreviewKey, &v.ConsentAt, &v.ConsentTextVersion,
		&v.CreatedAt, &v.UpdatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

// CreateVoice inserts a voice after checking the owner's live-voice count
// against limit inside the same transaction (advisory lock + count), so two
// concurrent creates cannot both pass. limit < 0 means unlimited.
func (s *Store) CreateVoice(ctx context.Context, v Voice, limit int) (*Voice, error) {
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
		if _, err := tx.Exec(ctx, voiceLockSQL, v.UserID); err != nil {
			return nil, err
		}
		var used int
		if err := tx.QueryRow(ctx, voiceCountSQL, v.UserID).Scan(&used); err != nil {
			return nil, err
		}
		if used >= limit {
			return nil, &VoiceLimitError{Limit: limit, Used: used}
		}
	}
	out, err := scanVoice(tx.QueryRow(ctx, voiceInsertSQL, v.ID, v.UserID, v.Name, v.Kind, v.EngineVersion,
		v.LanguageHint, v.Description, v.Seed, v.ReferenceKey, v.PreviewKey, v.ConsentAt, v.ConsentTextVersion))
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

// ListVoices returns the owner's live voices, newest first.
func (s *Store) ListVoices(ctx context.Context, userID string) ([]Voice, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, voiceListSQL, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Voice{}
	for rows.Next() {
		v, err := scanVoice(rows)
		if err != nil {
			return out, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// GetVoice reads one live voice of the owner. A missing, deleted or foreign
// voice is ErrVoiceNotFound.
func (s *Store) GetVoice(ctx context.Context, id, userID string) (*Voice, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	v, err := scanVoice(s.Pool.QueryRow(ctx, voiceGetSQL, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrVoiceNotFound
	}
	return v, err
}

// RenameVoice renames one live voice of the owner.
func (s *Store) RenameVoice(ctx context.Context, id, userID, name string) (*Voice, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	v, err := scanVoice(s.Pool.QueryRow(ctx, voiceRenameSQL, id, userID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrVoiceNotFound
	}
	return v, err
}

// SoftDeleteVoice hides one live voice of the owner. found=false when there
// was nothing to hide (already deleted, missing or foreign).
func (s *Store) SoftDeleteVoice(ctx context.Context, id, userID string) (bool, error) {
	if !s.Enabled() {
		return false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tag, err := s.Pool.Exec(ctx, voiceSoftDeleteSQL, id, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CountVoices is the owner's live-voice count.
func (s *Store) CountVoices(ctx context.Context, userID string) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	err := s.Pool.QueryRow(ctx, voiceCountSQL, userID).Scan(&n)
	return n, err
}

// DeleteVoiceRow hard-deletes one voice row (live or soft-deleted) and returns
// its S3 keys so the sweeper can delete the objects. Idempotent.
func (s *Store) DeleteVoiceRow(ctx context.Context, id string) ([]string, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var ref, preview string
	err := s.Pool.QueryRow(ctx, voiceHardDeleteSQL, id).Scan(&ref, &preview)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return nonEmpty(ref, preview), nil
}

// DeleteUserVoices hard-deletes every voice row of a user (account deletion).
// The reference and preview objects are ledgered s3_object rows owned by the
// user, so the account-wide sweep that runs before this deletes them.
func (s *Store) DeleteUserVoices(ctx context.Context, userID string) (int64, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	tag, err := s.Pool.Exec(ctx, voiceDeleteUserSQL, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ExportVoice is one voice in the account data export: metadata only, never
// audio or storage keys (generated audio is in History).
type ExportVoice struct {
	ID                 string
	Name               string
	Kind               string
	LanguageHint       string
	Description        string
	EngineVersion      string
	ConsentAt          *time.Time
	ConsentTextVersion string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ExportVoices lists the user's live voices for GET /api/account/data/export.
func (s *Store) ExportVoices(ctx context.Context, userID string) ([]ExportVoice, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.Pool.Query(ctx, voiceExportSQL, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExportVoice{}
	for rows.Next() {
		var e ExportVoice
		if err := rows.Scan(&e.ID, &e.Name, &e.Kind, &e.LanguageHint, &e.Description, &e.EngineVersion,
			&e.ConsentAt, &e.ConsentTextVersion, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
