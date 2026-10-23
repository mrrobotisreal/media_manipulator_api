package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// fakePurgeDB holds counts per rule and applies the cutoff semantics so the
// tests exercise batching, dry run and the legal-hold carve-out.
type fakePurgeDB struct {
	rows     map[string]int64
	holds    int64 // legal-hold incidents that must survive
	cutoffs  map[string]time.Time
	batches  map[string]int
	unparked int64
	failRule string
	dryCalls int
}

func (f *fakePurgeDB) PurgeBatch(_ context.Context, rule string, cutoff time.Time, limit int, dry bool) (int64, error) {
	if rule == f.failRule {
		return 0, errors.New("boom")
	}
	if f.cutoffs == nil {
		f.cutoffs, f.batches = map[string]time.Time{}, map[string]int{}
	}
	f.cutoffs[rule] = cutoff
	if dry {
		f.dryCalls++
		return f.rows[rule], nil
	}
	f.batches[rule]++
	n := f.rows[rule]
	if n > int64(limit) {
		n = int64(limit)
	}
	f.rows[rule] -= n
	return n, nil
}

func (f *fakePurgeDB) UnparkMetadataReports(context.Context, time.Time) (int64, error) {
	return f.unparked, nil
}

func TestPurgerAppliesEachRuleWithItsOwnCutoff(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	f := &fakePurgeDB{rows: map[string]int64{
		telemetry.PurgeUsageCounters: 3, telemetry.PurgeSafetyIncident: 2, telemetry.PurgeAPIRequestIPs: 4,
		telemetry.PurgeRateLimitIPs: 5, telemetry.PurgeCleanupRuns: 6, telemetry.PurgeLedgerDeleted: 7}}
	p := NewPurger(f, nil)
	p.SetClock(func() time.Time { return now })
	got := p.Run(t.Context(), false)

	want := map[string]int64{telemetry.PurgeUsageCounters: 3, telemetry.PurgeSafetyIncident: 2, telemetry.PurgeAPIRequestIPs: 4,
		telemetry.PurgeRateLimitIPs: 5, telemetry.PurgeCleanupRuns: 6, telemetry.PurgeLedgerDeleted: 7}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s purged %d, want %d", k, got[k], v)
		}
	}
	checks := map[string]time.Time{
		telemetry.PurgeUsageCounters:  now.AddDate(0, 0, -400),
		telemetry.PurgeSafetyIncident: now,
		telemetry.PurgeAPIRequestIPs:  now.AddDate(0, 0, -90),
		telemetry.PurgeRateLimitIPs:   now.AddDate(0, 0, -90),
		telemetry.PurgeCleanupRuns:    now.AddDate(0, 0, -180),
		telemetry.PurgeLedgerDeleted:  now.AddDate(0, 0, -30),
	}
	for rule, c := range checks {
		if !f.cutoffs[rule].Equal(c) {
			t.Errorf("%s cutoff = %v, want %v", rule, f.cutoffs[rule], c)
		}
	}
}

func TestPurgerBatchesAt5000(t *testing.T) {
	f := &fakePurgeDB{rows: map[string]int64{telemetry.PurgeAPIRequestIPs: 12001}}
	got := NewPurger(f, nil).Run(t.Context(), false)
	if got[telemetry.PurgeAPIRequestIPs] != 12001 || f.batches[telemetry.PurgeAPIRequestIPs] != 3 {
		t.Errorf("12001 rows should take 3 batches of <=5000: purged %d in %d", got[telemetry.PurgeAPIRequestIPs], f.batches[telemetry.PurgeAPIRequestIPs])
	}
}

func TestPurgerDryRunCountsAndChangesNothing(t *testing.T) {
	f := &fakePurgeDB{rows: map[string]int64{telemetry.PurgeUsageCounters: 9, telemetry.PurgeSafetyIncident: 1}, unparked: 4}
	got := NewPurger(f, nil).Run(t.Context(), true)
	if got[telemetry.PurgeUsageCounters] != 9 || f.rows[telemetry.PurgeUsageCounters] != 9 {
		t.Errorf("dry run must report and not change: %v rows=%v", got, f.rows)
	}
	if _, ok := got["metadata_unparked"]; ok {
		t.Error("dry run must not un-park metadata reports")
	}
	if len(f.batches) != 0 {
		t.Error("dry run must not call the mutating path")
	}
}

func TestPurgerOneFailingRuleDoesNotStopTheOthers(t *testing.T) {
	f := &fakePurgeDB{rows: map[string]int64{telemetry.PurgeUsageCounters: 1, telemetry.PurgeCleanupRuns: 2}, failRule: telemetry.PurgeUsageCounters}
	got := NewPurger(f, nil).Run(t.Context(), false)
	if got[telemetry.PurgeCleanupRuns] != 2 || got[telemetry.PurgeUsageCounters] != 0 {
		t.Errorf("got %v", got)
	}
}

func TestPurgerNilStoreIsANoOp(t *testing.T) {
	if got := NewPurger(nil, nil).Run(t.Context(), false); len(got) != 0 {
		t.Errorf("got %v", got)
	}
	var p *Purger
	if got := p.Run(t.Context(), false); len(got) != 0 {
		t.Errorf("nil purger: %v", got)
	}
}

func TestPurgerReportsUnparkedMetadata(t *testing.T) {
	f := &fakePurgeDB{rows: map[string]int64{}, unparked: 3}
	if got := NewPurger(f, nil).Run(t.Context(), false); got["metadata_unparked"] != 3 {
		t.Errorf("got %v", got)
	}
}
