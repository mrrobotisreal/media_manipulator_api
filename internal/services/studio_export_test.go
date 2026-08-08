package services

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// argIndex returns the index of want in args, or -1.
func argIndex(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func filterComplex(t *testing.T, args []string) string {
	t.Helper()
	idx := argIndex(args, "-filter_complex")
	if idx == -1 || idx+1 >= len(args) {
		t.Fatalf("no -filter_complex in args: %v", args)
	}
	return args[idx+1]
}

func TestBuildMultiTrackExportArgs_OverlayAndMix(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4", "/b.mp4", "/music.mp3"},
		Video: []StudioExportVideoSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1, TrackIndex: 0},
			{InputIndex: 1, SourceIn: 1, SourceOut: 3, TimelineStart: 2, Opacity: 0.5, TrackIndex: 1},
		},
		Audio: []StudioExportAudioSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Volume: 1},
			{InputIndex: 2, SourceIn: 0, SourceOut: 10, TimelineStart: 0, Volume: 0.3},
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 10,
	}
	args := buildMultiTrackExportArgs(plan, "h264_nvenc", "high", "/font.ttf", "/fontsdir", "/out.mp4")

	// One -i per input.
	inputs := 0
	for _, a := range args {
		if a == "-i" {
			inputs++
		}
	}
	if inputs != 3 {
		t.Fatalf("expected 3 inputs, got %d: %v", inputs, args)
	}

	fc := filterComplex(t, args)
	for _, want := range []string{
		"color=c=black:s=1920x1080:r=30:d=10.000,format=yuv420p[vbase]",
		"[0:v]trim=start=0.000:end=5.000",
		"setpts=PTS-STARTPTS+0.000/TB",
		"setpts=PTS-STARTPTS+2.000/TB",       // second clip offset to its timeline start
		"colorchannelmixer=aa=0.500",          // opacity on the top clip
		"overlay=x=(W-w)/2:y=(H-h)/2:enable='between(t,2.000,4.000)':eof_action=pass[vout]",
		"volume=0.300",                        // music gain
		"amix=inputs=2:normalize=0:dropout_transition=0[aout]",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("filter_complex missing %q\ngraph: %s", want, fc)
		}
	}

	if argIndex(args, "[vout]") == -1 || argIndex(args, "[aout]") == -1 {
		t.Errorf("expected -map [vout] and -map [aout], args=%v", args)
	}
	if tIdx := argIndex(args, "-t"); tIdx == -1 || args[tIdx+1] != "10.000" {
		t.Errorf("expected -t 10.000, args=%v", args)
	}
	if args[len(args)-1] != "/out.mp4" {
		t.Errorf("output must be last, got %q", args[len(args)-1])
	}
}

func TestBuildMultiTrackExportArgs_SingleVideoClipNoExtraAudio(t *testing.T) {
	// One video clip with its embedded audio: single overlay, single audio
	// source (no amix), both mapped.
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4"},
		Video:  []StudioExportVideoSeg{{InputIndex: 0, SourceIn: 2, SourceOut: 8, TimelineStart: 0, Opacity: 1, TrackIndex: 0}},
		Audio:  []StudioExportAudioSeg{{InputIndex: 0, SourceIn: 2, SourceOut: 8, TimelineStart: 0, Volume: 1}},
		Width:  1280, Height: 720, FPS: 24, Duration: 6,
	}
	args := buildMultiTrackExportArgs(plan, "libx264", "medium", "/font.ttf", "/fontsdir", "/out.mp4")
	fc := filterComplex(t, args)

	if strings.Contains(fc, "amix") {
		t.Errorf("single audio source should not use amix: %s", fc)
	}
	if !strings.Contains(fc, "volume=1.000,adelay=0|0[aout]") {
		t.Errorf("single audio should be labeled [aout]: %s", fc)
	}
	if !strings.Contains(fc, "eof_action=pass[vout]") {
		t.Errorf("single video should produce [vout]: %s", fc)
	}
	if cv := argIndex(args, "-c:v"); cv == -1 || args[cv+1] != "libx264" {
		t.Errorf("expected libx264, args=%v", args)
	}
}

