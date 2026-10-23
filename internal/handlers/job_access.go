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
