package handlers

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Usage & spend analytics + the manually-managed credit ledger. All money
// SUMMATION happens in SQL over numeric(14,6); Go only subtracts/relays the
// already-summed values for JSON display. All date bucketing is UTC. The
// dimension/bucket values are ALLOWLISTED enums switched through Go maps —
// user input is never concatenated into SQL.

// ----------------------------------------------------------------------- //
// Range + enum parsing (pure allowlists; unit-tested)
// ----------------------------------------------------------------------- //

// statsDimension is one validated breakdown dimension: the GROUP BY key
// expression, the label expression (aggregated with MAX so renamed snapshots
// collapse), and an optional live-name join.
type statsDimension struct {
	keyExpr   string
	labelExpr string
	join      string
}

// statsDimensions is the allowlist for GET /chatlab/stats/breakdown. Labels
// prefer the LIVE name and fall back to the insert-time snapshot, so spend
// survives hard deletes with a readable label.
var statsDimensions = map[string]statsDimension{
	"model": {keyExpr: "e.model", labelExpr: "e.model"},
	"user":  {keyExpr: "e.user_email", labelExpr: "e.user_email"},
	"kind":  {keyExpr: "e.kind", labelExpr: "e.kind"},
	"project": {
		keyExpr:   "COALESCE(e.project_id::text, 'general')",
		labelExpr: "CASE WHEN e.project_id IS NULL THEN 'General chats' ELSE COALESCE(p.name, e.project_name, 'deleted project') END",
		join:      "LEFT JOIN dr_chat_projects p ON p.id = e.project_id",
	},
	"session": {
		keyExpr:   "COALESCE(e.session_id::text, 'unknown')",
		labelExpr: "COALESCE(s.title, e.session_title, 'deleted chat')",
		join:      "LEFT JOIN dr_chat_sessions s ON s.id = e.session_id",
	},
}

// statsTimeseriesDimensions is the allowlist for the timeseries dimension
// (the key expression grouped per bucket). "none" has no key.
var statsTimeseriesDimensions = map[string]string{
	"none":  "",
	"model": "e.model",
	"kind":  "e.kind",
}

// statsBuckets is the allowlist for date_trunc units.
var statsBuckets = map[string]bool{"day": true, "week": true, "month": true}

// parseStatsRange reads optional from/to (RFC3339). ok=false → a 400 was
// already written.
func parseStatsRange(c *gin.Context) (from, to *time.Time, ok bool) {
	parse := func(name string) (*time.Time, bool) {
		raw := strings.TrimSpace(c.Query(name))
		if raw == "" {
			return nil, true
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid %s (RFC3339 expected)", name)})
			return nil, false
		}
		u := t.UTC()
		return &u, true
	}
	from, ok = parse("from")
	if !ok {
		return nil, nil, false
	}
	to, ok = parse("to")
	if !ok {
		return nil, nil, false
	}
	if from != nil && to != nil && from.After(*to) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from must be before to"})
		return nil, nil, false
	}
	return from, to, true
}

// statsRangeWhere builds the occurred_at range condition + args (1-based).
func statsRangeWhere(from, to *time.Time) (string, []any) {
	conds := []string{"true"}
	args := []any{}
	if from != nil {
		args = append(args, *from)
		conds = append(conds, fmt.Sprintf("e.occurred_at >= $%d", len(args)))
	}
	if to != nil {
		args = append(args, *to)
		conds = append(conds, fmt.Sprintf("e.occurred_at <= $%d", len(args)))
	}
	return strings.Join(conds, " AND "), args
}

// ----------------------------------------------------------------------- //
// Balance semantics (pure mirrors of the SQL; unit-tested)
// ----------------------------------------------------------------------- //

// ledgerEntryLite / usageCostEvent are fixture shapes for the pure balance
// functions below. Production summation runs in SQL (Hard Constraint 8); these
// functions PIN the semantics in unit tests: trackingSince = MIN(effective_at)
// over ALL entries; credited counts entries with effective_at <= now (a
// BACKDATED top-up retroactively shifts the balance — that is what "added on a
// date" means); spend counts usage from trackingSince to now, with NULL costs
// contributing 0 (they under-count — the summary surfaces unknownCostEvents).
type ledgerEntryLite struct {
	AmountUSD   float64
	EffectiveAt time.Time
}

