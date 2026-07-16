package models

import "encoding/json"

// Double Raven Tasks (Jira-style kanban board at /dr/tasks) DTOs. Request /
// response contracts for the endpoints in internal/handlers/dr_tasks.go.
// Authorship (created_by_uid/created_by_email) and activity actors are always
// taken from the verified Firebase claims in the gin context — never from these
// request bodies. Response JSON keys are camelCase, mirroring
// double-raven-portal/schemas/drTasks.ts. Task descriptions are the SAME
// restricted dr-blocks/v1 message subset as feedback messages (paragraph, code,
// list, blockquote — validated by validateDrMessageJSON), passed through as
// opaque json.RawMessage.

// ---- Request bodies --------------------------------------------------------

// DrCreateTaskRequest creates a task. Only title is required; status/type/
// priority default to backlog/task/medium. The new card is placed at the TOP of
// its status column by the handler. Description, when present, must be
// dr-blocks/v1; assigneeEmail, when non-empty, must be a portal user; dueDate
// is YYYY-MM-DD.
type DrCreateTaskRequest struct {
	Title         string          `json:"title"`
	Description   json.RawMessage `json:"description"`
	Status        string          `json:"status"`
	Type          string          `json:"type"`
	Priority      string          `json:"priority"`
	AssigneeEmail string          `json:"assigneeEmail"`
	Labels        []string        `json:"labels"`
	DueDate       string          `json:"dueDate"`
}

// DrUpdateTaskRequest is a partial (PATCH) update: every field is optional so
// the handler can distinguish "absent" (leave unchanged) from "set to empty".
// Description is tri-state: absent (nil RawMessage) = unchanged; explicit JSON
// null = clear the description; any other value = replace (validated as
// dr-blocks/v1). AssigneeEmail "" clears the assignment; DueDate "" clears the
// due date; Labels replace the whole set. Status is NOT patchable here — column
// changes go through the move endpoint so ordering stays consistent.
type DrUpdateTaskRequest struct {
	Title         *string         `json:"title"`
	Description   json.RawMessage `json:"description"`
	Type          *string         `json:"type"`
	Priority      *string         `json:"priority"`
	AssigneeEmail *string         `json:"assigneeEmail"`
	Labels        *[]string       `json:"labels"`
	DueDate       *string         `json:"dueDate"`
}

// DrMoveTaskRequest moves a task to a status column at a specific spot.
// BeforeTaskID/AfterTaskID identify the desired neighbors IN THE DESTINATION
// column (before = the card that will sit ABOVE the moved card, after = the
// card BELOW). Both nil = place at the TOP of the destination column. Neighbors
// must be active tasks in the destination column, otherwise the board changed
// under the client and the handler answers 409.
type DrMoveTaskRequest struct {
	Status       string  `json:"status"`
	BeforeTaskID *string `json:"beforeTaskId"`
	AfterTaskID  *string `json:"afterTaskId"`
}

// ---- Response DTOs ---------------------------------------------------------

// DrTask is one task card. Key is the display key "DR-<taskNumber>" (computed
// in the handler, never stored). Description is omitted when the task has none.
// Labels is never null (empty = []). Position is the exact numeric rendering of
// the fractional in-column ordering value — clients treat it as an opaque
// sortable decimal string.
type DrTask struct {
	ID             string          `json:"id"`
	Key            string          `json:"key"`
	TaskNumber     int64           `json:"taskNumber"`
	Title          string          `json:"title"`
	Description    json.RawMessage `json:"description,omitempty"`
	Status         string          `json:"status"`
	Type           string          `json:"type"`
	Priority       string          `json:"priority"`
	AssigneeEmail  *string         `json:"assigneeEmail"`
	CreatedByEmail string          `json:"createdByEmail"`
	Labels         []string        `json:"labels"`
	DueDate        *string         `json:"dueDate"` // YYYY-MM-DD
	Position       string          `json:"position"`
	ArchivedAt     *UTCTime        `json:"archivedAt"`
	CreatedAt      UTCTime         `json:"createdAt"`
	UpdatedAt      UTCTime         `json:"updatedAt"`
}

// DrTaskActivityEntry is one audit-feed row (one changed field per row). Field
// is nil for created/archived/restored rows; old/new values are human-readable
// renderings ('' for empty), never block JSON.
type DrTaskActivityEntry struct {
	ID         string  `json:"id"`
	ActorEmail string  `json:"actorEmail"`
	Action     string  `json:"action"`
	Field      *string `json:"field"`
	OldValue   *string `json:"oldValue"`
	NewValue   *string `json:"newValue"`
	CreatedAt  UTCTime `json:"createdAt"`
}

// DrTasksResponse is the board payload: every active task (plus archived ones
// appended when ?includeArchived=1), ordered by (status, position, created_at).
type DrTasksResponse struct {
	Tasks []DrTask `json:"tasks"`
}

// DrTaskDetailResponse is one task plus its full activity feed, ordered
// (created_at, seq) ascending.
type DrTaskDetailResponse struct {
	Task     DrTask                `json:"task"`
	Activity []DrTaskActivityEntry `json:"activity"`
}

// DrMoveTaskResponse returns the moved task. Rebalanced=true means the whole
// destination column's positions were rewritten inside the move transaction —
// the client should refetch the board rather than trust its local positions.
type DrMoveTaskResponse struct {
	Task       DrTask `json:"task"`
	Rebalanced bool   `json:"rebalanced"`
}
