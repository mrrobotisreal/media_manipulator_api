// Command assistant-eval runs Medi's checked-in eval cases against the real
// models and grades the answers with code (MM106, design §16).
//
// It costs real money, so it prints an estimate and refuses to call the model
// without --i-know-this-costs-money:
//
//	go run ./cmd/assistant-eval --estimate                       # print the estimate, no calls
//	go run ./cmd/assistant-eval --surface guide --i-know-this-costs-money
//	go run ./cmd/assistant-eval --case guide-gif-from-video --repeat 3 --i-know-this-costs-money
//	go run ./cmd/assistant-eval --suite adjust --i-know-this-costs-money   # MM115: the multi-round adjust suite
//	go run ./cmd/assistant-eval --suite routing --i-know-this-costs-money  # MM116a: the which-tool suite
//	go run ./cmd/assistant-eval --suite options --i-know-this-costs-money  # MM116a: the tool-options suite
//	go run ./cmd/assistant-eval --suite jobs --i-know-this-costs-money     # MM116b: submit_job confirmation cards
//	go run ./cmd/assistant-eval --gate --i-know-this-costs-money     # fail when a surface's pass rate drops
//	go run ./cmd/assistant-eval --record --i-know-this-costs-money   # write assistant_eval_runs rows
//
// --record and --gate read DATABASE_URL (from --env-file or the environment);
// without them the CLI never touches a database. --gate compares each surface
// with the latest recorded run of that surface, prints that run's prompt
// version and exits 1 on a drop; --gate with --record gates first, then records.
//
// The key is ASSISTANT_OPENROUTER_API_KEY from the environment (Medi's own key,
// never OPENROUTER_API_KEY). On the API host, --env-file loads the service's
// env file into this process (nothing is printed). Requests go through the
// same engine, prompt, knowledge and provider as POST /api/assistant/chat, as
// a guest with no store: nothing is persisted and no allowance is spent; the
// run stops early when its own spend passes --max-usd.
//
// Results: a table on stdout and, with --out, the evidence markdown
// (docs/evidence/medi-eval-<date>.md in the workspace).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant/evals"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/db"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

