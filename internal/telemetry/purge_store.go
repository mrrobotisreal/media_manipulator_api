package telemetry

import (
	"context"
	"fmt"
	"time"
)

// MM23 decision 6: the hourly Postgres purges. Every rule is "a statement over
// rows older than ONE cutoff", so a repeat is a no-op, dry-run is the same WHERE
// under count(*), and the unit suite can pin the safety clauses on the text.

// Purge rule names (also the keys of the per-rule counts in mm_cleanup_runs).
const (
	PurgeUsageCounters  = "usage_counters"
	PurgeSafetyIncident = "safety_incidents"
	PurgeAPIRequestIPs  = "api_request_ips"
	PurgeRateLimitIPs   = "rate_limit_ips"
	PurgeCleanupRuns    = "cleanup_runs"
	PurgeLedgerDeleted  = "ledger_deleted"
	PurgeAccountDeletes = "account_deletions"
)

type purgeRule struct {
	table string
	// where uses $1 as the single cutoff.
	where string
	// set, when non-empty, makes the rule an UPDATE (an IP scrub) instead of a DELETE.
	set string
}

var purgeRules = map[string]purgeRule{
	// day < cutoff (cutoff = today - 400 d).
	PurgeUsageCounters: {table: "mm_usage_counters", where: "day < $1::date"},
	// cutoff = now. legal_hold rows are never touched.
	PurgeSafetyIncident: {table: "mm_safety_incidents",
		where: "retention_until IS NOT NULL AND retention_until < $1 AND legal_hold = false"},
	// cutoff = now - 90 d. Only rows that still hold an address, so a repeat finds none.
	PurgeAPIRequestIPs: {table: "mm_api_requests", set: "ip = NULL, cf_connecting_ip = NULL, x_forwarded_for = NULL",
		where: "created_at < $1 AND (ip IS NOT NULL OR cf_connecting_ip IS NOT NULL OR x_forwarded_for IS NOT NULL)"},
	PurgeRateLimitIPs: {table: "mm_rate_limit_events", set: "ip = NULL", where: "created_at < $1 AND ip IS NOT NULL"},
	// cutoff = now - 180 d; mm_cleanup_deleted_paths cascades.
	PurgeCleanupRuns: {table: "mm_cleanup_runs", where: "started_at < $1"},
	// cutoff = now - 30 d: audit rows of already-deleted ledger entries.
	PurgeLedgerDeleted: {table: "mm_retained_objects", where: "deleted_at IS NOT NULL AND deleted_at < $1"},
	// cutoff = now - 30 d: MM24 account-deletion tombstones.
	PurgeAccountDeletes: {table: "mm_account_deletions", where: "deleted_at < $1"},
}

// PurgeRuleNames lists the rules in the order the purger runs them.
func PurgeRuleNames() []string {
	return []string{PurgeUsageCounters, PurgeSafetyIncident, PurgeAPIRequestIPs, PurgeRateLimitIPs, PurgeCleanupRuns, PurgeLedgerDeleted, PurgeAccountDeletes}
}

func purgeSQL(r purgeRule) string {
	if r.set != "" {
		return fmt.Sprintf(`UPDATE %[1]s SET %[2]s WHERE ctid IN (SELECT ctid FROM %[1]s WHERE %[3]s LIMIT $2)`, r.table, r.set, r.where)
	}
	return fmt.Sprintf(`DELETE FROM %[1]s WHERE ctid IN (SELECT ctid FROM %[1]s WHERE %[2]s LIMIT $2)`, r.table, r.where)
}

func purgeCountSQL(r purgeRule) string {
	return fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s`, r.table, r.where)
}

// PurgeBatch applies one rule to at most limit rows older than cutoff and returns
// how many it changed. With dryRun it returns how many rows the rule matches in
// total and changes nothing.
func (s *Store) PurgeBatch(ctx context.Context, rule string, cutoff time.Time, limit int, dryRun bool) (int64, error) {
	r, ok := purgeRules[rule]
	if !ok {
		return 0, fmt.Errorf("unknown purge rule %q", rule)
	}
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	if dryRun {
		var n int64
		err := s.Pool.QueryRow(ctx, purgeCountSQL(r), cutoff).Scan(&n)
		return n, err
	}
	tag, err := s.Pool.Exec(ctx, purgeSQL(r), cutoff, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
