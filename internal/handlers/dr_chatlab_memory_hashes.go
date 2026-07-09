package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Nightly, hash-gated project Memory.
//
// Instead of regenerating memory on every event (each regeneration is one
// OpenRouter completion — real money for a workspace that changes many times a
// day), every memory-relevant change now writes a git-style content hash for
// the changed ITEM into dr_chat_memory_hashes: a session's append cursor, the
// description, the instructions, the ready-asset manifest, or the project's
// feedback state. Hash writes sit on hot paths (every message persist), so
// they are cheap single upserts computed from values already in hand —
// NEVER a hash of full chat transcripts — and they log-and-continue like
// recordUsageEvent (Hard Constraint 6).
//
// One nightly job (default 4 AM America/Denver — see the scheduler in
// cmd/api/main.go) combines each project's hash rows into a deterministic
// fingerprint and regenerates memory ONLY when it differs from
// dr_chat_projects.memory_source_hash, the fingerprint stored at the last
// successful generation. An unchanged project costs zero API calls. The manual
// "Refresh memory" button keeps its immediate, single-flighted behavior and
// stamps the fingerprint on success too, so the nightly job won't redo work a
// human just did.

// memoryHashSentinelID is the item_id used for the four non-session item
// kinds. Postgres treats NULLs as DISTINCT inside primary keys, which would
// break the ON CONFLICT upsert — so the schema stores this sentinel zero UUID
// instead of NULL (documented in the 20260710001 migration header).
const memoryHashSentinelID = "00000000-0000-0000-0000-000000000000"

// The allowed item kinds (mirrors the dr_chat_memory_hashes CHECK constraint).
const (
	memoryHashKindSession      = "session"
	memoryHashKindDescription  = "description"
	memoryHashKindInstructions = "instructions"
	memoryHashKindAssets       = "assets"
	memoryHashKindFeedback     = "feedback"
)

// drChatLabMemoryJobName keys the nightly sweep's row in dr_chatlab_job_state.
const drChatLabMemoryJobName = "chatlab_memory_nightly"

// ----------------------------------------------------------------------- //
// Pure hash builders (unit-tested)
// ----------------------------------------------------------------------- //

// hashText is the hex SHA-256 of a string — used directly for the description
// and instructions (each hashed separately) and as the primitive under the
// composite builders below.
func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashSessionAppend hashes a session's APPEND CURSOR — the last persisted
// message id plus the message count — not chat content. Any new message moves
// the cursor, and both values are already in hand when the assistant row is
// persisted, keeping the write path O(1).
func hashSessionAppend(sessionID, lastMessageID string, messageCount int) string {
	return hashText(fmt.Sprintf("session|%s|%s|%d", sessionID, lastMessageID, messageCount))
}

// hashAssetManifest hashes the READY asset manifest over sorted
// (assetID, fileName) pairs — upload order must not matter.
func hashAssetManifest(assets []storedProjectAsset) string {
	pairs := make([]string, 0, len(assets))
	for _, a := range assets {
		pairs = append(pairs, a.ID+"|"+a.FileName)
	}
	sort.Strings(pairs)
	return hashText("assets|" + strings.Join(pairs, "\n"))
}

// hashFeedbackState hashes the project-level feedback cursor: row count plus
// the newest updated_at (upserts bump updated_at, deletes drop the count).
func hashFeedbackState(count int, maxUpdatedAt time.Time) string {
	return hashText(fmt.Sprintf("feedback|%d|%d", count, maxUpdatedAt.UTC().UnixNano()))
}

// ----------------------------------------------------------------------- //
// Fingerprint (pure; unit-tested)
// ----------------------------------------------------------------------- //

// memoryHashRow is one dr_chat_memory_hashes row as seen by the fingerprint.
type memoryHashRow struct {
	Kind   string
	ItemID string
	Hash   string
}

