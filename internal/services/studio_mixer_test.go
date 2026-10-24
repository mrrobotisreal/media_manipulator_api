package services

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func mixerTrack(id string, kind models.StudioTrackKind, extra func(*models.StudioTrack), clips ...models.StudioClip) models.StudioTrack {
	t := models.StudioTrack{ID: id, Kind: kind, Clips: clips}
	if extra != nil {
		extra(&t)
	}
	return t
}

func mixerClip(id, asset string) models.StudioClip {
	return models.StudioClip{ID: id, AssetID: asset, SourceIn: 0, SourceOut: 4, TimelineStart: 0}
}

func mixerPlan(t *testing.T, tracks []models.StudioTrack, master *float64) StudioExportPlan {
	t.Helper()
	assets := map[string]*models.StudioAsset{
		"v": {ID: "v", S3KeyOriginal: "v.mp4", HasAudio: true, MediaKind: models.StudioMediaKindVideo},
		"m": {ID: "m", S3KeyOriginal: "m.mp3", HasAudio: true, MediaKind: models.StudioMediaKindAudio},
		"n": {ID: "n", S3KeyOriginal: "n.mp3", HasAudio: true, MediaKind: models.StudioMediaKindAudio},
	}
	p := &models.StudioProject{Tracks: models.SanitizeTracks(tracks)}
	refs, dur, ok := CollectExportRefs(p, assets)
	if !ok {
		t.Fatal("no refs")
	}
	return AssembleExportPlan(refs, StudioPlanSpec{
		Width: 640, Height: 360, FPS: 30, Duration: dur, MasterGain: master,
		LocalByKey: map[string]string{"v.mp4": "/v.mp4", "m.mp3": "/m.mp3", "n.mp3": "/n.mp3"},
	})
}

func TestMixer_SoloExcludesOtherTracksAndVideoAudio(t *testing.T) {
	plan := mixerPlan(t, []models.StudioTrack{
		mixerTrack("v0", models.StudioTrackKindVideo, nil, mixerClip("cv", "v")),
		mixerTrack("a0", models.StudioTrackKindAudio, nil, mixerClip("c1", "m")),
		mixerTrack("a1", models.StudioTrackKindAudio, func(t *models.StudioTrack) { t.Solo = true; t.Muted = true }, mixerClip("c2", "n")),
	}, nil)
	if len(plan.Audio) != 1 || plan.Inputs[plan.Audio[0].InputIndex].Path != "/n.mp3" {
		t.Fatalf("only the soloed (and muted) track should mix: %+v", plan.Audio)
	}
	if len(plan.Video) != 1 {
		t.Errorf("video still composites while audio is soloed: %d", len(plan.Video))
	}
}

func TestMixer_HiddenVideoTrackDropsFromCompositeButKeepsAudio(t *testing.T) {
	plan := mixerPlan(t, []models.StudioTrack{
		mixerTrack("v0", models.StudioTrackKindVideo, func(t *models.StudioTrack) { t.Hidden = true }, mixerClip("cv", "v")),
	}, nil)
	if len(plan.Video) != 0 || !plan.BlankVideo {
		t.Fatalf("hidden track must not composite: video=%d blank=%v", len(plan.Video), plan.BlankVideo)
	}
	if len(plan.Audio) != 1 {
		t.Errorf("hidden video track still contributes audio")
	}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fd", "/o.mp4"))
	if !strings.Contains(fc, "[vbase]") {
		t.Errorf("black canvas expected: %s", fc)
	}
}

func TestMixer_TrackStageChainOrder(t *testing.T) {
	plan := mixerPlan(t, []models.StudioTrack{
		mixerTrack("a0", models.StudioTrackKindAudio, func(t *models.StudioTrack) {
			t.Gain = fp(0.5)
			t.Pan = fp(-0.25)
			t.Effects = &models.StudioTrackEffects{
				EQ:         &models.StudioTrackEQ{LowGainDb: 3, MidGainDb: -2, HighGainDb: 0},
				Compressor: &models.StudioTrackCompressor{ThresholdDb: -20, Ratio: 4, AttackMs: 10, ReleaseMs: 250, MakeupDb: 6},
			}
		}, mixerClip("c1", "m"), models.StudioClip{ID: "c2", AssetID: "m", SourceOut: 2, TimelineStart: 6}),
	}, fp(0.8))
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fd", "/o.mp4"))
	stage := "lowshelf=f=120:t=s:w=1:g=3.000,equalizer=f=1000:t=q:w=1:g=-2.000,acompressor=threshold=0.100000:ratio=4.000:attack=10.000:release=250.000:makeup=1.995:knee=2.828430,volume=0.500,stereotools=balance_in=-0.250"
	if !strings.Contains(fc, stage) {
		t.Fatalf("track stage chain missing/misordered:\n%s", fc)
	}
	if strings.Contains(fc, "highshelf") {
		t.Errorf("flat band must emit nothing: %s", fc)
	}
	// The stage sits after the clip mix and before the master gain.
	iClip, iStage, iMaster := strings.Index(fc, "adelay=0|0"), strings.Index(fc, "lowshelf"), strings.Index(fc, "volume=0.800[aout]")
	if !(iClip >= 0 && iClip < iStage && iStage < iMaster) {
		t.Errorf("order clip(%d) < track stage(%d) < master(%d) violated:\n%s", iClip, iStage, iMaster, fc)
	}
}