func TestBuildMultiTrackExportArgs_Effects(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4", "/b.mp4"},
		Video: []StudioExportVideoSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 6, TimelineStart: 0, Opacity: 1, TrackIndex: 0,
				Adjustments:  &models.StudioAdjustments{Brightness: 0.1, Contrast: 1.2, Saturation: 0.8},
				TextOverlays: []models.StudioTextOverlay{{Text: "Reykjavík", X: 0.05, Y: 0.9, FontSize: 48, Color: "#FFCC00"}}},
			{InputIndex: 1, SourceIn: 0, SourceOut: 6, TimelineStart: 4, Opacity: 1, TrackIndex: 0, FadeIn: 1.5},
		},
		Audio: []StudioExportAudioSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 6, TimelineStart: 0, Volume: 1, FadeOut: 1.5},
			{InputIndex: 1, SourceIn: 0, SourceOut: 6, TimelineStart: 4, Volume: 1, FadeIn: 1.5},
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 10,
	}
	args := buildMultiTrackExportArgs(plan, "h264_nvenc", "high", "/usr/share/fonts/x.ttf", "/fontsdir", "/out.mp4")
	fc := filterComplex(t, args)

	for _, want := range []string{
		"eq=brightness=0.100:contrast=1.200:saturation=0.800",
		"drawtext=fontfile='/usr/share/fonts/x.ttf':text='Reykjavík'",
		"fontcolor=0xFFCC00",
		"fade=t=in:st=4.000:d=1.500:alpha=1", // dissolve-in on the second clip
		"afade=t=out:st=4.500:d=1.500",        // outgoing audio fades over the overlap (6 - 1.5)
		"afade=t=in:st=0:d=1.500",             // incoming audio fades in
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("filter_complex missing %q\ngraph: %s", want, fc)
		}
	}
}

// TestBuildMultiTrackExportArgs_LegacyV1Regression locks the EDL v1 graph: a
// plan carrying only v1 fields must emit NONE of the v2 filters. Together with
// models.SanitizeTracks being a no-op on valid v1 clips (TestSanitizeTracks_V1NoOp),
// this guarantees existing projects export byte-identically after the v2 work.
func TestBuildMultiTrackExportArgs_LegacyV1Regression(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4", "/music.mp3"},
		Video: []StudioExportVideoSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 6, TimelineStart: 0, Opacity: 1, TrackIndex: 0,
				Adjustments: &models.StudioAdjustments{Brightness: 0.1, Contrast: 1.2, Saturation: 0.8}},
		},
		Audio: []StudioExportAudioSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 6, TimelineStart: 0, Volume: 1},
			{InputIndex: 1, SourceIn: 0, SourceOut: 6, TimelineStart: 0, Volume: 0.5},
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 6,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/font.ttf", "/fontsdir", "/out.mp4"))

	// Classic v1 chain still present.
	if !strings.Contains(fc, "eq=brightness=0.100:contrast=1.200:saturation=0.800") {
		t.Errorf("v1 eq adjustments missing: %s", fc)
	}
	// None of the v2 emitters may appear for a v1-only plan.
	for _, forbidden := range []string{
		"crop=", "lut3d", "chromakey", "despill", "colorbalance", "colorchannelmixer=rr",
		"vibrance", "stereotools", "sidechaincompress", "loudnorm", "ass=", "subtitles=", "rotate=", "blend=all_mode",
		"alphaextract", "alphamerge",
	} {
		if strings.Contains(fc, forbidden) {
			t.Errorf("v1-only plan unexpectedly emitted v2 filter %q\ngraph: %s", forbidden, fc)
		}
	}
}

func sp(s string) *string   { return &s }
func fp(v float64) *float64 { return &v }

func wantAll(t *testing.T, fc string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(fc, w) {
			t.Errorf("filter_complex missing %q\ngraph: %s", w, fc)
		}
	}
}

