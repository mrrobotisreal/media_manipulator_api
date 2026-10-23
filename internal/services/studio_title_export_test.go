package services

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Title-clip export goldens (part 16): layer-stack chain emission, font
// resolution, and escaping. The chain shape mirrors the SHARED TITLE LAYOUT
// SPEC in lib/studio/titleRaster.ts.

func sPtr(s string) *string   { return &s }
func fPtr(v float64) *float64 { return &v }
func iPtr(v int) *int         { return &v }

// realFontsDir is the committed font set, relative to this package directory.
const realFontsDir = "../../assets/fonts/studio"

func titleSeg(title *models.StudioTitle) StudioExportVideoSeg {
	return StudioExportVideoSeg{
		InputIndex: -1, SourceIn: 0, SourceOut: 5, TimelineStart: 2,
		Opacity: 1, TrackIndex: 1, Title: title,
	}
}

func multiLayerTitle() *models.StudioTitle {
	return &models.StudioTitle{Layers: []models.StudioTitleLayer{
		{
			Type: models.StudioTitleLayerRect,
			X:    fPtr(0.1), Y: fPtr(0.7), W: fPtr(0.8), H: fPtr(0.1),
			Color: sPtr("#102030"), Opacity: fPtr(0.9),
		},
		{
			Type:       models.StudioTitleLayerText,
			Text:       sPtr("Hello\nWorld"),
			FontFamily: sPtr("Inter"), FontSizePx: fPtr(64), FontWeight: iPtr(700),
			FillColor:   sPtr("#FFFFFF"),
			StrokeColor: sPtr("#000000"), StrokeWidthPx: fPtr(3),
			Shadow:     &models.StudioTitleShadow{XPx: 2, YPx: 4, BlurPx: 8, Color: "#111111"},
			Background: &models.StudioTitleBackground{Color: "#222222", Opacity: 0.5, PaddingXPx: 16, PaddingYPx: 8, RadiusPx: 12},
			LineHeight: fPtr(1.25),
			Align:      sPtr("center"),
			X:          fPtr(0.5), Y: fPtr(0.5),
		},
		{
			Type: models.StudioTitleLayerLine,
			X1:   fPtr(0.1), Y1: fPtr(0.65), X2: fPtr(0.9), Y2: fPtr(0.65),
			Color: sPtr("#FF0000"), ThicknessPx: fPtr(4),
		},
	}}
}

// The multi-layer title EDL golden: a transparent color source carries the
// stack (rect box → per-line drawtext → line box), then the standard clip
// chain and overlay composite.
func TestBuildExportArgs_TitleClipChain(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: inputs("/a.mp4"),
		Video: []StudioExportVideoSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 10, TimelineStart: 0, Opacity: 1, TrackIndex: 0},
			titleSeg(multiLayerTitle()),
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 10,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/fallback.ttf", realFontsDir, "/out.mp4"))

	for _, want := range []string{
		// Transparent 5s source, alpha-capable, shifted onto the timeline.
		"color=c=black@0:s=1920x1080:r=30:d=5.000,format=yuva420p",
		"setpts=PTS-STARTPTS+2.000/TB",
		// Rect layer: exact pixels, replace=1 writes color+alpha over the
		// transparent base, radius dropped.
		"drawbox=x=192:y=756:w=1536:h=108:color=0x102030@0.900:t=fill:replace=1",
		// Text layer: one drawtext per line, bold Inter from the committed set.
		"fontfile='" + realFontsDir + "/inter-700.ttf'",
		"text='Hello'",
		"text='World'",
		"fontsize=64",
		"fontcolor=0xFFFFFF",
		"borderw=3:bordercolor=0x000000",
		"shadowx=2:shadowy=4:shadowcolor=0x111111",
		"box=1:boxcolor=0x222222@0.500:boxborderw=12",
		// Line layer: horizontal → one exact thin drawbox (y = 0.65·1080 ± 2).
		"drawbox=x=192:y=700:w=1536:h=4:color=0xFF0000@1.000:t=fill:replace=1",
	} {
		if !strings.Contains(fc, want) {
			t.Fatalf("missing %q in filter graph:\n%s", want, fc)
		}
	}

	// Layout math: anchor (960, 540), lineH = 1.25·64 = 80, blockH = 160 →
	// line centers at 500 and 580; centered x = 960 - text_w/2.
	for _, want := range []string{
		"x=960.000-text_w/2:y=500.000-text_h/2",
		"x=960.000-text_w/2:y=580.000-text_h/2",
	} {
		if !strings.Contains(fc, want) {
			t.Fatalf("missing line placement %q in graph:\n%s", want, fc)
		}
	}

	// The title seg consumes no input: nothing may reference a [-1:v] pad.
	if strings.Contains(fc, "[-1:v]") {
		t.Fatalf("title seg leaked an input reference:\n%s", fc)
	}
	// It still composites via the standard overlay step with its enable window.
	if !strings.Contains(fc, "enable='between(t,2.000,7.000)'") {
		t.Fatalf("missing title overlay enable window:\n%s", fc)
	}
}

