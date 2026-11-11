package burst

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists burst_runs. The reaper, planner and ledger all go through it,
// so tests use a fake.
type Store interface {
	SpendSource
	Insert(ctx context.Context, r *Run) error
	Update(ctx context.Context, r *Run) error
	// ListUnterminated returns every row whose terminated_at is NULL.
	ListUnterminated(ctx context.Context) ([]Run, error)
	// ListSince returns rows offered at or after since, newest first.
	ListSince(ctx context.Context, since time.Time) ([]Run, error)
}

// PGStore is the Postgres Store.
type PGStore struct{ Pool *pgxpool.Pool }

// NewPGStore wraps a pool (nil is allowed; every call then errors).
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{Pool: pool} }

// ErrNoDB is returned when there is no pool.
var ErrNoDB = errors.New("burst: no database")

const runCols = `id::text, job_id, workload, kind, family, arch, instance_type, instance_id, az, ami_id,
 state, tier, priority, predicted_wait_ms, offered_at, launched_at, ready_at, started_at, finished_at,
 terminated_at, bytes_in, bytes_out, reserved_usd_micros, cost_usd_micros, error_type, COALESCE(adopted_from::text, ''), COALESCE(purpose, 'burst'), session_until`

const insertRunSQL = `INSERT INTO burst_runs (id, job_id, workload, kind, family, arch, instance_type, instance_id, az, ami_id,
 state, tier, priority, predicted_wait_ms, offered_at, launched_at, ready_at, started_at, finished_at, terminated_at,
 bytes_in, bytes_out, reserved_usd_micros, cost_usd_micros, error_type, adopted_from, purpose, session_until)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,NULLIF($26,'')::uuid,$27,$28)`

const updateRunSQL = `UPDATE burst_runs SET job_id=$2, workload=$3, kind=$4, family=$5, arch=$6, instance_type=$7, instance_id=$8,
 az=$9, ami_id=$10, state=$11, tier=$12, priority=$13, predicted_wait_ms=$14, launched_at=$15, ready_at=$16,
 started_at=$17, finished_at=$18, terminated_at=$19, bytes_in=$20, bytes_out=$21, reserved_usd_micros=$22,
 cost_usd_micros=$23, error_type=$24, session_until=$25 WHERE id=$1`

// settledSQL sums the cost of rows offered since the cutoff. Rows with no
// settled cost yet (live) are covered by in-memory reservations.
const settledSQL = `SELECT COALESCE(SUM(cost_usd_micros), 0)::bigint FROM burst_runs WHERE offered_at >= $1`

func (s *PGStore) SettledMicrosSince(ctx context.Context, since time.Time) (int64, error) {
	if s == nil || s.Pool == nil {
		return 0, ErrNoDB
	}
	var n int64
	err := s.Pool.QueryRow(ctx, settledSQL, since).Scan(&n)
	return n, err
}

func (s *PGStore) Insert(ctx context.Context, r *Run) error {
	if s == nil || s.Pool == nil {
		return ErrNoDB
	}
	_, err := s.Pool.Exec(ctx, insertRunSQL, r.ID, r.JobID, r.Workload, r.Kind, r.Family, r.Arch, r.InstanceType,
		r.InstanceID, r.AZ, r.AMIID, string(r.State), r.Tier, r.Priority, r.PredictedWaitMs, r.OfferedAt,
		r.LaunchedAt, r.ReadyAt, r.StartedAt, r.FinishedAt, r.TerminatedAt, r.BytesIn, r.BytesOut,
		r.ReservedMicros, r.CostMicros, r.ErrorType, r.AdoptedFrom, purposeOf(r), r.SessionUntil)
	return err
}

func (s *PGStore) Update(ctx context.Context, r *Run) error {
	if s == nil || s.Pool == nil {
		return ErrNoDB
	}
	_, err := s.Pool.Exec(ctx, updateRunSQL, r.ID, r.JobID, r.Workload, r.Kind, r.Family, r.Arch, r.InstanceType,
		r.InstanceID, r.AZ, r.AMIID, string(r.State), r.Tier, r.Priority, r.PredictedWaitMs, r.LaunchedAt,
		r.ReadyAt, r.StartedAt, r.FinishedAt, r.TerminatedAt, r.BytesIn, r.BytesOut, r.ReservedMicros,
		r.CostMicros, r.ErrorType, r.SessionUntil)
	return err
}

func purposeOf(r *Run) string {
	if r.Purpose == PurposeLab {
		return PurposeLab
	}
	return PurposeBurst
}

// PurposeSpendSource reads settled spend of one purpose (MM138: the Lab
// sub-cap sums only purpose='lab' rows).
type PurposeSpendSource interface {
	SettledMicrosSincePurpose(ctx context.Context, since time.Time, purpose string) (int64, error)
}

const settledPurposeSQL = `SELECT COALESCE(SUM(cost_usd_micros), 0)::bigint FROM burst_runs WHERE offered_at >= $1 AND COALESCE(purpose, 'burst') = $2`

func (s *PGStore) SettledMicrosSincePurpose(ctx context.Context, since time.Time, purpose string) (int64, error) {
	if s == nil || s.Pool == nil {
		return 0, ErrNoDB
	}
	var n int64
	err := s.Pool.QueryRow(ctx, settledPurposeSQL, since, purpose).Scan(&n)
	return n, err
}

// PurposeSource adapts a PurposeSpendSource to the ledger's SpendSource for
// one purpose, so a second Ledger can enforce the Lab sub-cap.
type PurposeSource struct {
	Src     PurposeSpendSource
	Purpose string
}

func (p PurposeSource) SettledMicrosSince(ctx context.Context, since time.Time) (int64, error) {
	return p.Src.SettledMicrosSincePurpose(ctx, since, p.Purpose)
}

func scanRuns(rows pgx.Rows) ([]Run, error) {
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var st string
		if err := rows.Scan(&r.ID, &r.JobID, &r.Workload, &r.Kind, &r.Family, &r.Arch, &r.InstanceType, &r.InstanceID,
			&r.AZ, &r.AMIID, &st, &r.Tier, &r.Priority, &r.PredictedWaitMs, &r.OfferedAt, &r.LaunchedAt, &r.ReadyAt,
			&r.StartedAt, &r.FinishedAt, &r.TerminatedAt, &r.BytesIn, &r.BytesOut, &r.ReservedMicros, &r.CostMicros,
			&r.ErrorType, &r.AdoptedFrom, &r.Purpose, &r.SessionUntil); err != nil {
			return nil, err
		}
		r.State = State(st)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PGStore) ListUnterminated(ctx context.Context) ([]Run, error) {
	if s == nil || s.Pool == nil {
		return nil, ErrNoDB
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM burst_runs WHERE terminated_at IS NULL ORDER BY offered_at`)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

func (s *PGStore) ListSince(ctx context.Context, since time.Time) ([]Run, error) {
	if s == nil || s.Pool == nil {
		return nil, ErrNoDB
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM burst_runs WHERE offered_at >= $1 ORDER BY offered_at DESC`, since)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}
