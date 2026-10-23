package services

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func imageSeg(input int, in, out, ts float64, track int) StudioExportVideoSeg {
	return StudioExportVideoSeg{
		InputIndex: input, SourceIn: in, SourceOut: out, TimelineStart: ts,
		Opacity: 1, TrackIndex: track, SourceKind: StudioSegKindImage,
	}
}

func TestBuildExportArgs_ImageClipChain(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []ExportInput{{Path: "/still.png", Pre: imageInputPre(30, 7200)}},
		Video:  []StudioExportVideoSeg{imageSeg(0, 0, 7200, 2, 0)},
		Width:  1920, Height: 1080, FPS: 30, Duration: 7202,
	}
	args := buildMultiTrackExportArgs(plan, "libx264", "", "", "/fontsdir", "/out.mp4")
	t.Logf("args: %q", args)
	wantPrefix := []string{"-y", "-loop", "1", "-framerate", "30", "-t", "7200.033", "-i", "/still.png", "-filter_complex"}
	if len(args) < len(wantPrefix) {
		t.Fatalf("args too short: %q", args)
	}
	for i, w := range wantPrefix {
		if args[i] != w {
			t.Fatalf("args[%d]=%q want %q (all: %q)", i, args[i], w, args)
		}
	}
	fc := filterComplex(t, args)
	want := "[0:v]format=rgba,setpts=PTS-STARTPTS+2.000/TB,scale=1920:1080:force_original_aspect_ratio=decrease:in_color_matrix=auto:out_color_matrix=bt709,setsar=1,fps=30,format=yuva420p[vc0]"
	if !strings.Contains(fc, want) {
		t.Errorf("image chain mismatch:\n got: %s\nwant: %s", fc, want)
	}
	if strings.Contains(fc, "trim=") {
		t.Errorf("image chain must not trim: %s", fc)
	}
}

func TestBuildExportArgs_ImageOverVideoDissolve(t *testing.T) {
	img := imageSeg(1, 0, 4, 3, 1)
	img.FadeIn = 1
	plan := StudioExportPlan{
		Inputs: []ExportInput{{Path: "/a.mp4"}, {Path: "/still.png", Pre: imageInputPre(30, 4)}},
		Video: []StudioExportVideoSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1, TrackIndex: 0},
			img,
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 7,
	}
	args := buildMultiTrackExportArgs(plan, "libx264", "", "", "/fontsdir", "/out.mp4")
	fc := filterComplex(t, args)
	for _, want := range []string{
		"[0:v]trim=start=0.000:end=5.000,setpts=PTS-STARTPTS+0.000/TB,scale=1920:1080:force_original_aspect_ratio=decrease,setsar=1",
		"[1:v]format=rgba,setpts=PTS-STARTPTS+3.000/TB,scale=1920:1080:force_original_aspect_ratio=decrease:in_color_matrix=auto:out_color_matrix=bt709",
		"fade=t=in:st=3.000:d=1.000:alpha=1",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("missing %q in %s", want, fc)
		}
	}
	// Only the image input carries pre-options.
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-i /a.mp4 -loop 1 -framerate 30 -t 4.033 -i /still.png") {
		t.Errorf("input ordering/options wrong: %s", joined)
	}
}

func TestBuildExportArgs_ImageWithLumetriAndLUT(t *testing.T) {
	seg := imageSeg(0, 0, 5, 0, 0)
	lutID := "lut1"
	seg.Effects = []models.StudioEffect{
		{ID: "l1", Type: models.StudioEffectLumetri, Enabled: true,
			Exposure: fp(1), Contrast: fp(1.2), Saturation: fp(1), Temperature: fp(0), Tint: fp(0), Vibrance: fp(0)},
		{ID: "l2", Type: models.StudioEffectLUT, Enabled: true, LutAssetID: &lutID},
	}
	seg.LutPaths = map[string]string{lutID: "/grade.cube"}
	plan := StudioExportPlan{
		Inputs: []ExportInput{{Path: "/still.png", Pre: imageInputPre(30, 5)}},
		Video:  []StudioExportVideoSeg{seg}, Width: 1920, Height: 1080, FPS: 30, Duration: 5,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "", "", "/fontsdir", "/out.mp4"))
	if !strings.HasPrefix(strings.SplitN(fc, ";", 2)[1], "[0:v]format=rgba,setpts=") {
		t.Errorf("image chain head wrong: %s", fc)
	}
	for _, want := range []string{"colorchannelmixer=rr=2.000", "lut3d=", "/grade.cube"} {
		if !strings.Contains(fc, want) {
			t.Errorf("missing %q in %s", want, fc)
		}
	}
}