func TestExportV2_TransformAndCrop(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4"},
		Video: []StudioExportVideoSeg{{
			InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1, TrackIndex: 0,
			Transform: &models.StudioTransform{X: 0.1, Y: -0.2, Scale: 0.5, RotationDeg: 90},
			Crop:      &models.StudioCrop{Left: 0.05, Top: 0.05, Right: 0.15, Bottom: 0.05},
		}},
		Width: 1920, Height: 1080, FPS: 30, Duration: 5,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc,
		"crop=w=max(16\\,iw*0.800):h=max(16\\,ih*0.900):x=iw*0.050:y=ih*0.050",
		"scale=960:540:force_original_aspect_ratio=decrease",
		"rotate=a=1.571:c=black@0:ow=rotw(1.571):oh=roth(1.571)",
		"overlay=x=(W-w)/2+(192.000):y=(H-h)/2+(-216.000):enable='between(t,0.000,5.000)':eof_action=pass[vout]",
	)
}

func TestExportV2_MultiplyBlend(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4", "/b.mp4"},
		Video: []StudioExportVideoSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1, TrackIndex: 0},
			{InputIndex: 1, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1, TrackIndex: 1, BlendMode: "multiply"},
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 5,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	// Alpha-correct blend path (part 12): RGB blend + layer-alpha composite, so
	// export matches the shader's mix(backdrop, blendRGB(backdrop, src), alpha).
	wantAll(t, fc,
		"color=c=black@0:s=1920x1080:r=30:d=5.000,format=yuva420p[bg1]",
		"[bg1][vc1]overlay=x=(W-w)/2:y=(H-h)/2[pos1]",
		"[pos1]split[posc1][posa1]",
		"[posa1]alphaextract[mask1]",
		"[posc1]format=gbrp[posc1r]",
		"[vov0]split[bk1][bt1]",
		"[bt1]format=gbrp[bt1r]",
		"[bt1r][posc1r]blend=all_mode=multiply[bln1]",
		"[bln1][mask1]alphamerge,format=yuva420p[lay1]",
		"[bk1][lay1]overlay=x=0:y=0:enable='between(t,0.000,5.000)':eof_action=pass[vout]",
	)
}

func TestExportV2_Lumetri(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4"},
		Video: []StudioExportVideoSeg{{
			InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1, TrackIndex: 0,
			Effects: []models.StudioEffect{{
				ID: "e1", Type: models.StudioEffectLumetri, Enabled: true,
				Exposure: fp(1), Contrast: fp(1.2), Saturation: fp(0.8), Temperature: fp(20), Tint: fp(-10), Vibrance: fp(0.5),
			}},
		}},
		Width: 1920, Height: 1080, FPS: 30, Duration: 5,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc,
		"colorchannelmixer=rr=2.000:gg=2.000:bb=2.000",
		"colorbalance=rm=0.200:gm=0.100:bm=-0.200",
		"vibrance=intensity=0.500",
		"eq=contrast=1.200:saturation=0.800",
	)
}

func TestExportV2_Lut3dIntensity(t *testing.T) {
	mk := func(intensity float64) string {
		plan := StudioExportPlan{
			Inputs: []string{"/a.mp4"},
			Video: []StudioExportVideoSeg{{
				InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1, TrackIndex: 0,
				Effects:  []models.StudioEffect{{ID: "e1", Type: models.StudioEffectLUT, Enabled: true, LutAssetID: sp("L1"), Intensity: fp(intensity)}},
				LutPaths: map[string]string{"L1": "/tmp/look.cube"},
			}},
			Width: 1920, Height: 1080, FPS: 30, Duration: 5,
		}
		return filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	}
	// Full strength → plain inline lut3d, no split.
	full := mk(1)
	if !strings.Contains(full, "lut3d=file='/tmp/look.cube':interp=trilinear") {
		t.Errorf("full-intensity lut3d missing: %s", full)
	}
	if strings.Contains(full, "split[") {
		t.Errorf("full-intensity LUT should not split: %s", full)
	}
	// Partial → split + blend mix. The GRADED stream is the top input: blend
	// normal = top*opacity + bottom*(1-opacity), so intensity weights the grade
	// (part 12 fixed the previously inverted orientation).
	part := mk(0.5)
	wantAll(t, part,
		"split[vc0ax][vc0ay]",
		"[vc0ay]lut3d=file='/tmp/look.cube':interp=trilinear[vc0al]",
		"[vc0al][vc0ax]blend=all_mode=normal:all_opacity=0.500[vc0g]",
		"[vc0g]format=yuva420p",
	)
}

