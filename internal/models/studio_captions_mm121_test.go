package models

import (
	"reflect"
	"testing"
)

func TestSanitizeCaptionWords(t *testing.T) {
	cue := StudioCaptionCue{ID: "c", StartSeconds: 1, EndSeconds: 3, Text: "one two three", Words: []StudioCaptionWord{
		{Text: "one", StartSeconds: 0.5, EndSeconds: 1.4}, // starts before the cue → clamped
		{Text: "two", StartSeconds: 1.2, EndSeconds: 1.8}, // starts before the previous → raised
		{Text: " three ", StartSeconds: 2, EndSeconds: 9, Emphasis: true},
	}}
	got := SanitizeCaptions([]StudioCaptionCue{cue})[0].Words
	want := []StudioCaptionWord{{"one", 1, 1.4, false}, {"two", 1.2, 1.8, false}, {"three", 2, 3, true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("words = %+v, want %+v", got, want)
	}
	for name, ws := range map[string][]StudioCaptionWord{
		"count mismatch": {{Text: "one", StartSeconds: 1, EndSeconds: 2}},
		"empty text":     {{Text: "one"}, {Text: " "}, {Text: "three"}},
	} {
		c := cue
		c.Words = ws
		if got := SanitizeCaptions([]StudioCaptionCue{c})[0].Words; got != nil {
			t.Errorf("%s: want the list dropped, got %+v", name, got)
		}
	}
	long := StudioCaptionCue{EndSeconds: 9}
	for i := 0; i < 65; i++ {
		long.Text += "w "
		long.Words = append(long.Words, StudioCaptionWord{Text: "w"})
	}
	if SanitizeCaptions([]StudioCaptionCue{long})[0].Words != nil {
		t.Error("> 64 words must drop the list")
	}
}

func TestSanitizeCaptionStyleMM121(t *testing.T) {
	ow, sh, as := 40.0, -1.0, 3.0
	st := SanitizeCaptionStyle(&StudioCaptionStyle{
		FontSizePct: 5, Color: "#fff000", BackgroundColor: "#000000", Position: "middle", MaxWidthPct: 90,
		FontFamily: "Comic Sans", HighlightColor: "#abcdef", OutlineColor: "nope", OutlineWidth: &ow, Shadow: &sh,
		BackgroundMode: "glow", WordsPerGroup: 9, Animation: "bounce", AnimationStrength: &as, LookID: "nope",
	})
	if st.Position != "middle" || st.FontFamily != "" || st.HighlightColor != "#ABCDEF" || st.OutlineColor != "" ||
		*st.OutlineWidth != 12 || *st.Shadow != 0 || st.BackgroundMode != "" || st.WordsPerGroup != 6 ||
		st.Animation != "" || *st.AnimationStrength != 1 || st.LookID != "" {
		t.Errorf("clamps/drops wrong: %+v", *st)
	}
	ok := SanitizeCaptionStyle(&StudioCaptionStyle{FontFamily: "Bangers", BackgroundMode: "word_box", Animation: "pop", LookID: "bold_pop", WordsPerGroup: 3})
	if ok.FontFamily != "Bangers" || ok.BackgroundMode != "word_box" || ok.Animation != "pop" || ok.LookID != "bold_pop" || ok.WordsPerGroup != 3 {
		t.Errorf("valid fields dropped: %+v", *ok)
	}
}

func TestAnonCaptionAllowlist(t *testing.T) {
	for _, c := range []struct{ look, anim, wantLook, wantAnim string }{
		{"bold_pop", "pop", "bold_pop", "pop"},
		{"karaoke_classic", "karaoke_fill", "clean_highlight", "highlight"},
		{"neon", "highlight", "bold_pop", "highlight"},
		{"podcast", "none", "minimal", "none"},
		{"", "karaoke_fill", "", "highlight"},
	} {
		st := &StudioCaptionStyle{LookID: c.look, Animation: c.anim}
		ApplyAnonCaptionAllowlist(st)
		if st.LookID != c.wantLook || st.Animation != c.wantAnim {
			t.Errorf("%s/%s → %s/%s, want %s/%s", c.look, c.anim, st.LookID, st.Animation, c.wantLook, c.wantAnim)
		}
	}
	n := 0
	for _, l := range StudioCaptionLooks {
		if l.Anon {
			n++
			if !CaptionAnonAnimations[l.Style.Animation] {
				t.Errorf("anonymous look %s uses a locked animation %s", l.ID, l.Style.Animation)
			}
		}
	}
	if n != 3 || len(StudioCaptionLooks) != 6 {
		t.Errorf("want 6 looks, 3 anonymous; have %d, %d", len(StudioCaptionLooks), n)
	}
}

func TestCaptionUnits(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"Hello,  world!\nNext", []string{"Hello,", "world!", "Next"}},
		{"今天我们来测试字幕。", []string{"今", "天", "我", "们", "来", "测", "试", "字", "幕。"}},
		{"「你好」世界", []string{"「你", "好」", "世", "界"}},
		{"Hi 今天!", []string{"Hi", "今", "天!"}},
		{"שלום עולם", []string{"שלום", "עולם"}},
		{"  ", nil},
	} {
		var got []string
		for _, u := range CaptionUnits(c.in) {
			got = append(got, u.Text)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("CaptionUnits(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if CaptionUpper("straße ǆ ﬁ ὒ ı i") != "STRAßE Ǆ ﬁ ὒ I I" {
		t.Errorf("CaptionUpper = %q", CaptionUpper("straße ǆ ﬁ ὒ ı i"))
	}
}