func main() {
	surface := flag.String("surface", "", "only cases of this surface (guide|photo|studio|vector)")
	suite := flag.String("suite", "", "\"adjust\" (MM115), \"routing\" or \"options\" (MM116a), \"jobs\" (MM116b) run a named suite; default runs the single-turn suites")
	caseID := flag.String("case", "", "only this case id")
	repeat := flag.Int("repeat", 1, "run each case N times")
	estimateOnly := flag.Bool("estimate", false, "print the cost estimate and exit (no model calls)")
	confirm := flag.Bool("i-know-this-costs-money", false, "required to call the model")
	maxUSD := flag.Float64("max-usd", 2, "stop starting cases once this run has spent this much")
	envFile := flag.String("env-file", "", "load environment variables from this file first (e.g. the API service's EnvironmentFile)")
	out := flag.String("out", "", "write the evidence markdown here")
	record := flag.Bool("record", false, "write one assistant_eval_runs row per surface (needs DATABASE_URL)")
	gate := flag.Bool("gate", false, "compare each surface's pass rate with its latest recorded run; exit 1 if lower (needs DATABASE_URL)")
	gitSHA := flag.String("git-sha", "", "git sha stored with --record (default: git rev-parse --short HEAD when available)")
	flag.Parse()

	if *envFile != "" {
		if err := godotenv.Load(*envFile); err != nil {
			fail("cannot load --env-file: %v", err)
		}
	}
	if *surface != "" && !validSurface(*surface) {
		fail("--surface must be guide, photo, studio or vector")
	}

	cases, err := evals.LoadCases()
	if err != nil {
		fail("cases: %v", err)
	}
	if *suite != "" && *suite != "adjust" && *suite != "routing" && *suite != "options" && *suite != "jobs" {
		fail("--suite must be adjust, routing, options or jobs (or empty)")
	}
	if *suite != "" && (*record || *gate) {
		fail("--record and --gate compare per-surface pass rates of the single-turn suites; the named suites are reported in their evidence files")
	}
	cases = evals.SuiteCases(cases, *suite)
	cfg := config.Load().Assistant
	engine := &assistant.Engine{Cfg: cfg, Ops: assistant.NewOpsValidator(), Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	schemas := map[string]bool{}
	for _, s := range []string{"photo", "studio", "vector"} {
		schemas[s] = engine.Ops.Has(s)
	}
	run, skipped := evals.Filter(cases, *surface, *caseID, schemas)
	if len(run) == 0 && len(skipped) == 0 {
		fail("no cases match")
	}
	est := evals.EstimateCost(run, *repeat, evals.ModelsOf(engine))
	printEstimate(est, len(skipped))
	if *estimateOnly {
		return
	}
	if !*confirm {
		fail("refusing to call the model: add --i-know-this-costs-money after reading the estimate above")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		fail("ASSISTANT_OPENROUTER_API_KEY is not set (Medi's own key; OPENROUTER_API_KEY is never used)")
	}
	k, err := assistant.NewEmbeddedKnowledge()
	if err != nil {
		fail("knowledge: %v", err)
	}
	engine.Knowledge = k
	// MM116b: submit_job is offered. The estimator is the real read-only one over the tier registry
	// (no counters, no GPU probe): it prices a card, and nothing in the eval can start a job.
	engine.Jobs = &handlers.AssistantJobs{Registry: tiers.New(config.Load())}
	engine.Provider = assistant.NewOpenRouterProvider(cfg.BaseURL, cfg.APIKey, cfg.AttributionURL, cfg.ProviderOrder)
	// The eval's own budget: the shared daily cap is not consulted (no store),
	// --max-usd is; the OpenRouter key's credit limit stays the hard backstop.
	engine.Spend = assistant.NewSpendCap(0, nil, nil)
	runner := &evals.Runner{Engine: engine}

	ctx := context.Background()
	var runStore evals.RunStore
	if *record || *gate {
		cfgAll := config.Load()
		pool, err := db.New(ctx, cfgAll)
		if err != nil || pool == nil {
			fail("--record/--gate need a reachable DATABASE_URL: %v", err)
		}
		defer pool.Close()
		runStore = telemetry.NewStore(pool, cfgAll, slog.Default())
	}
	fmt.Println("prompt version", assistant.PromptVersion())
	results := append([]evals.Result(nil), skipped...)
	spent := 0.0
	for _, c := range run {
		for i := 0; i < *repeat; i++ {
			if spent >= *maxUSD {
				results = append(results, evals.Result{Case: c, Skipped: fmt.Sprintf("run budget $%.2f reached", *maxUSD)})
				continue
			}
			o, err := runner.Run(ctx, c)
			if err != nil {
				fail("%v", err)
			}
			r := evals.Grade(c, o)
			spent += o.CostUSD
			results = append(results, r)
			mark := "PASS"
			if !r.Pass {
				mark = "FAIL " + strings.Join(r.Failures, "; ")
			}
			fmt.Printf("%-40s %-6s $%.4f  %s\n", c.ID, c.Surface, o.CostUSD, mark)
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Case.ID < results[j].Case.ID })
	s := evals.Summarize(results)
	fmt.Println()
	for _, sf := range []string{"guide", "photo", "studio", "vector"} {
		if n := s.Pass[sf] + s.Fail[sf]; n > 0 || s.Skipped[sf] > 0 {
			rate := 0.0
			if n > 0 {
				rate = 100 * float64(s.Pass[sf]) / float64(n)
			}
			fmt.Printf("%-6s pass %d / %d (%.0f %%), skipped %d\n", sf, s.Pass[sf], n, rate, s.Skipped[sf])
		}
	}
	fmt.Printf("cost $%.4f\n", s.CostUSD)
	exit := 0
	if runStore != nil {
		rows := evals.Records(results, evals.RunMeta{PromptVersion: assistant.PromptVersion(), GitSHA: gitSHAOr(*gitSHA),
			ModelGuide: cfg.ModelGuide, ModelCopilot: cfg.ModelCopilot, EvidencePath: *out})
		if *gate {
			verdicts, err := evals.Gate(ctx, runStore, rows)
			if err != nil {
				fail("gate: %v", err)
			}
			for _, v := range verdicts {
				fmt.Println(v.Line())
			}
			if evals.Regressed(verdicts) {
				exit = 1
			}
		}
		if *record {
			if err := evals.Record(ctx, runStore, rows); err != nil {
				fail("%v", err)
			}
			fmt.Printf("recorded %d assistant_eval_runs row(s)\n", len(rows))
		}
	}
	if *out != "" {
		md := evals.Markdown(time.Now().UTC().Format("2006-01-02"), results, s, evals.ModelsOf(engine))
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			fail("write %s: %v", *out, err)
		}
		fmt.Println("wrote", *out)
	}
	if exit != 0 {
		fmt.Fprintln(os.Stderr, "assistant-eval: pass rate regressed against the latest recorded run")
		os.Exit(exit)
	}
}

// gitSHAOr is the flag, else the checkout's short sha, else "".
func gitSHAOr(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func printEstimate(e evals.Estimate, skipped int) {
	var parts []string
	for _, s := range []string{"guide", "photo", "studio", "vector"} {
		if e.PerSurface[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", s, e.PerSurface[s]))
		}
	}
	fmt.Printf("Medi eval estimate: %d case runs (%s), %d skipped (op schema not merged)\n", e.Cases, strings.Join(parts, ", "), skipped)
	fmt.Printf("  typical ≈ $%.2f (no cache hits), worst case ≤ $%.2f (every round at the output cap)\n\n", e.USD, e.WorstUSD)
}

func validSurface(s string) bool {
	for _, v := range evals.Surfaces {
		if v == s {
			return true
		}
	}
	return false
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "assistant-eval: "+f+"\n", a...)
	os.Exit(2)
}
