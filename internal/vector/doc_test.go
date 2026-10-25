package vector

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimal wraps layers/defs/animations JSON fragments in a valid v1 doc.
func minimal(layers, defs, anims string) string {
	s := `{"version":1,"canvas":{"width":100,"height":100},"layers":` + layers
	if defs != "" {
		s += `,"defs":` + defs
	}
	if anims != "" {
		s += `,"animations":` + anims
	}
	return s + `}`
}

func wantCode(t *testing.T, raw string, l Limits, code, path string) {
	t.Helper()
	_, err := SanitizeWith([]byte(raw), l)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError %s, got %v", code, err)
	}
	if ve.Code != code {
		t.Fatalf("code = %s (%v), want %s", ve.Code, ve, code)
	}
	if path != "" && ve.Path != path {
		t.Fatalf("path = %q, want %q", ve.Path, path)
	}
}

func wantOK(t *testing.T, raw string, l Limits) Doc {
	t.Helper()
	d, err := SanitizeWith([]byte(raw), l)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return d
}

func TestFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.json")
	have := map[string]bool{}
	for _, f := range files {
		have[filepath.Base(f)] = true
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			d, err := Sanitize(b)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(d.CanonicalIndent(), b) {
				t.Fatalf("CanonicalIndent differs from fixture bytes")
			}
			d2, err := Sanitize(d.Canonical())
			if err != nil {
				t.Fatalf("compact re-sanitize: %v", err)
			}
			if !bytes.Equal(d2.CanonicalIndent(), b) {
				t.Fatalf("compact -> indent round trip differs")
			}
		})
	}
	for _, n := range []string{"blank", "shapes", "gradients", "masks-clip", "use-symbol", "nested-groups", "raw-unsupported", "tracks"} {
		if !have[n+".json"] {
			t.Errorf("missing fixture %s.json", n)
		}
	}
}

func TestCounts(t *testing.T) {
	b, _ := os.ReadFile("testdata/tracks.json")
	d, err := Sanitize(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version() != 1 || d.LayerCount() != 3 || d.TrackCount() != 5 {
		t.Fatalf("got v%d layers=%d tracks=%d", d.Version(), d.LayerCount(), d.TrackCount())
	}
}

func TestVersion(t *testing.T) {
	for _, v := range []string{`2`, `"1"`, `1.5`, `null`} {
		wantCode(t, `{"version":`+v+`,"canvas":{"width":1,"height":1},"layers":[]}`, Limits{}, CodeUnsupportedVersion, "version")
	}
	wantCode(t, `{"canvas":{"width":1,"height":1},"layers":[]}`, Limits{}, CodeUnsupportedVersion, "version")
	wantOK(t, `{"version":1.0,"canvas":{"width":1,"height":1},"layers":[]}`, Limits{})
}

func TestDanglingRefs(t *testing.T) {
	rect := func(extra string) string { return `[{"id":"a","type":"rect"` + extra + `}]` }
	cases := []struct{ name, doc, path string }{
		{"use", minimal(`[{"id":"u","type":"use","href":"#nope"}]`, "", ""), "layers[0].href"},
		{"gradient", minimal(rect(`,"fill":"url(#nope)"`), "", ""), "layers[0].fill"},
		{"stroke", minimal(rect(`,"stroke":"url(#nope)"`), "", ""), "layers[0].stroke"},
		{"mask", minimal(rect(`,"mask":"nope"`), "", ""), "layers[0].mask"},
		{"clip", minimal(rect(`,"clipPath":"url(#nope)"`), "", ""), "layers[0].clipPath"},
		{"filter", minimal(rect(`,"filter":"#nope"`), "", ""), "layers[0].filter"},
		{"gradient href", minimal(`[]`, `{"gradients":[{"id":"g","href":"#nope"}]}`, ""), "defs.gradients[0].href"},
		{"fill wrong kind", minimal(`[{"id":"a","type":"rect","fill":"url(#m)"}]`, `{"masks":[{"id":"m"}]}`, ""), "layers[0].fill"},
		{"track layerId", minimal(rect(""), "", `[{"layerId":"nope","property":"opacity","keyframes":[]}]`), "animations[0].layerId"},
		{"pathId non-path", minimal(rect(""), "", `[{"layerId":"a","property":"motionPath","keyframes":[{"t":0,"value":0,"extras":{"pathId":"a"}}]}]`), "animations[0].keyframes[0].extras.pathId"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantCode(t, c.doc, Limits{}, CodeDanglingRef, c.path) })
	}
	// Forward references and def targets for tracks are fine.
	wantOK(t, minimal(`[{"id":"a","type":"rect","fill":"url(#g)"}]`, `{"gradients":[{"id":"g"}]}`,
		`[{"layerId":"g","property":"gradientStop[2].offset","keyframes":[{"t":0,"value":0}]}]`), Limits{})
}