// projectFingerprint combines a project's hash rows into one deterministic
// fingerprint: rows sorted by (item_kind, item_id), SHA-256 over the
// concatenated kind|id|hash triples. Order-independent; sensitive to any row's
// hash changing and to rows being added or removed. The fingerprint of zero
// rows is still a defined value, so deleting a project's last session changes
// (rather than erases) its dirty state.
func projectFingerprint(rows []memoryHashRow) string {
	sorted := make([]memoryHashRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}
		return sorted[i].ItemID < sorted[j].ItemID
	})
	var b strings.Builder
	for _, r := range sorted {
		fmt.Fprintf(&b, "%s|%s|%s\n", r.Kind, r.ItemID, r.Hash)
	}
	return hashText(b.String())
}

// ----------------------------------------------------------------------- //
// Dirty marking (the hot-path writes)
// ----------------------------------------------------------------------- //

// markMemoryHash upserts one hash row. Background context + log-and-continue:
// it must never block or fail the user-facing operation that dirtied the item
// (Hard Constraint 6). An empty itemID maps to the sentinel zero UUID.
func (h *DrChatLabHandler) markMemoryHash(projectID, itemKind, itemID, hash string) {
	if h.pool == nil || projectID == "" {
		return
	}
	if itemID == "" {
		itemID = memoryHashSentinelID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_chat_memory_hashes (project_id, item_kind, item_id, hash)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, item_kind, item_id)
DO UPDATE SET hash = EXCLUDED.hash, updated_at = now()`,
		projectID, itemKind, itemID, hash); err != nil {
		log.Printf("dr chatlab memory hashes: mark %s for project %s: %v", itemKind, projectID, err)
	}
}

// markFeedbackHash recomputes and upserts the project's feedback-state hash
// from one tiny aggregate query. Same background/log-and-continue contract as
// markMemoryHash.
func (h *DrChatLabHandler) markFeedbackHash(projectID string) {
	if h.pool == nil || projectID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	var maxUpdated *time.Time
	if err := h.pool.QueryRow(ctx, `
SELECT COUNT(*), MAX(updated_at) FROM dr_chat_message_feedback WHERE project_id = $1`, projectID).
		Scan(&count, &maxUpdated); err != nil {
		log.Printf("dr chatlab memory hashes: feedback state for project %s: %v", projectID, err)
		return
	}
	at := time.Time{}
	if maxUpdated != nil {
		at = *maxUpdated
	}
	h.markMemoryHash(projectID, memoryHashKindFeedback, "", hashFeedbackState(count, at))
}

// markAssetsHash recomputes and upserts the project's ready-asset manifest
// hash (id + file name only — no bodies). Same contract as markMemoryHash.
func (h *DrChatLabHandler) markAssetsHash(projectID string) {
	if h.pool == nil || projectID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := h.pool.Query(ctx, `
SELECT id, file_name FROM dr_chat_project_assets
WHERE project_id = $1 AND status = 'ready'`, projectID)
	if err != nil {
		log.Printf("dr chatlab memory hashes: asset manifest for project %s: %v", projectID, err)
		return
	}
	var assets []storedProjectAsset
	func() {
		defer rows.Close()
		for rows.Next() {
			var a storedProjectAsset
			if err := rows.Scan(&a.ID, &a.FileName); err != nil {
				continue
			}
			assets = append(assets, a)
		}
	}()
	h.markMemoryHash(projectID, memoryHashKindAssets, "", hashAssetManifest(assets))
}

// removeSessionMemoryHash drops a deleted session's hash row — the row-set
// change alters the project fingerprint, so the nightly job regenerates.
// (Whole-project deletes need nothing: the project FK cascades.)
func (h *DrChatLabHandler) removeSessionMemoryHash(sessionID string) {
	if h.pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(ctx, `
DELETE FROM dr_chat_memory_hashes WHERE item_kind = 'session' AND item_id = $1`, sessionID); err != nil {
		log.Printf("dr chatlab memory hashes: remove session %s: %v", sessionID, err)
	}
}

// ----------------------------------------------------------------------- //
// The nightly job
// ----------------------------------------------------------------------- //

// loadMemoryHashRows loads one project's hash rows (the fingerprint snapshot
// taken around a single regeneration).
func (h *DrChatLabHandler) loadMemoryHashRows(ctx context.Context, projectID string) ([]memoryHashRow, error) {
	rows, err := h.pool.Query(ctx, `
SELECT item_kind, item_id, hash FROM dr_chat_memory_hashes WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	var out []memoryHashRow
	func() {
		defer rows.Close()
		for rows.Next() {
			var r memoryHashRow
			if err := rows.Scan(&r.Kind, &r.ItemID, &r.Hash); err != nil {
				continue
			}
			out = append(out, r)
		}
	}()
	return out, rows.Err()
}

