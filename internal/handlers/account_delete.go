package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/billing"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM24 account deletion: DELETE /api/account.
//
//	body  {"confirm":"DELETE MY ACCOUNT"}  (exact)
//	401   {"error":"reauth_required"}      auth_time older than 5 minutes
//	200   AccountDeleteReport              every step done
//	502   AccountDeleteReport (status "incomplete")  a step failed: retry the same call
//
// STEP ORDER (each idempotent, so a retry by the same signed-in user resumes):
//
//	stripe     cancel the subscription now (skipped when billing is not configured)
//	sweep      expire every ledger row of the user + inline sweep, parked metadata
//	           reports included. Files still pending halt the run: the mm_users row
//	           cannot go while ledger rows point at it (FK), and orphaning files is
//	           the one outcome this route must never produce.
//	projects   studio_projects (versions and assets cascade)
//	settings   mm_user_settings, mm_tier_grants
//	anonymize  mm_conversion_jobs: user_id NULL, file names and hashes dropped
//	analytics  POST /v1/dsr/delete, person scope, with the caller's token. A failure
//	           is REPORTED, never blocks: the account is going away either way and
//	           the token will not work again after step "firebase".
//	users      tombstone, then the mm_users row
//	firebase   Firebase Admin DeleteUser (last: a failed run can be retried by the
//	           same signed-in user; an already-deleted user counts as success)
//
// The tombstone (sha256 of the uid, 30 days) is written immediately BEFORE the
// mm_users row goes, so the window in which sign-in could re-create an empty
// account already refuses to claim old session rows.
//
// Resume after the mm_users row is gone: a verified, fresh-token caller with no
// account row skips the data steps and still runs analytics, tombstone and
// firebase.

// Step names, in order.
const (
	StepStripe    = "stripe"
	StepSweep     = "sweep"
	StepProjects  = "projects"
	StepSettings  = "settings"
	StepAnonymize = "anonymize"
	StepAnalytics = "analytics"
	StepUsers     = "users"
	StepFirebase  = "firebase"
)

// Step statuses.
const (
	StepDone    = "done"
	StepSkipped = "skipped"
	StepFailed  = "failed"
	StepNotRun  = "not_run"
)

// reauthWindow is how recent a sign-in must be.
const reauthWindow = 5 * time.Minute

// AccountDeleteStore is the slice of *telemetry.Store account deletion uses.
type AccountDeleteStore interface {
	AccountDataStore
	DeleteUserSettingsAndGrants(ctx context.Context, userID string) (settings, grants int64, err error)
	DeleteUserRow(ctx context.Context, userID string) (bool, error)
	LiveLedgerCount(ctx context.Context, userID string) (int, error)
	WriteAccountTombstone(ctx context.Context, firebaseUID string, now time.Time) error
}

// FirebaseUserDeleter deletes the Firebase user (firebase-admin; fake in tests).
type FirebaseUserDeleter interface {
	DeleteUser(ctx context.Context, uid string) error
}

// AccountDeleteConfig wires the collaborators.
type AccountDeleteConfig struct {
	Store     AccountDeleteStore
	Expirer   Expirer
	Stripe    billing.SubscriptionEnder // nil = NoopEnder
	Analytics analyticsemit.DSRClient   // nil = not configured
	Firebase  FirebaseUserDeleter
	// Metrics receives the outcome ("completed", "incomplete", "files_pending").
	Metrics func(result string)
	// Emitter gets server_account_deleted (dropped without visitor ids).
	Emitter *analyticsemit.Emitter
}

type accountDeleteDeps struct{ AccountDeleteConfig }

// SetAccountDeletion enables DELETE /api/account.
func (h *AccountDataHandler) SetAccountDeletion(cfg AccountDeleteConfig) {
	if cfg.Stripe == nil {
		cfg.Stripe = billing.NoopEnder{}
	}
	h.account = &accountDeleteDeps{cfg}
}

