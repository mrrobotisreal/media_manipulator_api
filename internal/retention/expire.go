package retention

import (
	"context"
	"errors"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM24: "expire now + sweep inline". One code path serves every user-initiated
// delete (a job, a project, all my files, account deletion): it marks the
// matching live ledger rows due and runs the SAME per-kind deleters the ticker
// uses on exactly those rows, so there is no second deleter.

// ExpireBudget bounds one inline sweep; what is left stays due for the ticker.
const ExpireBudget = 30 * time.Second

// ExpireStore is the extra slice of *telemetry.Store ExpireNow needs. The store
// passed to NewSweeper must implement it (the production store does).
type ExpireStore interface {
	ExpireLedger(ctx context.Context, f telemetry.LedgerFilter, now time.Time) ([]string, error)
	ClaimByIDs(ctx context.Context, ids []string, now time.Time, limit int, lease time.Duration) ([]telemetry.LedgerRow, error)
	PendingByIDs(ctx context.Context, ids []string) (int, error)
}

// ErrNoExpire is returned when the sweeper's store cannot expire rows.
var ErrNoExpire = errors.New("retention: store does not support inline expiry")

// ExpireResult is what an inline sweep did.
type ExpireResult struct {
	Matched int   `json:"-"` // live rows the filter matched
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`
	Pending int   `json:"pending"` // rows still undeleted (S3 failure, running job, budget)
}

// ExpireNow expires the rows matching f and sweeps exactly those rows
// synchronously for at most ExpireBudget. It ignores dry-run: dry run protects
// against a bulk sweep misjudging what is overdue, while this acts only on rows
// the user just asked to delete. It is idempotent: a repeat matches nothing.
func (s *Sweeper) ExpireNow(ctx context.Context, f telemetry.LedgerFilter) (ExpireResult, error) {
	var out ExpireResult
	if s == nil || s.store == nil {
		return out, ErrNoExpire
	}
	es, ok := s.store.(ExpireStore)
	if !ok {
		return out, ErrNoExpire
	}
	ids, err := es.ExpireLedger(ctx, f, s.now())
	if err != nil {
		return out, err
	}
	out.Matched = len(ids)
	if len(ids) == 0 {
		return out, nil
	}
	res := &Result{PerKind: map[string]*KindResult{}}
	deadline := s.now().Add(ExpireBudget)
	batch := s.cfg.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	for ctx.Err() == nil && s.now().Before(deadline) {
		rows, err := es.ClaimByIDs(ctx, ids, s.now(), batch, claimLease)
		if err != nil {
			s.logger.Warn("retention: inline claim failed", "error", err.Error())
			break
		}
		if len(rows) == 0 {
			break
		}
		s.processBatch(ctx, rows, res)
	}
	for _, kr := range res.PerKind {
		out.Objects += kr.Deleted
	}
	out.Bytes = res.Bytes
	pending, err := es.PendingByIDs(context.WithoutCancel(ctx), ids)
	if err != nil {
		// Unknown: report everything not provably gone as pending.
		pending = len(ids) - out.Objects
		if pending < 0 {
			pending = 0
		}
	}
	out.Pending = pending
	return out, nil
}
