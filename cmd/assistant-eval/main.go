// Command assistant-eval runs Medi's checked-in eval cases against the real
// models and grades the answers with code (MM106, design §16).
//
// It costs real money, so it prints an estimate and refuses to call the model
// without --i-know-this-costs-money:
//
//	go run ./cmd/assistant-eval --estimate                       # print the estimate, no calls
//	go run ./cmd/assistant-eval --surface guide --i-know-this-costs-money
//	go run ./cmd/assistant-eval --case guide-gif-from-video --repeat 3 --i-know-this-costs-money
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
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant/evals"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func main() {
	surface := flag.String("surface", "", "only cases of this surface (guide|photo|studio|vector)")
	caseID := flag.String("case", "", "only this case id")
	repeat := flag.Int("repeat", 1, "run each case N times")
	estimateOnly := flag.Bool("estimate", false, "print the cost estimate and exit (no model calls)")
	confirm := flag.Bool("i-know-this-costs-money", false, "required to call the model")
	maxUSD := flag.Float64("max-usd", 2, "stop starting cases once this run has spent this much")
	envFile := flag.String("env-file", "", "load environment variables from this file first (e.g. the API service's EnvironmentFile)")
	out := flag.String("out", "", "write the evidence markdown here")
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
	engine.Provider = assistant.NewOpenRouterProvider(cfg.BaseURL, cfg.APIKey, cfg.AttributionURL, cfg.ProviderOrder)
	// The eval's own budget: the shared daily cap is not consulted (no store),
	// --max-usd is; the OpenRouter key's credit limit stays the hard backstop.
	engine.Spend = assistant.NewSpendCap(0, nil, nil)
	runner := &evals.Runner{Engine: engine}

	ctx := context.Background()
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
	if *out != "" {
		md := evals.Markdown(time.Now().UTC().Format("2006-01-02"), results, s, evals.ModelsOf(engine))
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			fail("write %s: %v", *out, err)
		}
		fmt.Println("wrote", *out)
	}
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
