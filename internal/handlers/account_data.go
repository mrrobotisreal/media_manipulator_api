package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM24 account-level data controls, all under the authed /api/account group:
//
//	POST   /api/account/data/delete-all    (this file)
//	GET    /api/account/data/export        (account_export.go)
//	POST   /api/account/sessions/revoke    (account_data.go)
//	DELETE /api/account                    (account_delete.go)
//
// Every one is scoped to the verified caller's own mm_users row. Nothing takes a
// user id from the request.

// AccountDataStore is the slice of *telemetry.Store these routes use.
type AccountDataStore interface {
	UserByFirebaseUID(ctx context.Context, firebaseUID string) (*models.User, error)
	UserJobIDs(ctx context.Context, userID string) ([]string, error)
	DetachUserJobs(ctx context.Context, userID string) (int64, error)
	DeleteUserProjects(ctx context.Context, userID string) (int64, error)
	UserProjectIDs(ctx context.Context, userID string) ([]string, error)
}

// AccountDataHandler serves the account-wide data routes.
type AccountDataHandler struct {
	store   AccountDataStore
	expirer Expirer
	jobs    *services.JobManager
	logger  *slog.Logger
	now     func() time.Time
	// OnDelete is the account_data_deleted metric hook (scope = "all").
	OnDelete func(scope string)

	revoker SessionRevoker
	account *accountDeleteDeps
	export  *exportDeps
}

// NewAccountDataHandler builds the handler.
func NewAccountDataHandler(store AccountDataStore, expirer Expirer, jobs *services.JobManager, logger *slog.Logger) *AccountDataHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &AccountDataHandler{store: store, expirer: expirer, jobs: jobs, logger: logger, now: time.Now}
}

// callerUser loads the verified caller's account row. It writes the response and
// returns nil when the caller is signed out (401), has no account row yet (404)
// or the database is unavailable (503).
func (h *AccountDataHandler) callerUser(c *gin.Context) *models.User {
	p, ok := middleware.PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return nil
	}
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Accounts are temporarily unavailable"})
		return nil
	}
	u, err := h.store.UserByFirebaseUID(c.Request.Context(), p.UID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Accounts are temporarily unavailable"})
		return nil
	}
	if u == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Account not found"})
		return nil
	}
	return u
}

// deleteAllBody is the delete-all response.
type deleteAllBody struct {
	Deleted struct {
		Objects  int   `json:"objects"`
		Bytes    int64 `json:"bytes"`
		Projects int   `json:"projects"`
		Jobs     int64 `json:"jobs"`
	} `json:"deleted"`
	Pending int `json:"pending"`
}

// DeleteAll handles POST /api/account/data/delete-all.
//
//	body 	{"confirm":"DELETE"}  (exact; anything else is 400 confirm_required)
//	200 	{"deleted":{"objects":N,"bytes":N,"projects":N,"jobs":N},"pending":N}
//
// Every ledger row of the caller (files, S3 objects, parked metadata reports),
// every Darkroom project and every job row go; the account, its settings and the
// Firebase user remain. delete_metadata_reports is ignored: everything goes.
// Idempotent: a repeat deletes nothing and answers zeros.
func (h *AccountDataHandler) DeleteAll(c *gin.Context) {
	u := h.callerUser(c)
	if u == nil {
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Confirm != "DELETE" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "confirm_required", "message": `Send {"confirm":"DELETE"} to delete all your files`})
		return
	}
	if h.expirer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()

	var out deleteAllBody
	jobIDs, _ := h.store.UserJobIDs(ctx, u.ID)
	projIDs, _ := h.store.UserProjectIDs(ctx, u.ID)

	res, err := h.expirer.ExpireNow(ctx, telemetry.LedgerFilter{UserID: u.ID})
	if err != nil {
		h.logger.Error("data: delete-all failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	out.Deleted.Objects, out.Deleted.Bytes, out.Pending = res.Objects, res.Bytes, res.Pending

	// Projects the ledger never knew (created before MM22) and any rows a failed
	// S3 delete left behind are removed directly: the project rows go regardless;
	// S3 keys of ledgered projects stay retryable through their ledger rows.
	if n, err := h.store.DeleteUserProjects(ctx, u.ID); err != nil {
		h.logger.Warn("data: deleting project rows failed", "error", err.Error())
		out.Pending++
	} else {
		out.Deleted.Projects = int(n)
	}
	if n, err := h.store.DetachUserJobs(ctx, u.ID); err != nil {
		h.logger.Warn("data: detaching job rows failed", "error", err.Error())
		out.Pending++
	} else {
		out.Deleted.Jobs = n
	}
	if out.Pending == 0 {
		for _, id := range jobIDs {
			h.jobs.ForgetJob(id)
		}
	}
	_ = projIDs
	if h.OnDelete != nil {
		h.OnDelete("all")
	}
	c.JSON(http.StatusOK, out)
}

// SessionRevoker revokes a Firebase user's refresh tokens and deletes the user
// (firebase-admin behind an interface; fake in tests).
type SessionRevoker interface {
	RevokeRefreshTokens(ctx context.Context, uid string) error
}

// SetRevoker attaches the Firebase session revoker (nil = the route 503s).
func (h *AccountDataHandler) SetRevoker(r SessionRevoker) { h.revoker = r }

// RevokeSessions handles POST /api/account/sessions/revoke ("sign out
// everywhere"): Firebase Admin RevokeRefreshTokens(uid) for the verified caller.
//
//	200 {"revokedAt":"RFC3339"}
//
// Existing ID tokens stay valid until they expire (<= 1 h); refresh tokens stop
// working immediately, so every device is signed out at its next refresh.
func (h *AccountDataHandler) RevokeSessions(c *gin.Context) {
	p, ok := middleware.PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if h.revoker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sign-out everywhere is temporarily unavailable"})
		return
	}
	if err := h.revoker.RevokeRefreshTokens(c.Request.Context(), p.UID); err != nil {
		h.logger.Error("data: revoking sessions failed", "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "Could not sign you out everywhere; try again"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"revokedAt": h.now().UTC().Format(time.RFC3339)})
}

// Register mounts the account-wide data routes. authed already carries
// RequireFirebaseAuth; account is the bare /account group (DELETE /account is on
// the parent so its path is exactly /api/account).
func (h *AccountDataHandler) Register(account *gin.RouterGroup, authed *gin.RouterGroup) {
	authed.POST("/data/delete-all", h.DeleteAll)
	authed.POST("/sessions/revoke", h.RevokeSessions)
	authed.GET("/data/export", h.limitExport(), h.Export)
	// DELETE /api/account: fresh token + exact confirm string, enforced in the handler.
	authed.DELETE("", h.DeleteAccount)
}
