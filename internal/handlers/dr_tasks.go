package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// DrTasksHandler serves the Double Raven Tasks board (Jira-style kanban at
// /dr/tasks) on the /api/dr group, so every endpoint inherits
// RequireDoubleRavenAuth. Authorship and activity actors always come from the
// verified claims in the gin context (DRContextKey), never from request bodies.
// The assignee directory IS the existing DR_ALLOWED_EMAILS allowlist
// (cfg.DRAllowedEmails, already lowercased) — there is no users table. Task
// descriptions reuse the restricted dr-blocks/v1 message subset validated by
// validateDrMessageJSON (dr_feedback.go) — one canonical rich-text format.
//
// Ordering model: each card has a fractional NUMERIC(20,10) position that is
// meaningful only WITHIN its status column. Creates and restores go to the TOP
// of the column (min − 1024, or 1024 in an empty column); moves land between
// the client-named neighbors at their midpoint. When repeated midpoints
// collapse a gap below minTaskPositionGap the move rebalances the whole
// destination column (1024, 2048, 3072, …) inside the same transaction and
// reports Rebalanced=true so the client refetches. No S3 dependency at all.
type DrTasksHandler struct {
	pool *pgxpool.Pool
	cfg  *config.Config
}

// NewDrTasksHandler wires the handler (nil pool → every endpoint answers 503,
// see dbReady). Mirrors NewDrFeedbackHandler minus S3/broadcaster — Tasks needs
// neither.
func NewDrTasksHandler(pool *pgxpool.Pool, cfg *config.Config) *DrTasksHandler {
	return &DrTasksHandler{pool: pool, cfg: cfg}
}

// RegisterDrTasksRoutes wires the task endpoints onto the already-prefixed and
// already-authed /dr group (see setupRouter), so they resolve to
// /api/dr/tasks/…. The /tasks/… prefix never collides with the /docs/:slug or
// /feedback/… wildcards the other DR handlers pin.
func RegisterDrTasksRoutes(r gin.IRouter, h *DrTasksHandler) {
	r.GET("/tasks", h.ListTasks)
	r.POST("/tasks", h.CreateTask)
	r.GET("/tasks/:taskId", h.GetTask)
	r.PATCH("/tasks/:taskId", h.UpdateTask)
	r.POST("/tasks/:taskId/move", h.MoveTask)
	r.POST("/tasks/:taskId/archive", h.ArchiveTask)
	r.POST("/tasks/:taskId/restore", h.RestoreTask)
}

// ----------------------------------------------------------------------- //
// Constants + pure helpers (unit-tested in dr_tasks_test.go)
// ----------------------------------------------------------------------- //

const (
	drTaskMaxTitleChars = 250
	drTaskMaxLabels     = 10
	drTaskMaxLabelChars = 30
	// defaultTaskPositionGap is the spacing unit for column positions: the seed
	// position in an empty column, the offset for top/bottom drops, and the
	// stride used when a column is rebalanced.
	defaultTaskPositionGap = 1024
)

// drTaskStatuses / drTaskTypes / drTaskPriorities are the exact enum sets the
// dr_tasks CHECK constraints enforce; handlers validate against these BEFORE
// touching the database so violations are clean 400s, never pg errors.
var drTaskStatuses = map[string]bool{
	"backlog":     true,
	"todo":        true,
	"in_progress": true,
	"in_review":   true,
	"done":        true,
}

var drTaskTypes = map[string]bool{
	"task":        true,
	"bug":         true,
	"feature":     true,
	"improvement": true,
}

var drTaskPriorities = map[string]bool{
	"lowest":  true,
	"low":     true,
	"medium":  true,
	"high":    true,
	"highest": true,
}

// minTaskPositionGap (1e-6) is the smallest midpoint-to-neighbor gap a move may
// produce before the column is rebalanced. NUMERIC(20,10) storage rounds
// rendered positions to 10 decimal places (≤ 5e-11 error), so any gap ≥ 1e-6
// survives the round trip with ordering and distinctness intact.
var minTaskPositionGap = new(big.Rat).SetFrac64(1, 1_000_000)

