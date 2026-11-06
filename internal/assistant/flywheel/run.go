package flywheel

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

// Options are the parsed command-line flags.
type Options struct {
	Week            string
	EnvFile         string
	Out             string
	Label           bool
	Estimate        bool
	Confirm         bool
	MaxUSD          float64
	ExportRouting   string
	AnalyticsExport string
	Depth           int
	MaxClusters     int
}

// ParseFlags parses args (without the program name). Cross-flag rules are
// checked here so they are testable without a database.
func ParseFlags(args []string, errOut io.Writer) (Options, error) {
	var o Options
	fs := flag.NewFlagSet("assistant-flywheel", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&o.Week, "week", "", "ISO week to analyse, e.g. 2026-W46 (default: the last complete week)")
	fs.StringVar(&o.EnvFile, "env-file", "", "load environment variables from this file first (path only; never printed)")
	fs.StringVar(&o.Out, "out", "", "report path (default docs/evidence/medi-flywheel-<week>.md)")
	fs.BoolVar(&o.Label, "label", false, "name each cluster with the cheap title model (costs money; needs --i-know-this-costs-money)")
	fs.BoolVar(&o.Estimate, "estimate", false, "read the data, print the counts and the --label cost estimate, write nothing, call no model")
	fs.BoolVar(&o.Confirm, "i-know-this-costs-money", false, "required with --label")
	fs.Float64Var(&o.MaxUSD, "max-usd", 0.25, "stop labelling once this much has been spent")
	fs.StringVar(&o.ExportRouting, "export-routing", "", "write graded guide routing pairs to this local JSONL (never leaves the host)")
	fs.StringVar(&o.AnalyticsExport, "analytics-export", "", "optional JSONL of client-side signals (undo_within_60s, guide_no_click) keyed by turn_id or message_id")
	fs.IntVar(&o.Depth, "depth", 2, "retrieved doc ids per cluster key")
	fs.IntVar(&o.MaxClusters, "max-clusters", 15, "clusters written to the report")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if o.Label && !o.Estimate && !o.Confirm {
		return o, errors.New("refusing to call the model: --label costs money; run with --estimate first, then add --i-know-this-costs-money")
	}
	if o.Confirm && !o.Label {
		return o, errors.New("--i-know-this-costs-money only goes with --label")
	}
	if o.Depth < 1 || o.Depth > 5 {
		return o, errors.New("--depth must be 1..5")
	}
	if o.MaxClusters < 1 {
		return o, errors.New("--max-clusters must be >= 1")
	}
	if o.MaxUSD <= 0 {
		return o, errors.New("--max-usd must be > 0")
	}
	return o, nil
}

// Deps are the seams Run needs; tests fill them with fakes.
type Deps struct {
	Source     Source
	Retriever  assistant.Retriever
	Labeler    *Labeler // nil unless --label is allowed to run
	LabelModel string
	Analytics  *Analytics
	Now        func() time.Time
	Stdout     io.Writer
	// WriteFile writes a file (parents created). Default: os.WriteFile.
	WriteFile func(path string, data []byte) error
}

func defaultWrite(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o600)
}

// Run executes one weekly pass and returns the report.
func Run(ctx context.Context, o Options, d Deps) (*Report, error) {
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	out := d.Stdout
	if out == nil {
		out = io.Discard
	}
	write := d.WriteFile
	if write == nil {
		write = defaultWrite
	}
	w, err := ParseWeek(o.Week, now())
	if err != nil {
		return nil, err
	}
	rows, err := d.Source.Messages(ctx, w.From, w.To)
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	turns := BuildTurns(rows)
	flagged := Select(turns, d.Analytics, w.From, w.To)
	clusters := ClusterTurns(flagged, d.Retriever, o.Depth)
	if len(clusters) > o.MaxClusters {
		clusters = clusters[:o.MaxClusters]
	}
	inWeek := 0
	for _, t := range turns {
		if t.AssistantID != "" && !t.At.Before(w.From) && t.At.Before(w.To) {
			inWeek++
		}
	}
	rep := &Report{Week: w, Generated: now(), TurnsInWeek: inWeek, Flagged: len(flagged), Clusters: clusters}
	if d.Analytics == nil {
		rep.SignalGaps = []Signal{SigFastUndo, SigNoClick}
	}
	fmt.Fprintf(out, "week %s: %d turns, %d flagged, %d clusters\n", w.Label, inWeek, len(flagged), len(clusters))

	if o.Label {
		calls, usd := EstimateLabel(d.LabelModel, clusters)
		fmt.Fprintf(out, "label estimate: %d calls on %s, about $%.4f (cap $%.2f)\n", calls, d.LabelModel, usd, o.MaxUSD)
		if usd > o.MaxUSD {
			return rep, fmt.Errorf("label estimate $%.4f is over --max-usd $%.2f", usd, o.MaxUSD)
		}
	}
	if o.Estimate {
		fmt.Fprintln(out, "estimate only: nothing written, no model called")
		return rep, nil
	}
	if o.Label {
		if d.Labeler == nil {
			return rep, errors.New("--label needs a provider (ASSISTANT_OPENROUTER_API_KEY)")
		}
		spent, err := d.Labeler.LabelAll(ctx, clusters)
		rep.LabelUSD, rep.Labeled = spent, true
		if err != nil {
			return rep, fmt.Errorf("label: %w", err)
		}
		fmt.Fprintf(out, "labelled; cost $%.4f\n", spent)
	}
	path := o.Out
	if path == "" {
		path = "docs/evidence/medi-flywheel-" + w.Label + ".md"
	}
	if err := write(path, []byte(Render(*rep))); err != nil {
		return rep, fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(out, "report: %s\n", path)
	if o.ExportRouting != "" {
		pairs := RoutingPairs(turns, d.Analytics, w)
		var sb strings.Builder
		if err := WriteRoutingJSONL(&sb, pairs); err != nil {
			return rep, err
		}
		if err := write(o.ExportRouting, []byte(sb.String())); err != nil {
			return rep, fmt.Errorf("write routing export: %w", err)
		}
		fmt.Fprintf(out, "routing pairs: %d -> %s (local file; do not copy off the host)\n", len(pairs), o.ExportRouting)
	}
	return rep, nil
}
