package assistant

import (
	"context"
	"strings"
	"testing"
)

// MM107: the spend-cap answer and the out-of-scope line exist for all 18 site
// locales, differ from English outside English, and keep the /tools hint.
func TestStaticLinesAllLocales(t *testing.T) {
	enIntro, enNone := StaticFrame("en-US")
	enOOS := OutOfScopeLine("en-US")
	for _, tag := range []string{"uk-UA", "ru-RU", "he-IL", "de-DE", "es-ES", "es-MX", "fr-FR", "it-IT", "ja-JP",
		"zh-CN", "zh-TW", "ko-KR", "ar-SA", "hi-IN", "pt-PT", "pt-BR", "vi-VN"} {
		intro, none := StaticFrame(tag)
		if intro == enIntro || none == enNone || OutOfScopeLine(tag) == enOOS {
			t.Errorf("%s falls back to English", tag)
		}
		if !strings.Contains(none, "/tools") {
			t.Errorf("%s: none line lost the /tools hint: %q", tag, none)
		}
		if !strings.Contains(OutOfScopeLine(tag), "Medi") {
			t.Errorf("%s: out-of-scope line does not name Medi", tag)
		}
	}
	if a, _ := StaticFrame("es-MX"); a == func() string { s, _ := StaticFrame("es-ES"); return s }() {
		t.Error("es-MX must use its own Latin-American line")
	}
	if a, _ := StaticFrame("pt-BR"); a == func() string { s, _ := StaticFrame("pt-PT"); return s }() {
		t.Error("pt-BR must differ from pt-PT")
	}
	if a, _ := StaticFrame("zh-TW"); a == func() string { s, _ := StaticFrame("zh-CN"); return s }() {
		t.Error("zh-TW must differ from zh-CN")
	}
	if a, _ := StaticFrame("xx-YY"); a != enIntro {
		t.Error("unknown locales fall back to English")
	}
}

// MM107: the turn report carries the caller's tier and analytics ids (for the
// server event) and nothing else from the request.
func TestEngineReportCarriesIdentity(t *testing.T) {
	v := newEnv(t, round{deltas: []string{"Open Compress video."}})
	var got []TurnReport
	v.e.OnTurnCounted = func(r TurnReport) { got = append(got, r) }
	tr := v.turn(guideMsg("compress a video"), "")
	tr.Live.Tier = "free"
	tr.AnalyticsVisitorID, tr.AnalyticsSessionID = "vis", "ses"
	v.e.Run(context.Background(), tr, v.em, nil)
	if len(got) != 1 {
		t.Fatalf("reports = %d", len(got))
	}
	r := got[0]
	if r.Surface != "guide" || r.ModelTier != "guide" || r.Outcome != "text" || r.Tier != "free" || r.VisitorID != "vis" || r.SessionID != "ses" {
		t.Errorf("report = %+v", r)
	}
}