// taskPositionBetween returns the position for a card dropped between
// before (above) and after (below). Semantics:
//
//	nil, nil     → top of an EMPTY column handled by caller; this returns defaultGap (1024)
//	nil, after   → after.position − 1024   (drop at top)
//	before, nil  → before.position + 1024  (drop at bottom)
//	before, after→ midpoint (before+after)/2
//
// Returns needsRebalance=true when the midpoint gap to either neighbor is
// < minTaskPositionGap (1e-6) — the caller then rebalances the whole column
// in the same transaction (positions 1024, 2048, 3072, …) and recomputes.
// (Inverted neighbors — before ≥ after, a stale-board artifact — also surface
// as needsRebalance, so the column self-heals instead of interleaving.)
func taskPositionBetween(before, after *big.Rat) (pos *big.Rat, needsRebalance bool) {
	gap := new(big.Rat).SetInt64(defaultTaskPositionGap)
	switch {
	case before == nil && after == nil:
		return gap, false
	case before == nil:
		return new(big.Rat).Sub(after, gap), false
	case after == nil:
		return new(big.Rat).Add(before, gap), false
	}
	mid := new(big.Rat).Add(before, after)
	mid.Quo(mid, new(big.Rat).SetInt64(2))
	if new(big.Rat).Sub(mid, before).Cmp(minTaskPositionGap) < 0 ||
		new(big.Rat).Sub(after, mid).Cmp(minTaskPositionGap) < 0 {
		return mid, true
	}
	return mid, false
}

// parseTaskPosition parses a NUMERIC's text rendering into an exact big.Rat.
func parseTaskPosition(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return nil, fmt.Errorf("invalid position %q", s)
	}
	return r, nil
}

// renderTaskPosition renders a position for NUMERIC(20,10) storage: fixed
// 10 decimal places (see minTaskPositionGap for why rounding here is safe).
func renderTaskPosition(r *big.Rat) string {
	return r.FloatString(10)
}

// normalizeTaskLabel normalizes one label to its canonical slug: trim,
// lowercase, spaces/underscores → hyphens, then only [a-z0-9-] may remain
// (anything else — unicode, punctuation — rejects the label), repeated hyphens
// collapse, leading/trailing hyphens are trimmed, and the result must be 1–30
// chars. ok=false means the label is unusable as given (the caller 400s with
// the original input).
func normalizeTaskLabel(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.NewReplacer(" ", "-", "_", "-").Replace(s)
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", false
		}
	}
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) < 1 || len(s) > drTaskMaxLabelChars {
		return "", false
	}
	return s, true
}

// normalizeTaskLabels validates + normalizes a label set: max 10, each through
// normalizeTaskLabel, deduped preserving first occurrence. The error message is
// client-facing.
func normalizeTaskLabels(raw []string) ([]string, error) {
	if len(raw) > drTaskMaxLabels {
		return nil, fmt.Errorf("At most %d labels are allowed", drTaskMaxLabels)
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		label, ok := normalizeTaskLabel(r)
		if !ok {
			return nil, fmt.Errorf("Invalid label %q", r)
		}
		if !slices.Contains(out, label) {
			out = append(out, label)
		}
	}
	return out, nil
}

// parseTaskDueDate parses the wire due-date format (YYYY-MM-DD).
func parseTaskDueDate(raw string) (time.Time, error) {
	return time.Parse("2006-01-02", raw)
}

// isJSONNull reports whether a raw JSON value is the literal null (the
// PATCH-description "clear" sentinel).
func isJSONNull(raw []byte) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// taskDescriptionsEqual reports whether two raw description payloads are the
// same JSON document. Key-order/whitespace insensitive because stored jsonb
// re-renders normalized — a plain byte compare would flag spurious changes.
func taskDescriptionsEqual(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(av, bv)
}

// renderTaskDescriptionValue is the activity-feed rendering of a description:
// '' when absent, an '(updated)' marker when present — block JSON is never
// dumped into activity rows.
func renderTaskDescriptionValue(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return "(updated)"
}

// taskFields is the diffable field set of a task — the inputs to
// diffTaskUpdate. Description is the raw block JSON (nil = none); Assignee ""
// means unassigned; DueDate is "" or YYYY-MM-DD. Status is absent by design:
// status changes go through MoveTask, which writes its own 'moved' row.
type taskFields struct {
	Title       string
	Description []byte
	Type        string
	Priority    string
	Assignee    string
	Labels      []string
	DueDate     string
}

