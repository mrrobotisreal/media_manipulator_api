package assistant

import (
	"errors"
	"strings"
	"testing"
)

type fakeExports struct {
	tiers []string
	err   error
}

func (f *fakeExports) AdviseExport(tier string, params map[string]any) (map[string]any, error) {
	f.tiers = append(f.tiers, tier)
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]any{"preset": "mp4_h264", "container": "mp4", "upgrade": map[string]any{"required": "premium", "option": params["container"]}}
	return out, nil
}

func studioMsg(text string) ChatBody {
	return ChatBody{Surface: "studio", Locale: "en-US", Page: PageRef{Path: "/studio/p1"}, Message: &MessageIn{Text: text},
		Context: &ContextIn{Digest: "V1 clipA", ProjectID: "p1"}}
}

// MM118: a studio export suggestion goes through the advisor with the turn's
// plan before it streams; the streamed params are the advisor's, never the
// model's.
func TestEngineStudioExportSuggestionIsAdvised(t *testing.T) {
	v := newEnv(t, round{calls: []ToolCall{call("x1", ToolSuggestAction, `{"kind":"export","label":"Export for Final Cut","params":{"container":"mov"}}`)}})
	fx := &fakeExports{}
	v.e.Exports = fx
	turn := v.turn(studioMsg("export for final cut"), "user-1")
	turn.Live.Tier = "free"
	v.run(turn, nil)
	var got map[string]any
	for _, ev := range v.em.events {
		if ev["type"] == "action" {
			got = ev
		}
	}
	if got == nil || got["kind"] != "export" {
		t.Fatalf("no export action: %v", v.em.events)
	}
	p, _ := got["params"].(map[string]any)
	if p["container"] != "mp4" || p["preset"] != "mp4_h264" {
		t.Errorf("params = %v (must be the advisor's)", p)
	}
	if up, _ := p["upgrade"].(map[string]any); up["required"] != "premium" {
		t.Errorf("upgrade = %v", p["upgrade"])
	}
	if len(fx.tiers) != 1 || fx.tiers[0] != "free" {
		t.Errorf("advisor saw tiers %v", fx.tiers)
	}
}

// An invalid export goes back to the model as its one retry, like open_tool.options.
func TestEngineStudioExportInvalidIsRetried(t *testing.T) {
	v := newEnv(t,
		round{calls: []ToolCall{call("x1", ToolSuggestAction, `{"kind":"export","label":"Export","params":{"container":"mov","audioCodec":"opus"}}`)}},
		round{deltas: []string{"Here is the export."}, calls: []ToolCall{call("x2", ToolSuggestAction, `{"kind":"export","label":"Export","params":{"preset":"mov_h264"}}`)}},
	)
	calls := 0
	v.e.Exports = advisorFunc(func(tier string, params map[string]any) (map[string]any, error) {
		calls++
		if params["audioCodec"] == "opus" {
			return nil, errors.New("invalid export: MOV cannot carry opus audio")
		}
		return params, nil
	})
	v.run(v.turn(studioMsg("export a mov with opus"), "user-1"), nil)
	reqs := v.p.requests()
	// requests: the first round, the retry round (and the conversation title).
	if len(reqs) < 2 || calls != 2 {
		t.Fatalf("want a retry round: %d requests, %d advisor calls", len(reqs), calls)
	}
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	if !strings.Contains(last.Text, "MOV cannot carry opus") {
		t.Errorf("retry tool result = %q", last.Text)
	}
}

// Other surfaces and other kinds never reach the advisor.
func TestEngineExportAdvisorOnlyForStudioExport(t *testing.T) {
	v := newEnv(t, round{calls: []ToolCall{call("a1", ToolSuggestAction, `{"kind":"subject_mask","label":"Select"}`)}})
	fx := &fakeExports{err: errors.New("must not be called")}
	v.e.Exports = fx
	v.run(v.turn(photoMsg("select the person"), "user-1"), nil)
	if len(fx.tiers) != 0 {
		t.Errorf("advisor called for %v", fx.tiers)
	}
}

type advisorFunc func(string, map[string]any) (map[string]any, error)

func (f advisorFunc) AdviseExport(t string, p map[string]any) (map[string]any, error) { return f(t, p) }

// The studio surface line teaches the export params and moves the prompt version.
func TestStudioIntroTeachesExportParams(t *testing.T) {
	intro := surfaceIntro[SurfaceStudio]
	for _, want := range []string{"mov_h264", "mkv_hevc_aac", "audio_wav", "container (mp4 | mov | mkv | webm | mp3 | wav | opus | flac)", "audioBitrate", "captionsMode", "the user presses Export", "Premium",
		// MM119: the new rows, the MOV/Opus refusal and embedded vs burn-in.
		"webm_vp9_opus", "audio_opus", "mkv_hevc_opus", "vp9", "vorbis", "embedded", "MOV never carries Opus", "plain text", "sidecar",
		// MM120: the Pro rows, their destinations, the AV1 warning and the Pro plan.
		"mov_prores", "mp4_av1", "webm_av1_opus", "audio_flac", "audio_wav24", "older devices may not play AV1", "ProRes is MOV only", "need Pro"} {
		if !strings.Contains(intro, want) {
			t.Errorf("studio intro lacks %q", want)
		}
	}
}
