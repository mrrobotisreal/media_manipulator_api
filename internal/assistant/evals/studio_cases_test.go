package evals

import (
	"encoding/json"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

// MM109: every Darkroom case can be passed by a schema-valid op list against the
// fixture timeline (studio/two-clips.txt: V1 clipA beach 0–12 s, clipB city
// 12–24 s; A1 voice; A2 music), so a failing eval means the model chose badly,
// not that the case asks for something the vocabulary cannot express.
var studioReference = map[string]struct {
	text    string
	ops     string
	ask     bool
	actions []Action
}{
	"studio-cinematic":        {"A cinematic look on both clips.", `[{"op":"apply_look","clipIds":"all","lookId":"cinematic","intensity":0.8}]`, false, nil},
	"studio-lower-third":      {"Added a lower-third at 2 s that slides in.", `[{"op":"add_title","template":"lowerThird","lines":["Sam Lee","Designer"],"start":2,"duration":5,"animateIn":"slideLeft","animateOut":"fade"}]`, false, nil},
	"studio-title-card":       {"A big title over the start.", `[{"op":"add_title","template":"title","lines":["Summer"],"start":0,"duration":3,"animateIn":"fade"}]`, false, nil},
	"studio-duck":             {"The music now ducks under your voice.", `[{"op":"set_ducking","enabled":true,"voiceTrack":"A1","amountDb":12}]`, false, nil},
	"studio-voice-clear":      {"Voice clarity on the voice track.", `[{"op":"audio_preset","track":"A1","preset":"voiceClarity"}]`, false, nil},
	"studio-transition":       {"A cross dissolve between the clips.", `[{"op":"set_transition","clipId":"clipB","type":"crossDissolve","durationSeconds":0.8}]`, false, nil},
	"studio-vertical":         {"The canvas is vertical now.", `[{"op":"set_canvas","aspect":"9:16"}]`, false, nil},
	"studio-fade-in":          {"Fades in from black over a second.", `[{"op":"set_keyframes","clipId":"clipA","property":"opacity","keyframes":[{"t":0,"value":0,"ease":"easeOut"},{"t":1,"value":1,"ease":"linear"}]}]`, false, nil},
	"studio-slowmo":           {"The city clip plays at half speed.", `[{"op":"set_speed","clipId":"clipB","speed":0.5,"maintainPitch":true}]`, false, nil},
	"studio-ken-burns":        {"A slow push-in on the beach.", `[{"op":"set_keyframes","clipId":"clipA","property":"scale","keyframes":[{"t":0,"value":1,"ease":"easeBoth"},{"t":12,"value":1.15,"ease":"linear"}]}]`, false, nil},
	"studio-delete-asks":      {"Delete the city clip from 12 to 24 s?", "", true, nil},
	"studio-captions-suggest": {"Captions come from a transcription, which uses one AI edit.", "", false, []Action{{Kind: "generate_captions"}}},
	"studio-warmer":           {"Warmed the beach clip.", `[{"op":"grade","clipIds":["clipA"],"lumetri":{"temperature":20}}]`, false, nil},
	"studio-marker":           {"Marker added at 10 s.", `[{"op":"add_marker","at":10,"label":"b-roll here"}]`, false, nil},
	"studio-de-grade":         {"Die Farben sind jetzt kräftiger.", `[{"op":"grade","clipIds":"all","lumetri":{"saturation":1.2,"vibrance":0.3}}]`, false, nil},
	// MM118 export suggestions: the params as the server's export advisor returns them.
	"studio-export-reel":      {"A vertical MP4 in H.264 plays everywhere on Instagram.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mp4_h264", "container": "mp4", "videoCodec": "h264", "audioCodec": "aac"}}}},
	"studio-export-final-cut": {"MOV opens straight in Final Cut.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mov_h264", "container": "mov", "videoCodec": "h264", "audioCodec": "aac"}}}},
	"studio-export-smallest":  {"H.265 in MP4 is about half the size at the same quality.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mp4_hevc", "container": "mp4", "videoCodec": "hevc", "audioCodec": "aac"}}}},
	"studio-export-anon-hevc": {"H.265 needs a free account: sign up and it unlocks. For now the dialog has H.264.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mp4_h264", "container": "mp4", "videoCodec": "h264", "audioCodec": "aac", "upgrade": map[string]any{"required": "free", "option": "hevc"}}}}},
	// MM119 export suggestions.
	"studio-export-website-embed": {"WebM with VP9 and Opus at 96 kbps is the web's own pair and the smallest file for an embed.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "webm_vp9_opus", "container": "webm", "videoCodec": "vp9", "audioCodec": "opus", "audioBitrate": 96}}}},
	"studio-export-small-audio":   {"Opus is the smallest audio file at the same quality: an .opus at 96 kbps.", "", false, []Action{{Kind: "export", Params: map[string]any{"container": "opus", "videoCodec": "none", "audioCodec": "opus", "audioBitrate": 96}}}},
	"studio-export-opus-mov":      {"MOV can't carry Opus, so QuickTime-style MOV is out: MKV keeps Opus with your video.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mkv_hevc_opus", "container": "mkv", "videoCodec": "hevc", "audioCodec": "opus"}}}},
	"studio-export-toggle-subs":   {"An embedded captions track viewers can switch on and off; it is plain text, so styling only survives burn-in.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mp4_h264", "container": "mp4", "videoCodec": "h264", "audioCodec": "aac", "captionsMode": "embedded"}}}},
	"studio-export-free-webm":     {"WebM is a Premium format; the dialog has MP4, the closest your plan allows.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mp4_h264", "container": "mp4", "videoCodec": "h264", "audioCodec": "aac", "upgrade": map[string]any{"required": "premium", "option": "webm"}}}}},
	// MM120 Pro formats.
	"studio-export-premium-prores":   {"ProRes is a Pro format; MOV with H.265 is the closest on Premium and Resolve opens it.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mov_hevc", "container": "mov", "videoCodec": "hevc", "audioCodec": "aac", "upgrade": map[string]any{"required": "pro", "option": "prores"}}}}},
	"studio-export-pro-archive":      {"AV1 in MP4 keeps the most quality for its size; older devices may not play it, and ProRes is far larger.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mp4_av1", "container": "mp4", "videoCodec": "av1", "audioCodec": "aac"}}}},
	"studio-export-pro-music-master": {"FLAC is lossless and half the size of a 24-bit WAV.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "audio_flac", "container": "flac", "videoCodec": "none", "audioCodec": "flac"}}}},
	"studio-export-free-mov":         {"MOV is a Premium format; the dialog has MP4, the closest your plan allows.", "", false, []Action{{Kind: "export", Params: map[string]any{"preset": "mp4_h264", "container": "mp4", "videoCodec": "h264", "audioCodec": "aac", "upgrade": map[string]any{"required": "premium", "option": "mov"}}}}},
	// MM121 caption looks (the fixture has captions; the anonymous case may answer without an edit).
	"studio-captions-tiktok":              {"Bold Pop: big uppercase words that pop as they're spoken.", `[{"op":"set_caption_style","look":"bold_pop","position":"middle"}]`, false, nil},
	"studio-captions-karaoke":             {"Karaoke Classic fills each word as it is sung.", `[{"op":"set_caption_style","look":"karaoke_classic"}]`, false, nil},
	"studio-captions-calm":                {"The Podcast look: calm lines in a soft box, no animation.", `[{"op":"set_caption_style","look":"podcast"}]`, false, nil},
	"studio-captions-current-word-yellow": {"The word being spoken now turns yellow.", `[{"op":"set_caption_style","animation":"highlight","highlightColor":"#FFD400"}]`, false, nil},
	"studio-captions-anon-locked-look":    {"Neon needs a free account: sign up and it unlocks. Meanwhile I applied Bold Pop, which guests can use.", `[{"op":"set_caption_style","look":"bold_pop"}]`, false, nil},
}