// taskActivityRow is one pending dr_task_activity insert. Field == "" marks a
// field-less event row (created/archived/restored) — field/old/new store NULL.
type taskActivityRow struct {
	Action   string
	Field    string
	OldValue string
	NewValue string
}

// diffTaskUpdate produces one activity row per changed field (no-op sets are
// suppressed). Assignee changes use action 'assigned'; everything else
// 'updated'. Labels render comma-joined; descriptions render as markers via
// renderTaskDescriptionValue; empty values render as ''.
func diffTaskUpdate(old, new taskFields) []taskActivityRow {
	var rows []taskActivityRow
	if old.Title != new.Title {
		rows = append(rows, taskActivityRow{Action: "updated", Field: "title", OldValue: old.Title, NewValue: new.Title})
	}
	if !taskDescriptionsEqual(old.Description, new.Description) {
		rows = append(rows, taskActivityRow{
			Action: "updated", Field: "description",
			OldValue: renderTaskDescriptionValue(old.Description),
			NewValue: renderTaskDescriptionValue(new.Description),
		})
	}
	if old.Type != new.Type {
		rows = append(rows, taskActivityRow{Action: "updated", Field: "type", OldValue: old.Type, NewValue: new.Type})
	}
	if old.Priority != new.Priority {
		rows = append(rows, taskActivityRow{Action: "updated", Field: "priority", OldValue: old.Priority, NewValue: new.Priority})
	}
	if old.Assignee != new.Assignee {
		rows = append(rows, taskActivityRow{Action: "assigned", Field: "assignee", OldValue: old.Assignee, NewValue: new.Assignee})
	}
	if !slices.Equal(old.Labels, new.Labels) {
		rows = append(rows, taskActivityRow{
			Action: "updated", Field: "labels",
			OldValue: strings.Join(old.Labels, ", "),
			NewValue: strings.Join(new.Labels, ", "),
		})
	}
	if old.DueDate != new.DueDate {
		rows = append(rows, taskActivityRow{Action: "updated", Field: "dueDate", OldValue: old.DueDate, NewValue: new.DueDate})
	}
	return rows
}

// ----------------------------------------------------------------------- //
// Shared handler plumbing
// ----------------------------------------------------------------------- //

func (h *DrTasksHandler) dbReady(c *gin.Context) bool {
	if h.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Task storage is unavailable"})
		return false
	}
	return true
}

func drTaskID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("taskId"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid task id"})
		return "", false
	}
	return id, true
}

// isAssigneeAllowed reports whether a (lowercased, trimmed) email is on the DR
// allowlist — the assignee directory (the config already lowercases entries).
func (h *DrTasksHandler) isAssigneeAllowed(email string) bool {
	return slices.Contains(h.cfg.DRAllowedEmails, email)
}

// drTaskSelectColumns is every dr_tasks column the DTO needs; position is
// rendered to text IN the query so the exact numeric rendering survives the
// round trip into DrTask.Position.
const drTaskSelectColumns = `id, task_number, title, description, status, type, priority,
       assignee_email, created_by_uid, created_by_email, labels, due_date,
       position::text, archived_at, created_at, updated_at`

// scannedTask is the raw column set for one dr_tasks row.
type scannedTask struct {
	id             string
	taskNumber     int64
	title          string
	description    []byte
	status         string
	typ            string
	priority       string
	assigneeEmail  *string
	createdByUID   string
	createdByEmail string
	labels         []string
	dueDate        *time.Time
	position       string
	archivedAt     *time.Time
	createdAt      time.Time
	updatedAt      time.Time
}

func scanTaskRow(row pgx.Row) (scannedTask, error) {
	var s scannedTask
	err := row.Scan(&s.id, &s.taskNumber, &s.title, &s.description, &s.status, &s.typ, &s.priority,
		&s.assigneeEmail, &s.createdByUID, &s.createdByEmail, &s.labels, &s.dueDate,
		&s.position, &s.archivedAt, &s.createdAt, &s.updatedAt)
	return s, err
}

