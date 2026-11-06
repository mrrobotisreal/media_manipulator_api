package models

import (
	"encoding/json"
	"testing"
)

// MM122: ten animations, twelve looks, the anonymous allowlist unchanged
// (three looks), and every locked look/animation maps to an allowed one.
func TestCaptionCatalogMM122(t *testing.T) {
	if len(CaptionAnimations) != 10 {
		t.Fatalf("want 10 animations, have %d", len(CaptionAnimations))
	}
	for _, a := range CaptionAnimations {
		if CaptionAnonAnimations[a] {
			continue
		}
		to, ok := CaptionAnonNearestAnimation[a]
		if !ok || !CaptionAnonAnimations[to] {
			t.Errorf("locked animation %s has no allowed nearest (%q)", a, to)
		}
	}
	used := map[string]bool{}
	for _, l := range StudioCaptionLooks {
		used[l.Style.Animation] = true
		if l.Style.LookID != l.ID {
			t.Errorf("%s: style lookId %q", l.ID, l.Style.LookID)
		}
		if got := SanitizeCaptionStyle(&l.Style); got.Animation != l.Style.Animation || got.LookID != l.ID {
			t.Errorf("%s: the sanitizer drops its own look (%s/%s)", l.ID, got.Animation, got.LookID)
		}
		if l.Anon {
			continue
		}
		to := CaptionAnonNearestLook[l.ID]
		if n, ok := CaptionLookByID(to); !ok || !n.Anon {
			t.Errorf("locked look %s has no anonymous nearest (%q)", l.ID, to)
		}
	}
	for _, a := range []string{CaptionAnimationBounce, CaptionAnimationWordByWord, CaptionAnimationBoxHighlight, CaptionAnimationFade, CaptionAnimationTypewriter, CaptionAnimationGlow} {
		if !used[a] {
			t.Errorf("no look uses the MM122 animation %s", a)
		}
	}
	for _, c := range []struct{ look, anim, wantLook, wantAnim string }{
		{"bounce_kids", "bounce", "bold_pop", "pop"},
		{"cinema", "fade", "minimal", "none"},
		{"box_pop", "box_highlight", "bold_pop", "highlight"},
		{"", "glow", "", "highlight"},
		{"", "typewriter", "", "highlight"},
	} {
		st := &StudioCaptionStyle{LookID: c.look, Animation: c.anim}
		ApplyAnonCaptionAllowlist(st)
		if st.LookID != c.wantLook || st.Animation != c.wantAnim {
			t.Errorf("%s/%s → %s/%s, want %s/%s", c.look, c.anim, st.LookID, st.Animation, c.wantLook, c.wantAnim)
		}
	}
}

// MM122: the per-word editor's original-times pair survives the sanitizer
// (clamped into the cue), a lone half is dropped, and a pre-MM122 word is
// byte-stable (no orig fields appear).
func TestSanitizeCaptionWordOrigTimes(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cue := StudioCaptionCue{ID: "c", StartSeconds: 1, EndSeconds: 3, Text: "a b", Words: []StudioCaptionWord{
		{Text: "a", StartSeconds: 1.5, EndSeconds: 2, OrigStartSeconds: f(0.2), OrigEndSeconds: f(1.4)},
		{Text: "b", StartSeconds: 2, EndSeconds: 3, OrigStartSeconds: f(2.1)},
	}}
	out := SanitizeCaptions([]StudioCaptionCue{cue})[0].Words
	if out[0].OrigStartSeconds == nil || *out[0].OrigStartSeconds != 1 || *out[0].OrigEndSeconds != 1.4 {
		t.Errorf("orig pair not clamped into the cue: %+v", out[0])
	}
	if out[1].OrigStartSeconds != nil || out[1].OrigEndSeconds != nil {
		t.Errorf("a lone orig half must drop: %+v", out[1])
	}
	plain := SanitizeCaptions([]StudioCaptionCue{{ID: "p", StartSeconds: 0, EndSeconds: 1, Text: "x", Words: []StudioCaptionWord{{Text: "x", StartSeconds: 0, EndSeconds: 1}}}})
	if b, _ := json.Marshal(plain[0].Words[0]); string(b) != `{"text":"x","startSeconds":0,"endSeconds":1}` {
		t.Errorf("a plain word must marshal unchanged: %s", b)
	}
}