type usageCostEvent struct {
	OccurredAt time.Time
	CostUSD    *float64
}

func ledgerTrackingAndCredited(entries []ledgerEntryLite, now time.Time) (trackingSince *time.Time, totalCredited float64) {
	for _, e := range entries {
		et := e.EffectiveAt
		if trackingSince == nil || et.Before(*trackingSince) {
			t := et
			trackingSince = &t
		}
		if !et.After(now) {
			totalCredited += e.AmountUSD
		}
	}
	return trackingSince, totalCredited
}

func sumSpentUSD(events []usageCostEvent, trackingSince time.Time, now time.Time) float64 {
	var spent float64
	for _, e := range events {
		if e.OccurredAt.Before(trackingSince) || e.OccurredAt.After(now) {
			continue // pre-tracking usage is excluded from BALANCE (still in stats)
		}
		if e.CostUSD != nil {
			spent += *e.CostUSD
		}
	}
	return spent
}

// loadCreditBalance computes the "now" balance (independent of any from/to
// filter). Summation happens in SQL.
func (h *DrChatLabHandler) loadCreditBalance(ctx context.Context) (models.DrChatCreditBalance, error) {
	var balance models.DrChatCreditBalance
	var trackingSince *time.Time
	var credited float64
	err := h.pool.QueryRow(ctx, `
SELECT MIN(effective_at),
       COALESCE(SUM(amount_usd) FILTER (WHERE effective_at <= now()), 0)::float8
FROM dr_chat_credit_ledger`).Scan(&trackingSince, &credited)
	if err != nil {
		return balance, err
	}
	if trackingSince == nil {
		return balance, nil // hasLedger=false; UI shows the "set a starting balance" empty state
	}
	var spent float64
	err = h.pool.QueryRow(ctx, `
SELECT COALESCE(SUM(COALESCE(cost_usd, 0)), 0)::float8
FROM dr_chat_usage_events
WHERE occurred_at >= $1 AND occurred_at <= now()`, *trackingSince).Scan(&spent)
	if err != nil {
		return balance, err
	}
	ts := models.UTCTime{Time: *trackingSince}
	return models.DrChatCreditBalance{
		CurrentUsd:       credited - spent,
		TotalCreditedUsd: credited,
		TotalSpentUsd:    spent,
		TrackingSince:    &ts,
		HasLedger:        true,
	}, nil
}

// ----------------------------------------------------------------------- //
// GET /chatlab/stats/summary
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) StatsSummary(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	from, to, ok := parseStatsRange(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	where, args := statsRangeWhere(from, to)
	var totals models.DrChatStatsTotals
	err := h.pool.QueryRow(ctx, `
SELECT COALESCE(SUM(e.cost_usd), 0)::float8,
       COALESCE(SUM(e.prompt_tokens), 0)::bigint,
       COALESCE(SUM(e.completion_tokens), 0)::bigint,
       COALESCE(SUM(e.reasoning_tokens), 0)::bigint,
       COUNT(*)::bigint,
       COUNT(*) FILTER (WHERE e.kind = 'chat')::bigint,
       COUNT(*) FILTER (WHERE e.cost_usd IS NULL)::bigint,
       COUNT(*) FILTER (WHERE e.cost_estimated)::bigint
FROM dr_chat_usage_events e
WHERE `+where, args...).Scan(&totals.CostUsd, &totals.PromptTokens, &totals.CompletionTokens,
		&totals.ReasoningTokens, &totals.Events, &totals.ChatEvents, &totals.UnknownCostEvents, &totals.EstimatedCostEvents)
	if err != nil {
		log.Printf("dr chatlab stats: summary: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load stats"})
		return
	}

	balance, err := h.loadCreditBalance(ctx)
	if err != nil {
		log.Printf("dr chatlab stats: balance: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load stats"})
		return
	}
	c.JSON(http.StatusOK, models.DrChatStatsSummaryResponse{Totals: totals, Balance: balance})
}

// ----------------------------------------------------------------------- //
// GET /chatlab/stats/breakdown
// ----------------------------------------------------------------------- //

