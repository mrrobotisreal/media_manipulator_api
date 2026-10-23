package handlers

import (
	"context"
	"crypto/subtle"
	"log"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM24 ownership helper. MM05's authorizeJobAccess (job_access.go) answers
// "may this caller READ a job that is in the in-memory JobManager map"; it is
// header-only, allows ownerless jobs and lets operators in. The destructive
// routes need a stricter, database-backed question, "does this caller OWN this
// job or project", so they use the helpers here. The rules:
//
//   - a signed-in caller owns rows whose user_id is theirs;
//   - a guest owns rows whose session id is theirs AND that have no user owner;
//   - everyone else gets 404 (never 403: no existence leak);
//   - nobody is an owner by default: unowned rows are NOT deletable by anyone
//     (and operators have no bypass here).
//
// The download routes keep MM05's rules (authorizeJobAccess) and use
// recordAccessAllowed below for jobs that survived an API restart.

// JobRecordSource is the durable job lookup (*telemetry.Store).
type JobRecordSource interface {
	JobRecord(ctx context.Context, jobID string) (telemetry.JobRecord, error)
}

// callerSession is the raw X-MM-Session-ID header.
func callerSession(c *gin.Context) string { return strings.TrimSpace(c.GetHeader("X-MM-Session-ID")) }

// ownsRecord applies the ownership rule to a durable job record.
func ownsRecord(rec telemetry.JobRecord, userID, session string) bool {
	if !rec.Found {
		return false
	}
	if userID != "" {
		return rec.HasUser(userID)
	}
	return session != "" && len(rec.UserIDs) == 0 && rec.HasSession(session)
}

// mergeJobOwner folds the in-memory owner map (which also covers jobs that never
// reached Postgres) into a durable record.
func mergeJobOwner(rec telemetry.JobRecord, jm *services.JobManager, jobID string) telemetry.JobRecord {
	if jm == nil {
		return rec
	}
	if _, err := jm.GetJob(jobID); err == nil {
		rec.Found = true
	}
	if o, ok := jm.JobOwnerOf(jobID); ok {
		if o.UserID != "" && !rec.HasUser(o.UserID) {
			rec.UserIDs = append(rec.UserIDs, o.UserID)
		}
		if o.SessionID != "" && !rec.HasSession(o.SessionID) {
			rec.Sessions = append(rec.Sessions, o.SessionID)
		}
	}
	return rec
}

// recordAccessAllowed is jobAccessAllowed for a job known only from Postgres
// (the API restarted, so the in-memory map is empty). Same rules as MM05:
// operator, owning account, or owning session. A record with no recorded owner
// is DENIED (the in-memory path allows legacy ownerless jobs; a durable record
// that names nobody is not worth guessing about).
func recordAccessAllowed(c *gin.Context, rec telemetry.JobRecord, allowQuerySID bool) bool {
	if !rec.Found || (len(rec.UserIDs) == 0 && len(rec.Sessions) == 0) {
		return false
	}
	if res, ok := middleware.ResolveTier(c); ok {
		if res.Tier == tiers.TierOperator {
			return true
		}
		if res.User != nil && rec.HasUser(res.User.ID) {
			return true
		}
	}
	sid := callerSession(c)
	if sid == "" && allowQuerySID {
		sid = strings.TrimSpace(c.Query("sid"))
	}
	if sid != "" {
		for _, s := range rec.Sessions {
			if subtle.ConstantTimeCompare([]byte(sid), []byte(s)) == 1 {
				return true
			}
		}
	}
	return false
}

// authorizeRecordAccess applies RESULT_OWNERSHIP_ENFORCE around recordAccessAllowed.
func authorizeRecordAccess(c *gin.Context, rec telemetry.JobRecord, jobID string, allowQuerySID bool) bool {
	if recordAccessAllowed(c, rec, allowQuerySID) {
		return true
	}
	if !resultOwnershipEnforce.Load() {
		log.Printf("result ownership: would deny job %s from durable record (RESULT_OWNERSHIP_ENFORCE=false)", jobID)
		return rec.Found
	}
	return false
}
