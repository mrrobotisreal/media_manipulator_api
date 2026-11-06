// Command assistant-flywheel is Medi's weekly quality flywheel (MM117, design
// MEDI_ASSISTANT.md §20): it reads the week's signed-in conversations, flags the
// turns that went badly, clusters them and writes a reviewable evidence file
// with a proposal per cluster (a recipe stub, a tool-page whenToUse fix, or an
// eval case to paste). It never changes a prompt, recipe or case by itself.
//
//	go run ./cmd/assistant-flywheel --env-file ~/media-manipulator/api/.env --estimate
//	go run ./cmd/assistant-flywheel --env-file … --week 2026-W46 --out /tmp/medi-flywheel-2026-W46.md
//	go run ./cmd/assistant-flywheel --env-file … --label --i-know-this-costs-money --out …
//	go run ./cmd/assistant-flywheel --env-file … --export-routing /tmp/medi-routing-2026-W46.jsonl
//
// The database is opened READ ONLY (a READ ONLY transaction, only SELECTs, and a
// test pins both). Conversation text stays on the host: the report quotes at
// most 200 scrubbed characters per example, and only --label sends anything
// out (scrubbed examples to the title model). Run it on the API host, then copy
// the report (not the routing JSONL) into docs/evidence/.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/joho/godotenv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant/flywheel"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/db"
)

func main() {
	o, err := flywheel.ParseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if err.Error() != "flag: help requested" {
			fmt.Fprintln(os.Stderr, "assistant-flywheel:", err)
		}
		os.Exit(2)
	}
	if o.EnvFile != "" {
		if err := godotenv.Load(o.EnvFile); err != nil {
			fail("cannot load --env-file: %v", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfgAll := config.Load()
	pool, err := db.New(ctx, cfgAll)
	if err != nil || pool == nil {
		fail("needs a reachable DATABASE_URL: %v", err)
	}
	defer pool.Close()
	tx, err := flywheel.ReadOnlyTx(ctx, pool)
	if err != nil {
		fail("begin read-only transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // nothing to commit, ever

	k, err := assistant.NewEmbeddedKnowledge()
	if err != nil {
		fail("knowledge: %v", err)
	}
	cfg := cfgAll.Assistant
	d := flywheel.Deps{Source: flywheel.PGSource{Q: tx}, Retriever: k, LabelModel: cfg.ModelTitle, Stdout: os.Stdout}
	if o.AnalyticsExport != "" {
		f, err := os.Open(o.AnalyticsExport)
		if err != nil {
			fail("--analytics-export: %v", err)
		}
		d.Analytics, err = flywheel.ParseAnalytics(f)
		_ = f.Close()
		if err != nil {
			fail("%v", err)
		}
	}
	if o.Label && !o.Estimate {
		if cfg.APIKey == "" {
			fail("ASSISTANT_OPENROUTER_API_KEY is not set (Medi's own key; OPENROUTER_API_KEY is never used)")
		}
		d.Labeler = &flywheel.Labeler{Model: cfg.ModelTitle,
			Provider: assistant.NewOpenRouterProvider(cfg.BaseURL, cfg.APIKey, cfg.AttributionURL, cfg.ProviderOrder)}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if _, err := flywheel.Run(ctx, o, d); err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "assistant-flywheel: "+format+"\n", args...)
	os.Exit(1)
}