func (h *DrChatLabHandler) StatsBreakdown(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	dimension := strings.TrimSpace(c.Query("dimension"))
	dim, okDim := statsDimensions[dimension]
	if !okDim {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid dimension"})
		return
	}
	from, to, ok := parseStatsRange(c)
	if !ok {
		return
	}
	limit := 50
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	where, args := statsRangeWhere(from, to)
	query := fmt.Sprintf(`
SELECT %s AS key,
       MAX(%s) AS label,
       COALESCE(SUM(e.cost_usd), 0)::float8,
       COALESCE(SUM(e.prompt_tokens), 0)::bigint,
       COALESCE(SUM(e.completion_tokens), 0)::bigint,
       COALESCE(SUM(e.reasoning_tokens), 0)::bigint,
       COUNT(*)::bigint
FROM dr_chat_usage_events e
%s
WHERE %s
GROUP BY 1
ORDER BY 3 DESC
LIMIT %d`, dim.keyExpr, dim.labelExpr, dim.join, where, limit)

	rows, err := h.pool.Query(ctx, query, args...)
	if err != nil {
		log.Printf("dr chatlab stats: breakdown(%s): %v", dimension, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load stats"})
		return
	}
	out := make([]models.DrChatStatsBreakdownRow, 0)
	func() {
		defer rows.Close()
		for rows.Next() {
			var r models.DrChatStatsBreakdownRow
			if err := rows.Scan(&r.Key, &r.Label, &r.CostUsd, &r.PromptTokens, &r.CompletionTokens, &r.ReasoningTokens, &r.Events); err != nil {
				log.Printf("dr chatlab stats: scan breakdown: %v", err)
				continue
			}
			out = append(out, r)
		}
	}()

	// The model-comparison payoff of response feedback: per-model 👍/👎
	// counts merged onto the model breakdown (same time range, on the
	// feedback's created_at).
	if dimension == "model" && len(out) > 0 {
		fbWhere := []string{"true"}
		fbArgs := []any{}
		if from != nil {
			fbArgs = append(fbArgs, *from)
			fbWhere = append(fbWhere, fmt.Sprintf("created_at >= $%d", len(fbArgs)))
		}
		if to != nil {
			fbArgs = append(fbArgs, *to)
			fbWhere = append(fbWhere, fmt.Sprintf("created_at <= $%d", len(fbArgs)))
		}
		fbRows, err := h.pool.Query(ctx, `
SELECT model,
       COUNT(*) FILTER (WHERE rating = 'up')::bigint,
       COUNT(*) FILTER (WHERE rating = 'down')::bigint
FROM dr_chat_message_feedback
WHERE `+strings.Join(fbWhere, " AND ")+`
GROUP BY model`, fbArgs...)
		if err != nil {
			log.Printf("dr chatlab stats: feedback counts: %v", err)
		} else {
			type fbCount struct{ up, down int64 }
			counts := map[string]fbCount{}
			func() {
				defer fbRows.Close()
				for fbRows.Next() {
					var model string
					var up, down int64
					if err := fbRows.Scan(&model, &up, &down); err == nil {
						counts[model] = fbCount{up, down}
					}
				}
			}()
			for i := range out {
				fc := counts[out[i].Key]
				up, down := fc.up, fc.down
				out[i].ThumbsUp, out[i].ThumbsDown = &up, &down
			}
		}
	}

	c.JSON(http.StatusOK, models.DrChatStatsBreakdownResponse{Rows: out})
}

// ----------------------------------------------------------------------- //
// GET /chatlab/stats/timeseries
// ----------------------------------------------------------------------- //

// rollupTimeseriesPoints keeps the top `topN` keys by total cost and merges
// everything else into per-bucket "other" points so charts stay legible. Pure;
// unit-tested. Points without a key (dimension=none) pass through untouched.
// (Per-key sums come from SQL; this only re-buckets already-summed values for
// display.)
func rollupTimeseriesPoints(points []models.DrChatStatsTimeseriesPoint, topN int) []models.DrChatStatsTimeseriesPoint {
	totals := map[string]float64{}
	hasKeys := false
	for _, p := range points {
		if p.Key != "" {
			hasKeys = true
			totals[p.Key] += p.CostUsd
		}
	}
	if !hasKeys || len(totals) <= topN {
		return points
	}
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return keys[i] < keys[j]
	})
	top := map[string]bool{}
	for _, k := range keys[:topN] {
		top[k] = true
	}

	out := make([]models.DrChatStatsTimeseriesPoint, 0, len(points))
	otherByBucket := map[string]*models.DrChatStatsTimeseriesPoint{}
	bucketOrder := []string{}
	for _, p := range points {
		if p.Key == "" || top[p.Key] {
			out = append(out, p)
			continue
		}
		o, ok := otherByBucket[p.Bucket]
		if !ok {
			otherByBucket[p.Bucket] = &models.DrChatStatsTimeseriesPoint{Bucket: p.Bucket, Key: "other"}
			o = otherByBucket[p.Bucket]
			bucketOrder = append(bucketOrder, p.Bucket)
		}
		o.CostUsd += p.CostUsd
		o.TotalTokens += p.TotalTokens
		o.Events += p.Events
	}
	for _, b := range bucketOrder {
		out = append(out, *otherByBucket[b])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	return out
}

