package assistant

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func embedded(t *testing.T) *EmbeddedKnowledge {
	t.Helper()
	k, err := NewEmbeddedKnowledge()
	if err != nil {
		t.Fatalf("embedded knowledge: %v", err)
	}
	return k
}

func ids(hs []Hit) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Doc.ID
	}
	return out
}

// The embedded export parses and has every kind of doc (counts logged in the ticket).
func TestEmbeddedCorpusCounts(t *testing.T) {
	c := embedded(t).Corpus()
	n := map[string]int{}
	for _, d := range c.Docs {
		key := d.Kind
		if d.Kind == "editor" {
			key += "/" + d.Category
		}
		n[key]++
		if d.Body == "" || d.Title == "" {
			t.Errorf("%s: empty title/body", d.ID)
		}
		if (d.Kind == "tool" || d.Kind == "page") && !strings.HasPrefix(d.URL, "/") {
			t.Errorf("%s: no route", d.ID)
		}
	}
	t.Logf("docs: %v (total %d, locales %d)", n, len(c.Docs), len(c.names))
	if n["tool"] < 100 || n["page"] < 20 || n["editor/studio"] < 4 || n["editor/vector"] < 4 || n["editor/photo"] < 6 {
		t.Fatalf("corpus counts = %v", n)
	}
	if len(c.names) != 18 {
		t.Errorf("names for %d locales, want 18", len(c.names))
	}
	if c.routes == nil || !c.routes.HasRoute("/tools/compress-video") {
		t.Error("routes.json not loaded")
	}
}

// Ticket acceptance 3: the retrieval table (top 3 per query).
func TestRetrievalAcceptance(t *testing.T) {
	k := embedded(t)
	cases := []struct {
		surface, q, want string
		top              int // the doc must be within the first `top` hits
	}{
		{SurfaceGuide, "how do I make a gif from a video", "tools/convert-video-to-animated-gif", 3},
		{SurfaceGuide, "remove background", "tools/remove-background-from-image", 3},
		{SurfacePhoto, "grey except red", "editors/photo/color", 1},
		{SurfaceStudio, "lower third", "editors/studio/titles", 1},
		{SurfaceVector, "draw on", "editors/vector/animators", 1},
		{SurfaceGuide, "сжать видео", "tools/compress-video", 1},
		{SurfaceGuide, "動画を圧縮したい", "tools/compress-video", 1},
		{SurfaceGuide, "Hintergrund entfernen", "tools/remove-background-from-image", 1},
		{SurfaceGuide, "how do I cancel my subscription", "pages/pricing", 3},
		{SurfaceGuide, "what does premium include", "pages/pricing", 3},
		{SurfacePhoto, "it's backlit, make it brighter", "editors/photo/light", 3},
		{SurfaceStudio, "cinematic grade", "editors/studio/effects", 1},
		{SurfaceVector, "export to lottie", "editors/vector/export", 3},
		{SurfacePhoto, "vintage film look", "editors/photo/presets", 1},
	}
	for _, c := range cases {
		hs := k.Search(c.q, c.surface, 3)
		got := ids(hs)
		t.Logf("%-6s %-38q → %s", c.surface, c.q, strings.Join(got, ", "))
		found := false
		for i, id := range got {
			if id == c.want && i < c.top {
				found = true
			}
		}
		if !found {
			t.Errorf("%s %q: want %s in top %d, got %v", c.surface, c.q, c.want, c.top, got)
		}
		for _, h := range hs {
			if h.Snippet == "" || len([]rune(h.Snippet)) > snippetChars {
				t.Errorf("%s: snippet length %d", h.Doc.ID, len([]rune(h.Snippet)))
			}
		}
	}
}