func TestUseCycles(t *testing.T) {
	cases := map[string]string{
		"self": minimal(`[{"id":"a","type":"use","href":"#a"}]`, "", ""),
		"via group": minimal(`[{"id":"a","type":"use","href":"#b"},
			{"id":"b","type":"group","children":[{"id":"c","type":"use","href":"#a"}]}]`, "", ""),
		"symbol self":  minimal(`[]`, `{"symbols":[{"id":"s","children":[{"id":"u","type":"use","href":"#s"}]}]}`, ""),
		"own ancestor": minimal(`[{"id":"g","type":"group","children":[{"id":"u","type":"use","href":"#g"}]}]`, "", ""),
	}
	for n, d := range cases {
		t.Run(n, func(t *testing.T) { wantCode(t, d, Limits{}, CodeUseCycle, "") })
	}
	// A diamond is not a cycle.
	wantOK(t, minimal(`[{"id":"r","type":"rect"},{"id":"u1","type":"use","href":"#r"},{"id":"u2","type":"use","href":"#r"}]`, "", ""), Limits{})
}

func TestOversize(t *testing.T) {
	doc := minimal(`[]`, "", "")
	wantOK(t, doc, Limits{MaxDocBytes: len(doc)})
	wantCode(t, doc, Limits{MaxDocBytes: len(doc) - 1}, CodeTooLarge, "")
}

func TestLimits(t *testing.T) {
	nest := func(n int) string {
		s := `{"id":"leaf","type":"rect"}`
		for i := n - 1; i >= 1; i-- {
			s = fmt.Sprintf(`{"id":"g%d","type":"group","children":[%s]}`, i, s)
		}
		return `[` + s + `]`
	}
	wantOK(t, minimal(nest(4), "", ""), Limits{MaxDepth: 4})
	wantCode(t, minimal(nest(5), "", ""), Limits{MaxDepth: 4}, CodeTooDeep, "")
	// depth restarts at 1 inside defs
	wantCode(t, minimal(`[]`, `{"symbols":[{"id":"s","children":`+nest(3)+`}]}`, ""), Limits{MaxDepth: 2}, CodeTooDeep, "")

	three := `[{"id":"a","type":"rect"},{"id":"b","type":"rect"},{"id":"c","type":"rect"}]`
	wantOK(t, minimal(three, "", ""), Limits{MaxLayers: 3})
	wantCode(t, minimal(three, "", ""), Limits{MaxLayers: 2}, CodeTooManyLayers, "layers[2]")
	// def children count too
	wantCode(t, minimal(`[{"id":"a","type":"rect"}]`, `{"masks":[{"id":"m","children":[{"id":"b","type":"rect"}]}]}`, ""),
		Limits{MaxLayers: 1}, CodeTooManyLayers, "")

	kfs := `[{"t":0,"value":1},{"t":1,"value":2},{"t":2,"value":3}]`
	tr := func(k string) string {
		return minimal(`[{"id":"a","type":"rect"}]`, "", `[{"layerId":"a","property":"opacity","keyframes":`+k+`}]`)
	}
	wantOK(t, tr(kfs), Limits{MaxKeyframesPerTrack: 3})
	wantCode(t, tr(kfs), Limits{MaxKeyframesPerTrack: 2}, CodeTooManyKeyframes, "animations[0].keyframes")

	// JSON nesting beyond 160 is rejected before any model checks.
	wantCode(t, `{"a":`+strings.Repeat("[", 200)+strings.Repeat("]", 200)+`}`, Limits{}, CodeTooDeep, "")
}

