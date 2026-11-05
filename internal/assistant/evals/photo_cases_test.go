package evals

import (
	"encoding/json"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

// MM108: every photo case can be passed by a schema-valid op list, so a failing
// eval means the model chose badly, not that the case asks for ops the real
// Aperture vocabulary cannot express. The reference answers are hand-written
// professional edits; each must validate against the embedded op schema and
// pass the case's grader with no failures.
var photoReference = map[string]struct {
	text    string
	ops     string
	actions []Action
}{
	"photo-grey-except-red":      {"Everything is grey except the red.", `[{"op":"grey_except","ranges":["red"]}]`, nil},
	"photo-backlit":              {"I lifted the shadows and pulled the bright sky down.", `[{"op":"add_layer","type":"tone","params":{"shadows":45,"highlights":-55}},{"op":"add_layer","type":"exposure","params":{"ev":0.4},"mask":{"components":[{"kind":"radial","center":{"x":0.5,"y":0.45},"rx":0.3,"ry":0.4,"feather":0.6}]}}]`, nil},
	"photo-moody":                {"Darker shadows, less colour, a cool-warm split and a vignette.", `[{"op":"add_layer","type":"tone","params":{"highlights":-30,"blacks":-20}},{"op":"add_layer","type":"vibranceSaturation","params":{"saturation":-25}},{"op":"add_layer","type":"vignette","params":{"amount":-30}}]`, nil},
	"photo-brighter":             {"Brighter by about half a stop.", `[{"op":"add_layer","type":"exposure","params":{"ev":0.4}}]`, nil},
	"photo-vintage":              {"Vintage film look at 80 %.", `[{"op":"apply_preset","presetId":"vintage","intensity":0.8}]`, nil},
	"photo-bw":                   {"Black and white with a film mix.", `[{"op":"add_layer","type":"blackWhite","params":{"reds":50,"blues":10}}]`, nil},
	"photo-warmer":               {"Warmer white balance.", `[{"op":"add_layer","type":"whiteBalance","params":{"temperature":20}}]`, nil},
	"photo-cooler-fix-cast":      {"Cooled the yellow cast.", `[{"op":"add_layer","type":"whiteBalance","params":{"temperature":-18,"tint":4}}]`, nil},
	"photo-vignette":             {"A subtle vignette.", `[{"op":"add_layer","type":"vignette","params":{"amount":-25,"midpoint":45,"feather":65}}]`, nil},
	"photo-sharpen":              {"Light sharpening for the web.", `[{"op":"add_layer","type":"sharpen","params":{"amount":80,"radius":0.9,"threshold":4}}]`, nil},
	"photo-square-crop":          {"Cropped square.", `[{"op":"set_geometry","crop":{"aspect":"1:1"}}]`, nil},
	"photo-update-existing":      {"I toned the exposure down.", `[{"op":"update_layer","layerId":"exp1","params":{"ev":0.1}}]`, nil},
	"photo-heal-suggest":         {"Heal can remove that; it uses one AI edit.", "", []Action{{Kind: "heal"}}},
	"photo-subject-mask-suggest": {"An AI subject mask selects the person exactly; then I can blur the background.", "", []Action{{Kind: "subject_mask"}}},
	"photo-ru-moody":             {"Сделал фото теплее: баланс белого сдвинут в тёплую сторону.", `[{"op":"add_layer","type":"whiteBalance","params":{"temperature":20}}]`, nil},
}

func TestPhotoCasesPassableWithRealOps(t *testing.T) {
	cases, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	v := assistant.NewOpsValidator()
	if !v.Has("photo") {
		t.Fatal("photo op schema missing")
	}
	seen := 0
	for _, c := range cases {
		if c.Surface != "photo" || c.Adjust != nil {
			continue
		}
		seen++
		ref, ok := photoReference[c.ID]
		if !ok {
			t.Errorf("%s: no reference answer (add one when you add a photo case)", c.ID)
			continue
		}
		out := Output{Text: ref.text, Actions: ref.actions, Model: "reference"}
		if ref.ops != "" {
			if err := v.Validate("photo", json.RawMessage(ref.ops)); err != nil {
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
		t.Fatalf("photo cases = %d, want >= 15", seen)
	}
}