// Decision 5: the kind filter by surface.
func TestRetrievalSurfaceFilter(t *testing.T) {
	k := embedded(t)
	for _, h := range k.Search("lumetri exposure grade transitions keyframes", SurfaceGuide, 20) {
		if strings.HasPrefix(h.Doc.ID, "editors/") {
			t.Errorf("guide surfaced an editor doc %s", h.Doc.ID)
		}
	}
	for _, h := range k.Search("pricing privacy policy account billing", SurfacePhoto, 20) {
		if strings.HasPrefix(h.Doc.ID, "pages/") {
			t.Errorf("photo copilot surfaced a page %s", h.Doc.ID)
		}
	}
	for _, h := range k.Search("keyframe easing animate", SurfacePhoto, 20) {
		if strings.HasPrefix(h.Doc.ID, "editors/vector") || strings.HasPrefix(h.Doc.ID, "editors/studio") {
			t.Errorf("photo copilot surfaced another editor's doc %s", h.Doc.ID)
		}
	}
	if len(k.Search("", SurfaceGuide, 4)) != 0 || len(k.Search("compress", SurfaceGuide, 0)) != 0 {
		t.Error("empty query or k = 0 returns nothing")
	}
	if d, ok := k.Get("tools/compress-video"); !ok || d.URL != "/tools/compress-video" || !strings.Contains(d.Body, "CRF") {
		t.Errorf("Get tools/compress-video = %+v", d.ID)
	}
	if _, ok := k.Get("tools/nope"); ok {
		t.Error("unknown doc")
	}
}

// Decision 7, adapted from Winny's scope_test.go.
func TestScopeGate(t *testing.T) {
	k := embedded(t)
	cases := map[string]bool{
		"write me a poem about cats":          false,
		"what's the weather in Paris":         false,
		"who won the football game":           false,
		"tell me a joke":                      false,
		"best pizza recipe":                   false,
		"explain quantum physics":             false,
		"write a cover letter for me":         false,
		"напиши стихотворение":                false,
		"how do I compress a video":           true,
		"make a gif from a video":             true,
		"what does premium include?":          true,
		"what is a codec":                     true,
		"what bitrate for YouTube 1080p?":     true,
		"why did my job fail":                 true,
		"what can you do on this page":        true,
		"what can you do":                     true,
		"hi":                                  true,
		"Сколько стоит премиум?":              true,
		"сжать видео":                         true,
		"Was kostet Premium?":                 true,
		"¿Cómo quito el fondo de una foto?":   true,
		"動画を圧縮したい":                            true,
		"كيف أضغط فيديو":                      true,
		"Edit a photo like a pro":             true,
		"how do I add a lower third":          true,
		"teal and orange look":                true,
		"is my data safe, do you store files": true,
	}
	for q, want := range cases {
		if got := k.InScope(q); got != want {
			t.Errorf("%q: in scope = %v, want %v", q, got, want)
		}
	}
}

