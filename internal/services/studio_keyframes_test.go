package services

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// The shared curve fixture (testdata/studio_keyframe_curves.json) is a
// byte-identical copy of the UI repo's lib/studio/ease.fixtures.json — the
// cross-side contract for the part-15 ease curves. A curve change that lands
// on only one side fails that side's gate.

type curveFixture struct {
	Cases []struct {
		Name      string                  `json:"name"`
		Keyframes []models.StudioKeyframe `json:"keyframes"`
		Samples   []struct {
			T        float64 `json:"t"`
			Expected float64 `json:"expected"`
		} `json:"samples"`
	} `json:"cases"`
}

func TestKeyframeValueAt_SharedCurveFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "studio_keyframe_curves.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx curveFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	for _, c := range fx.Cases {
		for _, s := range c.Samples {
			got := KeyframeValueAt(c.Keyframes, s.T, math.NaN())
			if math.Abs(got-s.Expected) > 1e-9 {
				t.Errorf("%s: valueAt(%v) = %v, want %v", c.Name, s.T, got, s.Expected)
			}
		}
	}
}

func TestKeyframeValueAt_EmptyLaneFallback(t *testing.T) {
	if got := KeyframeValueAt(nil, 3, 0.25); got != 0.25 {
		t.Fatalf("fallback = %v, want 0.25", got)
	}
}

func kf(t, v float64, ease string) models.StudioKeyframe {
	return models.StudioKeyframe{T: t, Value: v, Ease: ease}
}

