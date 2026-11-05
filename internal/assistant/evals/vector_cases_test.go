package evals

import (
	"encoding/json"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

// MM110: every vector case can be passed by a schema-valid op list (the same
// rule as photo_cases_test.go), so a failing eval means the model chose badly,
// not that the case asks for ops the Motion vocabulary cannot express.
var vectorReference = map[string]struct {
	text    string
	ops     string
	actions []Action
}{
	"vector-lower-third":   {"A lower-third with your name slides in.", `[{"op":"template","kind":"lowerThird","text":["Jo Park"]}]`, nil},
	"vector-text-pop":      {"The headline pops in.", `[{"op":"template","kind":"titlePop","text":"Launch day"}]`, nil},
	"vector-logo-draw":     {"The logo outline draws itself on.", `[{"op":"apply_animator","layerIds":["logo"],"animator":"drawOn","span":{"start":0,"end":1400,"easing":"easeBoth"}}]`, nil},
	"vector-fade-in":       {"The headline fades in over half a second.", `[{"op":"animate","layerId":"headline","property":"opacity","keyframes":[{"t":0,"value":0,"easing":"easeOut"},{"t":500,"value":1,"easing":"linear"}]}]`, nil},
	"vector-spin":          {"The logo spins and loops.", `[{"op":"animate","layerId":"logo","property":"transform.rotation","keyframes":[{"t":0,"value":0,"easing":"linear"},{"t":1000,"value":360,"easing":"linear"}]},{"op":"set_loop","loop":true}]`, nil},
	"vector-bullets":       {"Three points slide in one by one.", `[{"op":"template","kind":"bulletReveal","text":["Fast","Simple","Free"]}]`, nil},
	"vector-counter":       {"A counter runs up to 5,000.", `[{"op":"template","kind":"counter","value":5000}]`, nil},
	"vector-badge":         {"A SALE badge bounces in.", `[{"op":"template","kind":"badgeBounce","text":"SALE"}]`, nil},
	"vector-slide-left":    {"The headline slides in from the left.", `[{"op":"animate","layerId":"headline","property":"transform.x","keyframes":[{"t":0,"value":-200,"easing":"easeOut"},{"t":500,"value":0,"easing":"linear"}]}]`, nil},
	"vector-duration":      {"The animation is now 6 seconds long.", `[{"op":"set_duration","ms":6000}]`, nil},
	"vector-color":         {"The headline is orange now.", `[{"op":"update_layer","layerId":"headline","patch":{"fill":"#ff8a00"}}]`, nil},
	"vector-shimmer":       {"I added a gradient shimmer to the headline.", `[{"op":"apply_animator","layerIds":["headline"],"animator":"gradient","span":{"start":0,"end":1600}},{"op":"set_loop","loop":true}]`, nil},
	"vector-export-advice": {"For a React app, export a Lottie file or a React component.", "", []Action{{Kind: "export", Params: map[string]any{"format": "lottie"}}}},
	"vector-blur-in":       {"The headline blurs in.", `[{"op":"apply_animator","layerIds":["headline"],"animator":"blur","span":{"start":0,"end":600},"options":{"amount":8}}]`, nil},
	"vector-es-fade":       {"El título aparece poco a poco.", `[{"op":"animate","layerId":"headline","property":"opacity","keyframes":[{"t":0,"value":0,"easing":"easeOut"},{"t":800,"value":1,"easing":"linear"}]}]`, nil},
}

func TestVectorCasesPassableWithRealOps(t *testing.T) {
	cases, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	v := assistant.NewOpsValidator()
	if !v.Has("vector") {
		t.Fatal("vector op schema missing")
	}
	seen := 0
	for _, c := range cases {
		if c.Surface != "vector" || c.Adjust != nil {
			continue
		}
		seen++
		ref, ok := vectorReference[c.ID]
		if !ok {
			t.Errorf("%s: no reference answer (add one when you add a vector case)", c.ID)
			continue
		}
		out := Output{Text: ref.text, Actions: ref.actions, Model: "reference"}
		if ref.ops != "" {
			if err := v.Validate("vector", json.RawMessage(ref.ops)); err != nil {
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
		t.Fatalf("vector cases = %d, want >= 15", seen)
	}
}