func (h *DrChatLabHandler) StatsTimeseries(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	bucket := strings.TrimSpace(c.Query("bucket"))
	if !statsBuckets[bucket] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid bucket"})
		return
	}
	dimension := strings.TrimSpace(c.Query("dimension"))
	if dimension == "" {
		dimension = "none"
	}
	keyExpr, okDim := statsTimeseriesDimensions[dimension]
	if !okDim {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid dimension"})
		return
	}
	from, to, ok := parseStatsRange(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	where, args := statsRangeWhere(from, to)
	args = append(args, bucket)
	bucketArg := fmt.Sprintf("$%d", len(args))

	// AT TIME ZONE 'UTC' pins the bucket boundaries to UTC regardless of the
	// server timezone (Hard Constraint 3).
	keySelect := "'' AS key"
	groupBy := "GROUP BY 1"
	orderBy := "ORDER BY 1"
	if keyExpr != "" {
		keySelect = keyExpr + " AS key"
		groupBy = "GROUP BY 1, 2"
		orderBy = "ORDER BY 1, 2"
	}
	query := fmt.Sprintf(`
SELECT date_trunc(%s, e.occurred_at AT TIME ZONE 'UTC') AS bucket,
       %s,
       COALESCE(SUM(e.cost_usd), 0)::float8,
       COALESCE(SUM(e.prompt_tokens + e.completion_tokens + e.reasoning_tokens), 0)::bigint,
       COUNT(*)::bigint
FROM dr_chat_usage_events e
WHERE %s
%s
%s`, bucketArg, keySelect, where, groupBy, orderBy)

	rows, err := h.pool.Query(ctx, query, args...)
	if err != nil {
		log.Printf("dr chatlab stats: timeseries: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load stats"})
		return
	}
	points := make([]models.DrChatStatsTimeseriesPoint, 0)
	func() {
		defer rows.Close()
		for rows.Next() {
			var p models.DrChatStatsTimeseriesPoint
			var bucketTime time.Time
			if err := rows.Scan(&bucketTime, &p.Key, &p.CostUsd, &p.TotalTokens, &p.Events); err != nil {
				log.Printf("dr chatlab stats: scan timeseries: %v", err)
				continue
			}
			p.Bucket = bucketTime.UTC().Format(time.RFC3339)
			points = append(points, p)
		}
	}()

	c.JSON(http.StatusOK, models.DrChatStatsTimeseriesResponse{Points: rollupTimeseriesPoints(points, 8)})
}

// ----------------------------------------------------------------------- //
// Credit ledger — GET / POST / PUT / DELETE (shared lab bookkeeping)
// ----------------------------------------------------------------------- //

// validateCreditEntry enforces the ledger rules, returning a client-facing
// message ("" = valid). Pure; unit-tested.
func validateCreditEntry(entryType string, amountUsd float64, effectiveAt time.Time) string {
	switch entryType {
	case "deposit":
		if amountUsd <= 0 {
			return "Deposit amount must be positive"
		}
	case "adjustment":
		if amountUsd == 0 {
			return "Adjustment amount must be non-zero"
		}
	default:
		return "Entry type must be 'deposit' or 'adjustment'"
	}
	if math.Abs(amountUsd) > 100000 {
		return "Amount is too large"
	}
	if y := effectiveAt.UTC().Year(); y < 2000 || y > 2100 {
		return "Effective date is out of range"
	}
	return ""
}