// Golden expression emission per ease — the exact strings the export compiles.
func TestKeyframeExpr_Goldens(t *testing.T) {
	cases := []struct {
		name string
		lane []models.StudioKeyframe
		time string
		want string
	}{
		{
			name: "single keyframe → constant",
			lane: []models.StudioKeyframe{kf(1, 0.7, "linear")},
			time: "t",
			want: "0.700",
		},
		{
			name: "linear pair with clip-local time",
			lane: []models.StudioKeyframe{kf(0, 0, "linear"), kf(2, 1, "linear")},
			time: "t",
			want: "if(lt(t,0.000),0.000,if(lt(t,2.000),(0.000+(1.000)*(t-0.000)/2.000),1.000))",
		},
		{
			name: "hold segment is constant until the next keyframe",
			lane: []models.StudioKeyframe{kf(0, 1, "hold"), kf(2, 0.5, "linear")},
			time: "t",
			want: "if(lt(t,0.000),1.000,if(lt(t,2.000),1.000,0.500))",
		},
		{
			name: "easeIn / easeOut / easeBoth cubics with video timeline time",
			lane: []models.StudioKeyframe{kf(0, 0, "easeIn"), kf(1, 1, "easeOut"), kf(2, 0, "easeBoth"), kf(3, 1, "linear")},
			time: "(t-2.000)",
			want: "if(lt((t-2.000),0.000),0.000," +
				"if(lt((t-2.000),1.000),(0.000+(1.000)*pow(((t-2.000)-0.000)/1.000,2))," +
				"if(lt((t-2.000),2.000),(1.000+(-1.000)*(1-pow(1-((t-2.000)-1.000)/1.000,2)))," +
				"if(lt((t-2.000),3.000),(0.000+(1.000)*if(lt(((t-2.000)-2.000)/1.000,0.5),2*pow(((t-2.000)-2.000)/1.000,2),1-2*pow(1-((t-2.000)-2.000)/1.000,2))),1.000))))",
		},
	}
	for _, c := range cases {
		if got := KeyframeExpr(c.lane, c.time); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

func TestVideoClipTimeExpr(t *testing.T) {
	if got := videoClipTimeExpr(0); got != "t" {
		t.Fatalf("timelineStart 0 → %q, want t", got)
	}
	if got := videoClipTimeExpr(2.5); got != "(t-2.500)" {
		t.Fatalf("timelineStart 2.5 → %q", got)
	}
}

// The sidecar is a pure string artifact — golden-tested as one blob (the whole
// point of the sendcmd design, research ticket 01).
func TestGenerateStudioSendCmd_Golden(t *testing.T) {
	lumetriID := "lum1"
	chromaID := "ck1"
	plan := StudioExportPlan{
		Video: []StudioExportVideoSeg{
			{
				InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 2, Opacity: 1, TrackIndex: 0,
				Effects: []models.StudioEffect{
					{ID: lumetriID, Type: models.StudioEffectLumetri, Enabled: true},
					{ID: chromaID, Type: models.StudioEffectChromaKey, Enabled: true},
				},
				ClipKeyframes: &models.StudioClipKeyframes{
					Opacity: []models.StudioKeyframe{kf(0, 1, "linear"), kf(2, 0.25, "easeBoth"), kf(4, 1, "hold"), kf(5, 0, "linear")},
					Effects: map[string][]models.StudioKeyframe{
						lumetriID + ".exposure":    {kf(0, 0, "easeBoth"), kf(2.5, 1, "linear")},
						lumetriID + ".temperature": {kf(0, -50, "linear"), kf(5, 50, "linear")},
						lumetriID + ".tint":        {kf(1, 20, "easeIn"), kf(3, -20, "linear")},
						lumetriID + ".contrast":    {kf(0, 1, "linear"), kf(2, 1.5, "linear")},
						chromaID + ".similarity":   {kf(0, 0.1, "linear"), kf(1, 0.4, "linear")},
					},
				},
			},
		},
		Audio: []StudioExportAudioSeg{
			{
				InputIndex: 0, SourceIn: 0, SourceOut: 5, TimelineStart: 2, Volume: 1, SegIndex: 0,
				ClipKeyframes: &models.StudioClipKeyframes{
					Pan: []models.StudioKeyframe{kf(0, -1, "easeOut"), kf(5, 1, "linear")},
				},
			},
		},
		Width: 1920, Height: 1080, FPS: 30, Duration: 7,
	}

	want := "" +
		// opacity → colorchannelmixer@op0 aa (timeline times = start 2 + kf.t)
		"2.000-4.000 [expr] colorchannelmixer@op0 aa 'lerp(1.000,0.250,TI)';\n" +
		"4.000-6.000 [expr] colorchannelmixer@op0 aa 'lerp(0.250,1.000,if(lt(TI,0.5),2*pow(TI,2),1-2*pow(1-TI,2)))';\n" +
		"6.000 [enter] colorchannelmixer@op0 aa 1.000;\n" +
		"7.000 [enter] colorchannelmixer@op0 aa 0.000;\n" +
		// exposure → gain = 2^v on all three channel mixers
		"2.000-4.500 [expr] colorchannelmixer@lume0 rr 'pow(2,lerp(0.000,1.000,if(lt(TI,0.5),2*pow(TI,2),1-2*pow(1-TI,2))))', [expr] colorchannelmixer@lume0 gg 'pow(2,lerp(0.000,1.000,if(lt(TI,0.5),2*pow(TI,2),1-2*pow(1-TI,2))))', [expr] colorchannelmixer@lume0 bb 'pow(2,lerp(0.000,1.000,if(lt(TI,0.5),2*pow(TI,2),1-2*pow(1-TI,2))))';\n" +
		"4.500 [enter] colorchannelmixer@lume0 rr 2.000, [enter] colorchannelmixer@lume0 gg 2.000, [enter] colorchannelmixer@lume0 bb 2.000;\n" +
		// temperature → rm = v/100, bm = −v/100
		"2.000-7.000 [expr] colorbalance@lumb0 rm 'lerp(-0.500,0.500,TI)', [expr] colorbalance@lumb0 bm 'lerp(0.500,-0.500,TI)';\n" +
		"7.000 [enter] colorbalance@lumb0 rm 0.500, [enter] colorbalance@lumb0 bm -0.500;\n" +
		// tint → gm = −v/100
		"3.000-5.000 [expr] colorbalance@lumb0 gm 'lerp(-0.200,0.200,pow(TI,2))';\n" +
		"5.000 [enter] colorbalance@lumb0 gm 0.200;\n" +
		// contrast → eq
		"2.000-4.000 [expr] eq@lumq0 contrast 'lerp(1.000,1.500,TI)';\n" +
		"4.000 [enter] eq@lumq0 contrast 1.500;\n" +
		// chroma similarity (backend-capable)
		"2.000-3.000 [expr] chromakey@ck0 similarity 'lerp(0.100,0.400,TI)';\n" +
		"3.000 [enter] chromakey@ck0 similarity 0.400;\n" +
		// pan → stereotools balance_in
		"2.000-7.000 [expr] stereotools@pan0 balance_in 'lerp(-1.000,1.000,(1-pow(1-TI,2)))';\n" +
		"7.000 [enter] stereotools@pan0 balance_in 1.000;\n"

	if got := GenerateStudioSendCmd(plan); got != want {
		t.Errorf("sidecar mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestGenerateStudioSendCmd_EmptyWhenNoLanes(t *testing.T) {
	plan := StudioExportPlan{
		Video: []StudioExportVideoSeg{{InputIndex: 0, SourceOut: 5, Opacity: 1}},
		Audio: []StudioExportAudioSeg{{InputIndex: 0, SourceOut: 5, Volume: 1}},
	}
	if got := GenerateStudioSendCmd(plan); got != "" {
		t.Fatalf("expected empty sidecar, got %q", got)
	}
	// A single-keyframe lane is fully covered by the chain's static seed.
	plan.Video[0].ClipKeyframes = &models.StudioClipKeyframes{
		Opacity: []models.StudioKeyframe{kf(1, 0.5, "linear")},
	}
	if got := GenerateStudioSendCmd(plan); got != "" {
		t.Fatalf("single-keyframe lane should emit nothing, got %q", got)
	}
}
