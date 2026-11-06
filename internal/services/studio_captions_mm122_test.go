package services

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MM122 effect rules the goldens hold implicitly, pinned by name.
func TestCaptionEffectsMM122(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cue := models.StudioCaptionCue{ID: "c", StartSeconds: 1, EndSeconds: 1.2, Text: "Hi"}
	st := &models.StudioCaptionStyle{FontSizePct: 5, Color: "#FFFFFF", BackgroundColor: "#000000", BackgroundOpacity: 0.5, Position: "bottom", MaxWidthPct: 90, Animation: models.CaptionAnimationFade, AnimationStrength: f(1)}
	// fade: 300 in + 165 out does not fit 200 ms, so both shrink in proportion.
	if got := BuildASS([]models.StudioCaptionCue{cue}, st, 1920, 1080, ""); !strings.Contains(got, `{\fad(129,70)}Hi`) {
		t.Errorf("fade clamp:\n%s", got)
	}
	// typewriter: combining marks stay with their base character.
	if got := captionClusters("café naïve"); strings.Join(got, "|") != "c|a|f|é| |n|a|ï|v|e" {
		t.Errorf("clusters %q", got)
	}
	// typewriter / word_by_word: nothing before the first word, no box under
	// a word not yet revealed.
	st2 := *st
	st2.Animation, st2.BackgroundMode = models.CaptionAnimationTypewriter, models.CaptionBackgroundWordBox
	words := models.StudioCaptionCue{ID: "w", StartSeconds: 0, EndSeconds: 2, Text: "one two", Words: []models.StudioCaptionWord{
		{Text: "one", StartSeconds: 0.5, EndSeconds: 1}, {Text: "two", StartSeconds: 1, EndSeconds: 1.5, Emphasis: true}}}
	got := BuildASS([]models.StudioCaptionCue{words}, &st2, 1920, 1080, "")
	if strings.Contains(got, "Dialogue: 1,0:00:00.00") || strings.Contains(got, "Dialogue: 0,0:00:00.00") {
		t.Errorf("a revealing animation drew before the first word:\n%s", got)
	}
	if !strings.Contains(got, `{\3a&HFF&}{\3a&H00&}one{\3a&HFF&} two`) {
		t.Errorf("the emphasised word two must not be boxed before it is typed:\n%s", got)
	}
	// box_highlight brings the WordBox layer without backgroundMode word_box,
	// and never recolours the active word (the box carries the highlight).
	st3 := *st
	st3.Animation = models.CaptionAnimationBoxHighlight
	got = BuildASS([]models.StudioCaptionCue{words}, &st3, 1920, 1080, "")
	if !strings.Contains(got, "Style: WordBox,") || strings.Contains(got, `\1c`) {
		t.Errorf("box_highlight:\n%s", got)
	}
}
