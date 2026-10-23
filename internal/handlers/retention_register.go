package handlers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"strings"
)

// MM22: every handler that creates durable data tells the retained-objects
// ledger. This file is the one place handlers do it, replacing the old per-directory marker.
// The ledger is process-wide (retention.SetDefault at boot); with none installed
// every call is a no-op, and a failed write never fails the request.

// retentionOwnerFor is who a request's durable data belongs to: the server-
// resolved account when signed in (never a client header), the session otherwise.
func retentionOwnerFor(c *gin.Context) retention.Owner {
	o := retention.Owner{SessionID: strings.TrimSpace(c.GetHeader("X-MM-Session-ID"))}
	if res, ok := middleware.ResolveTier(c); ok {
		o.Tier = string(res.Tier)
		if res.User != nil {
			o.UserID = res.User.ID
		}
	}
	return o
}

// retentionOwnerFromJob is retentionOwnerFor for code that only has a services
// JobOwner (the MM05 owner map).
func retentionOwnerFromJob(o services.JobOwner) retention.Owner {
	return retention.Owner{UserID: o.UserID, SessionID: o.SessionID, Tier: o.Tier}
}

// detachedCtx outlives the request: a client that disconnects right after the
// upload must not cancel the bookkeeping for a file we already wrote.
func detachedCtx(c *gin.Context) context.Context {
	return context.WithoutCancel(c.Request.Context())
}

// registerJobDirs records a job's upload and/or output directory.
func registerJobDirs(c *gin.Context, jobID string, upload, output bool) {
	retention.Default().RegisterJobDirs(detachedCtx(c), retentionOwnerFor(c), jobID, upload, output)
}

// registerS3Object records an S3 object the request created or adopted.
func registerS3Object(c *gin.Context, key, parent, jobID, projectID string, size int64) {
	retention.Default().RegisterS3(detachedCtx(c), retentionOwnerFor(c), key, parent, jobID, projectID, size)
}

// registerStudioRow records a Darkroom project or asset row.
func registerStudioRow(c *gin.Context, kind, id, projectID string, size int64) {
	retention.Default().Register(detachedCtx(c), retention.RegisterInput{
		Kind: kind, Locator: id, Owner: retentionOwnerFor(c), ProjectID: projectID, SizeBytes: size,
	})
}

// retentionScopeFor packs the request's owner for a background goroutine.
func retentionScopeFor(c *gin.Context, jobID, projectID string) retention.Scope {
	return retention.Scope{Owner: retentionOwnerFor(c), JobID: jobID, ProjectID: projectID}
}

// newJobID picks a job id before the job exists, so an upload can be written
// straight under uploads/<jobId>/ and a directory is the unit from byte one.
func newJobID() string { return uuid.NewString() }

// stagedUploadPath is where an upload is first written: inside its future job
// directory, never as a loose top-level file.
func stagedUploadPath(uploadRoot, jobID, prefix, name string) string {
	return filepath.Join(uploadRoot, jobID, fmt.Sprintf("%s_%s", prefix, name))
}

// discardStaged removes a staged upload and its (now empty) job directory.
func discardStaged(path string) {
	_ = os.Remove(path)
	_ = os.Remove(filepath.Dir(path)) // fails harmlessly if anything else is in it
}