// StepReport is one step's outcome.
type StepReport struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// AccountDeleteReport is the response body.
type AccountDeleteReport struct {
	Status string       `json:"status"` // "completed" | "incomplete"
	Steps  []StepReport `json:"steps"`
	// Deleted counts what this run removed (a resumed run counts only its own work).
	Deleted struct {
		Objects  int   `json:"objects"`
		Bytes    int64 `json:"bytes"`
		Projects int64 `json:"projects"`
		Jobs     int64 `json:"jobs"`
		Settings int64 `json:"settings"`
		Grants   int64 `json:"grants"`
	} `json:"deleted"`
	Stripe    string `json:"stripe"`
	Analytics string `json:"analytics"` // ok | failed | not_configured | skipped
	// Retryable is true when calling DELETE /api/account again resumes the run.
	Retryable bool `json:"retryable"`
	// Message tells the client what to do next when the run is incomplete.
	Message string `json:"message,omitempty"`
}

// freshEnough reports whether the principal authenticated within the window.
func freshEnough(p *middleware.Principal, now time.Time) bool {
	if p == nil || p.AuthTime <= 0 {
		return false
	}
	t := time.Unix(p.AuthTime, 0)
	return !t.After(now.Add(time.Minute)) && now.Sub(t) <= reauthWindow
}

func bearerOf(c *gin.Context) string {
	tok, _ := strings.CutPrefix(strings.TrimSpace(c.GetHeader("Authorization")), "Bearer ")
	return strings.TrimSpace(tok)
}

// DeleteAccount handles DELETE /api/account.
func (h *AccountDataHandler) DeleteAccount(c *gin.Context) {
	p, ok := middleware.PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Confirm != "DELETE MY ACCOUNT" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "confirm_required", "message": `Send {"confirm":"DELETE MY ACCOUNT"} to delete your account`})
		return
	}
	now := h.now()
	if !freshEnough(p, now) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "reauth_required", "message": "Sign in again, then retry within 5 minutes"})
		return
	}
	d := h.account
	if d == nil || d.Store == nil || d.Expirer == nil || d.Firebase == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Account deletion is temporarily unavailable"})
		return
	}
	user, err := d.Store.UserByFirebaseUID(c.Request.Context(), p.UID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Accounts are temporarily unavailable"})
		return
	}

	// The run must outlive a client that hangs up halfway: half a deletion is the
	// worst outcome, and every step is idempotent anyway.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 4*time.Minute)
	defer cancel()
	rep := h.runAccountDeletion(ctx, d, p, user, bearerOf(c), now)

	result := "completed"
	if rep.Status != "completed" {
		result = "incomplete"
		if rep.stepStatus(StepSweep) == StepFailed {
			result = "files_pending"
		}
	}
	if d.Metrics != nil {
		d.Metrics(result)
	}
	if rep.Status == "completed" {
		h.emitAccountDeleted(c, d, p, rep)
		c.JSON(http.StatusOK, rep)
		return
	}
	c.JSON(http.StatusBadGateway, rep)
}

func (r *AccountDeleteReport) stepStatus(name string) string {
	for _, s := range r.Steps {
		if s.Step == name {
			return s.Status
		}
	}
	return StepNotRun
}

func (r *AccountDeleteReport) add(step, status, detail string) {
	r.Steps = append(r.Steps, StepReport{Step: step, Status: status, Detail: detail})
}