func TestStudioCasesPassableWithRealOps(t *testing.T) {
	cases, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	v := assistant.NewOpsValidator()
	if !v.Has("studio") {
		t.Fatal("studio op schema missing")
	}
	seen := 0
	for _, c := range cases {
		if c.Surface != "studio" || c.Adjust != nil {
			continue
		}
		seen++
		ref, ok := studioReference[c.ID]
		if !ok {
			t.Errorf("%s: no reference answer (add one when you add a studio case)", c.ID)
			continue
		}
		out := Output{Text: ref.text, Actions: ref.actions, Model: "reference"}
		if ref.ask {
			out.Asks = []string{ref.text}
		}
		if ref.ops != "" {
			if err := v.Validate("studio", json.RawMessage(ref.ops)); err != nil {
				t.Errorf("%s: reference ops fail the schema: %v", c.ID, err)
				continue
			}
			var ops []map[string]any
			if err := json.Unmarshal([]byte(ref.ops), &ops); err != nil {
				t.Fatal(err)
			}
			out.Edits = []Edit{{Summary: "ref", Reason: "You asked: " + c.Message, Ops: ops}}
		}
		if r := Grade(c, out); !r.Pass {
			t.Errorf("%s: a schema-valid reference answer fails the grader: %v", c.ID, r.Failures)
		}
	}
	if seen < 15 {
		t.Fatalf("studio cases = %d, want >= 15", seen)
	}
}
