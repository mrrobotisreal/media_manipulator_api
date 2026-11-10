package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"text/tabwriter"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/manifest"
)

// imageStatus prints one line per configured worker image with its stale flag
// (MM126): the AMI's runtime-manifest tag against the host runtime computed now.
func (e *env) imageStatus(ctx context.Context) {
	if len(e.cfg.AMIs) == 0 {
		fmt.Fprintln(e.out, "images: none configured (BURST_AMIS is empty)")
		return
	}
	host := e.host
	if host == nil {
		host = func(f string) (manifest.Manifest, error) { return manifest.Compute(f) }
	}
	dc := &burst.DriftChecker{API: e.images, AMIs: e.cfg.AMIs, Host: host, Log: e.log, Now: e.now}
	sts := dc.Check(ctx)
	if e.images == nil {
		fmt.Fprintln(e.out, "images: cannot check (no BURST_AWS_* credentials)")
		return
	}
	fmt.Fprintf(e.out, "images: %d\n", len(sts))
	tw := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)
	for _, s := range sts {
		note := s.Reason
		if note == "" {
			note = "ok"
		}
		fmt.Fprintf(tw, "  %s\t%s\tstale: %v\tbuilt=%s\t%s\n", s.Family, s.AMI, s.Stale, orDash(s.Built), note)
	}
	tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// manifest prints the host fingerprint of a family. With --json the full
// manifest (what export-runtime.sh stores beside the tarball); with --paths one
// "src<TAB>dest" line per entry (what the script tars); default is the sha256.
func (e *env) manifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the full manifest as JSON")
	paths := fs.Bool("paths", false, "print the family's paths as src<TAB>dest lines (no filesystem access)")
	excl := fs.Bool("excludes", false, "print the exclude patterns")
	atDest := fs.Bool("from-dest", false, "read files at their image location (builder verification after unpacking)")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	if *excl {
		for _, p := range manifest.Excludes() {
			fmt.Fprintln(e.out, p)
		}
		return nil
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: burst-ctl manifest <family> [--json|--paths]")
	}
	fam := fs.Arg(0)
	if *paths {
		es, err := manifest.Entries(fam)
		if err != nil {
			return err
		}
		for _, p := range es {
			fmt.Fprintf(e.out, "%s\t%s\n", p.Src, p.Dest)
		}
		return nil
	}
	compute := manifest.Compute
	if *atDest {
		compute = manifest.ComputeAtDest
	}
	m, err := compute(fam)
	if err != nil {
		return err
	}
	if miss := m.MissingPaths(); len(miss) > 0 {
		return fmt.Errorf("host paths missing for %s: %v", fam, miss)
	}
	if *asJSON {
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		return enc.Encode(m)
	}
	fmt.Fprintln(e.out, m.SHA256)
	return nil
}

// reorderFlags moves flags before positionals so `manifest restore --json` works.
func reorderFlags(args []string) []string {
	var flags, pos []string
	for _, a := range args {
		if len(a) > 0 && a[0] == '-' {
			flags = append(flags, a)
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}
