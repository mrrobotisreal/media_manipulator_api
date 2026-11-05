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

// MM108: the embedded Aperture op schema (a byte copy of the UI's
// lib/assistant/ops/schema/photo.json) validates apply_edits before streaming.

func TestPhotoOpsSchemaAcceptsAndRefuses(t *testing.T) {
	v := NewOpsValidator()
	good := map[string]string{
		"grey except":       `[{"op":"grey_except","ranges":["red",{"hue":185,"width":40}]}]`,
		"add with mask":     `[{"op":"add_layer","type":"exposure","params":{"ev":0.6},"mask":{"components":[{"kind":"radial","center":{"x":0.5,"y":0.42},"rx":0.28,"ry":0.38,"feather":0.6}]}}]`,
		"update":            `[{"op":"update_layer","layerId":"exp1","params":{"ev":0.1},"opacity":0.8}]`,
		"preset":            `[{"op":"apply_preset","presetId":"vintage","intensity":0.8}]`,
		"auto":              `[{"op":"auto_tone"}]`,
		"geometry":          `[{"op":"set_geometry","straighten":-2,"crop":{"aspect":"4:5"}}]`,
		"crop reset":        `[{"op":"set_geometry","crop":null}]`,
		"set mask":          `[{"op":"set_mask","layerId":"a","components":[{"kind":"gradient","from":{"x":0.5,"y":0.55},"to":{"x":0.5,"y":0.1}},{"kind":"luminosity","lo":0.7,"hi":1}],"combine":"intersect","feather":10}]`,
		"move and remove":   `[{"op":"move_layer","layerId":"a","toIndex":0},{"op":"remove_layer","layerId":"b"}]`,
		"curves nested obj": `[{"op":"add_layer","type":"curves","params":{"master":{"points":[{"x":0,"y":0},{"x":0.25,"y":0.2},{"x":0.75,"y":0.8},{"x":1,"y":1}]}}}]`,
	}
	for name, ops := range good {
		if err := v.Validate("photo", json.RawMessage(ops)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"unknown op":        `[{"op":"make_cinematic"}]`,
		"out of range":      `[{"op":"add_layer","type":"exposure","params":{"ev":9}}]`,
		"unknown param":     `[{"op":"add_layer","type":"tone","params":{"brightness":10}}]`,
		"unknown preset":    `[{"op":"apply_preset","presetId":"teal-orange"}]`,
		"brush mask":        `[{"op":"set_mask","layerId":"a","components":[{"kind":"brush","png":"data:image/png;base64,","width":1,"height":1}]}]`,
		"subject mask":      `[{"op":"set_mask","layerId":"a","components":[{"kind":"subject","maskRef":"masks/a.png"}]}]`,
		"lut cube text":     `[{"op":"add_layer","type":"lut","params":{"cube":"LUT_3D_SIZE 2"}}]`,
		"empty list":        `[]`,
		"quarter turns":     `[{"op":"set_geometry","rotate":45}]`,
		"empty geometry":    `[{"op":"set_geometry"}]`,
		"extra op field":    `[{"op":"auto_tone","strength":1}]`,
		"bad layer id":      `[{"op":"remove_layer","layerId":"../x"}]`,
		"bad grey range":    `[{"op":"grey_except","ranges":["crimson"]}]`,
		"too many ops (25)": "[" + strings.TrimSuffix(strings.Repeat(`{"op":"auto_tone"},`, 25), ",") + "]",
	}
	for name, ops := range bad {
		if err := v.Validate("photo", json.RawMessage(ops)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The schema is inlined into apply_edits compact (every byte is an input token).
func TestPhotoOpsSchemaCompactInTool(t *testing.T) {
	v := NewOpsValidator()
	raw := v.Schema("photo")
	if bytes.Contains(raw, []byte("\n")) {
		t.Fatal("inlined schema keeps its indentation")
	}
	for _, td := range ToolsFor("photo", v) {
		if td.Name == ToolApplyEdits {
			if err := validateArgs(td.Parameters, `{"summary":"Grey except red","ops":[{"op":"grey_except","ranges":["red"]}]}`); err != nil {
				t.Fatalf("apply_edits refuses a valid call: %v", err)
			}
			if err := validateArgs(td.Parameters, `{"summary":"x","ops":[{"op":"grey_except","ranges":["crimson"]}]}`); err == nil {
				t.Fatal("apply_edits accepts an invalid op")
			}
			return
		}
	}
	t.Fatal("apply_edits is not offered on photo")
}

// Byte identity with the UI's generated copy (the photo-fixtures pattern).
// Skipped when the sibling UI checkout is absent (a lone API clone).
func TestPhotoOpsSchemaMatchesUI(t *testing.T) {
	ui := os.Getenv("MEDI_UI_OPS_SCHEMA")
	if ui == "" {
		ui = filepath.Join("..", "..", "..", "media-manipulator-ui", "lib", "assistant", "ops", "schema", "photo.json")
	}
	want, err := os.ReadFile(ui)
	if err != nil {
		t.Skipf("UI copy not found (%s): run `npm run export:ops` in media-manipulator-ui to check", ui)
	}
	got, err := os.ReadFile(filepath.Join("ops", "schema", "photo.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("internal/assistant/ops/schema/photo.json differs from the UI's lib/assistant/ops/schema/photo.json: run `npm run export:ops` in media-manipulator-ui")
	}
}

// Every embedded Aperture recipe's op example passes the schema the model is held to.
func TestPhotoRecipesMatchOpSchema(t *testing.T) {
	v := NewOpsValidator()
	k, err := NewEmbeddedKnowledge()
	if err != nil {
		t.Fatal(err)
	}
	fence := regexp.MustCompile("(?s)```json\\n(.*?)\\n```")
	n := 0
	for _, d := range k.Corpus().Docs {
		id := d.ID
		if !strings.HasPrefix(id, "recipes/photo/") {
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
			t.Errorf("%s: %v", id, err)
			continue
		}
		if err := v.Validate("photo", ex.Ops); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		n++
	}
	if n < 25 {
		t.Fatalf("only %d photo recipes with ops checked", n)
	}
}

// The real embedded photo schema in the engine: a valid edit streams, an
// out-of-range one goes back to the model once (with the validator's reason)
// and then fails as invalid_ops without ever reaching the client.
func TestEngineRealPhotoSchema(t *testing.T) {
	good := call("e1", ToolApplyEdits, `{"summary":"Backlit fix","ops":[{"op":"add_layer","type":"tone","params":{"shadows":45,"highlights":-55}}]}`)
	v := newEnv(t, round{calls: []ToolCall{good}})
	v.e.Ops = NewOpsValidator()
	v.run(v.turn(photoMsg("it's backlit"), "user-1"), nil)
	if got := strings.Join(v.em.types(), ","); got != "meta,edits,usage,done" {
		t.Fatalf("valid edit: events = %s", got)
	}

	bad := call("e2", ToolApplyEdits, `{"summary":"Too bright","ops":[{"op":"add_layer","type":"exposure","params":{"ev":9}}]}`)
	v = newEnv(t, round{calls: []ToolCall{bad}}, round{calls: []ToolCall{bad}})
	v.e.Ops = NewOpsValidator()
	v.run(v.turn(photoMsg("brighter!"), "user-1"), nil)
	reqs := v.p.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(reqs))
	}
	retry := reqs[1].Messages[len(reqs[1].Messages)-1]
	if !strings.HasPrefix(retry.Text, "Error:") || !strings.Contains(retry.Text, "maximum") {
		t.Errorf("retry tool result should carry the schema's reason: %q", retry.Text)
	}
	if l := v.em.last(); l["code"] != CodeInvalid {
		t.Fatalf("last = %v", l)
	}
	for _, typ := range v.em.types() {
		if typ == "edits" {
			t.Fatal("an invalid edit reached the client")
		}
	}
}
