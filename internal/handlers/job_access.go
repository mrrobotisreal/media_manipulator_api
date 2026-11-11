package handlers

import (
	"crypto/subtle"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// resultOwnershipEnforce is the RESULT_OWNERSHIP_ENFORCE switch (default true).
// When false, would-be denials are logged and the request is allowed, which is
// the rollout mode for callers that do not yet send X-MM-Session-ID.
var resultOwnershipEnforce atomic.Bool

func init() { resultOwnershipEnforce.Store(true) }

// SetResultOwnershipEnforce is called once at boot from configuration.
func SetResultOwnershipEnforce(on bool) { resultOwnershipEnforce.Store(on) }

// jobStatusOwnershipEnforce is the JOB_STATUS_OWNERSHIP_ENFORCE switch (default
// false, MA08): GET /api/job/:id and /events get the result-route ownership check.
var jobStatusOwnershipEnforce atomic.Bool

// SetJobStatusOwnershipEnforce is called once at boot from configuration.
func SetJobStatusOwnershipEnforce(on bool) { jobStatusOwnershipEnforce.Store(on) }

// jobStatusWouldDeny is the metric hook (mm_job_status_ownership_would_deny_total),
// set at boot; nil in tests.
var jobStatusWouldDeny atomic.Pointer[func()]

// SetJobStatusWouldDenyHook registers the would-deny counter increment.
func SetJobStatusWouldDenyHook(f func()) { jobStatusWouldDeny.Store(&f) }

// jobStatusWouldDenyLogged logs the would-deny line once per job.
var jobStatusWouldDenyLogged sync.Map

// authorizeJobStatus reports whether the caller may read a job's status or SSE
// stream. Same rules as authorizeJobAccessSID(..., true): ?sid= is accepted
// because EventSource cannot send headers. With the flag off a would-be denial is
// logged once per job, counted, and allowed. On false answer denyJobAccess.
// Call it after the job lookup and BEFORE TouchJobPoll: a stranger must not
// suppress the job-finished email by polling.
func authorizeJobStatus(c *gin.Context, jm *services.JobManager, job *models.ConversionJob) bool {
	if labJobBlocked(c, job) { // MM138: defence in depth, before any enforcement flag
		return false
	}
	if jobAccessAllowed(c, jm, job, true) {
		return true
	}
	if !jobStatusOwnershipEnforce.Load() {
		if _, seen := jobStatusWouldDenyLogged.LoadOrStore(job.ID, struct{}{}); !seen {
			log.Printf("job status ownership: would deny job %s (JOB_STATUS_OWNERSHIP_ENFORCE=false)", job.ID)
		}
		if f := jobStatusWouldDeny.Load(); f != nil && *f != nil {
			(*f)()
		}
		return true
	}
	return false
}

// legacyOwnerLogged ensures the "no recorded owner" exception logs once per job.
var legacyOwnerLogged sync.Map

// authorizeJobAccess reports whether the caller may read a job's results.
// Header-only; see authorizeJobAccessSID for the inline-route variant.
//
// On false the caller must answer 404 {"error":"Job not found"} (use
// denyJobAccess), identical to a missing job so ids cannot be probed.
func authorizeJobAccess(c *gin.Context, jm *services.JobManager, job *models.ConversionJob) bool {
	return authorizeJobAccessSID(c, jm, job, false)
}

// authorizeJobAccessSID is authorizeJobAccess that, when allowQuerySID is true,
// also accepts ?sid=<session id> because <video>/<img>/<iframe> cannot send
// headers. Only GET inline routes may pass true. Firebase tokens are never
// accepted from a query string.
func authorizeJobAccessSID(c *gin.Context, jm *services.JobManager, job *models.ConversionJob, allowQuerySID bool) bool {
	if labJobBlocked(c, job) { // MM138
		return false
	}
	if jobAccessAllowed(c, jm, job, allowQuerySID) {
		return true
	}
	if !resultOwnershipEnforce.Load() {
		log.Printf("result ownership: would deny job %s (RESULT_OWNERSHIP_ENFORCE=false)", job.ID)
		return true
	}
	return false
}

func jobAccessAllowed(c *gin.Context, jm *services.JobManager, job *models.ConversionJob, allowQuerySID bool) bool {
	if job == nil || jm == nil {
		return false
	}
	owner, ok := jm.JobOwnerOf(job.ID)
	if !ok || (owner.UserID == "" && owner.SessionID == "") {
		if _, seen := legacyOwnerLogged.LoadOrStore(job.ID, struct{}{}); !seen {
			log.Printf("result ownership: job %s has no recorded owner; allowing (debug)", job.ID)
		}
		return true
	}
	res, resolved := middleware.ResolveTier(c)
	if resolved {
		if res.Tier == tiers.TierOperator {
			return true
		}
		if res.User != nil && owner.UserID != "" && res.User.ID == owner.UserID {
			return true
		}
	}
	if owner.SessionID != "" {
		sid := strings.TrimSpace(c.GetHeader("X-MM-Session-ID"))
		if sid == "" && allowQuerySID {
			sid = strings.TrimSpace(c.Query("sid"))
		}
		if sid != "" && subtle.ConstantTimeCompare([]byte(sid), []byte(owner.SessionID)) == 1 {
			return true
		}
	}
	return false
}

// denyJobAccess writes the stranger response: byte-identical to a missing job.
func denyJobAccess(c *gin.Context) {
	c.JSON(404, gin.H{"error": "Job not found"})
}
