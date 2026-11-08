package burst

import (
	"context"
	"log/slog"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// Start is the wiring rule for cmd/api: the reaper runs whenever AWS
// credentials are configured, even with BURST_ENABLED=false. With no
// credentials it returns nil and nothing (no AWS or Tailscale call) happens.
// prov must be non-nil when creds are present; the caller builds it. The
// returned reaper is already running in a goroutine.
func Start(ctx context.Context, cfg config.BurstConfig, prov Provider, st Store, prefixes PrefixStore,
	live func() []Run, ledger *Ledger, log *slog.Logger) *Reaper {
	if log == nil {
		log = slog.Default()
	}
	log.Info(cfg.StartupLine())
	if !cfg.AWSConfigured() || prov == nil {
		return nil
	}
	r := &Reaper{Cfg: cfg, Provider: prov, Store: st, Prefixes: prefixes, Live: live, Ledger: ledger, Log: log}
	go r.Run(ctx)
	return r
}