func TestRaw(t *testing.T) {
	raw := func(m string) string {
		return minimal(fmt.Sprintf(`[{"id":"r","type":"raw","markup":%q}]`, m), "", "")
	}
	bad := []string{
		`<script>alert(1)</script>`,
		`<SCRIPT src=x>`,
		`<rect onload="x()"/>`,
		`<rect onclick = "x()"/>`,
		`<image href="https://evil.example/a.png"/>`,
		`<use xlink:href='//evil'/>`,
		`<use href=http://evil/x />`,
		`<a href="javascript:alert(1)">x</a>`,
		`<rect style="fill:url(http://evil/x)"/>`,
		`<rect style="fill:url( '//evil/x')"/>`,
		`<style>@import "x.css";</style>`,
		`<style>@IMPORT url(x)</style>`,
		`<svg:script>alert(1)</svg:script>`,
		`< script>x</script>`,
		`<foreignObject><div>x</div></foreignObject>`,
		`<iframe src="x"/>`,
		`<!DOCTYPE svg [<!ENTITY a "b">]>`,
		`<feImage src="https://evil/x.png"/>`,
	}
	for _, m := range bad {
		t.Run(m, func(t *testing.T) { wantCode(t, raw(m), Limits{}, CodeUnsafeContent, "layers[0].markup") })
	}
	wantOK(t, raw(`<g><use href="#a"/><image href="data:image/png;base64,AAAA"/><a href=""/><rect style="fill:url(#g)"/><objective/></g>`), Limits{})
	wantCode(t, raw(strings.Repeat("a", 11)), Limits{MaxRawBytes: 10}, CodeTooLarge, "layers[0].markup")
	wantOK(t, raw(strings.Repeat("a", 10)), Limits{MaxRawBytes: 10})
}

func TestImageHref(t *testing.T) {
	img := func(h string) string {
		return minimal(fmt.Sprintf(`[{"id":"i","type":"image","href":%q}]`, h), "", "")
	}
	for _, h := range []string{"https://x.example/a.png", "http://x/a.png", "javascript:alert(1)", "file:///etc/passwd", "//host/a.png", "data:text/html,<b>"} {
		wantCode(t, img(h), Limits{}, CodeUnsafeContent, "layers[0].href")
	}
	for _, h := range []string{"data:image/png;base64,AAAA", "DATA:IMAGE/PNG;base64,AAAA", "asset:abc", "images/a.png"} {
		wantOK(t, img(h), Limits{})
	}
	wantCode(t, minimal(`[{"id":"u","type":"use","href":"x"}]`, "", ""), Limits{}, CodeUnsafeContent, "layers[0].href")
}