// toDTO builds the response DTO: display key computed as "DR-<task_number>",
// labels never null, dueDate re-rendered YYYY-MM-DD.
func (s scannedTask) toDTO() models.DrTask {
	labels := s.labels
	if labels == nil {
		labels = []string{}
	}
	t := models.DrTask{
		ID:             s.id,
		Key:            fmt.Sprintf("DR-%d", s.taskNumber),
		TaskNumber:     s.taskNumber,
		Title:          s.title,
		Status:         s.status,
		Type:           s.typ,
		Priority:       s.priority,
		AssigneeEmail:  s.assigneeEmail,
		CreatedByEmail: s.createdByEmail,
		Labels:         labels,
		Position:       s.position,
		CreatedAt:      models.UTCTime{Time: s.createdAt},
		UpdatedAt:      models.UTCTime{Time: s.updatedAt},
	}
	if len(s.description) > 0 {
		t.Description = json.RawMessage(s.description)
	}
	if s.dueDate != nil {
		d := s.dueDate.Format("2006-01-02")
		t.DueDate = &d
	}
	if s.archivedAt != nil {
		u := models.UTCTime{Time: *s.archivedAt}
		t.ArchivedAt = &u
	}
	return t
}

// fields converts a scanned row into the diffable field set for diffTaskUpdate.
func (s scannedTask) fields() taskFields {
	f := taskFields{
		Title:       s.title,
		Description: s.description,
		Type:        s.typ,
		Priority:    s.priority,
		Labels:      s.labels,
	}
	if s.assigneeEmail != nil {
		f.Assignee = *s.assigneeEmail
	}
	if s.dueDate != nil {
		f.DueDate = s.dueDate.Format("2006-01-02")
	}
	return f
}

// insertTaskActivity appends the given rows to dr_task_activity inside the
// caller's transaction. Rows with Field == "" (created/archived/restored) store
// NULL for field/old/new; per-field rows store their values verbatim (''
// included — '' is the documented empty rendering).
func insertTaskActivity(ctx context.Context, tx pgx.Tx, taskID, actorUID, actorEmail string, rows []taskActivityRow) error {
	for _, r := range rows {
		var field, oldV, newV *string
		if r.Field != "" {
			f, o, n := r.Field, r.OldValue, r.NewValue
			field, oldV, newV = &f, &o, &n
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO dr_task_activity (task_id, actor_uid, actor_email, action, field, old_value, new_value)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			taskID, actorUID, actorEmail, r.Action, field, oldV, newV); err != nil {
			return err
		}
	}
	return nil
}

// lockTaskForUpdate loads + row-locks one task inside tx. Handles the shared
// 404 (unknown id) and, when requireActive is set, the shared 409 (archived
// task) — ok=false means a response was already written.
func lockTaskForUpdate(c *gin.Context, ctx context.Context, tx pgx.Tx, taskID string, requireActive bool) (scannedTask, bool) {
	s, err := scanTaskRow(tx.QueryRow(ctx, `SELECT `+drTaskSelectColumns+` FROM dr_tasks WHERE id = $1 FOR UPDATE`, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return scannedTask{}, false
	}
	if err != nil {
		log.Printf("dr tasks: load task %s: %v", taskID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Request failed"})
		return scannedTask{}, false
	}
	if requireActive && s.archivedAt != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Task is archived"})
		return scannedTask{}, false
	}
	return s, true
}

// ----------------------------------------------------------------------- //
// GET /tasks  (the board, one round trip)
// ----------------------------------------------------------------------- //

// ListTasks returns every ACTIVE task ordered (status, position, created_at) —
// the whole board in one payload; the client does all facet filtering
// (two-user scale, no server-side filters by design). ?includeArchived=1
// appends archived tasks after the active set.
func (h *DrTasksHandler) ListTasks(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	where := "archived_at IS NULL"
	order := "status, position, created_at"
	if c.Query("includeArchived") == "1" {
		// Active board first, archived appended (newest archive first is handled
		// by the same position ordering; archived cards keep their last spot).
		where = "TRUE"
		order = "(archived_at IS NOT NULL), status, position, created_at"
	}
	rows, err := h.pool.Query(ctx, fmt.Sprintf(
		`SELECT %s FROM dr_tasks WHERE %s ORDER BY %s`, drTaskSelectColumns, where, order))
	if err != nil {
		log.Printf("dr tasks: list: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load tasks"})
		return
	}
	defer rows.Close()

	tasks := make([]models.DrTask, 0)
	for rows.Next() {
		s, err := scanTaskRow(rows)
		if err != nil {
			log.Printf("dr tasks: scan task: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load tasks"})
			return
		}
		tasks = append(tasks, s.toDTO())
	}
	if err := rows.Err(); err != nil {
		log.Printf("dr tasks: list rows: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load tasks"})
		return
	}
	c.JSON(http.StatusOK, models.DrTasksResponse{Tasks: tasks})
}

