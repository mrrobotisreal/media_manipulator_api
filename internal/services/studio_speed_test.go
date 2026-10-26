package services

import (
	"math"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func fptr(v float64) *float64 { return &v }
func bptr(v bool) *bool       { return &v }

func TestAtempoFactors_Golden(t *testing.T) {
	cases := []struct {
		speed float64
		want  []float64
	}{
		{1, nil},
		{0.1, []float64{0.5, 0.5, 0.5, 0.8}},
		{0.25, []float64{0.5, 0.5}},
		{0.5, []float64{0.5}},
		{0.75, []float64{0.75}},
		{1.5, []float64{1.5}},
		{2, []float64{2}},
		{3, []float64{2, 1.5}},
		{4, []float64{2, 2}},
		{10, []float64{2, 2, 2.5}},
	}
	// 10x needs the remainder > 2 folded: 10 -> 2,2,2.5 is out of range, so the
	// factoring must keep every step inside [0.5, 2].
	for _, c := range cases {
		got := atempoFactors(c.speed)
		prod := 1.0
		for _, f := range got {
			if f < 0.5-1e-9 || f > 2.0+1e-9 {
				t.Errorf("speed %v: factor %v outside [0.5,2]: %v", c.speed, f, got)
			}
			prod *= f
		}
		if c.speed != 1 && math.Abs(prod-c.speed) > 1e-9 {
			t.Errorf("speed %v: product %v != speed (%v)", c.speed, prod, got)
		}
		if c.speed == 1 && got != nil {
			t.Errorf("speed 1 should yield no factors, got %v", got)
		}
	}
	if got := atempoFactors(10); len(got) != 4 {
		t.Errorf("10x should be 2,2,2,1.25 (4 steps), got %v", got)
	}
	if got := atempoFactors(0.1); len(got) != 4 {
		t.Errorf("0.1x should be 0.5,0.5,0.5,0.8 (4 steps), got %v", got)
	}
}

func TestAudioSpeedParts_Chains(t *testing.T) {
	cases := []struct {
		name     string
		speed    float64
		reverse  bool
		pitchOff bool
		want     string
	}{
		{"unity", 1, false, false, ""},
		{"2x pitch kept", 2, false, false, "atempo=2.000000,asetpts=PTS-STARTPTS"},
		{"4x factored", 4, false, false, "atempo=2.000000,atempo=2.000000,asetpts=PTS-STARTPTS"},
		{"0.5x", 0.5, false, false, "atempo=0.500000,asetpts=PTS-STARTPTS"},
		{"10x factored", 10, false, false, "atempo=2.000000,atempo=2.000000,atempo=2.000000,atempo=1.250000,asetpts=PTS-STARTPTS"},
		{"0.1x factored", 0.1, false, false, "atempo=0.500000,atempo=0.500000,atempo=0.500000,atempo=0.800000,asetpts=PTS-STARTPTS"},
		{"2x pitch off", 2, false, true, "asetrate=96000,aresample=48000,asetpts=PTS-STARTPTS"},
		{"0.5x pitch off", 0.5, false, true, "asetrate=24000,aresample=48000,asetpts=PTS-STARTPTS"},
		{"reverse only", 1, true, false, "areverse,asetpts=PTS-STARTPTS"},
		{"reverse 2x", 2, true, false, "areverse,atempo=2.000000,asetpts=PTS-STARTPTS"},
		{"pitch off at unity is a no-op", 1, false, true, ""},
	}
	for _, c := range cases {
		if got := strings.Join(audioSpeedParts(c.speed, c.reverse, c.pitchOff), ","); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestSpeedPtsParts(t *testing.T) {
	if got := strings.Join(speedPtsParts(0, false, "2.000"), ","); got != "setpts=PTS-STARTPTS+2.000/TB" {
		t.Errorf("default must stay byte-identical, got %q", got)
	}
	if got := strings.Join(speedPtsParts(2, false, "2.000"), ","); got != "setpts=(PTS-STARTPTS)/2.000000+2.000/TB" {
		t.Errorf("2x: %q", got)
	}
	if got := strings.Join(speedPtsParts(0.5, true, "0.000"), ","); got != "setpts=PTS-STARTPTS,reverse,setpts=(PTS-STARTPTS)/0.500000+0.000/TB" {
		t.Errorf("reverse 0.5x: %q", got)
	}
}

func speedPlan(vs StudioExportVideoSeg, as *StudioExportAudioSeg) StudioExportPlan {
	p := StudioExportPlan{
		Inputs: inputs("/a.mp4"),
		Video:  []StudioExportVideoSeg{vs},
		Width:  1280, Height: 720, FPS: 30, Duration: 10,
	}
	if as != nil {
		p.Audio = []StudioExportAudioSeg{*as}
	}
	return p
}

func TestExportV3_SpeedUpVideoAndAudio(t *testing.T) {
	plan := speedPlan(
		StudioExportVideoSeg{InputIndex: 0, SourceIn: 2, SourceOut: 8, TimelineStart: 1, Opacity: 1, Speed: 2},
		&StudioExportAudioSeg{InputIndex: 0, SourceIn: 2, SourceOut: 8, TimelineStart: 1, Volume: 1, Speed: 2},
	)
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fd", "/o.mp4"))
	for _, want := range []string{
		"[0:v]trim=start=2.000:end=8.000,setpts=(PTS-STARTPTS)/2.000000+1.000/TB",
		"overlay=x=(W-w)/2:y=(H-h)/2:enable='between(t,1.000,4.000)'", // 6s source at 2x = 3s on the timeline
		"[0:a]atrim=start=2.000:end=8.000,asetpts=PTS-STARTPTS,aformat=sample_rates=48000:channel_layouts=stereo,atempo=2.000000,asetpts=PTS-STARTPTS,volume=1.000",
		"adelay=1000|1000",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("missing %q\n%s", want, fc)
		}
	}
}

func TestExportV3_ReverseVideoAndAudio(t *testing.T) {
	plan := speedPlan(
		StudioExportVideoSeg{InputIndex: 0, SourceIn: 1, SourceOut: 4, TimelineStart: 0, Opacity: 1, Reverse: true},
		&StudioExportAudioSeg{InputIndex: 0, SourceIn: 1, SourceOut: 4, TimelineStart: 0, Volume: 1, Reverse: true},
	)
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fd", "/o.mp4"))
	for _, want := range []string{
		"[0:v]trim=start=1.000:end=4.000,setpts=PTS-STARTPTS,reverse,setpts=PTS-STARTPTS+0.000/TB",
		"channel_layouts=stereo,areverse,asetpts=PTS-STARTPTS,volume=1.000",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("missing %q\n%s", want, fc)
		}
	}
	// reverse must come AFTER the trim so the buffered span is clip-length.
	if strings.Index(fc, "reverse") < strings.Index(fc, "trim=start") {
		t.Errorf("reverse before trim: %s", fc)
	}
}

func TestExportV3_PitchOffResamplePath(t *testing.T) {
	plan := speedPlan(
		StudioExportVideoSeg{InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 0, Opacity: 1, Speed: 0.5},
		&StudioExportAudioSeg{InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 0, Volume: 1, Speed: 0.5, PitchOff: true},
	)
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fd", "/o.mp4"))
	if !strings.Contains(fc, "asetrate=24000,aresample=48000,asetpts=PTS-STARTPTS") {
		t.Errorf("pitch-off path missing: %s", fc)
	}
	if strings.Contains(fc, "atempo") {
		t.Errorf("pitch-off must not use atempo: %s", fc)
	}
	if !strings.Contains(fc, "between(t,0.000,8.000)") { // 4s at 0.5x = 8s
		t.Errorf("slow-down duration wrong: %s", fc)
	}
}

func TestExportV3_FreezeFrame(t *testing.T) {
	ft := 3.5
	plan := speedPlan(
		StudioExportVideoSeg{InputIndex: 0, SourceIn: 3.5, SourceOut: 5.5, TimelineStart: 2, Opacity: 1, FreezeAt: &ft},
		nil,
	)
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fd", "/o.mp4"))
	want := "[0:v]trim=start=3.500,setpts=PTS-STARTPTS,trim=end_frame=1,tpad=stop_mode=clone:stop_duration=2.000,setpts=PTS-STARTPTS+2.000/TB"
	if !strings.Contains(fc, want) {
		t.Errorf("freeze chain missing %q\n%s", want, fc)
	}
	if !strings.Contains(fc, "between(t,2.000,4.000)") {
		t.Errorf("freeze enable window wrong: %s", fc)
	}
}

func TestExportV3_NoSpeedFieldsByteStable(t *testing.T) {
	plan := speedPlan(
		StudioExportVideoSeg{InputIndex: 0, SourceIn: 2, SourceOut: 8, TimelineStart: 0, Opacity: 1},
		&StudioExportAudioSeg{InputIndex: 0, SourceIn: 2, SourceOut: 8, TimelineStart: 0, Volume: 1},
	)
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fd", "/o.mp4"))
	for _, bad := range []string{"reverse", "atempo", "asetrate", "tpad", "PTS)/"} {
		if strings.Contains(fc, bad) {
			t.Errorf("default graph gained %q: %s", bad, fc)
		}
	}
}

func clip(id string, start, in, out float64) models.StudioClip {
	return models.StudioClip{ID: id, AssetID: "a", TimelineStart: start, SourceIn: in, SourceOut: out}
}

func TestAssembleExportPlan_SpeedFields(t *testing.T) {
	asset := &models.StudioAsset{ID: "a", S3KeyOriginal: "k", HasAudio: true, MediaKind: models.StudioMediaKindVideo}
	c := clip("c1", 0, 0, 6)
	c.Speed = fptr(3)
	c.Reverse = true
	c.MaintainPitch = bptr(false)
	f := clip("c2", 6, 1, 3)
	f.Freeze = &models.StudioFreeze{SourceTime: 1}
	refs := []StudioClipRef{
		{Clip: c, Asset: asset, TrackKind: models.StudioTrackKindVideo},
		{Clip: f, Asset: asset, TrackKind: models.StudioTrackKindVideo},
	}
	plan := AssembleExportPlan(refs, StudioPlanSpec{Width: 640, Height: 360, FPS: 30, Duration: 8, LocalByKey: map[string]string{"k": "/a.mp4"}})
	if len(plan.Video) != 2 || plan.Video[0].Speed != 3 || !plan.Video[0].Reverse || plan.Video[0].FreezeAt != nil {
		t.Fatalf("video seg 0: %+v", plan.Video[0])
	}
	if plan.Video[1].FreezeAt == nil || *plan.Video[1].FreezeAt != 1 || plan.Video[1].Speed != 0 {
		t.Fatalf("freeze seg: %+v", plan.Video[1])
	}
	if len(plan.Audio) != 1 {
		t.Fatalf("freeze clips are silent; want 1 audio seg, got %d", len(plan.Audio))
	}
	if a := plan.Audio[0]; a.Speed != 3 || !a.Reverse || !a.PitchOff {
		t.Fatalf("audio seg: %+v", a)
	}
}

func TestCollectExportRefs_DurationIsSpeedAware(t *testing.T) {
	asset := &models.StudioAsset{ID: "a", S3KeyOriginal: "k", MediaKind: models.StudioMediaKindVideo}
	c := clip("c1", 1, 0, 8)
	c.Speed = fptr(4) // 8s of source in 2s
	p := &models.StudioProject{Tracks: []models.StudioTrack{{ID: "t", Kind: models.StudioTrackKindVideo, Clips: []models.StudioClip{c}}}}
	_, dur, ok := CollectExportRefs(p, map[string]*models.StudioAsset{"a": asset})
	if !ok || math.Abs(dur-3) > 1e-9 {
		t.Fatalf("duration = %v ok=%v, want 3", dur, ok)
	}
}

func TestPlanReverseSegments(t *testing.T) {
	segs := PlanReverseSegments(20, 8)
	want := []ReverseSegment{{16, 4}, {8, 8}, {0, 8}}
	if len(segs) != len(want) {
		t.Fatalf("got %v", segs)
	}
	for i := range want {
		if segs[i] != want[i] {
			t.Errorf("seg %d = %v, want %v", i, segs[i], want[i])
		}
	}
	if s := PlanReverseSegments(0, 8); len(s) != 1 || s[0].Len != 0 {
		t.Errorf("unknown duration: %v", s)
	}
}

func TestReverseSegmentArgs(t *testing.T) {
	args := strings.Join(reverseSegmentArgs("libx264", models.StudioMediaKindVideo, true, "/in.mp4", "/o.mp4", ReverseSegment{Start: 8, Len: 8}), " ")
	for _, want := range []string{"-ss 8.000", "-t 8.000", "-vf reverse", "-af areverse", "-c:a aac"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %s", want, args)
		}
	}
	a := strings.Join(reverseSegmentArgs("libx264", models.StudioMediaKindAudio, true, "/in.m4a", "/o.m4a", ReverseSegment{}), " ")
	if !strings.Contains(a, "-af areverse") || strings.Contains(a, "-vf") {
		t.Errorf("audio args: %s", a)
	}
}

