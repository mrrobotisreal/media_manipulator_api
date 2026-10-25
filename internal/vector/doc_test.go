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

func rect(id string, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"type":"rect","id":%q,"x":0,"y":0,"width":1,"height":1%s}`, id, extra)
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

var contractNames = []string{
	"defaults", "animated", "additive", "shapes", "gradients", "masks", "clippath",
	"text", "use-symbol", "nested-groups", "unsupported",
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
				t.Fatalf("sanitize: %v", err)
			}
			if got := d.CanonicalIndent(); !bytes.Equal(got, b) {
				t.Fatalf("canonical indent differs from file\n got: %s\nwant: %s", got, b)
			}
			d2, err := Sanitize(d.Canonical())
			if err != nil {
				t.Fatalf("re-sanitize compact: %v", err)
			}
			if !bytes.Equal(d2.CanonicalIndent(), b) {
				t.Fatal("compact canonical does not round-trip to the same indent bytes")
			}
		})
	}
	for _, n := range contractNames {
		if !have[n+".json"] {
			t.Errorf("missing fixture %s.json", n)
		}
	}
	if len(files) != len(contractNames) {
		t.Errorf("found %d top-level fixtures, want %d", len(files), len(contractNames))
	}
}

func TestInputDefaults(t *testing.T) {
	in, _ := os.ReadFile("testdata/inputs/defaults.json")
	want, _ := os.ReadFile("testdata/defaults.json")
	d, err := Sanitize(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.CanonicalIndent(), want) {
		t.Fatalf("got %s want %s", d.CanonicalIndent(), want)
	}
}

func TestInvalidFixtures(t *testing.T) {
	want := map[string]string{
		"attrs-handler":         CodeUnsafeContent,
		"attrs-not-allowlisted": CodeUnsafeContent,
		"background-url":        CodeUnsafeContent,
		"bad-id":                CodeInvalidDoc,
		"dangling-gradient":     CodeDanglingRef,
		"dangling-mask":         CodeDanglingRef,
		"duplicate-id":          CodeDuplicateID,
		"image-external":        CodeUnsafeContent,
		"paint-css-escape":      CodeUnsafeContent,
		"raw-cdata":             CodeUnsafeContent,
		"raw-entity-js":         CodeUnsafeContent,
		"raw-external-href":     CodeUnsafeContent,
		"raw-handler":           CodeUnsafeContent,
		"raw-iframe":            CodeUnsafeContent,
		"raw-script":            CodeUnsafeContent,
		"raw-style":             CodeUnsafeContent,
		"raw-unquoted-href":     CodeUnsafeContent,
		"track-bad-property":    CodeInvalidDoc,
		"track-missing-layer":   CodeDanglingRef,
		"unknown-layer-type":    CodeInvalidDoc,
		"use-cycle":             CodeUseCycle,
		"version-2":             CodeUnsupportedVersion,
		"version-missing":       CodeUnsupportedVersion,
	}
	files, _ := filepath.Glob("testdata/invalid/*.json")
	if len(files) != len(want) {
		t.Fatalf("found %d invalid fixtures, table has %d", len(files), len(want))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			b, _ := os.ReadFile(f)
			wantCode(t, string(b), Limits{}, want[name], "")
		})
	}
}

func TestCounts(t *testing.T) {
	b, _ := os.ReadFile("testdata/animated.json")
	d, err := Sanitize(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version() != 1 || d.LayerCount() == 0 || d.TrackCount() == 0 {
		t.Fatalf("counts: v=%d layers=%d tracks=%d", d.Version(), d.LayerCount(), d.TrackCount())
	}
}

func TestVersion(t *testing.T) {
	wantCode(t, `{"layers":[]}`, Limits{}, CodeUnsupportedVersion, "version")
	wantCode(t, `{"version":2}`, Limits{}, CodeUnsupportedVersion, "version")
	wantCode(t, `{"version":"1"}`, Limits{}, CodeUnsupportedVersion, "version")
	wantOK(t, `{"version":1}`, Limits{})
}

func TestDanglingRefs(t *testing.T) {
	cases := map[string]string{
		"use":          minimal(`[{"type":"use","id":"u","ref":"nope"}]`, "", ""),
		"gradient":     minimal(`[`+rect("r", `"fill":"url(#nope)"`)+`]`, "", ""),
		"stroke":       minimal(`[`+rect("r", `"stroke":"url(#nope) red"`)+`]`, "", ""),
		"mask":         minimal(`[`+rect("r", `"mask":"nope"`)+`]`, "", ""),
		"clip":         minimal(`[`+rect("r", `"clipPath":"nope"`)+`]`, "", ""),
		"filter":       minimal(`[`+rect("r", `"filter":"nope"`)+`]`, "", ""),
		"fill wrong":   minimal(`[`+rect("r", `"fill":"url(#m)"`)+`]`, `{"masks":[{"id":"m","layers":[]}]}`, ""),
		"track layer":  minimal(`[`+rect("r", "")+`]`, `{"symbols":[{"id":"s","layers":[]}]}`, `[{"layerId":"s","property":"opacity","keyframes":[]}]`),
		"mask as clip": minimal(`[`+rect("r", `"clipPath":"m"`)+`]`, `{"masks":[{"id":"m","layers":[]}]}`, ""),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) { wantCode(t, doc, Limits{}, CodeDanglingRef, "") })
	}
	// Raw ids resolve any reference kind; use may also target a layer or symbol.
	raw := `{"type":"raw","id":"raw1","markup":"<g id=\"x\"/>","ids":["x"]}`
	ok := minimal(`[`+raw+`,`+rect("r", `"fill":"url(#x)","mask":"x","clipPath":"x","filter":"x"`)+`,{"type":"use","id":"u1","ref":"x"},{"type":"use","id":"u2","ref":"r"},{"type":"use","id":"u3","ref":"s"}]`,
		`{"symbols":[{"id":"s","layers":[]}]}`, `[{"layerId":"r","property":"opacity","keyframes":[]}]`)
	wantOK(t, ok, Limits{})
	// Paint url that is not a local ref
	wantCode(t, minimal(`[`+rect("r", `"fill":"url(#a b)"`)+`]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].fill")
}