func TestAssembleExportPlan_SharedImageAssetTwoInputs(t *testing.T) {
	asset := &models.StudioAsset{ID: "img", S3KeyOriginal: "k/img.jpg", MediaKind: models.StudioMediaKindImage}
	mk := func(id string, ts, hold float64) StudioClipRef {
		return StudioClipRef{
			Clip:  models.StudioClip{ID: id, AssetID: "img", TimelineStart: ts, SourceIn: 0, SourceOut: hold},
			Asset: asset, TrackKind: models.StudioTrackKindVideo,
		}
	}
	plan := AssembleExportPlan([]StudioClipRef{mk("a", 0, 5), mk("b", 5, 7200)}, StudioPlanSpec{
		Width: 1920, Height: 1080, FPS: 30, Duration: 7205,
		LocalByKey: map[string]string{"k/img.jpg": "/local/img.jpg"},
	})
	if len(plan.Inputs) != 2 {
		t.Fatalf("want 2 inputs, got %d", len(plan.Inputs))
	}
	if plan.Inputs[0].Path != "/local/img.jpg" || plan.Inputs[1].Path != "/local/img.jpg" {
		t.Errorf("paths: %+v", plan.Inputs)
	}
	if got := plan.Inputs[0].Pre; strings.Join(got, " ") != "-loop 1 -framerate 30 -t 5.033" {
		t.Errorf("pre0=%v", got)
	}
	if got := plan.Inputs[1].Pre; strings.Join(got, " ") != "-loop 1 -framerate 30 -t 7200.033" {
		t.Errorf("pre1=%v", got)
	}
	for i, s := range plan.Video {
		if s.SourceKind != StudioSegKindImage || s.InputIndex != i {
			t.Errorf("seg %d: %+v", i, s)
		}
	}
	if len(plan.Audio) != 0 {
		t.Errorf("image must not contribute audio")
	}
}

// Image clips ignore speed/reverse/freeze at the compiler (they are never
// read), and a video-only plan emits no looped inputs.
func TestBuildExportArgs_VideoOnlyHasNoLoop(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: inputs("/a.mp4"),
		Video:  []StudioExportVideoSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 4, Opacity: 1}},
		Width:  1920, Height: 1080, FPS: 30, Duration: 4,
	}
	args := strings.Join(buildMultiTrackExportArgs(plan, "libx264", "", "", "/fontsdir", "/out.mp4"), " ")
	for _, bad := range []string{"-loop", "-framerate", "in_color_matrix"} {
		if strings.Contains(args, bad) {
			t.Errorf("video-only plan leaked %q: %s", bad, args)
		}
	}
}

func TestStudioClipSpeedReverseFreezeNeverReadByCompiler(t *testing.T) {
	// Guard: the compiler/assembler must not branch on speed/reverse/freeze
	// until part 17 lands, so an image clip carrying them compiles identically.
	asset := &models.StudioAsset{ID: "img", S3KeyOriginal: "k/img.jpg", MediaKind: models.StudioMediaKindImage}
	base := models.StudioClip{ID: "a", AssetID: "img", SourceOut: 5}
	dirty := base
	sp := 2.0
	dirty.Speed = &sp
	dirty.Reverse = true
	dirty.Freeze = &models.StudioFreeze{SourceTime: 1}
	spec := StudioPlanSpec{Width: 1920, Height: 1080, FPS: 30, Duration: 5, LocalByKey: map[string]string{"k/img.jpg": "/i.jpg"}}
	mk := func(c models.StudioClip) []string {
		p := AssembleExportPlan([]StudioClipRef{{Clip: c, Asset: asset, TrackKind: models.StudioTrackKindVideo}}, spec)
		return buildMultiTrackExportArgs(p, "libx264", "", "", "/fontsdir", "/out.mp4")
	}
	if a, b := strings.Join(mk(base), "\x00"), strings.Join(mk(dirty), "\x00"); a != b {
		t.Errorf("speed/reverse/freeze changed the image compile:\n%s\n%s", a, b)
	}
}