// ----------------------------------------------------------------------- //
// POST /tasks  (create — new card lands at the TOP of its column)
// ----------------------------------------------------------------------- //

func (h *DrTasksHandler) CreateTask(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	var req models.DrCreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	title := strings.TrimSpace(req.Title)
	if n := utf8.RuneCountInString(title); n < 1 || n > drTaskMaxTitleChars {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Title must be 1–250 characters"})
		return
	}
	status := req.Status
	if status == "" {
		status = "backlog"
	}
	if !drTaskStatuses[status] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid status"})
		return
	}
	taskType := req.Type
	if taskType == "" {
		taskType = "task"
	}
	if !drTaskTypes[taskType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid type"})
		return
	}
	priority := req.Priority
	if priority == "" {
		priority = "medium"
	}
	if !drTaskPriorities[priority] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid priority"})
		return
	}
	var assignee *string
	if a := strings.ToLower(strings.TrimSpace(req.AssigneeEmail)); a != "" {
		if !h.isAssigneeAllowed(a) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Assignee is not a portal user"})
			return
		}
		assignee = &a
	}
	labels, err := normalizeTaskLabels(req.Labels)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var description json.RawMessage
	if len(req.Description) > 0 && !isJSONNull(req.Description) {
		if err := validateDrMessageJSON(req.Description); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		description = req.Description
	}
	var dueDate *time.Time
	if d := strings.TrimSpace(req.DueDate); d != "" {
		t, err := parseTaskDueDate(d)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Due date must be in YYYY-MM-DD format"})
			return
		}
		dueDate = &t
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create task"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Top-of-column placement in one statement: min(position) − 1024, seeded so
	// an empty column yields exactly 1024 (COALESCE(…, 2048) − 1024).
	actorEmail := strings.ToLower(strings.TrimSpace(claims.Email))
	s, err := scanTaskRow(tx.QueryRow(ctx, `
INSERT INTO dr_tasks (title, description, status, type, priority, assignee_email,
                      created_by_uid, created_by_email, labels, due_date, position)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
        COALESCE((SELECT min(t.position) FROM dr_tasks t
                  WHERE t.status = $3 AND t.archived_at IS NULL), 2048) - 1024)
RETURNING `+drTaskSelectColumns,
		title, description, status, taskType, priority, assignee,
		claims.UID, actorEmail, labels, dueDate))
	if err != nil {
		log.Printf("dr tasks: insert task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create task"})
		return
	}
	if err := insertTaskActivity(ctx, tx, s.id, claims.UID, actorEmail,
		[]taskActivityRow{{Action: "created"}}); err != nil {
		log.Printf("dr tasks: insert created activity: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create task"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr tasks: commit create: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create task"})
		return
	}
	c.JSON(http.StatusCreated, s.toDTO())
}

// ----------------------------------------------------------------------- //
// GET /tasks/:taskId  (detail + full activity feed; archived retrievable)
// ----------------------------------------------------------------------- //

