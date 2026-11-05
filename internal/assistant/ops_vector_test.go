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

// MM110: the embedded Motion op schema (a byte copy of the UI's
// lib/assistant/ops/schema/vector.json) validates apply_edits before streaming.

func TestVectorOpsSchemaAcceptsAndRefuses(t *testing.T) {
	v := NewOpsValidator()
	if !v.Has(SurfaceVector) {
		t.Fatal("vector op schema missing")
	}
	good := map[string]string{
		"template":      `[{"op":"template","kind":"lowerThird","text":["Jo Park","Producer"],"colors":["#ff5a36","#ffffff"],"at":0}]`,
		"animate":       `[{"op":"animate","layerId":"headline","property":"transform.scaleX","keyframes":[{"t":0,"value":0,"easing":"easeOut"},{"t":280,"value":1.1,"easing":{"cubicBezier":[0.3,0,0.2,1]}},{"t":420,"value":1,"easing":"linear"}]}]`,
		"colour track":  `[{"op":"animate","layerId":"a","property":"fill","keyframes":[{"t":0,"value":"#fff","easing":"linear"},{"t":500,"value":"rgb(255, 90, 54)","easing":"linear"}]}]`,
		"animator":      `[{"op":"apply_animator","layerIds":["logo"],"animator":"drawOn","span":{"start":0,"end":1400,"easing":"easeBoth"}}]`,
		"motion path":   `[{"op":"apply_animator","layerIds":["dot"],"animator":"motionPath","span":{"start":0,"end":2000},"options":{"pathId":"guide","autoOrient":true}}]`,
		"add rect":      `[{"op":"add_layer","type":"rect","props":{"x":0,"y":400,"width":300,"height":60,"rx":8,"fill":"#ff5a36","transform":{"rotation":5}}}]`,
		"add text":      `[{"op":"add_text","text":"Jo Park","x":40,"y":450,"size":32,"weight":"700","anchor":"start"}]`,
		"update":        `[{"op":"update_layer","layerId":"headline","patch":{"fill":"orange","fontSize":64}}]`,
		"import":        `[{"op":"import_svg","markup":"<svg xmlns=\"http://www.w3.org/2000/svg\"><path d=\"M0 0L1 1\"/></svg>"}]`,
		"timing":        `[{"op":"set_duration","ms":6000},{"op":"set_loop","loop":true},{"op":"set_canvas","width":1080,"height":1920}]`,
		"delete":        `[{"op":"delete_layer","layerId":"old"}]`,
		"counter value": `[{"op":"template","kind":"counter","value":10000,"text":"users","duration":1800}]`,
	}
	for name, ops := range good {
		if err := v.Validate(SurfaceVector, json.RawMessage(ops)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"unknown op":         `[{"op":"make_it_pop"}]`,
		"bad property":       `[{"op":"animate","layerId":"a","property":"transform.zoom","keyframes":[{"t":0,"value":0,"easing":"linear"},{"t":1,"value":1,"easing":"linear"}]}]`,
		"one keyframe":       `[{"op":"animate","layerId":"a","property":"opacity","keyframes":[{"t":0,"value":0,"easing":"linear"}]}]`,
		"bad easing":         `[{"op":"animate","layerId":"a","property":"opacity","keyframes":[{"t":0,"value":0,"easing":"bounce"},{"t":1,"value":1,"easing":"linear"}]}]`,
		"unknown template":   `[{"op":"template","kind":"explosion"}]`,
		"unknown animator":   `[{"op":"apply_animator","layerIds":["a"],"animator":"explode","span":{"start":0,"end":500}}]`,
		"url paint":          `[{"op":"add_layer","type":"rect","props":{"x":0,"y":0,"width":1,"height":1,"fill":"url(#evil)"}}]`,
		"model-set id":       `[{"op":"add_layer","type":"rect","props":{"id":"x","x":0,"y":0,"width":1,"height":1}}]`,
		"raw layer":          `[{"op":"add_layer","type":"raw","props":{"markup":"<g/>"}}]`,
		"image layer":        `[{"op":"add_layer","type":"image","props":{"x":0,"y":0,"width":1,"height":1}}]`,
		"attrs patch":        `[{"op":"update_layer","layerId":"a","patch":{"attrs":{"onclick":"x"}}}]`,
		"script in path":     `[{"op":"add_layer","type":"path","props":{"d":"M0 0<script>"}}]`,
		"negative time":      `[{"op":"apply_animator","layerIds":["a"],"animator":"blur","span":{"start":-1,"end":500}}]`,
		"empty":              `[]`,
		"too many ops (25)":  "[" + strings.TrimSuffix(strings.Repeat(`{"op":"set_loop","loop":true},`, 25), ",") + "]",
		"bad layer id":       `[{"op":"delete_layer","layerId":"../x"}]`,
		"extra field":        `[{"op":"set_loop","loop":true,"speed":2}]`,
		"duration too short": `[{"op":"set_duration","ms":5}]`,
	}
	for name, ops := range bad {
		if err := v.Validate(SurfaceVector, json.RawMessage(ops)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// apply_edits is offered on vector with the compact schema inlined.
func TestVectorOpsSchemaInTool(t *testing.T) {
	v := NewOpsValidator()
	if bytes.Contains(v.Schema(SurfaceVector), []byte("\n")) {
		t.Fatal("inlined schema keeps its indentation")
	}
	for _, td := range ToolsFor(SurfaceVector, v) {
		if td.Name == ToolApplyEdits {
			if err := validateArgs(td.Parameters, `{"summary":"Title pop","reason":"Because the digest shows it","ops":[{"op":"template","kind":"titlePop","text":"Hi"}]}`); err != nil {
				t.Fatalf("apply_edits refuses a valid call: %v", err)
			}
			if err := validateArgs(td.Parameters, `{"summary":"x","reason":"Because the digest shows it","ops":[{"op":"template","kind":"nope"}]}`); err == nil {
				t.Fatal("apply_edits accepts an invalid op")
			}
			return
		}
	}
	t.Fatal("apply_edits is not offered on vector")
}

// Byte identity with the UI's generated copy. Skipped when the sibling UI
// checkout is absent (a lone API clone).
func TestVectorOpsSchemaMatchesUI(t *testing.T) {
	ui := os.Getenv("MEDI_UI_OPS_SCHEMA_DIR")
	if ui == "" {
		ui = filepath.Join("..", "..", "..", "media-manipulator-ui", "lib", "assistant", "ops", "schema")
	}
	want, err := os.ReadFile(filepath.Join(ui, "vector.json"))
	if err != nil {
		t.Skipf("UI copy not found (%s): run `npm run export:ops` in media-manipulator-ui to check", ui)
	}
	got, err := os.ReadFile(filepath.Join("ops", "schema", "vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("internal/assistant/ops/schema/vector.json differs from the UI's copy: run `npm run export:ops` in media-manipulator-ui")
	}
}

// Every embedded Motion recipe's op example passes the schema the model is held to.
func TestVectorRecipesMatchOpSchema(t *testing.T) {
	v := NewOpsValidator()
	k, err := NewEmbeddedKnowledge()
	if err != nil {
		t.Fatal(err)
	}
	fence := regexp.MustCompile("(?s)```json\\n(.*?)\\n```")
	n := 0
	for _, d := range k.Corpus().Docs {
		if !strings.HasPrefix(d.ID, "recipes/vector/") {
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
		if err := v.Validate(SurfaceVector, ex.Ops); err != nil {
			t.Errorf("%s: %v", d.ID, err)
		}
		n++
	}
	if n < 14 {
		t.Fatalf("only %d vector recipes with ops checked", n)
	}
}

// The Motion surface intro names the ops (the cached block is stable bytes).
func TestVectorSurfaceIntro(t *testing.T) {
	intro := surfaceIntro[SurfaceVector]
	for _, op := range []string{"template", "animate", "apply_animator", "import_svg", "set_duration", "suggest_action"} {
		if !strings.Contains(intro, op) {
			t.Errorf("vector intro does not mention %s", op)
		}
	}
}