func TestTitleTextAlignmentExpressions(t *testing.T) {
	mk := func(align string) *models.StudioTitle {
		return &models.StudioTitle{Layers: []models.StudioTitleLayer{{
			Type: models.StudioTitleLayerText,
			Text: sPtr("Hi"), FontFamily: sPtr("Inter"), FontSizePx: fPtr(50), FontWeight: iPtr(400),
			FillColor: sPtr("#FFFFFF"), Align: sPtr(align), X: fPtr(0.25), Y: fPtr(0.5),
		}}}
	}
	cases := map[string]string{
		"left":   "x=480.000:",
		"center": "x=480.000-text_w/2:",
		"right":  "x=480.000-text_w:",
	}
	for align, want := range cases {
		parts := titleLayerParts(mk(align), 1920, 1080, realFontsDir, "/fallback.ttf")
		if len(parts) != 1 {
			t.Fatalf("%s: expected 1 drawtext, got %v", align, parts)
		}
		if !strings.Contains(parts[0], want) {
			t.Fatalf("%s: missing %q in %q", align, want, parts[0])
		}
	}
}

func TestTitleDiagonalLineStaircase(t *testing.T) {
	title := &models.StudioTitle{Layers: []models.StudioTitleLayer{{
		Type: models.StudioTitleLayerLine,
		X1:   fPtr(0.1), Y1: fPtr(0.1), X2: fPtr(0.9), Y2: fPtr(0.9),
		Color: sPtr("#FFFFFF"), ThicknessPx: fPtr(2),
	}}}
	parts := titleLayerParts(title, 1920, 1080, realFontsDir, "/fallback.ttf")
	if len(parts) != titleStaircaseSegments {
		t.Fatalf("expected %d staircase segments, got %d", titleStaircaseSegments, len(parts))
	}
	for _, p := range parts {
		if !strings.HasPrefix(p, "drawbox=") || !strings.Contains(p, "replace=1") {
			t.Fatalf("unexpected staircase part %q", p)
		}
	}
}

func TestResolveTitleFontFile(t *testing.T) {
	// Known family + weight → the committed file.
	if got := resolveTitleFontFile(realFontsDir, "Playfair Display", 700, "/fb.ttf"); !strings.HasSuffix(got, "playfair-display-700.ttf") {
		t.Fatalf("bold resolution: got %q", got)
	}
	// 400-only family asked bold → its regular file (no synthetic bold).
	if got := resolveTitleFontFile(realFontsDir, "Bebas Neue", 700, "/fb.ttf"); !strings.HasSuffix(got, "bebas-neue-400.ttf") {
		t.Fatalf("400-only bold fallback: got %q", got)
	}
	// Unknown family → the default family's file (defense in depth).
	if got := resolveTitleFontFile(realFontsDir, "Comic Sans MS", 400, "/fb.ttf"); !strings.HasSuffix(got, "inter-400.ttf") {
		t.Fatalf("unknown family: got %q", got)
	}
	// Missing directory → the DejaVu fallback file, never a guessed path.
	if got := resolveTitleFontFile("/nonexistent-dir", "Inter", 400, "/fb.ttf"); got != "/fb.ttf" {
		t.Fatalf("missing dir: got %q", got)
	}
}