// loadMemoryHashRowsByProject loads ALL hash rows grouped by project id (the
// nightly job's one bulk read).
func (h *DrChatLabHandler) loadMemoryHashRowsByProject(ctx context.Context) (map[string][]memoryHashRow, error) {
	rows, err := h.pool.Query(ctx, `SELECT project_id, item_kind, item_id, hash FROM dr_chat_memory_hashes`)
	if err != nil {
		return nil, err
	}
	byProject := map[string][]memoryHashRow{}
	func() {
		defer rows.Close()
		for rows.Next() {
			var projectID string
			var r memoryHashRow
			if err := rows.Scan(&projectID, &r.Kind, &r.ItemID, &r.Hash); err != nil {
				continue
			}
			byProject[projectID] = append(byProject[projectID], r)
		}
	}()
	return byProject, rows.Err()
}

// runMemoryUpdateBlocking runs one stamped regeneration through the
// single-flight latch and WAITS for it, returning its error. Sharing the latch
// with the manual refresh means the nightly job can never race a
// button-triggered run on the same project. If a manual run is ALREADY in
// flight, our closure never executes — the manual run (plus its coalesced
// rerun) regenerates and stamps the fingerprint itself, which is exactly the
// outcome the sweep wanted, so that counts as success.
func (h *DrChatLabHandler) runMemoryUpdateBlocking(projectID, actorUID, actorEmail string) error {
	var runErr error
	ran := false
	h.memoryFlights.DoWait(projectID, func() {
		// May run twice when a manual trigger coalesces mid-run: the LAST
		// execution's error wins (its state is what's left behind).
		runErr = h.runMemoryUpdateStamped(projectID, actorUID, actorEmail)
		ran = true
	})
	if !ran {
		return nil
	}
	return runErr
}

// RunNightlyMemoryJob is the 4 AM sweep: regenerate memory for every project
// whose combined fingerprint differs from the one stored at its last
// successful generation, then record the sweep in dr_chatlab_job_state.
// Projects run SEQUENTIALLY — it's the middle of the night, latency is
// irrelevant, and one-at-a-time keeps OpenRouter usage gentle. Nightly runs
// are attributed to user 'system'/'system' on their kind='memory' usage
// events, so the stats "user" dimension shows a system row (expected; no UI
// special-casing).
func (h *DrChatLabHandler) RunNightlyMemoryJob(ctx context.Context) {
	if h.pool == nil {
		return
	}
	if h.cfg == nil || strings.TrimSpace(h.cfg.DRChatLabMemoryModel) == "" || h.or == nil {
		log.Printf("dr chatlab nightly memory: skipped — DR_CHATLAB_MEMORY_MODEL is not configured")
		return
	}

	type projectState struct {
		id         string
		sourceHash *string
	}
	var projects []projectState
	rows, err := h.pool.Query(ctx, `SELECT id, memory_source_hash FROM dr_chat_projects`)
	if err != nil {
		log.Printf("dr chatlab nightly memory: load projects: %v", err)
		return
	}
	func() {
		defer rows.Close()
		for rows.Next() {
			var p projectState
			if err := rows.Scan(&p.id, &p.sourceHash); err != nil {
				continue
			}
			projects = append(projects, p)
		}
	}()

	hashesByProject, err := h.loadMemoryHashRowsByProject(ctx)
	if err != nil {
		log.Printf("dr chatlab nightly memory: load hash rows: %v", err)
		return
	}

	regenerated, failed, skipped := 0, 0, 0
	for _, p := range projects {
		if ctx.Err() != nil {
			return // shutdown mid-sweep; no job-state update, the catch-up reruns
		}
		hashRows := hashesByProject[p.id]
		if len(hashRows) == 0 && p.sourceHash == nil {
			skipped++ // nothing has ever been marked and nothing generated — nothing to summarize
			continue
		}
		fingerprint := projectFingerprint(hashRows)
		if p.sourceHash != nil && *p.sourceHash == fingerprint {
			skipped++ // unchanged since the last successful generation — zero API calls
			continue
		}
		if err := h.runMemoryUpdateBlocking(p.id, "system", "system"); err != nil {
			// The old fingerprint stays in place → tomorrow night retries.
			log.Printf("dr chatlab nightly memory: project %s failed (retries next night): %v", p.id, err)
			failed++
			continue
		}
		regenerated++
	}

	if _, err := h.pool.Exec(ctx, `
INSERT INTO dr_chatlab_job_state (job_name, last_run_at) VALUES ($1, now())
ON CONFLICT (job_name) DO UPDATE SET last_run_at = now()`, drChatLabMemoryJobName); err != nil {
		log.Printf("dr chatlab nightly memory: record job state: %v", err)
	}
	log.Printf("dr chatlab nightly memory: sweep done — %d regenerated, %d failed, %d skipped", regenerated, failed, skipped)
}