func (h *DrTasksHandler) GetTask(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	if _, ok := drCallerClaims(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	taskID, ok := drTaskID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	s, err := scanTaskRow(h.pool.QueryRow(ctx, `SELECT `+drTaskSelectColumns+` FROM dr_tasks WHERE id = $1`, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}
	if err != nil {
		log.Printf("dr tasks: get task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load task"})
		return
	}

	rows, err := h.pool.Query(ctx, `
SELECT id, actor_email, action, field, old_value, new_value, created_at
FROM dr_task_activity WHERE task_id = $1 ORDER BY created_at, seq`, taskID)
	if err != nil {
		log.Printf("dr tasks: load activity: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load task"})
		return
	}
	defer rows.Close()

	activity := make([]models.DrTaskActivityEntry, 0)
	for rows.Next() {
		var e models.DrTaskActivityEntry
		var createdAt time.Time
		if err := rows.Scan(&e.ID, &e.ActorEmail, &e.Action, &e.Field, &e.OldValue, &e.NewValue, &createdAt); err != nil {
			log.Printf("dr tasks: scan activity: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load task"})
			return
		}
		e.CreatedAt = models.UTCTime{Time: createdAt}
		activity = append(activity, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("dr tasks: activity rows: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load task"})
		return
	}
	c.JSON(http.StatusOK, models.DrTaskDetailResponse{Task: s.toDTO(), Activity: activity})
}

// ----------------------------------------------------------------------- //
// PATCH /tasks/:taskId  (partial update; one activity row per changed field)
// ----------------------------------------------------------------------- //

func (h *DrTasksHandler) UpdateTask(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	taskID, ok := drTaskID(c)
	if !ok {
		return
	}
	var req models.DrUpdateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update task"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	s, ok := lockTaskForUpdate(c, ctx, tx, taskID, true)
	if !ok {
		return
	}

	old := s.fields()
	next := old
	newDueDate := s.dueDate

	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if n := utf8.RuneCountInString(title); n < 1 || n > drTaskMaxTitleChars {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Title must be 1–250 characters"})
			return
		}
		next.Title = title
	}
	// Description tri-state: absent = unchanged, JSON null = clear, value =
	// replace (validated as the restricted dr-blocks/v1 subset).
	if len(req.Description) > 0 {
		if isJSONNull(req.Description) {
			next.Description = nil
		} else {
			if err := validateDrMessageJSON(req.Description); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			next.Description = req.Description
		}
	}
	if req.Type != nil {
		if !drTaskTypes[*req.Type] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid type"})
			return
		}
		next.Type = *req.Type
	}
	if req.Priority != nil {
		if !drTaskPriorities[*req.Priority] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid priority"})
			return
		}
		next.Priority = *req.Priority
	}
	if req.AssigneeEmail != nil {
		a := strings.ToLower(strings.TrimSpace(*req.AssigneeEmail))
		if a != "" && !h.isAssigneeAllowed(a) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Assignee is not a portal user"})
			return
		}
		next.Assignee = a
	}
	if req.Labels != nil {
		labels, err := normalizeTaskLabels(*req.Labels)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		next.Labels = labels
	}
	if req.DueDate != nil {
		if d := strings.TrimSpace(*req.DueDate); d == "" {
			next.DueDate = ""
			newDueDate = nil
		} else {
			t, err := parseTaskDueDate(d)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Due date must be in YYYY-MM-DD format"})
				return
			}
			next.DueDate = d
			newDueDate = &t
		}
	}

	activityRows := diffTaskUpdate(old, next)
	if len(activityRows) == 0 {
		// Nothing actually changed: no write, no updated_at bump, no activity.
		c.JSON(http.StatusOK, s.toDTO())
		return
	}

	var assignee *string
	if next.Assignee != "" {
		assignee = &next.Assignee
	}
	var description json.RawMessage
	if len(next.Description) > 0 {
		description = next.Description
	}
	updated, err := scanTaskRow(tx.QueryRow(ctx, `
UPDATE dr_tasks
SET title = $1, description = $2, type = $3, priority = $4, assignee_email = $5,
    labels = $6, due_date = $7, updated_at = now()
WHERE id = $8
RETURNING `+drTaskSelectColumns,
		next.Title, description, next.Type, next.Priority, assignee,
		next.Labels, newDueDate, taskID))
	if err != nil {
		log.Printf("dr tasks: update task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update task"})
		return
	}
	actorEmail := strings.ToLower(strings.TrimSpace(claims.Email))
	if err := insertTaskActivity(ctx, tx, taskID, claims.UID, actorEmail, activityRows); err != nil {
		log.Printf("dr tasks: insert update activity: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update task"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr tasks: commit update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update task"})
		return
	}
	c.JSON(http.StatusOK, updated.toDTO())
}