func TestUseCycles(t *testing.T) {
	cases := map[string]string{
		"self":        minimal(`[{"type":"use","id":"u","ref":"u"}]`, "", ""),
		"via group":   minimal(`[{"type":"group","id":"g","children":[{"type":"use","id":"u","ref":"g"}]}]`, "", ""),
		"symbol self": minimal(`[]`, `{"symbols":[{"id":"s","layers":[{"type":"use","id":"u","ref":"s"}]}]}`, ""),
		"two symbols": minimal(`[]`, `{"symbols":[{"id":"a","layers":[{"type":"use","id":"u1","ref":"b"}]},{"id":"b","layers":[{"type":"use","id":"u2","ref":"a"}]}]}`, ""),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) { wantCode(t, doc, Limits{}, CodeUseCycle, "") })
	}
	wantOK(t, minimal(`[{"type":"use","id":"u1","ref":"s"},{"type":"use","id":"u2","ref":"s"}]`, `{"symbols":[{"id":"s","layers":[]}]}`, ""), Limits{})
}

func TestOversize(t *testing.T) {
	doc := minimal(`[]`, "", "")
	wantOK(t, doc, Limits{MaxDocBytes: len(doc)})
	wantCode(t, doc, Limits{MaxDocBytes: len(doc) - 1}, CodeTooLarge, "")
	if DefaultLimits().MaxRawBytes != 1<<20 {
		t.Fatal("raw cap default must be 1 MiB")
	}
}