// Registry sanity: every family has a regular file on disk in the committed
// set, and bold entries exist on disk too — the lockstep invariant's Go half.
func TestStudioTitleFontFilesExist(t *testing.T) {
	for family := range models.StudioTitleFonts {
		if got := resolveTitleFontFile(realFontsDir, family, 400, ""); got == "" {
			t.Fatalf("family %q regular file missing from %s", family, realFontsDir)
		}
		if models.StudioTitleFontSupportsBold(family) {
			if got := resolveTitleFontFile(realFontsDir, family, 700, ""); got == "" || strings.HasSuffix(got, "-400.ttf") {
				t.Fatalf("family %q bold file missing from %s (got %q)", family, realFontsDir, got)
			}
		}
	}
	if !models.IsKnownStudioTitleFont(models.StudioTitleFontDefault) {
		t.Fatalf("default family %q not in registry", models.StudioTitleFontDefault)
	}
}

func TestTitleTextEscaping(t *testing.T) {
	title := &models.StudioTitle{Layers: []models.StudioTitleLayer{{
		Type: models.StudioTitleLayerText,
		Text: sPtr(`100% 'sure', right: yes`), FontFamily: sPtr("Inter"), FontSizePx: fPtr(40),
		FontWeight: iPtr(400), FillColor: sPtr("#FFFFFF"), Align: sPtr("left"), X: fPtr(0.1), Y: fPtr(0.1),
	}}}
	parts := titleLayerParts(title, 1920, 1080, realFontsDir, "/fb.ttf")
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %v", parts)
	}
	if !strings.Contains(parts[0], `text='100\% \'sure\', right: yes'`) {
		t.Fatalf("escaping wrong: %q", parts[0])
	}
}

// Plan seam: a title clip flattens to a video ref/seg with no input, no audio.
func TestCollectAndAssemble_TitleClip(t *testing.T) {
	project := &models.StudioProject{
		FPS: 30, Width: 1920, Height: 1080,
		Tracks: []models.StudioTrack{
			{ID: "v1", Kind: models.StudioTrackKindVideo, Index: 0, Clips: []models.StudioClip{
				{ID: "c1", AssetID: "a1", TimelineStart: 0, SourceIn: 0, SourceOut: 10},
			}},
			{ID: "v2", Kind: models.StudioTrackKindVideo, Index: 1, Clips: []models.StudioClip{
				{ID: "t1", AssetID: "", TimelineStart: 2, SourceIn: 0, SourceOut: 7, Title: multiLayerTitle()},
			}},
		},
	}
	assets := map[string]*models.StudioAsset{
		"a1": {ID: "a1", S3KeyOriginal: "a1.mp4", MediaKind: models.StudioMediaKindVideo, HasAudio: true},
	}
	refs, duration, ok := CollectExportRefs(project, assets)
	if !ok {
		t.Fatal("expected refs")
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if duration != 10 {
		t.Fatalf("duration: got %v", duration)
	}
	plan := AssembleExportPlan(refs, StudioPlanSpec{
		Width: 1920, Height: 1080, FPS: 30, Duration: duration,
		LocalByKey: map[string]string{"a1.mp4": "/local/a1.mp4"},
	})
	if len(plan.Inputs) != 1 {
		t.Fatalf("title clip must consume no input; inputs=%v", plan.Inputs)
	}
	if len(plan.Video) != 2 {
		t.Fatalf("expected 2 video segs, got %d", len(plan.Video))
	}
	var found bool
	for _, seg := range plan.Video {
		if seg.Title != nil {
			found = true
			if seg.InputIndex != -1 {
				t.Fatalf("title seg InputIndex: got %d", seg.InputIndex)
			}
		}
	}
	if !found {
		t.Fatal("no title seg in plan")
	}
	if len(plan.Audio) != 1 {
		t.Fatalf("title clip must not contribute audio; audio segs=%d", len(plan.Audio))
	}

	// A title-only project renders too (video from the color source alone).
	titleOnly := &models.StudioProject{
		FPS: 30, Width: 1280, Height: 720,
		Tracks: []models.StudioTrack{
			{ID: "v1", Kind: models.StudioTrackKindVideo, Index: 0, Clips: []models.StudioClip{
				{ID: "t1", AssetID: "", TimelineStart: 0, SourceIn: 0, SourceOut: 5, Title: multiLayerTitle()},
			}},
		},
	}
	refs2, _, ok2 := CollectExportRefs(titleOnly, map[string]*models.StudioAsset{})
	if !ok2 || len(refs2) != 1 {
		t.Fatalf("title-only project: ok=%v refs=%d", ok2, len(refs2))
	}
}