// Decision 6: capsules are byte-stable, budgeted, numberless and per surface.
func TestCapsules(t *testing.T) {
	k := embedded(t)
	k2, err := NewKnowledgeFrom(knowledgeFS, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{SurfaceGuide, SurfaceStudio, SurfaceVector, SurfacePhoto} {
		c := k.Capsule(s)
		if c == "" || c != k2.Capsule(s) {
			t.Fatalf("%s capsule empty or not byte-stable", s)
		}
		tok := CapsuleTokens(c)
		t.Logf("capsule %-6s %6d chars ≈ %5d tokens", s, len(c), tok)
		if tok > MaxCapsuleTokens {
			t.Errorf("%s capsule %d tokens > %d", s, tok, MaxCapsuleTokens)
		}
		for _, banned := range []string{"$", "€", " a day", "per day", "2026"} {
			if strings.Contains(c, banned) {
				t.Errorf("%s capsule contains %q", s, banned)
			}
		}
	}
	if !strings.Contains(k.Capsule(SurfaceGuide), "/tools/compress-video") || !strings.Contains(k.Capsule(SurfaceGuide), "/pricing") {
		t.Error("guide capsule is the site map")
	}
	if !strings.Contains(k.Capsule(SurfacePhoto), "`hsl`") || strings.Contains(k.Capsule(SurfacePhoto), "lumetri") {
		t.Error("photo capsule holds Aperture's vocabulary only")
	}
	if !strings.Contains(k.Capsule(SurfaceStudio), "crossDissolve") || !strings.Contains(k.Capsule(SurfaceVector), "drawOn") {
		t.Error("studio/vector capsules hold their vocabularies")
	}
	// The cached prompt block carries the capsule.
	if b := CachedBlock(SurfacePhoto, k); !b.Cache || !strings.HasSuffix(b.Text, k.Capsule(SurfacePhoto)) {
		t.Error("CachedBlock appends the capsule")
	}
}

// Decision 8: localized names for retrieved docs, locale prefix fallbacks.
func TestLocalNames(t *testing.T) {
	k := embedded(t)
	for _, c := range []struct{ locale, name, path string }{
		{"ru-RU", "Сжатие видео", "/ru/tools/compress-video"},
		{"ru", "Сжатие видео", "/ru/tools/compress-video"},
		{"es", "", "/es/tools/compress-video"},
		{"es-MX", "", "/es-mx/tools/compress-video"},
		{"pt-PT", "", "/pt-pt/tools/compress-video"},
		{"en-US", "Compress Video", "/tools/compress-video"},
	} {
		n, ok := k.LocalName("tools/compress-video", c.locale)
		if !ok || n.Path != c.path || (c.name != "" && n.Name != c.name) {
			t.Errorf("%s: %+v %v", c.locale, n, ok)
		}
	}
	if _, ok := k.LocalName("tools/compress-video", "xx"); ok {
		t.Error("unknown locale has no names")
	}
	hits := k.Search("сжать видео", SurfaceGuide, 3)
	names := localNames(k, hits, "ru-RU")
	if names["tools/compress-video"].Name != "Сжатие видео" {
		t.Fatalf("local names = %v", names)
	}
	if localNames(k, hits, "en-US") != nil {
		t.Error("English needs no name block")
	}
	vb := VolatileBlock(LiveContext{Surface: SurfaceGuide, Locale: "ru-RU", Hits: hits, LocalNames: names})
	if !strings.Contains(vb.Text, `"Сжатие видео" at /ru/tools/compress-video`) {
		t.Errorf("volatile block lacks the localized name:\n%s", vb.Text)
	}
}

// The off-topic guide message is answered without a model call or a charge.
func TestEngineScopeGateRefuses(t *testing.T) {
	v := newEnv(t)
	v.e.Knowledge = embedded(t)
	v.run(v.turn(guideMsg("write me a poem about cats"), ""), nil)
	if len(v.p.requests()) != 0 {
		t.Fatal("no model call for an off-topic guide message")
	}
	if got := strings.Join(v.em.types(), ","); got != "meta,delta,usage,done" {
		t.Fatalf("events = %s", got)
	}
	if txt := v.em.events[1]["text"].(string); txt != OutOfScopeLine("en-US") {
		t.Errorf("reply = %q", txt)
	}
	// An in-scope guide message goes to the model with retrieved docs in block 2.
	v2 := newEnv(t, round{deltas: []string{"Open Compress video."}})
	v2.e.Knowledge = embedded(t)
	v2.run(v2.turn(guideMsg("how do I compress a video"), ""), nil)
	reqs := v2.p.requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].System[1].Text, `<help id="tools/compress-video">`) {
		t.Fatalf("in-scope request missing the retrieved doc")
	}
	if !strings.Contains(reqs[0].System[0].Text, "Site map.") {
		t.Error("guide cached block carries the site-map capsule")
	}
	// Copilot surfaces are never scope-gated.
	v3 := newEnv(t, round{deltas: []string{"ok"}})
	v3.e.Knowledge = embedded(t)
	v3.run(v3.turn(photoMsg("write me a poem about cats"), "user-1"), nil)
	if v3.em.events[1]["text"] != "ok" {
		t.Errorf("the copilot is not scope-gated: %v", v3.em.events)
	}
	_ = context.Background
}

