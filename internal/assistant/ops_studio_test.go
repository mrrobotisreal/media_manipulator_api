package assistant

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// MM109: the embedded Darkroom op schema (a byte copy of the UI's
// lib/assistant/ops/schema/studio.json) validates apply_edits before streaming.

func TestStudioOpsSchemaAcceptsAndRefuses(t *testing.T) {
	v := NewOpsValidator()
	good := map[string]string{
		"look":        `[{"op":"apply_look","clipIds":"all","lookId":"cinematic","intensity":0.8}]`,
		"grade":       `[{"op":"grade","clipIds":["clipA"],"lumetri":{"temperature":20,"saturation":0.9}}]`,
		"lower third": `[{"op":"add_title","template":"lowerThird","lines":["Sam Lee","Designer"],"start":2,"duration":5,"style":{"fontFamily":"Montserrat","backgroundOpacity":0.6},"animateIn":"slideLeft","animateOut":"fade"}]`,
		"transition":  `[{"op":"set_transition","clipId":"clipB","type":"crossDissolve","durationSeconds":0.8}]`,
		"keyframes":   `[{"op":"set_keyframes","clipId":"clipA","property":"scale","keyframes":[{"t":0,"value":1,"ease":"easeBoth"},{"t":12,"value":1.15,"ease":"linear"}]}]`,
		"effect lane": `[{"op":"set_keyframes","clipId":"clipA","property":"fx1.exposure","keyframes":[{"t":0,"value":0,"ease":"linear"}]}]`,
		"audio":       `[{"op":"audio_preset","track":"A1","preset":"podcast"},{"op":"set_ducking","enabled":true,"voiceTrack":"A1","amountDb":12}]`,
		"volume clip": `[{"op":"set_volume","clipId":"music1","gainDb":-6}]`,
		"volume trk":  `[{"op":"set_volume","track":"A2","gainDb":-6}]`,
		"cut":         `[{"op":"split","clipId":"clipA","at":5},{"op":"trim","clipId":"clipB","sourceIn":4},{"op":"delete_clip","clipId":"clipA","ripple":true}]`,
		"misc":        `[{"op":"set_speed","clipId":"clipB","speed":0.5},{"op":"freeze_frame","clipId":"clipB","at":14,"duration":2},{"op":"add_marker","at":10,"label":"b-roll"},{"op":"set_canvas","aspect":"9:16"},{"op":"move_clip","clipId":"clipB","timelineStart":30,"track":"V1"}]`,
		"captions":    `[{"op":"set_caption_style","fontSizePct":6.5,"position":"bottom"}]`,
	}
	for name, ops := range good {
		if err := v.Validate("studio", json.RawMessage(ops)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"unknown op":       `[{"op":"make_cinematic"}]`,
		"unknown lumetri":  `[{"op":"grade","clipIds":"all","lumetri":{"highlights":-20}}]`,
		"out of range":     `[{"op":"set_speed","clipId":"c","speed":20}]`,
		"unknown look":     `[{"op":"apply_look","clipIds":"all","lookId":"sepia"}]`,
		"unknown wipe":     `[{"op":"set_transition","clipId":"c","type":"zoomBlur","durationSeconds":1}]`,
		"bad font":         `[{"op":"add_title","template":"title","lines":["x"],"start":0,"duration":3,"style":{"fontFamily":"Comic Sans"}}]`,
		"bad lane":         `[{"op":"set_keyframes","clipId":"c","property":"fx1.glow","keyframes":[{"t":0,"value":1,"ease":"linear"}]}]`,
		"volume both":      `[{"op":"set_volume","clipId":"c","track":"A1","gainDb":0}]`,
		"volume neither":   `[{"op":"set_volume","gainDb":0}]`,
		"empty trim":       `[{"op":"trim","clipId":"c"}]`,
		"empty captions":   `[{"op":"set_caption_style"}]`,
		"bad aspect":       `[{"op":"set_canvas","aspect":"21:9"}]`,
		"extra field":      `[{"op":"add_marker","at":1,"label":"x","color":"#fff000"}]`,
		"empty list":       `[]`,
		"bad clip id":      `[{"op":"split","clipId":"../x","at":1}]`,
		"captions not ops": `[{"op":"generate_captions"}]`,
	}
	for name, ops := range bad {
		if err := v.Validate("studio", json.RawMessage(ops)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStudioOpsInApplyEdits(t *testing.T) {
	v := NewOpsValidator()
	for _, td := range ToolsFor("studio", v) {
		if td.Name == ToolApplyEdits {
			if err := validateArgs(td.Parameters, `{"summary":"Cinematic grade","reason":"Because the digest shows it","ops":[{"op":"apply_look","clipIds":"all","lookId":"cinematic"}]}`); err != nil {
				t.Fatalf("apply_edits refuses a valid call: %v", err)
			}
			if err := validateArgs(td.Parameters, `{"summary":"x","reason":"Because the digest shows it","ops":[{"op":"apply_look","clipIds":"all","lookId":"sepia"}]}`); err == nil {
				t.Fatal("apply_edits accepts an invalid op")
			}
			return
		}
	}
	t.Fatal("apply_edits is not offered on studio")
}

// Byte identity with the UI's generated copy. Skipped without the sibling UI checkout.
func TestStudioOpsSchemaMatchesUI(t *testing.T) {
	dir := os.Getenv("MEDI_UI_OPS_SCHEMA_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "media-manipulator-ui", "lib", "assistant", "ops", "schema")
	}
	want, err := os.ReadFile(filepath.Join(dir, "studio.json"))
	if err != nil {
		t.Skipf("UI copy not found (%s): run `npm run export:ops` in media-manipulator-ui to check", dir)
	}
	got, err := os.ReadFile(filepath.Join("ops", "schema", "studio.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("internal/assistant/ops/schema/studio.json differs from the UI's lib/assistant/ops/schema/studio.json: run `npm run export:ops` in media-manipulator-ui")
	}
}

// Every embedded Darkroom recipe's op example passes the schema the model is held to.
func TestStudioRecipesMatchOpSchema(t *testing.T) {
	v := NewOpsValidator()
	k, err := NewEmbeddedKnowledge()
	if err != nil {
		t.Fatal(err)
	}
	fence := regexp.MustCompile("(?s)```json\\n(.*?)\\n```")
	n := 0
	for _, d := range k.Corpus().Docs {
		if !strings.HasPrefix(d.ID, "recipes/studio/") {
			continue
		}
		m := fence.FindStringSubmatch(d.Body)
		if m == nil {
			continue
		}
		var ex struct {
			Ops json.RawMessage `json:"ops"`
		}
		if err := json.Unmarshal([]byte(m[1]), &ex); err != nil {
			t.Errorf("%s: %v", d.ID, err)
			continue
		}
		if err := v.Validate("studio", ex.Ops); err != nil {
			t.Errorf("%s: %v", d.ID, err)
		}
		n++
	}
	if n < 20 {
		t.Fatalf("only %d studio recipes with ops checked", n)
	}
}