func TestExportV2_ChromaKey(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4"},
		Video: []StudioExportVideoSeg{{
			InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 0, Opacity: 1, TrackIndex: 0,
			Effects: []models.StudioEffect{{ID: "e1", Type: models.StudioEffectChromaKey, Enabled: true, KeyColor: sp("#00FF00"), Similarity: fp(0.2), Blend: fp(0.1), Despill: fp(0.6)}},
		}},
		Width: 1920, Height: 1080, FPS: 30, Duration: 5,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc,
		"format=yuva420p,chromakey=color=0x00FF00:similarity=0.200:blend=0.100",
		"despill=type=green:mix=0.600",
	)
}

func TestExportV2_VolumeKeyframesAndPan(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4"},
		Audio: []StudioExportAudioSeg{{
			InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 0, Volume: 1, Pan: 0.5,
			VolumeKeyframes: []models.StudioVolumeKeyframe{{T: 0, Gain: 0.5}, {T: 2, Gain: 1.5}, {T: 4, Gain: 1.0}},
		}},
		Width: 1920, Height: 1080, FPS: 30, Duration: 4,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc,
		"volume='if(lt(t,0.000),0.500,if(lt(t,2.000),(0.500+(1.000)*(t-0.000)/(2.000)),if(lt(t,4.000),(1.500+(-0.500)*(t-2.000)/(2.000)),1.000)))':eval=frame",
		"stereotools=balance_in=0.500",
	)
}

func TestExportV2_QsinFades(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4"},
		Audio:  []StudioExportAudioSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 6, TimelineStart: 0, Volume: 1, FadeIn: 1.5, FadeOut: 1.5}},
		Width:  1920, Height: 1080, FPS: 30, Duration: 6,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc, "afade=t=in:st=0:d=1.500:curve=qsin", "afade=t=out:st=4.500:d=1.500:curve=qsin")
}

func TestExportV2_DuckingGraph(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/voice.mp3", "/music.mp3"},
		Audio: []StudioExportAudioSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 10, TimelineStart: 0, Volume: 1, Voice: true},
			{InputIndex: 1, SourceIn: 0, SourceOut: 10, TimelineStart: 0, Volume: 0.6, Voice: false},
		},
		Ducking: &StudioDucking{AmountDb: 9, AttackMs: 120, ReleaseMs: 400},
		Width:   1920, Height: 1080, FPS: 30, Duration: 10,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc,
		"[voice]asplit[voicemix][voicesc]",
		"[bed][voicesc]sidechaincompress=threshold=0.02:ratio=10.000:attack=120.000:release=400.000:makeup=1[duckedbed]",
		"[duckedbed][voicemix]amix=inputs=2:normalize=0:dropout_transition=0[aout]",
	)
}

func TestExportV2_LoudnessPreset(t *testing.T) {
	plan := StudioExportPlan{
		Inputs:   []string{"/a.mp4"},
		Audio:    []StudioExportAudioSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 0, Volume: 1}},
		Loudness: "streaming",
		Width:    1920, Height: 1080, FPS: 30, Duration: 4,
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc, "[apm]", "[apm]loudnorm=I=-14:TP=-1.5:LRA=11[aout]")
}