// ----------------------------------------------------------------------- //
// POST /tasks/:taskId/move  (status + position in ONE transaction)
// ----------------------------------------------------------------------- //

func (h *DrTasksHandler) MoveTask(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	taskID, ok := drTaskID(c)
	if !ok {
		return
	}
	var req models.DrMoveTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if !drTaskStatuses[req.Status] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid status"})
		return
	}
	// Neighbor ids must be well-formed UUIDs (malformed = client bug → 400);
	// whether they are still valid neighbors is checked under the row locks
	// below (stale = concurrent drag → 409).
	neighborID := func(p *string, name string) (string, bool) {
		if p == nil || strings.TrimSpace(*p) == "" {
			return "", true
		}
		id := strings.TrimSpace(*p)
		if _, err := uuid.Parse(id); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid " + name})
			return "", false
		}
		return id, true
	}
	beforeID, ok := neighborID(req.BeforeTaskID, "beforeTaskId")
	if !ok {
		return
	}
	afterID, ok := neighborID(req.AfterTaskID, "afterTaskId")
	if !ok {
		return
	}
	// A neighbor that IS the moved card (or a duplicated neighbor) can only
	// come from a board that shifted mid-drag — same conflict signal.
	if beforeID == taskID || afterID == taskID || (beforeID != "" && beforeID == afterID) {
		c.JSON(http.StatusConflict, gin.H{"error": "Board changed — please retry"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	s, ok := lockTaskForUpdate(c, ctx, tx, taskID, true)
	if !ok {
		return
	}
	oldStatus := s.status

	// Resolve + lock a neighbor's position; it must be an ACTIVE task in the
	// DESTINATION column or the board changed under the client (→ 409).
	lockNeighbor := func(id string) (*big.Rat, bool) {
		var posText string
		err := tx.QueryRow(ctx, `
SELECT position::text FROM dr_tasks
WHERE id = $1 AND status = $2 AND archived_at IS NULL FOR UPDATE`, id, req.Status).Scan(&posText)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "Board changed — please retry"})
			return nil, false
		}
		if err != nil {
			log.Printf("dr tasks: lock neighbor %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
			return nil, false
		}
		pos, err := parseTaskPosition(posText)
		if err != nil {
			log.Printf("dr tasks: neighbor position: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
			return nil, false
		}
		return pos, true
	}

	var before, after *big.Rat
	if beforeID != "" {
		if before, ok = lockNeighbor(beforeID); !ok {
			return
		}
	}
	if afterID != "" {
		if after, ok = lockNeighbor(afterID); !ok {
			return
		}
	}
	// No neighbors named = drop at the TOP of the destination column: the
	// current top card (excluding the moved card itself) becomes `after`.
	if beforeID == "" && afterID == "" {
		var posText string
		err := tx.QueryRow(ctx, `
SELECT position::text FROM dr_tasks
WHERE status = $1 AND archived_at IS NULL AND id <> $2
ORDER BY position, created_at LIMIT 1 FOR UPDATE`, req.Status, taskID).Scan(&posText)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("dr tasks: top of column: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
			return
		}
		if err == nil {
			if after, err = parseTaskPosition(posText); err != nil {
				log.Printf("dr tasks: top position: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
				return
			}
		}
	}

	pos, rebalanced := taskPositionBetween(before, after)
	if rebalanced {
		// Gaps collapsed: rewrite the WHOLE destination column to a 1024 stride
		// (in current order, the moved card included if it already lives there),
		// then re-place the moved card between its neighbors' fresh positions.
		rows, err := tx.Query(ctx, `
SELECT id FROM dr_tasks
WHERE status = $1 AND archived_at IS NULL
ORDER BY position, created_at FOR UPDATE`, req.Status)
		if err != nil {
			log.Printf("dr tasks: rebalance select: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
			return
		}
		var ids []string
		func() {
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err == nil {
					ids = append(ids, id)
				}
			}
		}()
		newPositions := make(map[string]*big.Rat, len(ids))
		for i, id := range ids {
			p := new(big.Rat).SetInt64(int64(defaultTaskPositionGap * (i + 1)))
			newPositions[id] = p
			if _, err := tx.Exec(ctx, `UPDATE dr_tasks SET position = $1::numeric WHERE id = $2`,
				renderTaskPosition(p), id); err != nil {
				log.Printf("dr tasks: rebalance update: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
				return
			}
		}
		// Rebalance only triggers on the midpoint path, so both neighbors exist.
		pos, _ = taskPositionBetween(newPositions[beforeID], newPositions[afterID])
	}

	moved, err := scanTaskRow(tx.QueryRow(ctx, `
UPDATE dr_tasks SET status = $1, position = $2::numeric, updated_at = now()
WHERE id = $3
RETURNING `+drTaskSelectColumns, req.Status, renderTaskPosition(pos), taskID))
	if err != nil {
		log.Printf("dr tasks: move update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
		return
	}
	// A pure in-column reorder writes NO activity; only a real column change
	// records a 'moved' row.
	if oldStatus != req.Status {
		actorEmail := strings.ToLower(strings.TrimSpace(claims.Email))
		if err := insertTaskActivity(ctx, tx, taskID, claims.UID, actorEmail, []taskActivityRow{
			{Action: "moved", Field: "status", OldValue: oldStatus, NewValue: req.Status},
		}); err != nil {
			log.Printf("dr tasks: insert moved activity: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr tasks: commit move: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move task"})
		return
	}
	c.JSON(http.StatusOK, models.DrMoveTaskResponse{Task: moved.toDTO(), Rebalanced: rebalanced})
}

// ----------------------------------------------------------------------- //
// POST /tasks/:taskId/archive  /  POST /tasks/:taskId/restore  (soft delete)
// ----------------------------------------------------------------------- //

func (h *DrTasksHandler) ArchiveTask(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	taskID, ok := drTaskID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to archive task"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	s, ok := lockTaskForUpdate(c, ctx, tx, taskID, false)
	if !ok {
		return
	}
	if s.archivedAt != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Task is already archived"})
		return
	}
	archived, err := scanTaskRow(tx.QueryRow(ctx, `
UPDATE dr_tasks SET archived_at = now(), updated_at = now()
WHERE id = $1
RETURNING `+drTaskSelectColumns, taskID))
	if err != nil {
		log.Printf("dr tasks: archive: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to archive task"})
		return
	}
	actorEmail := strings.ToLower(strings.TrimSpace(claims.Email))
	if err := insertTaskActivity(ctx, tx, taskID, claims.UID, actorEmail,
		[]taskActivityRow{{Action: "archived"}}); err != nil {
		log.Printf("dr tasks: insert archived activity: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to archive task"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr tasks: commit archive: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to archive task"})
		return
	}
	c.JSON(http.StatusOK, archived.toDTO())
}

func (h *DrTasksHandler) RestoreTask(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	taskID, ok := drTaskID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to restore task"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	s, ok := lockTaskForUpdate(c, ctx, tx, taskID, false)
	if !ok {
		return
	}
	if s.archivedAt == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Task is not archived"})
		return
	}
	// Restored cards go to the TOP of their status column (same seed math as
	// create; the row itself is excluded from min() because it is archived).
	restored, err := scanTaskRow(tx.QueryRow(ctx, `
UPDATE dr_tasks SET archived_at = NULL, updated_at = now(),
    position = COALESCE((SELECT min(t.position) FROM dr_tasks t
                         WHERE t.status = dr_tasks.status AND t.archived_at IS NULL), 2048) - 1024
WHERE id = $1
RETURNING `+drTaskSelectColumns, taskID))
	if err != nil {
		log.Printf("dr tasks: restore: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to restore task"})
		return
	}
	actorEmail := strings.ToLower(strings.TrimSpace(claims.Email))
	if err := insertTaskActivity(ctx, tx, taskID, claims.UID, actorEmail,
		[]taskActivityRow{{Action: "restored"}}); err != nil {
		log.Printf("dr tasks: insert restored activity: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to restore task"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("dr tasks: commit restore: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to restore task"})
		return
	}
	c.JSON(http.StatusOK, restored.toDTO())
}
