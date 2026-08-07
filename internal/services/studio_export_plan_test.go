package services

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Part 14: CollectExportRefs derives the audio crossfade windows from the
// ACTUAL same-track overlaps (automatic, no user action), and resolves the
// typed transition (with legacy upgrade) for the video side.

func planAsset(id string, hasAudio bool) *models.StudioAsset {
	return &models.StudioAsset{ID: id, S3KeyOriginal: "orig/" + id, HasAudio: hasAudio, MediaKind: models.StudioMediaKindVideo}
}

func planProject(tracks []models.StudioTrack) *models.StudioProject {
	return &models.StudioProject{ID: "p1", FPS: 30, Width: 1920, Height: 1080, Tracks: tracks}
}

func refByClipID(refs []StudioClipRef, id string) *StudioClipRef {
	for i := range refs {
		if refs[i].Clip.ID == id {
			return &refs[i]
		}
	}
	return nil
}

func TestCollectExportRefs_AudioCrossfadeFromOverlap(t *testing.T) {
	// Two audio-bearing clips overlapping by 1s on one track — no transition
	// field anywhere: the crossfade windows must still appear.
	project := planProject([]models.StudioTrack{{
		ID: "a1", Kind: models.StudioTrackKindAudio, Index: 0,
		Clips: []models.StudioClip{
			{ID: "c1", AssetID: "A", TimelineStart: 0, SourceIn: 0, SourceOut: 5},
			{ID: "c2", AssetID: "B", TimelineStart: 4, SourceIn: 0, SourceOut: 5},
		},
	}})
	assets := map[string]*models.StudioAsset{"A": planAsset("A", true), "B": planAsset("B", true)}

	refs, duration, ok := CollectExportRefs(project, assets)
	if !ok {
		t.Fatal("expected refs")
	}
	if duration != 9 {
		t.Fatalf("duration = %v, want 9", duration)
	}
	c1 := refByClipID(refs, "c1")
	c2 := refByClipID(refs, "c2")
	if c1 == nil || c2 == nil {
		t.Fatal("missing refs")
	}
	if c1.FadeOut != 1 || c1.FadeIn != 0 {
		t.Errorf("c1 fades = in %v / out %v, want 0 / 1 (overlap-derived)", c1.FadeIn, c1.FadeOut)
	}
	if c2.FadeIn != 1 || c2.FadeOut != 0 {
		t.Errorf("c2 fades = in %v / out %v, want 1 / 0 (overlap-derived)", c2.FadeIn, c2.FadeOut)
	}

	// End to end: the emitted graph carries the equal-power qsin pair.
	plan := AssembleExportPlan(refs, StudioPlanSpec{
		Width: 1920, Height: 1080, FPS: 30, Duration: duration,
		LocalByKey: map[string]string{"orig/A": "/a.mp4", "orig/B": "/b.mp4"},
	})
	fc := filterComplex(t, buildMultiTrackExportArgs(plan, "libx264", "medium", "/f.ttf", "/out.mp4"))
	for _, want := range []string{
		"afade=t=out:st=4.000:d=1.000:curve=qsin", // c1 tail over the overlap
		"afade=t=in:st=0:d=1.000:curve=qsin",      // c2 head over the overlap
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("filter_complex missing %q\ngraph: %s", want, fc)
		}
	}
}

func TestCollectExportRefs_TypedTransitionResolution(t *testing.T) {
	wipe := &models.StudioTransition{Type: models.StudioTransitionWipeLeft, DurationSeconds: 1}
	legacy := 1.5
	project := planProject([]models.StudioTrack{{
		ID: "v1", Kind: models.StudioTrackKindVideo, Index: 0,
		Clips: []models.StudioClip{
			{ID: "c1", AssetID: "A", TimelineStart: 0, SourceIn: 0, SourceOut: 5},
			{ID: "c2", AssetID: "B", TimelineStart: 4, SourceIn: 0, SourceOut: 5, Transition: wipe},
			{ID: "c3", AssetID: "A", TimelineStart: 7.5, SourceIn: 0, SourceOut: 5, TransitionInSeconds: &legacy},
		},
	}})
	assets := map[string]*models.StudioAsset{"A": planAsset("A", false), "B": planAsset("B", false)}

	refs, _, ok := CollectExportRefs(project, assets)
	if !ok {
		t.Fatal("expected refs")
	}
	c1 := refByClipID(refs, "c1")
	c2 := refByClipID(refs, "c2")
	c3 := refByClipID(refs, "c3")
	if c2.TransInType != models.StudioTransitionWipeLeft || c2.TransInDur != 1 {
		t.Errorf("c2 typed transition = %q/%v, want wipeLeft/1", c2.TransInType, c2.TransInDur)
	}
	// Legacy transitionInSeconds upgrades to crossDissolve.
	if c3.TransInType != models.StudioTransitionCrossDissolve || c3.TransInDur != 1.5 {
		t.Errorf("c3 legacy upgrade = %q/%v, want crossDissolve/1.5", c3.TransInType, c3.TransInDur)
	}
	// The outgoing pairing carries the NEXT clip's transition + window.
	if c1.TransOutType != models.StudioTransitionWipeLeft || c1.TransOutStart != 4 || c1.TransOutDur != 1 {
		t.Errorf("c1 TransOut = %q/%v/%v, want wipeLeft/4/1", c1.TransOutType, c1.TransOutStart, c1.TransOutDur)
	}

	// AssembleExportPlan routing: wipe → TransitionType, dissolve → FadeIn.
	plan := AssembleExportPlan(refs, StudioPlanSpec{
		Width: 1920, Height: 1080, FPS: 30, Duration: 12.5,
		LocalByKey: map[string]string{"orig/A": "/a.mp4", "orig/B": "/b.mp4"},
	})
	if plan.Video[1].TransitionType != models.StudioTransitionWipeLeft || plan.Video[1].FadeIn != 0 {
		t.Errorf("wipe seg routed wrong: type %q fadeIn %v", plan.Video[1].TransitionType, plan.Video[1].FadeIn)
	}
	if plan.Video[2].FadeIn != 1.5 || plan.Video[2].TransitionType != "" {
		t.Errorf("dissolve seg routed wrong: type %q fadeIn %v", plan.Video[2].TransitionType, plan.Video[2].FadeIn)
	}
}