func TestLimits(t *testing.T) {
	nest := func(n int) string {
		s := rect("leaf", "")
		for i := n - 1; i >= 1; i-- {
			s = fmt.Sprintf(`{"type":"group","id":"g%d","children":[%s]}`, i, s)
		}
		return `[` + s + `]`
	}
	wantOK(t, minimal(nest(4), "", ""), Limits{MaxDepth: 4})
	wantCode(t, minimal(nest(5), "", ""), Limits{MaxDepth: 4}, CodeTooDeep, "")
	// symbol layers start at depth 2
	wantOK(t, minimal(`[]`, `{"symbols":[{"id":"s","layers":`+nest(2)+`}]}`, ""), Limits{MaxDepth: 3})
	wantCode(t, minimal(`[]`, `{"symbols":[{"id":"s","layers":`+nest(2)+`}]}`, ""), Limits{MaxDepth: 2}, CodeTooDeep, "")

	three := `[` + rect("a", "") + `,` + rect("b", "") + `,` + rect("c", "") + `]`
	wantOK(t, minimal(three, "", ""), Limits{MaxLayers: 3})
	wantCode(t, minimal(three, "", ""), Limits{MaxLayers: 2}, CodeTooManyLayers, "")
	wantCode(t, minimal(`[`+rect("a", "")+`]`, `{"masks":[{"id":"m","layers":[`+rect("b", "")+`]}]}`, ""), Limits{MaxLayers: 1}, CodeTooManyLayers, "")

	kfs := `[{"t":0,"value":1,"easing":"linear"},{"t":1,"value":2,"easing":"hold"},{"t":2,"value":3,"easing":"easeIn"}]`
	tr := func(k string) string {
		return minimal(`[`+rect("a", "")+`]`, "", `[{"layerId":"a","property":"opacity","keyframes":`+k+`}]`)
	}
	wantOK(t, tr(kfs), Limits{MaxKeyframesPerTrack: 3})
	wantCode(t, tr(kfs), Limits{MaxKeyframesPerTrack: 2}, CodeTooManyKeyframes, "animations[0].keyframes")
	wantCode(t, `{"a":`+strings.Repeat("[", 200)+strings.Repeat("]", 200)+`}`, Limits{}, CodeTooDeep, "")

	raw := func(m string) string {
		return minimal(fmt.Sprintf(`[{"type":"raw","id":"r","markup":%q}]`, m), "", "")
	}
	wantCode(t, raw(strings.Repeat("a", 11)), Limits{MaxRawBytes: 10}, CodeTooLarge, "layers[0].markup")
	wantOK(t, raw(strings.Repeat("a", 10)), Limits{MaxRawBytes: 10})
}