func TestHexToFFColor(t *testing.T) {
	cases := map[string]string{"#FFCC00": "0xFFCC00", "00ff00": "0x00FF00", "bad": "white", "#12345": "white"}
	for in, want := range cases {
		if got := hexToFFColor(in); got != want {
			t.Errorf("hexToFFColor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildMultiTrackExportArgs_AudioOnly(t *testing.T) {
	// No video segments → audio-only output (no video map, no -c:v).
	plan := StudioExportPlan{
		Inputs: []string{"/voice.mp3"},
		Audio:  []StudioExportAudioSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 1, Volume: 0.8}},
		Width:  1920, Height: 1080, FPS: 30, Duration: 5,
	}
	args := buildMultiTrackExportArgs(plan, "h264_nvenc", "high", "/font.ttf", "/fontsdir", "/out.mp4")
	if argIndex(args, "-c:v") != -1 {
		t.Errorf("audio-only export should not set -c:v: %v", args)
	}
	if argIndex(args, "[aout]") == -1 {
		t.Errorf("expected audio map, args=%v", args)
	}
	fc := filterComplex(t, args)
	if !strings.Contains(fc, "adelay=1000|1000") {
		t.Errorf("expected 1000ms delay for timelineStart=1: %s", fc)
	}
}

// --- Part 14: typed transitions ---------------------------------------------

// twoClipPlan builds the canonical transition scene: A [0,2.5), B entering at
// 1.5 with a 1s overlap transition of the given type (B carries the entrance,
// A carries the matching TransOut pairing CollectExportRefs derives).
func twoClipPlan(transitionType string) StudioExportPlan {
	return StudioExportPlan{
		Inputs: []string{"/a.mp4", "/b.mp4"},
		Video: []StudioExportVideoSeg{
			{InputIndex: 0, SourceIn: 0, SourceOut: 2.5, TimelineStart: 0, Opacity: 1, TrackIndex: 0,
				TransOutType: transitionType, TransOutStart: 1.5, TransOutDur: 1},
			{InputIndex: 1, SourceIn: 0, SourceOut: 2.5, TimelineStart: 1.5, Opacity: 1, TrackIndex: 0,
				TransitionType: transitionType, TransitionDur: 1},
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 4,
	}
}

func TestExportV3_DipToBlack(t *testing.T) {
	fc := filterComplex(t, buildMultiTrackExportArgs(twoClipPlan(models.StudioTransitionDipToBlack), "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc,
		// The color quad: full canvas, alpha peaking at the overlap midpoint.
		"color=c=black:s=1920x1080:r=30:d=4.000,format=yuva420p,fade=t=in:st=1.500:d=0.500:alpha=1,fade=t=out:st=2.000:d=0.500:alpha=1[dip1]",
		// B composites into an intermediate label with its enable shifted to the
		// midpoint (the cut happens under full color), then the dip goes on top.
		"enable='between(t,2.000,4.000)':eof_action=pass[vtr1]",
		"[vtr1][dip1]overlay=x=0:y=0:enable='between(t,1.500,2.500)':eof_action=pass[vout]",
	)
	if strings.Contains(fc, "fade=t=in:st=1.500:d=1.000:alpha=1") {
		t.Errorf("dip must not emit the dissolve alpha fade on the entering clip: %s", fc)
	}
}

func TestExportV3_DipToWhite(t *testing.T) {
	fc := filterComplex(t, buildMultiTrackExportArgs(twoClipPlan(models.StudioTransitionDipToWhite), "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc, "color=c=white:s=1920x1080")
}

func TestExportV3_WipeLeft(t *testing.T) {
	fc := filterComplex(t, buildMultiTrackExportArgs(twoClipPlan(models.StudioTransitionWipeLeft), "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc,
		// Animated crop on B: revealed width grows with progress p.
		"crop=w='max(2,iw*min(1,max(0,(t-1.500)/1.000)))':h=ih:x=0:y=0",
		// The shrinking crop stays anchored to the left canvas edge until the
		// transition ends, then the normal centering takes over.
		"overlay=x='if(lt(t,2.500),0,(W-w)/2)':y=(H-h)/2:enable='between(t,1.500,4.000)'",
	)
	// A wipe leaves the outgoing clip alone.
	if !strings.Contains(fc, "overlay=x=(W-w)/2:y=(H-h)/2:enable='between(t,0.000,2.500)'") {
		t.Errorf("outgoing clip must keep the plain centered overlay under a wipe: %s", fc)
	}
}

func TestExportV3_WipeDirections(t *testing.T) {
	cases := map[string][]string{
		models.StudioTransitionWipeRight: {"crop=w='max(2,iw*min(1,max(0,(t-1.500)/1.000)))':h=ih:x=iw-ow:y=0", "x='if(lt(t,2.500),W-w,(W-w)/2)'"},
		models.StudioTransitionWipeUp:    {"crop=w=iw:h='max(2,ih*min(1,max(0,(t-1.500)/1.000)))':x=0:y=0", "y='if(lt(t,2.500),0,(H-h)/2)'"},
		models.StudioTransitionWipeDown:  {"crop=w=iw:h='max(2,ih*min(1,max(0,(t-1.500)/1.000)))':x=0:y=ih-oh", "y='if(lt(t,2.500),H-h,(H-h)/2)'"},
	}
	for ty, wants := range cases {
		fc := filterComplex(t, buildMultiTrackExportArgs(twoClipPlan(ty), "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
		wantAll(t, fc, wants...)
	}
}

func TestExportV3_PushLeft(t *testing.T) {
	fc := filterComplex(t, buildMultiTrackExportArgs(twoClipPlan(models.StudioTransitionPushLeft), "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc,
		// B translates in from the left: offset −W·(1−p) toward home.
		"overlay=x='(W-w)/2-W*(1-min(1,max(0,(t-1.500)/1.000)))':y=(H-h)/2:enable='between(t,1.500,4.000)'",
		// A is pushed out to the right by W·p over the same window.
		"overlay=x='(W-w)/2+W*min(1,max(0,(t-1.500)/1.000))':y=(H-h)/2:enable='between(t,0.000,2.500)'",
	)
}

func TestExportV3_PushAndSlideDirections(t *testing.T) {
	type want struct{ b, a string }
	cases := map[string]want{
		models.StudioTransitionPushRight: {b: "x='(W-w)/2+W*(1-min(1,max(0,(t-1.500)/1.000)))'", a: "x='(W-w)/2-W*min(1,max(0,(t-1.500)/1.000))'"},
		models.StudioTransitionPushUp:    {b: "y='(H-h)/2-H*(1-min(1,max(0,(t-1.500)/1.000)))'", a: "y='(H-h)/2+H*min(1,max(0,(t-1.500)/1.000))'"},
		models.StudioTransitionPushDown:  {b: "y='(H-h)/2+H*(1-min(1,max(0,(t-1.500)/1.000)))'", a: "y='(H-h)/2-H*min(1,max(0,(t-1.500)/1.000))'"},
		// Slides move ONLY the entering clip; the outgoing stays centered.
		models.StudioTransitionSlideLeft: {b: "x='(W-w)/2-W*(1-min(1,max(0,(t-1.500)/1.000)))'", a: "overlay=x=(W-w)/2:y=(H-h)/2:enable='between(t,0.000,2.500)'"},
		models.StudioTransitionSlideUp:   {b: "y='(H-h)/2-H*(1-min(1,max(0,(t-1.500)/1.000)))'", a: "overlay=x=(W-w)/2:y=(H-h)/2:enable='between(t,0.000,2.500)'"},
	}
	for ty, w := range cases {
		fc := filterComplex(t, buildMultiTrackExportArgs(twoClipPlan(ty), "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
		wantAll(t, fc, w.b, w.a)
	}
}

func TestExportV3_CrossDissolveStaysOnLegacyFadePath(t *testing.T) {
	// crossDissolve arrives as FadeIn (AssembleExportPlan routes it there), so
	// the graph is byte-identical to the pre-part-14 dissolve.
	plan := twoClipPlan("")
	plan.Video[1].FadeIn = 1
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fontsdir", "/out.mp4"))
	wantAll(t, fc, "fade=t=in:st=1.500:d=1.000:alpha=1")
	for _, forbidden := range []string{"color=c=black:s=1920x1080,format=yuva420p,fade", "crop=w='max(2", "W*(1-min", "[dip"} {
		if strings.Contains(fc, forbidden) {
			t.Errorf("dissolve plan unexpectedly emitted typed-transition graph %q: %s", forbidden, fc)
		}
	}
}

// --- part 15: keyframed motion / opacity / volume / pan ---------------------

func kfLane(points ...models.StudioKeyframe) []models.StudioKeyframe { return points }

func TestBuildMultiTrackExportArgs_KeyframedMotionAndOpacity(t *testing.T) {
	plan := StudioExportPlan{
		Inputs: []string{"/a.mp4"},
		Video: []StudioExportVideoSeg{
			{
				InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 1, Opacity: 1, TrackIndex: 0,
				ClipKeyframes: &models.StudioClipKeyframes{
					PositionX: kfLane(kf(0, -0.5, "linear"), kf(4, 0.5, "linear")),
					Scale:     kfLane(kf(0, 0.5, "easeIn"), kf(4, 2, "linear")),
					Rotation:  kfLane(kf(0, 0, "linear"), kf(4, 90, "linear")),
					Opacity:   kfLane(kf(0, 0, "linear"), kf(4, 1, "linear")),
				},
			},
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 5,
	}
	args := buildMultiTrackExportArgs(plan, "libx264", "", "/font.ttf", "/fontsdir", "/out.mp4")
	fc := filterComplex(t, args)

	for _, want := range []string{
		// The static fit targets the MAX keyframed scale (2 → 3840x2160 box).
		"scale=3840:2160:force_original_aspect_ratio=decrease",
		// Animated rotation: piecewise degrees × PI/180, constant diagonal box.
		"rotate=a='(if(lt((t-1.000),0.000),0.000,if(lt((t-1.000),4.000),(0.000+(90.000)*((t-1.000)-0.000)/4.000),90.000)))*PI/180':c=black@0:ow='ceil(hypot(iw,ih))':oh='ceil(hypot(iw,ih))'",
		// Animated scale: per-frame downscale by s(t)/sMax with even dims.
		"scale=w='max(2,trunc(iw*(if(lt((t-1.000),0.000),0.500,if(lt((t-1.000),4.000),(0.500+(1.500)*pow(((t-1.000)-0.000)/4.000,2)),2.000)))/2.000/2)*2)':h='max(2,trunc(ih*(if(lt((t-1.000),0.000),0.500,if(lt((t-1.000),4.000),(0.500+(1.500)*pow(((t-1.000)-0.000)/4.000,2)),2.000)))/2.000/2)*2)':eval=frame",
		// Keyframed opacity: instance-named mixer seeded with the lane's t=0 value.
		"colorchannelmixer@op0=aa=0.000",
		// Keyframed position: overlay x as a per-frame expression of timeline t.
		"overlay=x='(W-w)/2+W*(if(lt((t-1.000),0.000),-0.500,if(lt((t-1.000),4.000),(-0.500+(1.000)*((t-1.000)-0.000)/4.000),0.500)))':y=(H-h)/2",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("filter graph missing %q\n\ngraph: %s", want, fc)
		}
	}
}

func TestBuildMultiTrackExportArgs_SendCmdInsertion(t *testing.T) {
	plan := StudioExportPlan{
		Inputs:      []string{"/a.mp4"},
		Video:       []StudioExportVideoSeg{{InputIndex: 0, SourceOut: 4, Opacity: 1}},
		Width:       1920, Height: 1080, FPS: 30, Duration: 4,
		SendCmdPath: "/tmp/job/keyframes.cmd",
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "", "", "/fontsdir", "/out.mp4"))
	want := "color=c=black:s=1920x1080:r=30:d=4.000,format=yuv420p,sendcmd=f='/tmp/job/keyframes.cmd'[vbase]"
	if !strings.Contains(fc, want) {
		t.Errorf("missing sendcmd on vbase: %s", fc)
	}

	// Audio-only project: the sidecar rides an asendcmd on the mix output.
	audioPlan := StudioExportPlan{
		Inputs:      []string{"/a.mp3"},
		Audio:       []StudioExportAudioSeg{{InputIndex: 0, SourceOut: 4, Volume: 1}},
		Width:       1920, Height: 1080, FPS: 30, Duration: 4,
		SendCmdPath: "/tmp/job/keyframes.cmd",
	}
	args := buildMultiTrackExportArgs(audioPlan, "libx264", "", "", "/fontsdir", "/out.mp4")
	fc = filterComplex(t, args)
	if !strings.Contains(fc, "[aout]asendcmd=f='/tmp/job/keyframes.cmd'[aoutsc]") {
		t.Errorf("missing asendcmd fallback: %s", fc)
	}
	if idx := argIndex(args, "-map"); idx == -1 || args[idx+1] != "[aoutsc]" {
		t.Errorf("audio map should target [aoutsc]: %v", args)
	}
}

func TestAudioSegChain_V3VolumeAndPanKeyframes(t *testing.T) {
	ac := StudioExportAudioSeg{
		InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 0, Volume: 1, SegIndex: 3,
		// Legacy lane present too: the v3 lane must win.
		VolumeKeyframes: []models.StudioVolumeKeyframe{{T: 0, Gain: 0.1}, {T: 4, Gain: 0.1}},
		Pan:             0,
		ClipKeyframes: &models.StudioClipKeyframes{
			Volume: kfLane(kf(0, 0, "easeOut"), kf(4, 2, "linear")),
			Pan:    kfLane(kf(0, -1, "linear"), kf(4, 1, "linear")),
		},
	}
	chain := audioSegChain(ac, "ac0")
	if !strings.Contains(chain, "volume='if(lt(t,0.000),0.000,if(lt(t,4.000),(0.000+(2.000)*(1-pow(1-(t-0.000)/4.000,2))),2.000))':eval=frame") {
		t.Errorf("v3 eased volume expression missing: %s", chain)
	}
	if strings.Contains(chain, "0.100") {
		t.Errorf("legacy volume lane leaked into the chain: %s", chain)
	}
	// Pan keyframed: instance-named stereotools seeded from the lane at t=0.
	if !strings.Contains(chain, "stereotools@pan3=balance_in=-1.000") {
		t.Errorf("keyframed pan instance missing: %s", chain)
	}
}

// Byte-stability: a clip with NO keyframes must compile to the exact pre-15
// chain — no instance names, no extra stages.
func TestBuildMultiTrackExportArgs_NoKeyframesByteStable(t *testing.T) {
	seg := StudioExportVideoSeg{
		InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 0, Opacity: 0.5, TrackIndex: 0,
		Transform: &models.StudioTransform{X: 0.1, Y: 0, Scale: 0.5, RotationDeg: 45},
		Effects: []models.StudioEffect{{
			ID: "l1", Type: models.StudioEffectLumetri, Enabled: true,
			Exposure: fp(1), Contrast: fp(1.2), Saturation: fp(1), Temperature: fp(0), Tint: fp(0), Vibrance: fp(0),
		}},
	}
	plan := StudioExportPlan{Inputs: []string{"/a.mp4"}, Video: []StudioExportVideoSeg{seg}, Width: 1920, Height: 1080, FPS: 30, Duration: 4}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "", "", "/fontsdir", "/out.mp4"))
	for _, marker := range []string{"@op", "@lume", "@lumb", "@lumv", "@lumq", "@ck", "@dsp", "@pan"} {
		if strings.Contains(fc, marker) {
			t.Errorf("static plan must not emit instance names (%s): %s", marker, fc)
		}
	}
	if strings.Contains(fc, "eval=frame") {
		t.Errorf("static plan must not emit per-frame video expressions: %s", fc)
	}
	for _, want := range []string{
		"scale=960:540:force_original_aspect_ratio=decrease",
		"colorchannelmixer=rr=2.000:gg=2.000:bb=2.000",
		"eq=contrast=1.200:saturation=1.000",
		"colorchannelmixer=aa=0.500",
		"rotate=a=0.785:c=black@0:ow=rotw(0.785):oh=roth(0.785)",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("static chain missing %q: %s", want, fc)
		}
	}
}
