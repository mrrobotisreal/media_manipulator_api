package telemetry

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// MM24: who owns a job, read from Postgres so ownership (and downloads) survive
// an API restart. The in-memory JobManager map only lives as long as the process.

// JobRecord is what Postgres knows about a job: the durable mm_conversion_jobs
// row plus the owners recorded on its retention-ledger rows.
//
// UserIDs / Sessions are SETS of every owner id seen on either source. The
// ledger is consulted too because mm_conversion_jobs.session_id is a uuid
// column: a client session id that is not a uuid is stored there as NULL, but
// the ledger keeps the raw header text.
type JobRecord struct {
	Found          bool
	Status         string
	Tool           string
	Mode           string
	TargetFormat   string
	ResultFileName string
	UserIDs        []string
	Sessions       []string
	// AnyLedger is true when the job has retention-ledger rows; AnyLive when at
	// least one is undeleted and unexpired. A job with ledger rows and none live
	// has been deleted or swept: its results are no longer ours to serve.
	AnyLedger bool
	AnyLive   bool
}

// HasUser reports whether any recorded owner is this account.
func (r JobRecord) HasUser(id string) bool {
	for _, u := range r.UserIDs {
		if u != "" && u == id {
			return true
		}
	}
	return false
}

// HasSession reports whether this session id is a recorded owner.
func (r JobRecord) HasSession(id string) bool {
	for _, s := range r.Sessions {
		if s != "" && s == id {
			return true
		}
	}
	return false
}

const jobRecordRowQuery = `
SELECT status, coalesce(tool, ''), coalesce(mode, ''), coalesce(target_format, ''),
       coalesce(result_file_name, ''), coalesce(user_id::text, ''), coalesce(session_id::text, '')
FROM mm_conversion_jobs WHERE job_id = $1::uuid`

const jobRecordLedgerQuery = `
SELECT DISTINCT coalesce(owner_user_id::text, ''), coalesce(owner_session_id, ''),
       (deleted_at IS NULL AND expires_at > now())
FROM mm_retained_objects WHERE job_id = $1::uuid`

// JobRecord loads the durable record of a job. Found is false when neither the
// job row nor any ledger row exists (or jobID is not a uuid).
func (s *Store) JobRecord(ctx context.Context, jobID string) (JobRecord, error) {
	var rec JobRecord
	if !s.Enabled() {
		return rec, ErrNoStore
	}
	if uuidOrNil(jobID) == nil {
		return rec, nil
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()

	var userID, sessionID string
	err := s.Pool.QueryRow(ctx, jobRecordRowQuery, jobID).Scan(&rec.Status, &rec.Tool, &rec.Mode,
		&rec.TargetFormat, &rec.ResultFileName, &userID, &sessionID)
	switch {
	case err == nil:
		rec.Found = true
		rec.UserIDs = appendNonEmpty(rec.UserIDs, userID)
		rec.Sessions = appendNonEmpty(rec.Sessions, sessionID)
	case !errors.Is(err, pgx.ErrNoRows):
		return rec, err
	}

	rows, err := s.Pool.Query(ctx, jobRecordLedgerQuery, jobID)
	if err != nil {
		return rec, err
	}
	defer rows.Close()
	for rows.Next() {
		var u, sess string
		var live bool
		if err := rows.Scan(&u, &sess, &live); err != nil {
			return rec, err
		}
		rec.Found = true
		rec.AnyLedger = true
		rec.AnyLive = rec.AnyLive || live
		rec.UserIDs = appendNonEmpty(rec.UserIDs, u)
		rec.Sessions = appendNonEmpty(rec.Sessions, sess)
	}
	return rec, rows.Err()
}

func appendNonEmpty(dst []string, v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return dst
	}
	for _, d := range dst {
		if d == v {
			return dst
		}
	}
	return append(dst, v)
}