// The corpus parser reads the exporter's doc shape, recipes included (MM106 hook).
func TestLoadCorpusShapeAndRecipes(t *testing.T) {
	fsys := fstest.MapFS{
		"k/tools/a.md":                {Data: []byte("<!-- generated -->\n\n# Tool A\n\n- Route: `/tools/a`\n- Kind: `tool`\n- Category: `video`\n- Slug: `a`\n\n## Summary\n\nDoes A.\n\n## Details\n\nLong text about widgets.\n\n## Search keywords\n\n`a`, `widget maker`\n\n## Links\n\n- (none)\n\n## Opens\n\n- Panel: A.\n")},
		"k/recipes/photo/moody.md":    {Data: []byte("# Moody photo\n\n- Kind: `recipe`\n- Category: `photo`\n\n## Summary\n\nDark and rich.\n\n## Details\n\nLower exposure.\n\n## Search keywords\n\n`moody`, `dark`, `dramatic`\n\n## Links\n\n- (none)\n")},
		"k/recipes/workflow/short.md": {Data: []byte("# YouTube Short\n\n- Kind: `recipe`\n- Category: `workflow`\n\n## Summary\n\nVertical cut.\n\n## Details\n\nSteps.\n\n## Search keywords\n\n`short`\n\n## Links\n\n- (none)\n")},
		"k/names/ru-RU.json":          {Data: []byte(`{"tools/a":{"name":"Инструмент А","path":"/ru/tools/a"}}`)},
		"k/routes.json":               {Data: []byte(`[{"route":"/tools/a","kind":"tool","slug":"a"}]`)},
		"k/index.json":                {Data: []byte(`{}`)},
		"k/README.md":                 {Data: []byte("# readme")},
	}
	k, err := NewKnowledgeFrom(fsys, "k")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := k.Corpus().Doc("tools/a")
	if !ok || d.URL != "/tools/a" || d.Kind != "tool" || d.Slug != "a" || d.Summary != "Does A." || d.Opens != "- Panel: A." ||
		len(d.Keywords) != 2 || d.Keywords[1] != "widget maker" || len(d.Names) != 1 {
		t.Fatalf("parsed = %+v", d)
	}
	if r, _ := k.Corpus().Doc("recipes/photo/moody"); r.Kind != "recipe" || r.Category != "photo" {
		t.Fatalf("recipe = %+v", r)
	}
	if got := ids(k.Search("moody dramatic", SurfacePhoto, 3)); len(got) == 0 || got[0] != "recipes/photo/moody" {
		t.Errorf("photo copilot finds its recipe: %v", got)
	}
	if got := ids(k.Search("moody dramatic", SurfaceGuide, 3)); len(got) != 0 {
		t.Errorf("guide does not see editor recipes: %v", got)
	}
	if got := ids(k.Search("youtube short", SurfaceGuide, 3)); len(got) == 0 || got[0] != "recipes/workflow/short" {
		t.Errorf("guide sees workflow recipes: %v", got)
	}
	if !strings.Contains(k.Capsule(SurfacePhoto), "Moody photo [recipes/photo/moody]") {
		t.Error("the photo capsule lists the editor's recipes")
	}
	if _, err := NewKnowledgeFrom(fstest.MapFS{"k/tools/x.md": {Data: []byte("no title")}}, "k"); err == nil {
		t.Error("a malformed doc is an error")
	}
}

func TestTokenizerAndAliases(t *testing.T) {
	for in, want := range map[string]string{
		"Trimming videos": "trim video",
		"Resized images":  "resiz imag",
		"colour grading":  "color grad",
		"Сжать видео":     "сжат виде",
		"动画":              "动画",
		"what is the":     "",
	} {
		if got := strings.Join(tokenize(in), " "); got != want {
			t.Errorf("tokenize(%q) = %q, want %q", in, got, want)
		}
	}
	if q := expandQuery("remove bg please"); !strings.Contains(q, "background") {
		t.Errorf("alias bg: %q", q)
	}
	if q := expandQuery("a lower third"); !strings.Contains(q, "title") {
		t.Errorf("alias lower third: %q", q)
	}
	if q := expandQuery("background"); q != "background" {
		t.Errorf("no alias inside words: %q", q)
	}
}

// The knowledge dir is generated: no hand-written file besides the export.
func TestKnowledgeDirIsGenerated(t *testing.T) {
	err := filepath.Walk("knowledge", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b[:min(len(b), 400)]), "npm run export:medi") {
			t.Errorf("%s lacks the generated line (fix the exporter, never the file)", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