func TestStructuralErrors(t *testing.T) {
	wantCode(t, minimal(`[{"id":"a","type":"rect"}]`, `{"filters":[{"id":"a"}]}`, ""), Limits{}, CodeDuplicateID, "defs.filters[0].id")
	wantCode(t, minimal(`[{"id":"a","type":"rect"},{"id":"a","type":"rect"}]`, "", ""), Limits{}, CodeDuplicateID, "layers[1].id")
	wantCode(t, `{"version":1,"version":1}`, Limits{}, CodeInvalidJSON, "")
	wantCode(t, minimal(`[]`, "", "")+`x`, Limits{}, CodeInvalidJSON, "")
	wantCode(t, minimal(`[]`, "", "")+`{}`, Limits{}, CodeInvalidJSON, "")
	wantCode(t, `[]`, Limits{}, CodeInvalidJSON, "") // documented choice: non-object top level is invalid_json
	wantCode(t, "{\"version\":1,\"x\":\"\xff\"}", Limits{}, CodeInvalidJSON, "")
	wantCode(t, ``, Limits{}, CodeInvalidJSON, "")
	wantCode(t, minimal(`[{"id":"a","type":"blob"}]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].type")
	wantCode(t, minimal(`[{"id":"a","type":"path"}]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].d")
	wantCode(t, minimal(`[{"id":"a","type":"rect","opacity":"x"}]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].opacity")
	wantCode(t, minimal(`[{"id":"a","type":"rect","transform":{"x":"1"}}]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].transform.x")
	wantCode(t, `{"version":1,"canvas":{"width":0,"height":5},"layers":[]}`, Limits{}, CodeInvalidDoc, "canvas.width")
	wantCode(t, `{"version":1,"canvas":{"width":5,"height":5},"fps":300,"layers":[]}`, Limits{}, CodeInvalidDoc, "fps")
	wantCode(t, `{"version":1,"canvas":{"width":5,"height":5}}`, Limits{}, CodeInvalidDoc, "layers")
}

func TestTopLevelKeys(t *testing.T) {
	in := `{"zzz":1,"version":1,"loop":true,"canvas":{"width":10,"height":10,"future":{"a":[1,2]}},"layers":[{"id":"a","type":"rect","futureField":{"k":1}}],"junk":[],"meta":{"x":1}}`
	d := wantOK(t, in, Limits{})
	want := `{"version":1,"loop":true,"canvas":{"width":10,"height":10,"future":{"a":[1,2]}},"layers":[{"id":"a","type":"rect","futureField":{"k":1}}],"meta":{"x":1}}`
	if got := string(d.Canonical()); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCanonNumbersAndStrings(t *testing.T) {
	in := `{"version":1,"canvas":{"width":1.0,"height":1e2},"layers":[],"meta":{"a":-0,"b":0.1,"c":1e21,"d":5e-7,"e":-0.0,"f":123456789012345680000,"g":1E+2,"s":"<>&\u0001\n \"\\/é\b\f\r\t"}}`
	d := wantOK(t, in, Limits{})
	want := `{"version":1,"canvas":{"width":1,"height":100},"layers":[],"meta":{"a":0,"b":0.1,"c":1e+21,"d":5e-7,"e":0,"f":123456789012345680000,"g":100,"s":"<>&\u0001\n` + " " + `\"\\/é\b\f\r\t"}}`
	if got := string(d.Canonical()); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Indent form: empty containers stay inline, trailing newline.
	ind := string(wantOK(t, `{"version":1,"canvas":{"width":1,"height":1},"layers":[],"meta":{}}`, Limits{}).CanonicalIndent())
	wantInd := "{\n  \"version\": 1,\n  \"canvas\": {\n    \"width\": 1,\n    \"height\": 1\n  },\n  \"layers\": [],\n  \"meta\": {}\n}\n"
	if ind != wantInd {
		t.Fatalf("got %q want %q", ind, wantInd)
	}
}

func TestAnimationValidation(t *testing.T) {
	tr := func(prop, kf string) string {
		return minimal(`[{"id":"a","type":"rect"}]`, "", `[{"layerId":"a","property":"`+prop+`","keyframes":[`+kf+`]}]`)
	}
	wantCode(t, tr("transform.z", `{"t":0,"value":1}`), Limits{}, CodeInvalidDoc, "animations[0].property")
	wantCode(t, tr("gradientStop[x].color", `{"t":0,"value":1}`), Limits{}, CodeInvalidDoc, "animations[0].property")
	for _, e := range []string{`"bounce"`, `{"cubicBezier":[0,0,1]}`, `{"cubicBezier":[2,0,0.5,1]}`, `{"cubicBezier":[0,0,-1,1]}`, `{"other":1}`, `3`} {
		wantCode(t, tr("opacity", `{"t":0,"value":1,"easing":`+e+`}`), Limits{}, CodeInvalidDoc, "animations[0].keyframes[0].easing")
	}
	wantCode(t, tr("opacity", `{"t":-1,"value":1}`), Limits{}, CodeInvalidDoc, "animations[0].keyframes[0].t")
	wantCode(t, tr("opacity", `{"t":0}`), Limits{}, CodeInvalidDoc, "animations[0].keyframes[0].value")
	wantOK(t, tr("opacity", `{"t":0,"value":null,"easing":{"cubicBezier":[0,5,1,-5]}}`), Limits{})
}