// MM70: every preview proxy (ingest + reversed) pins a one-second keyframe
// cadence so Darkroom preview seeks decode at most ~1 s of video.
func TestProxyKeyframeArgs(t *testing.T) {
	cases := []struct {
		encoder   string
		want      []string
		forbidden []string
	}{
		{"libx264", []string{"-force_key_frames expr:gte(t,n_forced*1)", "-g 60"}, []string{"-forced-idr"}},
		{"h264_nvenc", []string{"-force_key_frames expr:gte(t,n_forced*1)", "-g 60", "-forced-idr 1"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.encoder, func(t *testing.T) {
			got := strings.Join(proxyKeyframeArgs(tc.encoder), " ")
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %s", w, got)
				}
			}
			for _, f := range tc.forbidden {
				if strings.Contains(got, f) {
					t.Errorf("unexpected %q in %s", f, got)
				}
			}
			full := strings.Join(videoProxyEncodeArgs(tc.encoder), " ")
			if !strings.Contains(full, "-c:v "+tc.encoder) || !strings.Contains(full, "-b:v 2M") || !strings.Contains(full, got) {
				t.Errorf("videoProxyEncodeArgs(%s) = %s", tc.encoder, full)
			}
		})
	}
}

func TestReverseSegmentArgsShareProxyKeyframes(t *testing.T) {
	for _, enc := range []string{"libx264", "h264_nvenc"} {
		args := strings.Join(reverseSegmentArgs(enc, models.StudioMediaKindVideo, true, "/in.mp4", "/o.mp4", ReverseSegment{Len: 8}), " ")
		if !strings.Contains(args, strings.Join(proxyKeyframeArgs(enc), " ")) {
			t.Errorf("%s reverse segment lacks proxy keyframe args: %s", enc, args)
		}
		// The last -b:v wins in ffmpeg; it must stay the proxy's 2M (nvenc's
		// preset emits an earlier "-b:v 0", unchanged since part 17).
		if i := strings.LastIndex(args, "-b:v "); i < 0 || !strings.HasPrefix(args[i:], "-b:v 2M") {
			t.Errorf("%s reverse segment's effective -b:v is not 2M: %s", enc, args)
		}
	}
	audio := strings.Join(reverseSegmentArgs("libx264", models.StudioMediaKindAudio, true, "/in.m4a", "/o.m4a", ReverseSegment{}), " ")
	if strings.Contains(audio, "-force_key_frames") {
		t.Errorf("audio reverse must not carry video keyframe args: %s", audio)
	}
}

// The export encode is untouched by MM70: no forced proxy keyframes there.
func TestExportH264ArgsHaveNoProxyKeyframes(t *testing.T) {
	for _, enc := range []string{"libx264", "h264_nvenc", "libx264_lossless"} {
		for _, q := range []string{"low", "medium", "high"} {
			if got := strings.Join(h264EncodeArgs(enc, q), " "); strings.Contains(got, "-force_key_frames") || strings.Contains(got, "-forced-idr") {
				t.Errorf("h264EncodeArgs(%s,%s) gained proxy args: %s", enc, q, got)
			}
		}
	}
}
