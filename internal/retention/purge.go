package retention

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM23 decision 6. Windows for the hourly Postgres purges.
const (
	UsageCounterRetentionDays = 400
	RawIPRetentionDays        = 90
	CleanupRunRetentionDays   = 180
	LedgerAuditRetentionDays  = 30
	PurgeBatchRows            = 5000
	PurgeInterval             = time.Hour
	maxPurgeBatchesPerRule    = 200
)

// PurgeStore is the slice of *telemetry.Store the purger uses.
type PurgeStore interface {
	PurgeBatch(ctx context.Context, rule string, cutoff time.Time, limit int, dryRun bool) (int64, error)
	UnparkMetadataReports(ctx context.Context, now time.Time) (int64, error)
}

// PurgeResult is rule name -> rows purged (or, in dry run, rows that match).
type PurgeResult map[string]int64

// Purger runs the Postgres retention rules.
type Purger struct {
	store  PurgeStore
	logger *slog.Logger
	now    func() time.Time
	// Batch is rows per statement (default PurgeBatchRows).
	Batch int
}

// NewPurger builds a purger.
func NewPurger(store PurgeStore, logger *slog.Logger) *Purger {
	if logger == nil {
		logger = slog.Default()
	}
	return &Purger{store: store, logger: logger, now: time.Now, Batch: PurgeBatchRows}
}

// SetClock replaces the clock (tests).
func (p *Purger) SetClock(now func() time.Time) {
	if p != nil && now != nil {
		p.now = now
	}
}

// Cutoff is the single cutoff each rule compares against.
func Cutoff(rule string, now time.Time) time.Time {
	day := func(n int) time.Time { return now.AddDate(0, 0, -n) }
	switch rule {
	case telemetry.PurgeUsageCounters:
		return day(UsageCounterRetentionDays)
	case telemetry.PurgeSafetyIncident:
		return now
	case telemetry.PurgeAPIRequestIPs, telemetry.PurgeRateLimitIPs:
		return day(RawIPRetentionDays)
	case telemetry.PurgeCleanupRuns:
		return day(CleanupRunRetentionDays)
	case telemetry.PurgeLedgerDeleted, telemetry.PurgeAccountDeletes:
		return day(LedgerAuditRetentionDays)
	}
	return time.Time{}
}

// Run applies every rule. In dry run each rule reports how many rows it would
// touch and nothing is changed (including the parked-metadata un-parking). A
// failing rule is logged and skipped; the others still run.
func (p *Purger) Run(ctx context.Context, dryRun bool) PurgeResult {
	out := PurgeResult{}
	if p == nil || p.store == nil {
		return out
	}
	now := p.now().UTC()
	batch := p.Batch
	if batch <= 0 {
		batch = PurgeBatchRows
	}
	for _, rule := range telemetry.PurgeRuleNames() {
		if ctx.Err() != nil {
			return out
		}
		cutoff := Cutoff(rule, now)
		var total int64
		for i := 0; i < maxPurgeBatchesPerRule && ctx.Err() == nil; i++ {
			n, err := p.store.PurgeBatch(ctx, rule, cutoff, batch, dryRun)
			if err != nil {
				if !errors.Is(err, telemetry.ErrNoStore) {
					p.logger.Warn("retention purge failed", "rule", rule, "error", err.Error())
				}
				break
			}
			total += n
			if dryRun || n < int64(batch) {
				break
			}
		}
		if total > 0 {
			out[rule] = total
		}
	}
	if !dryRun {
		if n, err := p.store.UnparkMetadataReports(ctx, now); err != nil {
			if !errors.Is(err, telemetry.ErrNoStore) {
				p.logger.Warn("retention: un-parking metadata reports failed", "error", err.Error())
			}
		} else if n > 0 {
			out["metadata_unparked"] = n
		}
	}
	return out
}
