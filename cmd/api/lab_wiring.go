package main

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/lab"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// labHandler and labGateVal are the Lab's handler and gate; setupRouter mounts
// them. nil (tests) mounts nothing.
var (
	labHandler *handlers.LabHandler
	labGateVal *lab.Gate
)

// newLabWiring builds the Lab (MM138): the gate (LAB_EMAILS), the runner and
// the handler. rt is the shared burst runtime; nil when burst is not configured,
// in which case a Lab model that needs a remote GPU fails closed with a clear
// error and FitsHome models still run locally.
func newLabWiring(cfg *config.Config, jobs *services.JobManager, inspector *services.MediaInspector,
	_ *gpu.Manager, runner *cmdaudit.Runner, rt *burstRuntime, logging *slog.Logger) (*handlers.LabHandler, *lab.Gate) {
	gate := lab.NewGate(cfg.Lab.Emails)
	logging.Info(gate.StartupLine())
	handlers.SetLabGate(gate)
	r := &lab.Runner{
		Cfg: cfg, Jobs: jobs, Local: exec.AuditRunner(runner),
		Validator: exec.Validator{Roots: exec.RootsFrom(cfg.UploadDir, cfg.OutputDir, cfg.TempDir, cfg.PhotoDir)},
		Log:       logging,
	}
	var ledger *burst.Ledger
	if rt != nil {
		r.Launch, r.Remote, r.Forget = rt.Planner, rt.Remote, rt.Tokens.Forget
		ledger = rt.LabLedger
	}
	return handlers.NewLabHandler(cfg, jobs, inspector, r, ledger), gate
}

// mountLab registers /api/lab/* behind the gate; with LAB_EMAILS empty every
// route answers 404.
func mountLab(api gin.IRouter) {
	if labHandler != nil && labGateVal != nil {
		handlers.RegisterLabRoutes(api, labHandler, labGateVal)
	}
}