func TestMixer_SingleClipTrackStageAndMasterBeforeLoudnorm(t *testing.T) {
	plan := mixerPlan(t, []models.StudioTrack{
		mixerTrack("a0", models.StudioTrackKindAudio, func(t *models.StudioTrack) { t.Gain = fp(1.5) }, mixerClip("c1", "m")),
	}, fp(0.5))
	plan.Loudness = "streaming"
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/fd", "/o.mp4"))
	iMaster, iLoud := strings.Index(fc, "volume=0.500[apm]"), strings.Index(fc, "[apm]loudnorm")
	if iMaster < 0 || iLoud < iMaster {
		t.Errorf("master gain must precede loudnorm: %s", fc)
	}
	if !strings.Contains(fc, "volume=1.500") {
		t.Errorf("track gain missing: %s", fc)
	}
}

func TestMixer_DefaultsAreByteStable(t *testing.T) {
	base := []models.StudioTrack{
		mixerTrack("a0", models.StudioTrackKindAudio, nil, mixerClip("c1", "m")),
		mixerTrack("a1", models.StudioTrackKindAudio, nil, mixerClip("c2", "n")),
	}
	withDefaults := []models.StudioTrack{
		mixerTrack("a0", models.StudioTrackKindAudio, func(t *models.StudioTrack) {
			t.Gain = fp(1)
			t.Pan = fp(0)
			t.Effects = &models.StudioTrackEffects{EQ: &models.StudioTrackEQ{}}
		}, mixerClip("c1", "m")),
		mixerTrack("a1", models.StudioTrackKindAudio, nil, mixerClip("c2", "n")),
	}
	a := filterComplex(t, buildMultiTrackExportArgs(mixerPlan(t, base, nil), "libx264", "medium", "/f", "/fd", "/o"))
	b := filterComplex(t, buildMultiTrackExportArgs(mixerPlan(t, withDefaults, fp(1)), "libx264", "medium", "/f", "/fd", "/o"))
	if a != b {
		t.Errorf("neutral mixer values must not change the graph:\n%s\n%s", a, b)
	}
	for _, f := range []string{"lowshelf", "acompressor", "anull", "amixpre"} {
		if strings.Contains(a, f) {
			t.Errorf("unexpected %s in pre-mixer graph", f)
		}
	}
}

func TestMixer_TwoTracksMixAfterStages(t *testing.T) {
	plan := mixerPlan(t, []models.StudioTrack{
		mixerTrack("a0", models.StudioTrackKindAudio, func(t *models.StudioTrack) { t.Gain = fp(0.5) }, mixerClip("c1", "m")),
		mixerTrack("a1", models.StudioTrackKindAudio, nil, mixerClip("c2", "n")),
	}, nil)
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f", "/fd", "/o"))
	if !strings.Contains(fc, "amix=inputs=2") || !strings.Contains(fc, "volume=0.500[act0]") {
		t.Errorf("expected one staged track + one plain seg mixed: %s", fc)
	}
}

func TestMixer_DuckingKeepsStagesInsideGroups(t *testing.T) {
	plan := mixerPlan(t, []models.StudioTrack{
		mixerTrack("bed", models.StudioTrackKindAudio, func(t *models.StudioTrack) {
			t.Effects = &models.StudioTrackEffects{Compressor: &models.StudioTrackCompressor{ThresholdDb: -24, Ratio: 4, AttackMs: 10, ReleaseMs: 250}}
		}, mixerClip("c1", "m")),
		mixerTrack("voice", models.StudioTrackKindAudio, nil, mixerClip("c2", "n")),
	}, nil)
	for i := range plan.Audio {
		plan.Audio[i].Voice = plan.Audio[i].TrackID == "" // the plain track is the voice
	}
	plan.Ducking = &StudioDucking{AmountDb: 9, AttackMs: 100, ReleaseMs: 400}
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f", "/fd", "/o"))
	iComp, iSide := strings.Index(fc, "acompressor"), strings.Index(fc, "sidechaincompress")
	if iComp < 0 || iSide < iComp {
		t.Errorf("track compressor must precede ducking: %s", fc)
	}
}

func TestMixer_SpeedReverseChainsStayBeforeTrackStage(t *testing.T) {
	c := mixerClip("c1", "m")
	sp := 2.0
	c.Speed = &sp
	c.Reverse = true
	plan := mixerPlan(t, []models.StudioTrack{
		mixerTrack("a0", models.StudioTrackKindAudio, func(t *models.StudioTrack) { t.Gain = fp(0.5) }, c),
	}, nil)
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f", "/fd", "/o"))
	iRev, iTempo, iStage := strings.Index(fc, "areverse"), strings.Index(fc, "atempo"), strings.LastIndex(fc, "volume=0.500")
	if iRev < 0 || iTempo < 0 || iStage < iRev || iStage < iTempo {
		t.Errorf("speed/reverse must precede the track stage: %s", fc)
	}
}

func TestMixer_FreezeClipsStaySilent(t *testing.T) {
	c := mixerClip("c1", "v")
	c.Freeze = &models.StudioFreeze{SourceTime: 1}
	plan := mixerPlan(t, []models.StudioTrack{
		mixerTrack("v0", models.StudioTrackKindVideo, nil, c),
		mixerTrack("a0", models.StudioTrackKindAudio, func(t *models.StudioTrack) { t.Gain = fp(0.5) }, mixerClip("c2", "m")),
	}, nil)
	if len(plan.Audio) != 1 {
		t.Errorf("freeze clip must contribute no audio: %d", len(plan.Audio))
	}
}