func TestStructuralErrors(t *testing.T) {
	wantCode(t, minimal(`[`+rect("a", "")+`]`, `{"filters":[{"type":"blur","id":"a","stdDeviation":1}]}`, ""), Limits{}, CodeDuplicateID, "defs.filters[0].id")
	wantCode(t, minimal(`[`+rect("a", "")+`,`+rect("a", "")+`]`, "", ""), Limits{}, CodeDuplicateID, "layers[1].id")
	wantCode(t, minimal(`[{"type":"raw","id":"r","markup":"","ids":["r"]}]`, "", ""), Limits{}, CodeDuplicateID, "layers[0].ids[0]")
	wantCode(t, `{"version":1,"version":1}`, Limits{}, CodeInvalidJSON, "")
	wantCode(t, minimal(`[]`, "", "")+`x`, Limits{}, CodeInvalidJSON, "")
	wantCode(t, minimal(`[]`, "", "")+`{}`, Limits{}, CodeInvalidJSON, "")
	wantCode(t, `[]`, Limits{}, CodeInvalidJSON, "")
	wantCode(t, "{\"version\":1,\"x\":\"\xff\"}", Limits{}, CodeInvalidJSON, "")
	wantCode(t, ``, Limits{}, CodeInvalidJSON, "")
	wantCode(t, minimal(`[{"type":"blob","id":"a"}]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].type")
	wantCode(t, minimal(`[{"type":"path","id":"a"}]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].d")
	wantCode(t, minimal(`[`+rect("a", `"opacity":2`)+`]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].opacity")
	wantCode(t, minimal(`[`+rect("a", `"opacity":"x"`)+`]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].opacity")
	wantCode(t, minimal(`[`+rect("a", `"transform":{"x":1}`)+`]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].transform.y")
	wantCode(t, `{"version":1,"canvas":{"width":0,"height":5}}`, Limits{}, CodeInvalidDoc, "canvas.width")
	wantCode(t, `{"version":1,"fps":300}`, Limits{}, CodeInvalidDoc, "fps")
	wantCode(t, `{"version":1,"fps":0}`, Limits{}, CodeInvalidDoc, "fps")
	wantCode(t, `{"version":1,"duration":1.5}`, Limits{}, CodeInvalidDoc, "duration")
	wantCode(t, `{"version":1,"duration":3600001}`, Limits{}, CodeInvalidDoc, "duration")
	wantCode(t, minimal(`[{"type":"polyline","id":"p","points":[1,2,3]}]`, "", ""), Limits{}, CodeInvalidDoc, "layers[0].points")
	wantCode(t, `{"version":1,"canvas":{"viewBox":{"x":0,"y":0,"width":0,"height":1}}}`, Limits{}, CodeInvalidDoc, "canvas.viewBox.width")
	wantCode(t, minimal(`[]`, `{"filters":[{"type":"blur","id":"b","stdDeviation":1,"x":"abc"}]}`, ""), Limits{}, CodeInvalidDoc, "defs.filters[0].x")
	wantCode(t, minimal(`[]`, `{"gradients":[{"type":"linear","id":"g","x1":0,"y1":0,"x2":1,"y2":0,"stops":[{"offset":0,"color":"url(#a)"}]}]}`, ""), Limits{}, CodeInvalidDoc, "defs.gradients[0].stops[0].color")
	wantCode(t, minimal(`[]`, `{"gradients":[{"type":"linear","id":"g","x1":0,"y1":0,"x2":1,"y2":0,"stops":[],"gradientTransform":[1,2,3]}]}`, ""), Limits{}, CodeInvalidDoc, "defs.gradients[0].gradientTransform")
	wantCode(t, minimal(`[`+rect("a", "")+`]`, "", `[{"layerId":"a","property":"opacity","keyframes":[],"extras":3}]`), Limits{}, CodeInvalidDoc, "animations[0].extras")
}

func TestKeyOrderAndDefaults(t *testing.T) {
	// Input order is scrambled and a newer-build key is present; output follows the schema.
	in := `{"zzz":1,"layers":[{"height":2,"width":1,"y":0,"x":0,"id":"a","extra":{"k":1},"type":"rect"}],"version":1,"canvas":{"future":true,"background":"red","width":3}}`
	want := `{"version":1,"canvas":{"width":3,"height":512,"viewBox":{"x":0,"y":0,"width":3,"height":512},"background":"red","future":true},"duration":3000,"fps":24,"layers":[{"type":"rect","id":"a","x":0,"y":0,"width":1,"height":2,"extra":{"k":1}}],"defs":{"gradients":[],"masks":[],"clipPaths":[],"filters":[],"symbols":[]},"animations":[],"zzz":1}`
	d := wantOK(t, in, Limits{})
	if got := string(d.Canonical()); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// groups put children last.
	g := wantOK(t, `{"version":1,"layers":[{"children":[],"fill":"red","hidden":true,"id":"g","type":"group"}]}`, Limits{})
	if !strings.Contains(string(g.Canonical()), `{"type":"group","id":"g","hidden":true,"fill":"red","children":[]}`) {
		t.Fatalf("group order: %s", g.Canonical())
	}
}

func TestCanonNumbersAndStrings(t *testing.T) {
	in := `{"version":1,"canvas":{"width":1.0,"height":1e2,"m":{"a":-0,"b":0.1,"c":1e21,"d":5e-7,"e":-0.0,"f":123456789012345680000,"g":1E+2,"s":"<>&\u0001\n \"\\/é\b\f\r\t"}}}`
	d := wantOK(t, in, Limits{})
	want := `"m":{"a":0,"b":0.1,"c":1e+21,"d":5e-7,"e":0,"f":123456789012345680000,"g":100,"s":"<>&\u0001\n` + " " + `\"\\/é\b\f\r\t"}`
	if got := string(d.Canonical()); !strings.Contains(got, want) || !strings.Contains(got, `"width":1,"height":100`) {
		t.Fatalf("got %s\nwant substring %s", got, want)
	}
	ind := string(wantOK(t, `{"version":1}`, Limits{}).CanonicalIndent())
	if !strings.HasSuffix(ind, "\"animations\": []\n}\n") || !strings.Contains(ind, "\"layers\": [],") {
		t.Fatalf("indent form: %q", ind)
	}
}

func TestAnimationValidation(t *testing.T) {
	tr := func(prop, kf string) string {
		return minimal(`[`+rect("a", "")+`]`, "", `[{"layerId":"a","property":"`+prop+`","keyframes":[`+kf+`]}]`)
	}
	for _, p := range []string{"transform.z", "gradientStop[x].color", "gradientStop[01].color", "gradientStop[1000].color", "fill ", "filter"} {
		wantCode(t, tr(p, ""), Limits{}, CodeInvalidDoc, "animations[0].property")
	}
	for _, p := range []string{"transform.anchorY", "gradientStop[0].offset", "gradientStop[999].color", "filter.blur", "motionPath", "d"} {
		wantOK(t, tr(p, ""), Limits{})
	}
	for _, e := range []string{`"bounce"`, `{"cubicBezier":[0,0,1]}`, `{"cubicBezier":[2,0,0.5,1]}`, `{"cubicBezier":[0,0,-1,1]}`, `{"other":1}`, `3`} {
		wantCode(t, tr("opacity", `{"t":0,"value":1,"easing":`+e+`}`), Limits{}, CodeInvalidDoc, "")
	}
	wantCode(t, tr("opacity", `{"t":0,"value":1}`), Limits{}, CodeInvalidDoc, "animations[0].keyframes[0].easing")
	wantCode(t, tr("opacity", `{"t":-1,"value":1,"easing":"linear"}`), Limits{}, CodeInvalidDoc, "animations[0].keyframes[0].t")
	wantCode(t, tr("opacity", `{"t":0,"value":null,"easing":"linear"}`), Limits{}, CodeInvalidDoc, "animations[0].keyframes[0].value")
	wantOK(t, tr("opacity", `{"t":0,"value":"M0 0","easing":{"cubicBezier":[0,5,1,-5],"x":1}}`), Limits{})
}

func raw(m string) string {
	return minimal(fmt.Sprintf(`[{"type":"raw","id":"r","markup":%q}]`, m), "", "")
}

func TestSecurity(t *testing.T) {
	const U = CodeUnsafeContent
	type row struct {
		name, doc, code, path string
	}
	rawBad := map[string]string{
		"script":                 `<script>alert(1)</script>`,
		"svg:script":             `<svg:script>alert(1)</svg:script>`,
		"spaced script":          `< script>x</script>`,
		"foreignObject":          `<foreignObject><div>x</div></foreignObject>`,
		"svg:foreignObject":      `<svg:foreignObject/>`,
		"mixed foreignobject":    `<g><ForeignObject/></g>`,
		"iframe srcdoc":          `<iframe srcdoc="x"/>`,
		"cdata":                  `<g><![CDATA[x]]></g>`,
		"comment":                `<g><!-- x --></g>`,
		"pi":                     `<?xml version="1.0"?><g/>`,
		"doctype":                `<!DOCTYPE svg><g/>`,
		"entity":                 `<!ENTITY a "b">`,
		"char ref js":            `<a href="#"><set attributeName="x" to="&#106;avascript:alert(1)"/></a>`,
		"nbsp":                   `<text>&nbsp;</text>`,
		"unquoted attr":          `<rect width=1/>`,
		"onload":                 `<rect onload="x()"/>`,
		"srcdoc":                 `<g srcdoc="x"/>`,
		"formaction":             `<g formaction="x"/>`,
		"srcset":                 `<g srcset="x"/>`,
		"ping":                   `<g ping="x"/>`,
		"background":             `<g background="x"/>`,
		"xmlns":                  `<g xmlns="http://www.w3.org/2000/svg"/>`,
		"xmlns:xlink":            `<g xmlns:xlink="http://www.w3.org/1999/xlink"/>`,
		"external href":          `<image href="https://e.test/a.png"/>`,
		"external xlink:href":    `<image xlink:href="//e.test/a.png"/>`,
		"external src":           `<image src="http://e.test/a.png"/>`,
		"svg data href":          `<image href="data:image/svg+xml;base64,AAAA"/>`,
		"use data href":          `<use href="data:image/png;base64,AAAA"/>`,
		"use external":           `<use xlink:href="http://e.test/x#a"/>`,
		"set href":               `<set attributeName="href" to="x"/>`,
		"set xlink:href":         `<animate attributeName="xlink:href" to="x"/>`,
		"url http":               `<rect fill="url(http://e.test/x)"/>`,
		"url protocol-relative":  `<rect fill="url('//e.test/x')"/>`,
		"image-set":              `<rect style="x:image-set(a)"/>`,
		"@import":                `<g>@import</g>`,
		"expression":             `<g>expression(1)</g>`,
		"backslash attr":         `<rect fill="\75rl(x)"/>`,
		"javascript url":         `<a href="javascript:alert(1)"/>`,
		"marquee":                `<marquee/>`,
		"style element":          `<style>.a{fill:red}</style>`,
		"namespaced unknown":     `<x:g/>`,
		"unknown closing":        `<g></marquee>`,
		"iframe":                 `<g><iframe/></g>`,
		"upper case element":     `<RECT/>`,
		"on handler after quote": `<rect fill="red"onclick="x()"/>`,
	}
	var rows []row
	for n, m := range rawBad {
		rows = append(rows, row{"raw " + n, raw(m), U, "layers[0].markup"})
	}
	paint := func(prop, val string) string {
		return minimal(`[`+rect("r", fmt.Sprintf(`%q:%q`, prop, val))+`]`, `{"gradients":[{"type":"linear","id":"g","x1":0,"y1":0,"x2":1,"y2":1,"stops":[]}]}`, "")
	}
	rows = append(rows,
		row{"fill backslash", paint("fill", `red\;`), U, "layers[0].fill"},
		row{"fill url https", paint("fill", `url(https://x)`), U, "layers[0].fill"},
		row{"fill url quoted", paint("fill", `url('#g')`), U, "layers[0].fill"},
		row{"fill url space", paint("fill", `url( #g )`), U, "layers[0].fill"},
		row{"fill semicolon", paint("fill", `red;x`), CodeInvalidDoc, "layers[0].fill"},
		row{"fill empty", paint("fill", ``), CodeInvalidDoc, "layers[0].fill"},
		row{"fill javascript", paint("stroke", `javascript:x`), U, "layers[0].stroke"},
		row{"background url(#a)", `{"version":1,"canvas":{"background":"url(#a)"}}`, CodeInvalidDoc, "canvas.background"},
		row{"background url https", `{"version":1,"canvas":{"background":"url(https://x)"}}`, U, "canvas.background"},
		row{"attr onclick", minimal(`[`+rect("r", `"attrs":{"onclick":"x"}`)+`]`, "", ""), U, "layers[0].attrs.onclick"},
		row{"attr href", minimal(`[`+rect("r", `"attrs":{"href":"#a"}`)+`]`, "", ""), U, "layers[0].attrs.href"},
		row{"attr style", minimal(`[`+rect("r", `"attrs":{"style":"fill:red"}`)+`]`, "", ""), U, "layers[0].attrs.style"},
		row{"attr bad key", minimal(`[`+rect("r", `"attrs":{"Class":"x"}`)+`]`, "", ""), CodeInvalidDoc, "layers[0].attrs.Class"},
		row{"attr javascript", minimal(`[`+rect("r", `"attrs":{"class":"javascript:x"}`)+`]`, "", ""), U, "layers[0].attrs.class"},
		row{"attr data-html", minimal(`[`+rect("r", `"attrs":{"data-x":"data:text/html,x"}`)+`]`, "", ""), U, "layers[0].attrs.data-x"},
		row{"attr url", minimal(`[`+rect("r", `"attrs":{"marker-end":"url(http://x)"}`)+`]`, "", ""), U, "layers[0].attrs.marker-end"},
		row{"attr backslash", minimal(`[`+rect("r", `"attrs":{"class":"a\\b"}`)+`]`, "", ""), U, "layers[0].attrs.class"},
	)
	for _, h := range []string{"https://x.example/a.png", "data:image/svg+xml;base64,AAAA", "javascript:x", "images/a.png", "asset:", "asset:a b", "DATA:IMAGE/PNG;base64,AA"} {
		rows = append(rows, row{"image " + h, minimal(fmt.Sprintf(`[{"type":"image","id":"i","x":0,"y":0,"width":1,"height":1,"href":%q}]`, h), "", ""), U, "layers[0].href"})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { wantCode(t, r.doc, Limits{}, r.code, r.path) })
	}

	ok := []struct{ name, doc string }{
		{"fill url + fallback", paint("fill", `url(#g) red`)},
		{"fill none", paint("fill", `none`)},
		{"fill rgb", paint("fill", `rgb(1, 2, 3)`)},
		{"data-x attr", minimal(`[`+rect("r", `"attrs":{"data-x":"1"}`)+`]`, "", "")},
		{"paint-order attr", minimal(`[`+rect("r", `"attrs":{"paint-order":"stroke"}`)+`]`, "", "")},
		{"png data", minimal(`[{"type":"image","id":"i","x":0,"y":0,"width":1,"height":1,"href":"data:image/png;base64,AAAA"}]`, "", "")},
		{"asset", minimal(`[{"type":"image","id":"i","x":0,"y":0,"width":1,"height":1,"href":"asset:abc"}]`, "", "")},
		{"raw ok", raw(`<g><use href="#a"/><image href="data:image/png;base64,AAAA"/><a><rect fill="url(#g)" style="fill:url(#g)"/></a><text>a &amp; b &lt;</text><feGaussianBlur stdDeviation="2"/></g>`)},
		{"raw use xlink local", raw(`<use xlink:href="#a"/>`)},
		{"raw animate other attr", raw(`<animate attributeName="r" values="1;2" dur="1s"/>`)},
	}
	for _, c := range ok {
		t.Run("ok "+c.name, func(t *testing.T) { wantOK(t, c.doc, Limits{}) })
	}
}