// runAccountDeletion executes the steps in order. user may be nil (resume after
// the mm_users row is already gone).
func (h *AccountDataHandler) runAccountDeletion(ctx context.Context, d *accountDeleteDeps, p *middleware.Principal, user *models.User, token string, now time.Time) *AccountDeleteReport {
	rep := &AccountDeleteReport{Status: "incomplete", Stripe: "skipped", Analytics: "skipped"}
	halt := func(step string, err error) *AccountDeleteReport {
		rep.add(step, StepFailed, err.Error())
		rep.Retryable = true
		rep.Message = "Deletion did not finish at step \"" + step + "\". Nothing was left half-visible: try again and it continues from here."
		h.logger.Error("account deletion halted", "step", step, "error", err.Error())
		return rep
	}
	skip := func(step, why string) { rep.add(step, StepSkipped, why) }

	hasRow := user != nil
	uid := ""
	if hasRow {
		uid = user.ID
	}

	// 1. stripe
	switch {
	case !hasRow:
		skip(StepStripe, "no account row")
	default:
		res, err := d.Stripe.EndSubscription(ctx, user.StripeSubscriptionID)
		if err != nil {
			return halt(StepStripe, fmt.Errorf("cancelling the subscription: %w", err))
		}
		rep.Stripe = res
		if res == billing.EndSkippedNoBilling || res == billing.EndNoSubscription {
			skip(StepStripe, res)
		} else {
			rep.add(StepStripe, StepDone, res)
		}
	}

	// 2. sweep
	if hasRow {
		res, err := d.Expirer.ExpireNow(ctx, telemetry.LedgerFilter{UserID: uid})
		if err != nil {
			return halt(StepSweep, err)
		}
		rep.Deleted.Objects, rep.Deleted.Bytes = res.Objects, res.Bytes
		if res.Pending > 0 {
			return halt(StepSweep, fmt.Errorf("%d stored files could not be deleted yet", res.Pending))
		}
		if n, err := d.Store.LiveLedgerCount(ctx, uid); err != nil || n > 0 {
			if err == nil {
				err = fmt.Errorf("%d stored files remain", n)
			}
			return halt(StepSweep, err)
		}
		rep.add(StepSweep, StepDone, "")
	} else {
		skip(StepSweep, "no account row")
	}

	// 3-5. rows
	if hasRow {
		n, err := d.Store.DeleteUserProjects(ctx, uid)
		if err != nil {
			return halt(StepProjects, err)
		}
		rep.Deleted.Projects = n
		rep.add(StepProjects, StepDone, "")

		s, g, err := d.Store.DeleteUserSettingsAndGrants(ctx, uid)
		if err != nil {
			return halt(StepSettings, err)
		}
		rep.Deleted.Settings, rep.Deleted.Grants = s, g
		rep.add(StepSettings, StepDone, "")

		jobs, err := d.Store.DetachUserJobs(ctx, uid)
		if err != nil {
			return halt(StepAnonymize, err)
		}
		rep.Deleted.Jobs = jobs
		rep.add(StepAnonymize, StepDone, "")
	} else {
		skip(StepProjects, "no account row")
		skip(StepSettings, "no account row")
		skip(StepAnonymize, "no account row")
	}

	// 6. analytics DSR: reported, never blocking.
	switch {
	case d.Analytics == nil:
		rep.Analytics = "not_configured"
		skip(StepAnalytics, "not_configured")
	case token == "":
		rep.Analytics = "failed"
		rep.add(StepAnalytics, StepFailed, "no token to present")
	default:
		if _, err := d.Analytics.DeletePerson(ctx, token); err != nil {
			rep.Analytics = "failed"
			rep.add(StepAnalytics, StepFailed, err.Error())
			h.logger.Warn("account deletion: analytics erasure failed; continuing", "error", err.Error())
			rep.Message = "Your analytics data could not be erased automatically; use the data-deletion request in the privacy settings or contact support."
		} else {
			rep.Analytics = "ok"
			rep.add(StepAnalytics, StepDone, "")
		}
	}

	// 7. tombstone, then the mm_users row.
	if err := d.Store.WriteAccountTombstone(ctx, p.UID, now); err != nil {
		return halt(StepUsers, fmt.Errorf("writing the tombstone: %w", err))
	}
	if hasRow {
		if _, err := d.Store.DeleteUserRow(ctx, uid); err != nil {
			return halt(StepUsers, err)
		}
		rep.add(StepUsers, StepDone, "")
	} else {
		skip(StepUsers, "no account row")
	}

	// 8. firebase
	if err := d.Firebase.DeleteUser(ctx, p.UID); err != nil {
		return halt(StepFirebase, err)
	}
	rep.add(StepFirebase, StepDone, "")

	rep.Status = "completed"
	if rep.Analytics == "failed" && rep.Message == "" {
		rep.Message = "Account deleted. Analytics erasure failed; contact support to complete it."
	}
	return rep
}

func (h *AccountDataHandler) emitAccountDeleted(c *gin.Context, d *accountDeleteDeps, p *middleware.Principal, rep *AccountDeleteReport) {
	if d.Emitter == nil || !d.Emitter.Enabled() {
		return
	}
	d.Emitter.Emit(analyticsemit.Event{
		EventName: analyticsemit.ServerAccountDeleted,
		EventTS:   h.now().UTC(),
		VisitorID: strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")),
		SessionID: strings.TrimSpace(c.GetHeader("X-MM-Session-ID")),
		Properties: map[string]any{
			"analytics": rep.Analytics,
			"stripe":    rep.Stripe,
		},
	})
}
