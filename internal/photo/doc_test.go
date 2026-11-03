package photo

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canonicalDocs are the valid fixtures: sanitize(file) must equal the file
// (canonical bytes plus one trailing newline).
var canonicalDocs = []string{
	"blank", "clamped", "curves-s", "defaults", "detail-all", "geometry-perspective",
	"geometry-rotate-crop", "heal-base-refs", "heal-two-bases", "hsl-grey-except-red", "levels-channels",
	"masks-all-kinds", "masks-named", "rung1-all-layers", "unknown-fields",
}

// inputPairs: non-canonical input -> the canonical fixture it must produce.
var inputPairs = map[string]string{"minimal": "blank", "defaults": "defaults", "clamp": "clamped"}

var invalidDocs = map[string]string{
	"invalid-version":      CodeUnsupportedVersion,
	"invalid-layer-type":   CodeUnknownLayerType,
	"invalid-curve-points": CodeInvalidCurve,
	"oversize-brush":       CodeBrushTooLarge,
}

func read(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sanitize(t *testing.T, raw string) map[string]any {
	t.Helper()
	out, err := Sanitize([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func wantCode(t *testing.T, raw string, lim Limits, code string) {
	t.Helper()
	_, err := Sanitize([]byte(raw), lim)
	var ve *ValidationError
	if !errors.As(err, &ve) || !errors.Is(err, ErrInvalidDoc) {
		t.Fatalf("want ValidationError(%s), got %v", code, err)
	}
	if ve.Code != code {
		t.Fatalf("code = %s (%v), want %s", ve.Code, ve, code)
	}
}

func TestPhotoSanitizeFixturesCanonical(t *testing.T) {
	for _, n := range canonicalDocs {
		t.Run(n, func(t *testing.T) {
			want := read(t, "docs/"+n+".json")
			if !bytes.HasSuffix(want, []byte("}\n")) || bytes.Contains(want[:len(want)-1], []byte("\n")) {
				t.Fatal("fixture must be one compact line plus one trailing newline")
			}
			got, err := Sanitize(want, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(append(got, '\n'), want) {
				t.Fatalf("not canonical:\n got %s\nwant %s", got, want)
			}
			again, err := Sanitize(got, Limits{})
			if err != nil || !bytes.Equal(again, got) {
				t.Fatal("sanitize is not idempotent")
			}
		})
	}
}

func TestPhotoSanitizeInputsFillDefaults(t *testing.T) {
	for in, out := range inputPairs {
		got, err := Sanitize(read(t, "inputs/"+in+".json"), Limits{})
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if want := read(t, "docs/"+out+".json"); !bytes.Equal(append(got, '\n'), want) {
			t.Fatalf("%s -> %s mismatch:\n got %s\nwant %s", in, out, got, want)
		}
	}
}

func TestPhotoSanitizeInvalidFixtures(t *testing.T) {
	for n, code := range invalidDocs {
		t.Run(n, func(t *testing.T) { wantCode(t, string(read(t, "docs/"+n+".json")), Limits{}, code) })
	}
}

func TestPhotoSanitizeUnknownFieldsSurvive(t *testing.T) {
	m := sanitize(t, string(read(t, "docs/unknown-fields.json")))
	if m["futureTop"].(map[string]any)["nested"].(map[string]any)["n"] != 1.5 {
		t.Fatal("top-level unknown lost")
	}
	l := m["layers"].([]any)[0].(map[string]any)
	if l["futureLayerKey"] != "keep-me" {
		t.Fatal("layer unknown lost")
	}
	if l["params"].(map[string]any)["futureParam"] == nil {
		t.Fatal("params unknown lost")
	}
	if l["mask"].(map[string]any)["components"].([]any)[0].(map[string]any)["softness"] != 7.0 {
		t.Fatal("mask component unknown lost")
	}
	if m["geometry"].(map[string]any)["cameraHint"] != "wide" || m["source"].(map[string]any)["make"] != "Test" {
		t.Fatal("geometry/source unknown lost")
	}
}

func layer0Params(m map[string]any) map[string]any {
	return m["layers"].([]any)[0].(map[string]any)["params"].(map[string]any)
}

func TestPhotoSanitizeClamping(t *testing.T) {
	base := `{"version":1,"source":{"width":10,"height":10},"layers":[%s]}`
	mk := func(layer string) string { return strings.Replace(base, "%s", layer, 1) }
	if ev := layer0Params(sanitize(t, mk(`{"id":"a","type":"exposure","params":{"ev":9}}`)))["ev"]; ev != 5.0 {
		t.Fatalf("exposure.ev = %v, want 5", ev)
	}
	m := sanitize(t, mk(`{"id":"a","type":"hsl","params":{"ranges":[{"id":"r","width":500}]}}`))
	if w := layer0Params(m)["ranges"].([]any)[0].(map[string]any)["width"]; w != 120.0 {
		t.Fatalf("hsl width = %v, want 120", w)
	}
	m = sanitize(t, mk(`{"id":"a","type":"gamma","opacity":-2,"params":{"gamma":-1}}`))
	l := m["layers"].([]any)[0].(map[string]any)
	if l["opacity"] != 0.0 || layer0Params(m)["gamma"] != 0.1 {
		t.Fatalf("clamp low: %v", l)
	}
	// -0 prints 0
	out, err := Sanitize([]byte(`{"version":1,"source":{"width":10,"height":10},"geometry":{"rotate":-0.0}}`), Limits{})
	if err != nil || !bytes.Contains(out, []byte(`"rotate":0`)) || bytes.Contains(out, []byte(`-0`)) {
		t.Fatalf("negative zero: %s %v", out, err)
	}
}

func TestPhotoSanitizeRefusals(t *testing.T) {
	src := `"version":1,"source":{"width":10,"height":10}`
	l := func(s string) string { return `{` + src + `,"layers":[` + s + `]}` }
	cases := []struct {
		name, raw, code string
		lim             Limits
	}{
		{"missing version", `{"source":{"width":1,"height":1}}`, CodeUnsupportedVersion, Limits{}},
		{"version string", `{"version":"1","source":{"width":1,"height":1}}`, CodeUnsupportedVersion, Limits{}},
		{"not object", `[1]`, CodeInvalidJSON, Limits{}},
		{"bad json", `{`, CodeInvalidJSON, Limits{}},
		{"duplicate key", `{"version":1,"version":1}`, CodeInvalidJSON, Limits{}},
		{"trailing data", `{"version":1} x`, CodeInvalidJSON, Limits{}},
		{"missing source", `{"version":1}`, CodeInvalidDoc, Limits{}},
		{"bad layer id", l(`{"id":"a b","type":"invert"}`), CodeBadID, Limits{}},
		{"missing layer id", l(`{"type":"invert"}`), CodeBadID, Limits{}},
		{"id too long", l(`{"id":"` + strings.Repeat("a", 41) + `","type":"invert"}`), CodeBadID, Limits{}},
		{"duplicate layer id", l(`{"id":"a","type":"invert"},{"id":"a","type":"invert"}`), CodeDuplicateID, Limits{}},
		{"unknown layer type", l(`{"id":"a","type":"nope"}`), CodeUnknownLayerType, Limits{}},
		{"unknown mask kind", l(`{"id":"a","type":"invert","mask":{"components":[{"kind":"wand"}]}}`), CodeUnknownMaskKind, Limits{}},
		{"wrong type", l(`{"id":"a","type":"exposure","params":{"ev":"x"}}`), CodeInvalidDoc, Limits{}},
		{"bad enum", l(`{"id":"a","type":"invert","blend":"multiply"}`), CodeInvalidDoc, Limits{}},
		{"curve one point", l(`{"id":"a","type":"curves","params":{"r":{"points":[{"x":0,"y":0}]}}}`), CodeInvalidCurve, Limits{}},
		{"curve too many", l(`{"id":"a","type":"curves","params":{"r":{"points":[{"x":0,"y":0},{"x":0.1,"y":0},{"x":0.2,"y":0}]}}}`), CodeInvalidCurve, Limits{MaxCurvePoints: 2}},
		{"bad mask ref", `{` + src + `,"heals":[{"id":"h","maskRef":"../x.png"}]}`, CodeInvalidDoc, Limits{}},
		{"bad colour", l(`{"id":"a","type":"photoFilter","params":{"color":"red"}}`), CodeInvalidDoc, Limits{}},
		{"lut without size", l(`{"id":"a","type":"lut","params":{"cube":"1 2 3"}}`), CodeInvalidDoc, Limits{}},
		{"lut size 66", l(`{"id":"a","type":"lut","params":{"cube":"LUT_3D_SIZE 66\n"}}`), CodeLutTooLarge, Limits{}},
		{"lut bytes", l(`{"id":"a","type":"lut","params":{"cube":"LUT_3D_SIZE 2\n"}}`), CodeLutTooLarge, Limits{MaxLutBytes: 5}},
		{"too large", `{"version":1}`, CodeTooLarge, Limits{MaxDocBytes: 5}},
		{"layer name too long", l(`{"id":"a","type":"invert","name":"` + strings.Repeat("n", 81) + `"}`), CodeInvalidDoc, Limits{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantCode(t, c.raw, c.lim, c.code) })
	}
}

func TestPhotoSanitizeLimits(t *testing.T) {
	var ls []string
	for i := 0; i < 65; i++ {
		ls = append(ls, `{"id":"l`+itoa(i)+`","type":"invert"}`)
	}
	raw := `{"version":1,"source":{"width":1,"height":1},"layers":[` + strings.Join(ls, ",") + `]}`
	wantCode(t, raw, Limits{}, CodeTooManyLayers)
	if _, err := Sanitize([]byte(raw), Limits{MaxLayers: 65}); err != nil {
		t.Fatal(err)
	}
	var cs []string
	for i := 0; i < 17; i++ {
		cs = append(cs, `{"kind":"luminosity"}`)
	}
	wantCode(t, `{"version":1,"source":{"width":1,"height":1},"layers":[{"id":"a","type":"invert","mask":{"components":[`+strings.Join(cs, ",")+`]}}]}`, Limits{}, CodeTooManyComponents)
}

func TestPhotoSanitizeBrushLimits(t *testing.T) {
	good := string(read(t, "docs/masks-all-kinds.json"))
	if !strings.Contains(good, "data:image/png;base64,") {
		t.Fatal("fixture lost its brush")
	}
	// Byte cap: same doc, tiny cap.
	wantCode(t, good, Limits{MaxBrushPNGBytes: 10}, CodeBrushTooLarge)
	// Pixel cap from the declared width/height.
	wantCode(t, good, Limits{MaxBrushPx: 3}, CodeBrushTooLarge)
	// Not a PNG.
	notPNG := strings.Replace(good, "data:image/png;base64,", "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("GIF89a-not-a-png-at-all")), 1)
	wantCode(t, notPNG, Limits{}, CodeInvalidDoc)
}

func TestPhotoSanitizeDefaultHSLRanges(t *testing.T) {
	m := sanitize(t, `{"version":1,"source":{"width":1,"height":1},"layers":[{"id":"h","type":"hsl"}]}`)
	rs := layer0Params(m)["ranges"].([]any)
	ids := []string{"reds", "yellows", "greens", "cyans", "blues", "magentas"}
	if len(rs) != 6 {
		t.Fatalf("ranges = %d", len(rs))
	}
	for i, r := range rs {
		rm := r.(map[string]any)
		if rm["id"] != ids[i] || rm["center"] != float64(i*60) || rm["width"] != 30.0 || rm["feather"] != 30.0 {
			t.Fatalf("range %d = %v", i, rm)
		}
		if _, has := rm["name"]; has {
			t.Fatal("default range has a name")
		}
	}
}

func TestPhotoSanitizeCanonicalRules(t *testing.T) {
	// Keys sorted at every depth; HTML and U+2028 raw; big/small numbers like JS.
	raw := `{"version":1,"source":{"width":10,"height":10},"zz":"<a&b>   é","aa":{"y":1e21,"x":0.0000001,"w":1.50}}`
	out, err := Sanitize([]byte(raw), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"aa":{"w":1.5,"x":1e-7,"y":1e+21}`) {
		t.Fatalf("numbers/sorting: %s", s)
	}
	if !strings.Contains(s, "\"<a&b>   é\"") {
		t.Fatalf("string escaping: %s", s)
	}
	if strings.Index(s, `"aa"`) > strings.Index(s, `"geometry"`) || strings.Index(s, `"source"`) > strings.Index(s, `"version"`) {
		t.Fatalf("top-level key order: %s", s)
	}
	// Optional fields absent, nullable present.
	if strings.Contains(s, `"format"`) || !strings.Contains(s, `"crop":null`) || !strings.Contains(s, `"perspective":null`) {
		t.Fatalf("optional/nullable: %s", s)
	}
}

func TestPhotoNamedMasksAndHealRefs(t *testing.T) {
	src := `"version":1,"source":{"width":10,"height":10}`
	wantCode(t, `{`+src+`,"layers":[{"id":"a","type":"invert","mask":{"ref":"nope"}}]}`, Limits{}, CodeInvalidDoc)
	wantCode(t, `{`+src+`,"heals":[{"id":"h"}]}`, Limits{}, CodeInvalidDoc)
	wantCode(t, `{`+src+`,"heals":[{"id":"h","baseRef":"../x.png"}]}`, Limits{}, CodeInvalidDoc)
	wantCode(t, `{`+src+`,"masks":{"bad id":{}}}`, Limits{}, CodeBadID)
	m := sanitize(t, `{`+src+`,"masks":{}}`)
	if _, has := m["masks"]; has {
		t.Fatal("empty masks must canonicalise to absent")
	}
	m = sanitize(t, `{`+src+`,"masks":{"a":{}},"layers":[{"id":"l","type":"invert","mask":{"ref":"a"}}]}`)
	if m["masks"].(map[string]any)["a"].(map[string]any)["opacity"].(float64) != 1 {
		t.Fatal("named mask defaults not filled")
	}
}
