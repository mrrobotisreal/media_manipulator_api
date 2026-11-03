package photo

import (
	"encoding/json"
	"strings"
	"testing"
)

const oneLayer = `{"id":"a","type":"exposure","params":{"ev":0.5}}`

func TestSanitizeLayersCanonicalAndClamps(t *testing.T) {
	out, err := SanitizeLayers([]byte(`[{"type":"exposure","id":"a","opacity":7,"params":{"ev":99}}]`), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(out, &got); err != nil || len(got) != 1 {
		t.Fatalf("%s %v", out, err)
	}
	if got[0]["opacity"] != 1.0 || got[0]["blend"] != "normal" || got[0]["enabled"] != true {
		t.Errorf("not clamped / defaulted: %v", got[0])
	}
	again, err := SanitizeLayers(out, Limits{})
	if err != nil || string(again) != string(out) {
		t.Errorf("not idempotent: %s vs %s (%v)", again, out, err)
	}
}

func TestSanitizeLayersMatchesDocLayers(t *testing.T) {
	// The layers of a sanitized document equal SanitizeLayers over the same
	// input: the rules are one spec.
	layers := `[` + oneLayer + `,{"id":"b","type":"clarity","params":{"amount":15},"group":"Preset: X"}]`
	doc, err := Sanitize([]byte(`{"version":1,"source":{"width":10,"height":10},"layers":`+layers+`}`), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Layers json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatal(err)
	}
	got, err := SanitizeLayers([]byte(layers), Limits{})
	if err != nil || string(got) != string(m.Layers) {
		t.Errorf("drift:\n doc %s\n pre %s (%v)", m.Layers, got, err)
	}
}

func TestSanitizeLayersRefusals(t *testing.T) {
	cases := map[string]struct{ in, code string }{
		"not an array":     {`{"id":"a"}`, CodeInvalidJSON},
		"a whole doc":      {`{"version":1,"source":{"width":1,"height":1},"layers":[]}`, CodeInvalidJSON},
		"unknown type":     {`[{"id":"a","type":"sparkle","params":{}}]`, CodeUnknownLayerType},
		"layer not object": {`[1]`, CodeInvalidDoc},
		"duplicate ids":    {`[` + oneLayer + `,` + oneLayer + `]`, CodeDuplicateID},
		"mask ref":         {`[{"id":"a","type":"exposure","params":{},"mask":{"ref":"m1"}}]`, CodeInvalidDoc},
		"bad id":           {`[{"id":"has space","type":"exposure","params":{}}]`, CodeBadID},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := SanitizeLayers([]byte(c.in), Limits{})
			ve, ok := err.(*ValidationError)
			if !ok || ve.Code != c.code {
				t.Fatalf("got %v, want code %s", err, c.code)
			}
		})
	}
}

func TestSanitizeLayersRefusesTooManyLayersAndBytes(t *testing.T) {
	var parts []string
	for i := 0; i < 65; i++ {
		parts = append(parts, `{"id":"l`+itoa(i)+`","type":"exposure","params":{}}`)
	}
	_, err := SanitizeLayers([]byte("["+strings.Join(parts, ",")+"]"), PresetLimits(Limits{}))
	if ve, ok := err.(*ValidationError); !ok || ve.Code != CodeTooManyLayers {
		t.Fatalf("65 layers: %v", err)
	}
	// 64 is fine.
	if _, err := SanitizeLayers([]byte("["+strings.Join(parts[:64], ",")+"]"), PresetLimits(Limits{})); err != nil {
		t.Fatalf("64 layers: %v", err)
	}
	// Over 1 MiB of input is refused before parsing.
	big := `[{"id":"a","type":"exposure","params":{},"name":"` + strings.Repeat("x", PresetMaxBytes) + `"}]`
	if _, err := SanitizeLayers([]byte(big), PresetLimits(Limits{})); err == nil {
		t.Fatal("1 MiB+ input must be refused")
	} else if ve := err.(*ValidationError); ve.Code != CodeTooLarge {
		t.Fatalf("code %s", ve.Code)
	}
}

func TestPresetLimitsCaps(t *testing.T) {
	l := PresetLimits(Limits{MaxLayers: 500, MaxDocBytes: 8 << 20})
	if l.MaxLayers != 64 || l.MaxDocBytes != 1<<20 {
		t.Errorf("%+v", l)
	}
	if PresetLimits(Limits{MaxLayers: 10}).MaxLayers != 10 {
		t.Error("a lower layer cap is kept")
	}
}
