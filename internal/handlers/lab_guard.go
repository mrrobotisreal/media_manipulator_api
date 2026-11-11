package handlers

import (
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/lab"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// labGate is the Lab gate the job routes consult (MM138, defence in depth: the
// owner check already keeps a stranger out of Mitch's job; this keeps a Lab job
// from anyone failing the gate even if the owner rules were ever loosened, for
// example by RESULT_OWNERSHIP_ENFORCE=false or JOB_STATUS_OWNERSHIP_ENFORCE=false).
var labGate atomic.Pointer[lab.Gate]

// SetLabGate installs the gate once at boot. nil (tests) leaves every Lab job
// blocked: a nil gate passes nobody.
func SetLabGate(g *lab.Gate) { labGate.Store(g) }

func labPasses(c *gin.Context) bool { return labGate.Load().Passes(c) }

// isLabJob reports whether an in-memory job belongs to the Lab.
func isLabJob(job *models.ConversionJob) bool {
	if job == nil {
		return false
	}
	if lab.IsLabTool(jobMode(job)) {
		return true
	}
	if t, _ := job.Options["tool"].(string); lab.IsLabTool(t) {
		return true
	}
	return false
}

// labJobBlocked is true for a Lab job and a caller who fails the Lab gate.
func labJobBlocked(c *gin.Context, job *models.ConversionJob) bool {
	return isLabJob(job) && !labPasses(c)
}

// labRecordBlocked is labJobBlocked for a job known only from Postgres.
func labRecordBlocked(c *gin.Context, rec telemetry.JobRecord) bool {
	return (lab.IsLabTool(rec.Tool) || lab.IsLabTool(rec.Mode)) && !labPasses(c)
}

// labResultFile is the declared result file of a Lab job.
func labResultFile(job *models.ConversionJob) (string, bool) {
	if !isLabJob(job) {
		return "", false
	}
	m, ok := lab.Get(trimLab(jobMode(job)))
	if !ok {
		if t, _ := job.Options["tool"].(string); t != "" {
			m, ok = lab.Get(trimLab(t))
		}
	}
	if !ok || m.ResultFile == "" {
		return "", false
	}
	return m.ResultFile, true
}

func trimLab(s string) string {
	if len(s) >= len(lab.ToolPrefix) {
		return s[len(lab.ToolPrefix):]
	}
	return s
}

// labResultFiles are the result file names of every Lab model, for the
// recorded-job fallback that scans an output directory.
func labResultFiles() []string {
	var out []string
	for _, m := range lab.Models() {
		out = append(out, m.ResultFile)
	}
	return out
}