const chatCreditEntryCols = `id, entry_type, amount_usd::float8, effective_at, note, created_by_email, created_at, updated_at`

func scanCreditEntry(row pgx.Row) (models.DrChatCreditEntry, error) {
	var e models.DrChatCreditEntry
	var effectiveAt, createdAt, updatedAt time.Time
	err := row.Scan(&e.ID, &e.EntryType, &e.AmountUsd, &effectiveAt, &e.Note, &e.CreatedByEmail, &createdAt, &updatedAt)
	if err != nil {
		return e, err
	}
	e.EffectiveAt = models.UTCTime{Time: effectiveAt}
	e.CreatedAt = models.UTCTime{Time: createdAt}
	e.UpdatedAt = models.UTCTime{Time: updatedAt}
	return e, nil
}

func drChatLabCreditID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("creditId"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid entry id"})
		return "", false
	}
	return id, true
}

func parseCreditRequest(c *gin.Context) (req models.DrChatCreditEntryRequest, effectiveAt time.Time, ok bool) {
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return req, effectiveAt, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(req.EffectiveAt))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid effectiveAt (RFC3339 expected)"})
		return req, effectiveAt, false
	}
	if msg := validateCreditEntry(req.EntryType, req.AmountUsd, t); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return req, effectiveAt, false
	}
	return req, t.UTC(), true
}

func (h *DrChatLabHandler) ListCredits(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	balance, err := h.loadCreditBalance(ctx)
	if err != nil {
		log.Printf("dr chatlab credits: balance: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load credits"})
		return
	}
	rows, err := h.pool.Query(ctx, `SELECT `+chatCreditEntryCols+` FROM dr_chat_credit_ledger ORDER BY effective_at DESC, created_at DESC`)
	if err != nil {
		log.Printf("dr chatlab credits: list: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load credits"})
		return
	}
	entries := make([]models.DrChatCreditEntry, 0)
	func() {
		defer rows.Close()
		for rows.Next() {
			e, err := scanCreditEntry(rows)
			if err != nil {
				log.Printf("dr chatlab credits: scan: %v", err)
				continue
			}
			entries = append(entries, e)
		}
	}()
	c.JSON(http.StatusOK, models.DrChatCreditsResponse{Balance: balance, Entries: entries})
}

func (h *DrChatLabHandler) CreateCreditEntry(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	req, effectiveAt, ok := parseCreditRequest(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	entry, err := scanCreditEntry(h.pool.QueryRow(ctx, `
INSERT INTO dr_chat_credit_ledger (entry_type, amount_usd, effective_at, note, created_by_uid, created_by_email)
VALUES ($1, $2, $3, $4, $5, lower($6))
RETURNING `+chatCreditEntryCols,
		req.EntryType, req.AmountUsd, effectiveAt, strings.TrimSpace(req.Note), claims.UID, claims.Email))
	if err != nil {
		log.Printf("dr chatlab credits: create: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save entry"})
		return
	}
	c.JSON(http.StatusCreated, entry)
}

// UpdateCreditEntry / DeleteCreditEntry: any allowlisted user may edit or
// delete any entry — the ledger is shared lab bookkeeping, like project edits.
func (h *DrChatLabHandler) UpdateCreditEntry(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	entryID, ok := drChatLabCreditID(c)
	if !ok {
		return
	}
	req, effectiveAt, ok := parseCreditRequest(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	entry, err := scanCreditEntry(h.pool.QueryRow(ctx, `
UPDATE dr_chat_credit_ledger
SET entry_type = $1, amount_usd = $2, effective_at = $3, note = $4, updated_at = now()
WHERE id = $5
RETURNING `+chatCreditEntryCols,
		req.EntryType, req.AmountUsd, effectiveAt, strings.TrimSpace(req.Note), entryID))
	if err != nil {
		if err == pgx.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "Entry not found"})
			return
		}
		log.Printf("dr chatlab credits: update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save entry"})
		return
	}
	c.JSON(http.StatusOK, entry)
}

func (h *DrChatLabHandler) DeleteCreditEntry(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	entryID, ok := drChatLabCreditID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_chat_credit_ledger WHERE id = $1`, entryID); err != nil {
		log.Printf("dr chatlab credits: delete: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete entry"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
