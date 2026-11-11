package main

import (
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/staging"
	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// burstRuntime is everything that launches and drives a burst instance, built
// once at start-up when burst is fully configured (MM138). The Lab uses it
// today; MM127 session b's runGPUStep and `NewBurstPlanner(cfg, runtime.Planner,
// runtime.Remote, scheduler)` use the SAME value instead of building their own.
type burstRuntime struct {
	// Planner launches (Launch for the trigger, LaunchDirect for the Lab),
	// advances and terminates runs, and settles the ledgers.
	Planner *burst.Planner
	// Remote is the remote executor (WaitReady, Run).
	Remote *burst.Remote
	// Staging is the S3 object store both the executor and the worker use.
	Staging staging.ObjectStore
	// Tokens holds each run's HMAC token (UserDataBuilder); Remote reads it.
	Tokens *burst.UserDataBuilder
	// Ledger enforces the burst caps; LabLedger the Lab sub-caps (summing only
	// purpose = 'lab' rows).
	Ledger    *burst.Ledger
	LabLedger *burst.Ledger
}

// newBurstRuntime builds the runtime, or returns nil when burst is not
// Configured() (enabled and every credential, subnet, image and Tailscale
// setting present), the database is down, or there is no S3 client. nil means
// "fail closed": no instance is ever launched. prov is the EC2 provider
// startBurst already built; prefixes is the staging prefix store.
//
// It makes no AWS, Tailscale or S3 call: the clients are lazy and the worker
// binary is only uploaded when a launch builds its user-data.
func newBurstRuntime(cfg *config.Config, pool *pgxpool.Pool, s3Client *s3.Client, prov burst.Provider,
	prefixes burst.PrefixStore, drift *burst.DriftChecker, logging *slog.Logger) *burstRuntime {
	bc := cfg.Burst
	if !bc.Configured() || pool == nil || s3Client == nil || prov == nil {
		return nil
	}
	st := burst.NewPGStore(pool)
	ledger := burst.NewLedger(bc.DailySpendCapUSD, bc.MonthlySpendCapUSD, st, nil)
	labLedger := burst.NewLedger(cfg.Lab.DailySpendCapUSD, cfg.Lab.MonthlySpendCapUSD,
		burst.PurposeSource{Src: st, Purpose: burst.PurposeLab}, nil)
	store := &staging.S3Store{API: s3Client, Bucket: cfg.S3Bucket}
	minter := &burst.TailscaleMinter{ClientID: bc.TailscaleClientID, ClientSecret: bc.TailscaleClientSecret,
		Tailnet: bc.TailscaleTailnet, Tag: bc.TailscaleTag, Log: logging}
	ub := &burst.UserDataBuilder{Cfg: burst.UserDataConfig{
		Bucket: cfg.S3Bucket, Region: bc.Region, Tag: bc.TailscaleTag, Prefix: bc.S3Prefix, Port: bc.WorkerPort,
		MaxMinutes: bc.MaxInstanceMinutes, OrphanMinutes: 10,
	}}
	if path, ok := burst.FindWorkerBinary(burst.DefaultWorkerBinaryPaths()); ok {
		ub.Binary = &burst.S3WorkerBinary{Store: store, Path: path, Prefix: bc.S3Prefix, Log: logging}
	} // else UserDataBuilder.Build fails closed ("no worker binary configured")
	planner := &burst.Planner{
		Cfg: bc, Provider: prov, Store: st, Ledger: ledger, LabLedger: labLedger, Minter: minter,
		Prefixes: prefixes, UserData: ub.Build, Log: logging,
	}
	if drift != nil {
		planner.Stale = drift.Stale
	}
	remote := &burst.Remote{
		Store: store, Life: planner, Tokens: ub, Resolver: minter,
		Validator: exec.Validator{Roots: exec.RootsFrom(cfg.UploadDir, cfg.OutputDir, cfg.TempDir, cfg.PhotoDir)},
		Prefix:    bc.S3Prefix, Port: bc.WorkerPort, RemoteRoot: exec.DefaultRemoteRoot,
		Sanitizer: cmdaudit.NewPathSanitizer(cfg.UploadDir, cfg.OutputDir, cfg.TempDir), Log: logging,
		ResolveTimeout: time.Duration(bc.BootTimeoutSeconds) * time.Second,
	}
	return &burstRuntime{Planner: planner, Remote: remote, Staging: store, Tokens: ub, Ledger: ledger, LabLedger: labLedger}
}