// ----------------------------------------------------------------------- //
// Scheduling math (pure; unit-tested)
// ----------------------------------------------------------------------- //

// nextRunAfter returns the next occurrence of HH:00 in loc strictly after now.
// Building each occurrence fresh with time.Date in the LOCATION makes DST
// handling automatic: 4 AM Denver is 10:00 UTC in summer and 11:00 UTC in
// winter, and this code never needs to know — the wall clock stays 4 AM.
func nextRunAfter(now time.Time, hour int, loc *time.Location) time.Time {
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), hour, 0, 0, 0, loc)
	if !next.After(now) {
		// Today's HH:00 already passed — tomorrow's, by DATE arithmetic (+1
		// day, not +24h) so a DST transition between them can't skew the hour.
		next = time.Date(n.Year(), n.Month(), n.Day()+1, hour, 0, 0, 0, loc)
	}
	return next
}

// prevRunBefore returns the most recent occurrence of HH:00 in loc at or
// before now (the mirror of nextRunAfter, used by the catch-up decision).
func prevRunBefore(now time.Time, hour int, loc *time.Location) time.Time {
	n := now.In(loc)
	prev := time.Date(n.Year(), n.Month(), n.Day(), hour, 0, 0, 0, loc)
	if prev.After(now) {
		prev = time.Date(n.Year(), n.Month(), n.Day()-1, hour, 0, 0, 0, loc)
	}
	return prev
}

// NextMemoryJobRun is the exported face of nextRunAfter for the scheduler
// loop in cmd/api/main.go.
func NextMemoryJobRun(now time.Time, hour int, loc *time.Location) time.Time {
	return nextRunAfter(now, hour, loc)
}

// memoryJobCatchUpDue decides whether a boot-time catch-up run is needed: yes
// when the job has never recorded a run, or when its last run predates the
// most recent scheduled occurrence (server down overnight, or restarted at
// 3:59 AM — a night must never be silently skipped).
func memoryJobCatchUpDue(lastRun *time.Time, now time.Time, hour int, loc *time.Location) bool {
	if lastRun == nil {
		return true
	}
	return lastRun.Before(prevRunBefore(now, hour, loc))
}

// MaybeRunNightlyMemoryCatchUp reads dr_chatlab_job_state and runs the sweep
// when a scheduled occurrence was missed. Called once, ~1 minute after boot
// (mirroring the reaper warm-up), from the scheduler in cmd/api/main.go.
func (h *DrChatLabHandler) MaybeRunNightlyMemoryCatchUp(ctx context.Context, hour int, loc *time.Location) {
	if h.pool == nil {
		return
	}
	var lastRun *time.Time
	err := h.pool.QueryRow(ctx, `SELECT last_run_at FROM dr_chatlab_job_state WHERE job_name = $1`, drChatLabMemoryJobName).Scan(&lastRun)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("dr chatlab nightly memory: read job state: %v", err)
		// Fall through with lastRun=nil — running a redundant sweep is safe
		// (unchanged projects all skip), silently skipping a night is not.
	}
	if !memoryJobCatchUpDue(lastRun, time.Now(), hour, loc) {
		return
	}
	log.Printf("dr chatlab nightly memory: catch-up run (missed or never-recorded occurrence)")
	h.RunNightlyMemoryJob(ctx)
}
