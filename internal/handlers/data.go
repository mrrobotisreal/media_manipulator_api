package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM24 per-item data controls: DELETE /api/jobs/:jobId and
// PATCH /api/jobs/:jobId/retention. Both are owner-scoped (ownership.go); a
// stranger gets the same 404 a missing job gets.

// Expirer is "expire now + sweep inline" (retention.Sweeper.ExpireNow).
type Expirer interface {
	ExpireNow(ctx context.Context, f telemetry.LedgerFilter) (retention.ExpireResult, error)
}

// DataStore is the slice of *telemetry.Store the per-item routes need.
type DataStore interface {
	JobRecordSource
	PinJobLedger(ctx context.Context, userID, jobID string, until time.Time) (int64, time.Time, error)
	DetachJob(ctx context.Context, jobID string) error
}

// DataHandler serves the per-item delete and keep-longer routes.
type DataHandler struct {
	store    DataStore
	expirer  Expirer
	jobs     *services.JobManager
	logger   *slog.Logger
	now      func() time.Time
	onDelete func(scope string) // account_data_deleted metric hook (optional)
}

// NewDataHandler builds the handler. Any dependency may be nil; routes then 503.
func NewDataHandler(store DataStore, expirer Expirer, jobs *services.JobManager, logger *slog.Logger) *DataHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &DataHandler{store: store, expirer: expirer, jobs: jobs, logger: logger, now: time.Now}
}

// deletedBody is the shared response of every delete route.
type deletedBody struct {
	Deleted struct {
		Objects int   `json:"objects"`
		Bytes   int64 `json:"bytes"`
	} `json:"deleted"`
	Pending int `json:"pending"`
}

func newDeletedBody(r retention.ExpireResult) deletedBody {
	var b deletedBody
	b.Deleted.Objects, b.Deleted.Bytes, b.Pending = r.Objects, r.Bytes, r.Pending
	return b
}

// deleteTimeout bounds a whole delete request (inline sweep budget + slack).
const deleteTimeout = retention.ExpireBudget + 15*time.Second

// callerIdentity is who is asking: a server-resolved account id and/or the raw
// session header.
func callerIdentity(c *gin.Context) (userID, session string) {
	if res, ok := middleware.ResolveTier(c); ok && res.User != nil {
		userID = res.User.ID
	}
	return userID, callerSession(c)
}

func (h *DataHandler) ready() bool {
	return h != nil && h.store != nil && h.expirer != nil
}

// requireJobOwner answers 404 unless the caller owns the job. It returns the
// caller's user id and session on success. 401 when the caller has no identity
// at all (no account, no session header).
func (h *DataHandler) requireJobOwner(c *gin.Context, jobID string) (userID, session string, ok bool) {
	userID, session = callerIdentity(c)
	if userID == "" && session == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return "", "", false
	}
	rec, err := h.store.JobRecord(c.Request.Context(), jobID)
	if err != nil {
		h.logger.Warn("data: job lookup failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Job lookup is temporarily unavailable"})
		return "", "", false
	}
	rec = mergeJobOwner(rec, h.jobs, jobID)
	if !ownsRecord(rec, userID, session) {
		denyJobAccess(c)
		return "", "", false
	}
	return userID, session, true
}

// DeleteJob handles DELETE /api/jobs/:jobId.
//
//	200 {"deleted":{"objects":N,"bytes":N},"pending":N}
//
// Ledger rows with that job id expire and sweep (upload dir, output dir, S3
// objects, parked metadata reports); the job row is detached from the account and
// anonymized; the in-memory job is dropped. pending>0 means something could not
// be deleted yet (S3 error, job still running) and stays retryable.
func (h *DataHandler) DeleteJob(c *gin.Context) {
	if !h.ready() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	jobID := strings.TrimSpace(c.Param("jobId"))
	if _, _, ok := h.requireJobOwner(c, jobID); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	res, err := h.expirer.ExpireNow(ctx, telemetry.LedgerFilter{JobID: jobID})
	if err != nil {
		h.logger.Error("data: delete job failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	if err := h.store.DetachJob(ctx, jobID); err != nil {
		h.logger.Warn("data: detaching job row failed; a repeat will retry", "error", err.Error())
		if res.Pending == 0 {
			res.Pending = 1
		}
	}
	if res.Pending == 0 {
		h.jobs.ForgetJob(jobID)
	}
	if h.onDelete != nil {
		h.onDelete("item")
	}
	c.JSON(http.StatusOK, newDeletedBody(res))
}

// Keep-longer bounds, hours.
const (
	keepMinHours = 1
	keepMaxHours = retention.MaxHours // 17,520 = 2 years
)

// KeepJobLonger handles PATCH /api/jobs/:jobId/retention.
//
//	body     {"keepForHours":N}   1 <= N <= 17520
//	200      {"keepForHours":N,"expiresAt":"RFC3339","keptLonger":true,"updated":K}
//	400      guests (sign-in required) and out-of-range / malformed bodies
//	404      not the owner
//	409      nothing left to keep (already deleted or expired)
//
// The job's live ledger rows are extended to at least now+N h (never shortened)
// and marked kept_longer, so a later retention-window change keeps the pin.
func (h *DataHandler) KeepJobLonger(c *gin.Context) {
	if h == nil || h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Retention is temporarily unavailable"})
		return
	}
	userID, _ := callerIdentity(c)
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sign_in_required", "message": "Keeping a result longer needs an account"})
		return
	}
	jobID := strings.TrimSpace(c.Param("jobId"))
	if _, _, ok := h.requireJobOwner(c, jobID); !ok {
		return
	}
	var body struct {
		KeepForHours *int `json:"keepForHours"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.KeepForHours == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "keepForHours is required", "field": "keepForHours"})
		return
	}
	n := *body.KeepForHours
	if n < keepMinHours || n > keepMaxHours {
		c.JSON(http.StatusBadRequest, gin.H{"error": "keepForHours must be between 1 and 17520", "field": "keepForHours"})
		return
	}
	until := h.now().UTC().Add(time.Duration(n) * time.Hour)
	updated, latest, err := h.store.PinJobLedger(c.Request.Context(), userID, jobID, until)
	if err != nil {
		h.logger.Error("data: keep longer failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Retention is temporarily unavailable"})
		return
	}
	if updated == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "nothing_to_keep", "message": "This result has already been deleted"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"keepForHours": n,
		"expiresAt":    latest.UTC().Format(time.RFC3339),
		"keptLonger":   true,
		"updated":      updated,
	})
}

func ledgerFilterForProject(id string) telemetry.LedgerFilter {
	return telemetry.LedgerFilter{ProjectID: id}
}
