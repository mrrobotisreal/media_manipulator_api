// Command burst-ctl is the operator CLI for GPU burst (MM124, design
// docs/design/GPU_BURST_EC2.md). It reads the same .env as the API.
//
//	burst-ctl status
//	burst-ctl runs --since 24h
//	burst-ctl reap [--yes]
//	burst-ctl launch-test --workload remove_background --i-know-this-costs-money
//	burst-ctl prices
//	burst-ctl manifest <family> [--json]   (MM126: the host runtime fingerprint)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/db"
	"github.com/mrrobotisreal/media_manipulator_api/internal/lab"
)

// env bundles what the commands need; tests build it from fakes.
type env struct {
	cfg      config.BurstConfig
	provider burst.Provider
	store    burst.Store
	prefixes burst.PrefixStore
	minter   burst.KeyMinter
	images   burst.ImageAPI // MM126: DescribeImages for the stale flag (nil = skip)
	host     burst.HostManifest
	now      func() time.Time
	out      io.Writer
	log      *slog.Logger
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if os.Args[1] == "manifest" {
		// MM126: a pure filesystem read. Never loads .env, never touches AWS or the DB.
		if err := (&env{out: os.Stdout}).manifest(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}
	_ = godotenv.Load(".env")
	_ = godotenv.Load(".env.local")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	full := config.Load()
	e := &env{cfg: full.Burst, now: time.Now, out: os.Stdout, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	if e.cfg.AWSConfigured() {
		client, err := burst.NewEC2Client(ctx, e.cfg.Region, e.cfg.AccessKey, e.cfg.SecretKey)
		if err != nil {
			fatal(err)
		}
		e.provider = burst.NewEC2Provider(client, e.log)
		e.images = client
	}
	if pool, err := db.New(ctx, full); err == nil && pool != nil {
		defer pool.Close()
		e.store = burst.NewPGStore(pool)
	}
	if e.cfg.TailscaleClientID != "" {
		e.minter = &burst.TailscaleMinter{ClientID: e.cfg.TailscaleClientID, ClientSecret: e.cfg.TailscaleClientSecret,
			Tailnet: e.cfg.TailscaleTailnet, Tag: e.cfg.TailscaleTag, Log: e.log}
	}
	if e.cfg.AWSConfigured() {
		// Staging deletes use the controller's own credentials (S3 on burst/* only).
		if awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(e.cfg.Region),
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(e.cfg.AccessKey, e.cfg.SecretKey, ""))); err == nil {
			e.prefixes = &burst.S3Staging{API: s3.NewFromConfig(awsCfg), Bucket: full.S3Bucket, Prefix: e.cfg.S3Prefix}
		}
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "status":
		err = e.status(ctx)
	case "runs":
		err = e.runs(ctx, args)
	case "reap":
		err = e.reap(ctx, args)
	case "launch-test":
		err = e.launchTest(ctx, args)
	case "prices":
		err = e.prices(ctx)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: burst-ctl <command>
  status                                   live instances, today's and this month's spend vs caps
  runs --since 24h                         recent burst runs
  reap [--yes]                             one reaper pass (dry run unless --yes)
  launch-test --workload <key|lab_<model>> --i-know-this-costs-money
  prices                                   AWS Pricing API vs BURST_PRICE_USD_PER_HOUR
  manifest <family> [--json|--paths]       host runtime fingerprint of a worker-image family (MM126)`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "burst-ctl:", err)
	os.Exit(1)
}

func usd(micros int64) string { return fmt.Sprintf("$%.2f", float64(micros)/1e6) }

func (e *env) ledger() *burst.Ledger {
	return burst.NewLedger(e.cfg.DailySpendCapUSD, e.cfg.MonthlySpendCapUSD, e.store, e.now)
}

func (e *env) status(ctx context.Context) error {
	fmt.Fprintln(e.out, e.cfg.StartupLine())
	var live []burst.Instance
	if e.provider != nil {
		var err error
		if live, err = e.provider.ListTagged(ctx); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(e.out, "(no BURST_AWS_* credentials: cannot list instances)")
	}
	fmt.Fprintf(e.out, "live instances: %d\n", len(live))
	tw := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)
	for _, in := range live {
		age := "?"
		if !in.LaunchTime.IsZero() {
			age = e.now().Sub(in.LaunchTime).Round(time.Second).String()
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\trun=%s\tage=%s\n", in.ID, in.Type, in.AZ, in.State, in.RunID, age)
	}
	tw.Flush()
	e.imageStatus(ctx)
	if e.store == nil {
		fmt.Fprintln(e.out, "(no database: spend unavailable)")
		return nil
	}
	sp := e.ledger().Spent(ctx)
	fmt.Fprintf(e.out, "today (UTC):      %s of %s", usd(sp.DayMicros), usd(sp.DayCapMicros))
	fmt.Fprintf(e.out, " (reserved %s)\n", usd(sp.DayReservedMicros))
	fmt.Fprintf(e.out, "this month (UTC): %s of %s\n", usd(sp.MonthMicros), usd(sp.MonthCapMicros))
	return nil
}

func (e *env) runs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	since := fs.Duration("since", 24*time.Hour, "look back this far")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if e.store == nil {
		return fmt.Errorf("no database")
	}
	rows, err := e.store.ListSince(ctx, e.now().Add(-*since))
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "OFFERED\tRUN\tWORKLOAD\tTYPE\tSTATE\tCOST\tERROR")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.OfferedAt.UTC().Format("01-02 15:04:05"), short(r.ID), r.Workload,
			r.InstanceType, r.State, usd(r.CostMicros), r.ErrorType)
	}
	return tw.Flush()
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (e *env) reap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "act (default is a dry run)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if e.provider == nil {
		return fmt.Errorf("no BURST_AWS_* credentials")
	}
	// burst-ctl owns no runs: every tagged instance is an orphan from its view,
	// so a fresh launch-test (its own process, grace window) is the only spare.
	rp := &burst.Reaper{Cfg: e.cfg, Provider: e.provider, Store: e.store, Prefixes: e.prefixes, Now: e.now, Log: e.log}
	rep := rp.Pass(ctx, false, !*yes)
	mode := "DRY RUN (use --yes to act)"
	if *yes {
		mode = "ACTING"
	}
	fmt.Fprintf(e.out, "reaper pass: %s\n", mode)
	ids := append([]string(nil), rep.Terminated...)
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(e.out, "  terminate %s (%s)\n", id, rep.Reasons[id])
	}
	fmt.Fprintf(e.out, "  rows orphaned: %d, staging prefixes deleted: %d\n", len(rep.RowsOrphaned), len(rep.PrefixesDeleted))
	for _, m := range rep.Errors {
		fmt.Fprintln(e.out, "  error:", m)
	}
	return nil
}

// launchTest launches one instance with a no-op user-data that shuts down in
// 5 minutes, waits for running, terminates, and prints the cost. This is the
// live smoke; it needs the explicit flag.
func (e *env) launchTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("launch-test", flag.ContinueOnError)
	workload := fs.String("workload", "", "profile key, e.g. remove_background")
	ack := fs.Bool("i-know-this-costs-money", false, "required")
	waitFor := fs.Duration("wait", 5*time.Minute, "how long to wait for running")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*ack {
		return fmt.Errorf("refusing: this launches a real instance; pass --i-know-this-costs-money")
	}
	// MM139: a Lab workload (a Lab model's tool name) is accepted too, so the first live launch can
	// be tested with the very instance and image a Lab run will use. It goes through LaunchDirect
	// with the lab flag, exactly like a Lab job; product workloads keep the trigger path.
	isLab := lab.IsLabTool(*workload)
	var prof burst.Profile
	var ok bool
	if isLab {
		prof, ok = burst.LookupAny(*workload)
	} else {
		prof, ok = burst.Lookup(*workload)
	}
	if !ok || prof.Never {
		return fmt.Errorf("unknown or non-burstable workload %q", *workload)
	}
	if e.provider == nil || e.store == nil {
		return fmt.Errorf("needs BURST_AWS_* credentials and the database")
	}
	if e.cfg.AMIs == nil {
		return fmt.Errorf("BURST_AMIS is empty")
	}
	ledger := e.ledger()
	pl := &burst.Planner{Cfg: e.cfg, Provider: e.provider, Store: e.store, Ledger: ledger, Minter: noKey{}, Prefixes: e.prefixes,
		Now: e.now, Log: e.log,
		UserData: func(burst.Run, burst.Profile, string) (string, error) {
			return "#!/bin/bash\nshutdown -h +5\n", nil // no-op: no worker, no Tailscale
		}}
	offer := burst.Offer{JobID: "launch-test", Kind: prof.Key, Tier: "operator", InstanceTypeOverride: firstOf(burst.CandidatesFor(prof, e.cfg.InstanceTypes)), Lab: isLab}
	var run *burst.Run
	var err error
	if isLab {
		run, err = pl.LaunchDirect(ctx, prof, offer)
	} else {
		run, err = pl.Launch(ctx, offer)
	}
	if err != nil {
		return err
	}
	run.Kind = burst.LaunchTestKind
	_ = e.store.Update(ctx, run)
	fmt.Fprintf(e.out, "launched %s (%s) run %s\n", run.InstanceID, run.InstanceType, run.ID)
	deadline := e.now().Add(*waitFor)
	for e.now().Before(deadline) {
		got, err := e.provider.Describe(ctx, []string{run.InstanceID})
		if err == nil && len(got) == 1 && got[0].State == "running" {
			fmt.Fprintln(e.out, "instance is running")
			break
		}
		time.Sleep(10 * time.Second)
	}
	for _, s := range []burst.State{burst.StateReady, burst.StateStagingIn, burst.StateRunning, burst.StateStagingOut} {
		_ = pl.Advance(ctx, run, s)
	}
	if err := pl.Terminate(ctx, run, burst.StateCompleted); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "terminated; cost %s (%d micros), state %s\n", usd(run.CostMicros), run.CostMicros, run.State)
	return nil
}

// noKey is the launch-test minter: the no-op user-data never joins Tailscale.
type noKey struct{}

func (noKey) Mint(context.Context) (string, error) { return "", nil }

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// prices queries the AWS Pricing API (us-east-1 endpoint) for the configured
// types and prints them next to the config.
func (e *env) prices(ctx context.Context) error {
	if !e.cfg.AWSConfigured() {
		return fmt.Errorf("no BURST_AWS_* credentials (the controller needs pricing:GetProducts)")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(e.cfg.AccessKey, e.cfg.SecretKey, "")))
	if err != nil {
		return err
	}
	return e.printPrices(ctx, pricing.NewFromConfig(awsCfg))
}

type pricingAPI interface {
	GetProducts(ctx context.Context, in *pricing.GetProductsInput, opts ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

// locationName is the Pricing API's name for a region.
var locationName = map[string]string{"us-west-2": "US West (Oregon)", "us-east-1": "US East (N. Virginia)", "us-east-2": "US East (Ohio)"}

func (e *env) printPrices(ctx context.Context, api pricingAPI) error {
	types := map[string]bool{}
	for t := range e.cfg.PriceUSDPerHour {
		types[t] = true
	}
	for _, p := range burst.Profiles() {
		for _, c := range burst.CandidatesFor(p, e.cfg.InstanceTypes) {
			types[c] = true
		}
	}
	names := make([]string, 0, len(types))
	for t := range types {
		names = append(names, t)
	}
	sort.Strings(names)
	loc := locationName[e.cfg.Region]
	tw := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tCONFIG $/h\tAWS $/h\tDIFF")
	for _, t := range names {
		aws1, err := onDemandPrice(ctx, api, t, loc)
		cfgP := e.cfg.PriceUSDPerHour[t]
		switch {
		case err != nil:
			fmt.Fprintf(tw, "%s\t%.4f\t(%v)\t\n", t, cfgP, err)
		default:
			fmt.Fprintf(tw, "%s\t%.4f\t%.4f\t%+.4f\n", t, cfgP, aws1, aws1-cfgP)
		}
	}
	return tw.Flush()
}

func onDemandPrice(ctx context.Context, api pricingAPI, instanceType, location string) (float64, error) {
	f := func(k, v string) pricingtypes.Filter {
		return pricingtypes.Filter{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String(k), Value: aws.String(v)}
	}
	out, err := api.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []pricingtypes.Filter{f("instanceType", instanceType), f("location", location), f("operatingSystem", "Linux"),
			f("tenancy", "Shared"), f("preInstalledSw", "NA"), f("capacitystatus", "Used")},
		MaxResults: aws.Int32(5),
	})
	if err != nil {
		return 0, err
	}
	for _, raw := range out.PriceList {
		if p, ok := parseOnDemandUSD(raw); ok {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no price found")
}

// parseOnDemandUSD reads the Linux on-demand USD/hour out of one Pricing API
// PriceList item (terms.OnDemand.<sku>.priceDimensions.<rate>.pricePerUnit.USD).
func parseOnDemandUSD(raw string) (float64, bool) {
	var doc struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					Unit         string            `json:"unit"`
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return 0, false
	}
	for _, term := range doc.Terms.OnDemand {
		for _, d := range term.PriceDimensions {
			if v, err := strconv.ParseFloat(d.PricePerUnit["USD"], 64); err == nil && v > 0 {
				return v, true
			}
		}
	}
	return 0, false
}
