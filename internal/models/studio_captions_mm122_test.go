package models

import "testing"

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
