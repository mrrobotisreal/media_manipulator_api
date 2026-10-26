package vector

import (
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// TestCheckExportMatrix is the design §3.4 matrix, cell by cell, against the
// published registry defaults: every countable format × every tier, with a
// request inside every cap. "A" = allow, "L" = locked. Pro (MM49) adds the
// three raster video formats on top of Premium; operator has everything.
func TestCheckExportMatrix(t *testing.T) {
	r := tiers.New(config.Load())
	ok := ExportRequest{DurationMs: 3000, Height: 480, Fps: 12}

	want := map[string]map[tiers.Tier]string{
		tiers.VectorFormatSVGAnimated:        {tiers.TierAnonymous: "A", tiers.TierFree: "A", tiers.TierPremium: "A", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatLottie:             {tiers.TierAnonymous: "L", tiers.TierFree: "A", tiers.TierPremium: "A", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatImportLottie:       {tiers.TierAnonymous: "L", tiers.TierFree: "A", tiers.TierPremium: "A", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatImportSVGVectorize: {tiers.TierAnonymous: "L", tiers.TierFree: "A", tiers.TierPremium: "A", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatReactTSX:           {tiers.TierAnonymous: "L", tiers.TierFree: "L", tiers.TierPremium: "A", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatJSTS:               {tiers.TierAnonymous: "L", tiers.TierFree: "L", tiers.TierPremium: "A", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatFlutter:            {tiers.TierAnonymous: "L", tiers.TierFree: "L", tiers.TierPremium: "A", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatGIF:                {tiers.TierAnonymous: "L", tiers.TierFree: "L", tiers.TierPremium: "A", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatPNGSequence:        {tiers.TierAnonymous: "L", tiers.TierFree: "L", tiers.TierPremium: "L", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatMP4:                {tiers.TierAnonymous: "L", tiers.TierFree: "L", tiers.TierPremium: "L", tiers.TierPro: "A", tiers.TierOperator: "A"},
		tiers.VectorFormatWebM:               {tiers.TierAnonymous: "L", tiers.TierFree: "L", tiers.TierPremium: "L", tiers.TierPro: "A", tiers.TierOperator: "A"},
	}
	if len(want) != len(tiers.AllVectorFormats()) {
		t.Fatalf("matrix covers %d formats, registry has %d", len(want), len(tiers.AllVectorFormats()))
	}
	all := append(tiers.All(), tiers.TierOperator)
	for _, format := range tiers.AllVectorFormats() {
		for _, tier := range all {
			req := ok
			req.Format = format
			got := CheckExport(r.For(tier), req)
			wantOutcome := map[string]string{"A": DecisionAllow, "L": DecisionLocked}[want[format][tier]]
			if wantOutcome == "" {
				t.Fatalf("no expectation for %s × %s", format, tier)
			}
			if got.Outcome != wantOutcome {
				t.Errorf("%s × %s = %+v, want %s", format, tier, got, wantOutcome)
			}
		}
	}
}

// The cap cells: anonymous 10 s duration, premium GIF 720p / 15 s, pro raster
// 2160p / 60 fps / 60 s (MM49), raster
// fps (no default cap, env-tunable), operator at the deployment ceilings.
func TestCheckExportCaps(t *testing.T) {
	r := tiers.New(config.Load())
	anon, free, premium, pro, op := r.For(tiers.TierAnonymous), r.For(tiers.TierFree), r.For(tiers.TierPremium), r.For(tiers.TierPro), r.For(tiers.TierOperator)
	fpsCapped := premium
	fpsCapped.VectorVideo.MaxFps = 30

	cases := []struct {
		name string
		l    tiers.Limits
		req  ExportRequest
		want Decision
	}{
		{"anonymous svg 10 000 ms is allowed", anon, ExportRequest{Format: "svg_animated", DurationMs: 10000}, Decision{Outcome: DecisionAllow}},
		{"anonymous svg 10 001 ms is capped", anon, ExportRequest{Format: "svg_animated", DurationMs: 10001}, Decision{Outcome: DecisionCap, Cap: CapDuration, Max: 10000}},
		{"anonymous mp4 is locked (lock before cap)", anon, ExportRequest{Format: "mp4", DurationMs: 99999, Height: 4320}, Decision{Outcome: DecisionLocked}},
		{"free svg has no duration cap", free, ExportRequest{Format: "svg_animated", DurationMs: 600000}, Decision{Outcome: DecisionAllow}},
		{"free lottie long is allowed", free, ExportRequest{Format: "lottie", DurationMs: 600000, Height: 4000}, Decision{Outcome: DecisionAllow}},
		{"free import ignores dimensions", free, ExportRequest{Format: "import_svg_vectorize", DurationMs: 999999, Height: 9999, Fps: 999}, Decision{Outcome: DecisionAllow}},
		{"premium gif 720p 15 s is allowed", premium, ExportRequest{Format: "gif", DurationMs: 15000, Height: 720, Fps: 60}, Decision{Outcome: DecisionAllow}},
		{"premium gif 1080p is capped on height", premium, ExportRequest{Format: "gif", DurationMs: 5000, Height: 1080, Fps: 24}, Decision{Outcome: DecisionCap, Cap: CapHeight, Max: 720}},
		{"premium gif 15 001 ms is capped on duration", premium, ExportRequest{Format: "gif", DurationMs: 15001, Height: 480}, Decision{Outcome: DecisionCap, Cap: CapDuration, Max: 15000}},
		{"premium gif fps cap when configured", fpsCapped, ExportRequest{Format: "gif", DurationMs: 5000, Height: 480, Fps: 31}, Decision{Outcome: DecisionCap, Cap: CapFps, Max: 30}},
		{"premium svg is not raster-capped", premium, ExportRequest{Format: "svg_animated", DurationMs: 600000, Height: 4320, Fps: 240}, Decision{Outcome: DecisionAllow}},
		{"premium react is not raster-capped", premium, ExportRequest{Format: "react_tsx", DurationMs: 600000, Height: 4320}, Decision{Outcome: DecisionAllow}},
		{"premium unknown height (0) passes", premium, ExportRequest{Format: "gif", DurationMs: 5000}, Decision{Outcome: DecisionAllow}},
		// Pro (MM49): 2160p / 60 fps / 60 s on every raster format.
		{"pro mp4 4K60 60 s is allowed", pro, ExportRequest{Format: "mp4", DurationMs: 60000, Height: 2160, Fps: 60}, Decision{Outcome: DecisionAllow}},
		{"pro webm 4K60 60 s is allowed", pro, ExportRequest{Format: "webm", DurationMs: 60000, Height: 2160, Fps: 60}, Decision{Outcome: DecisionAllow}},
		{"pro png sequence 4K60 60 s is allowed", pro, ExportRequest{Format: "png_sequence", DurationMs: 60000, Height: 2160, Fps: 60}, Decision{Outcome: DecisionAllow}},
		{"pro gif 4K 60 s is allowed (premium stops at 720p / 15 s)", pro, ExportRequest{Format: "gif", DurationMs: 60000, Height: 2160, Fps: 50}, Decision{Outcome: DecisionAllow}},
		{"pro mp4 61 s is capped on duration", pro, ExportRequest{Format: "mp4", DurationMs: 61000, Height: 1080, Fps: 30}, Decision{Outcome: DecisionCap, Cap: CapDuration, Max: 60000}},
		{"pro mp4 60 001 ms is capped on duration", pro, ExportRequest{Format: "mp4", DurationMs: 60001, Height: 1080, Fps: 30}, Decision{Outcome: DecisionCap, Cap: CapDuration, Max: 60000}},
		{"pro webm 2161 px is capped on height", pro, ExportRequest{Format: "webm", DurationMs: 5000, Height: 2161, Fps: 30}, Decision{Outcome: DecisionCap, Cap: CapHeight, Max: 2160}},
		{"pro png sequence 61 fps is capped on fps", pro, ExportRequest{Format: "png_sequence", DurationMs: 5000, Height: 1080, Fps: 61}, Decision{Outcome: DecisionCap, Cap: CapFps, Max: 60}},
		{"pro svg is not raster-capped", pro, ExportRequest{Format: "svg_animated", DurationMs: 600000, Height: 4320, Fps: 240}, Decision{Outcome: DecisionAllow}},
		{"operator mp4 2160p60 is allowed", op, ExportRequest{Format: "mp4", DurationMs: 60000, Height: 2160, Fps: 60}, Decision{Outcome: DecisionAllow}},
		{"operator above the deployment height is capped", op, ExportRequest{Format: "webm", DurationMs: 1000, Height: 4321}, Decision{Outcome: DecisionCap, Cap: CapHeight, Max: tiers.DeploymentMaxOutputHeight}},
		{"svg_static is not countable", op, ExportRequest{Format: "svg_static"}, Decision{Outcome: DecisionNotCountable}},
		{"unknown format is not countable", op, ExportRequest{Format: "pdf"}, Decision{Outcome: DecisionNotCountable}},
		{"empty format is not countable", anon, ExportRequest{}, Decision{Outcome: DecisionNotCountable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckExport(tc.l, tc.req); got != tc.want {
				t.Fatalf("CheckExport = %+v, want %+v", got, tc.want)
			}
		})
	}
}
