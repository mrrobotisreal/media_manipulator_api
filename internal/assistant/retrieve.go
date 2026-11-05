package assistant

import (
	"embed"
	"encoding/json"
	"io/fs"
	"strings"
)

// Surface names (the wire values of the request's `surface`).
const (
	SurfaceGuide  = "guide"
	SurfaceStudio = "studio"
	SurfaceVector = "vector"
	SurfacePhoto  = "photo"
)

// ValidSurface reports whether s is a known surface.
func ValidSurface(s string) bool {
	switch s {
	case SurfaceGuide, SurfaceStudio, SurfaceVector, SurfacePhoto:
		return true
	}
	return false
}

// Doc is one knowledge document (a tool page, a site page, an editor
// capability doc or a recipe). ID is stable, e.g. "tools/compress-video",
// "pages/pricing", "recipes/photo/grey-except-red".
type Doc struct {
	ID    string
	Title string
	// URL is the site-relative route of the doc ("" for editor docs/recipes).
	URL  string
	Body string
}

// Hit is one search result: a doc and a short snippet.
type Hit struct {
	Doc     Doc
	Snippet string
	Score   float64
}

// Retriever finds knowledge for a message. MM105 implements it with BM25 over
// the generated corpus + recipes; MM104 ships a stub that finds nothing.
//
//	Search  ranks docs for q on a surface ("guide" | "studio" | "vector" | "photo"), top k
//	Get     returns one doc by id (get_recipe "recipes/<editor>/<id>", get_tool_page "tools/<slug>")
type Retriever interface {
	Search(q, surface string, k int) []Hit
	Get(id string) (Doc, bool)
}

// Knowledge is everything the engine reads from the generated corpus.
//
//	Retriever  search + get
//	Capsule    the byte-stable per-surface block appended to the persona in the
//	           cached system block 1 (MM105: guide site map; editor op vocabulary)
//	Routes     the closed list of navigable targets (knowledge/routes.json)
//	InScope    the guide's cheap pre-model scope gate (MM105; the stub says yes)
type Knowledge interface {
	Retriever
	Capsule(surface string) string
	Routes() *Routes
	InScope(q string) bool
	// ToolOptions is a tool's option schema (MM116a); false when the corpus has none.
	ToolOptions(slug string) (*ToolOptions, bool)
}

// RouteEntry is one navigable target in routes.json (design §7.1).
type RouteEntry struct {
	Route   string   `json:"route"`
	Kind    string   `json:"kind"` // page | tool | editor
	Slug    string   `json:"slug,omitempty"`
	Presets []string `json:"presets,omitempty"`
}

// Routes indexes routes.json for the navigate/open_tool validators. A nil
// *Routes (no routes.json yet) validates the shape only.
type Routes struct {
	byRoute map[string]RouteEntry
	bySlug  map[string]RouteEntry
}

// ParseRoutes reads routes.json (an array of RouteEntry).
func ParseRoutes(b []byte) (*Routes, error) {
	var entries []RouteEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	r := &Routes{byRoute: map[string]RouteEntry{}, bySlug: map[string]RouteEntry{}}
	for _, e := range entries {
		r.byRoute[e.Route] = e
		if e.Kind == "tool" && e.Slug != "" {
			r.bySlug[e.Slug] = e
		}
	}
	return r, nil
}

// HasRoute reports whether target is a listed route. A locale prefix
// ("/ru/pricing") is accepted when the unprefixed route is listed.
func (r *Routes) HasRoute(target string) bool {
	if r == nil {
		return true
	}
	if _, ok := r.byRoute[target]; ok {
		return true
	}
	if i := strings.Index(strings.TrimPrefix(target, "/"), "/"); i > 0 {
		_, ok := r.byRoute[target[i+1:]]
		return ok
	}
	return false
}

// Tool returns a tool route by slug. With no routes.json every slug passes
// and presets are unchecked.
func (r *Routes) Tool(slug string) (RouteEntry, bool) {
	if r == nil {
		return RouteEntry{Kind: "tool", Slug: slug}, true
	}
	e, ok := r.bySlug[slug]
	return e, ok
}

// knowledgeFS is the generated corpus (MM105 writes it with `npm run
// export:medi`; never hand-edited).
//
//go:embed knowledge
var knowledgeFS embed.FS

// EmbeddedRoutes parses knowledge/routes.json when it exists (nil otherwise).
func EmbeddedRoutes() *Routes {
	b, err := fs.ReadFile(knowledgeFS, "knowledge/routes.json")
	if err != nil {
		return nil
	}
	r, err := ParseRoutes(b)
	if err != nil {
		return nil
	}
	return r
}

// stubKnowledge is MM104's Knowledge: no documents, no capsule, everything in
// scope, routes from knowledge/routes.json when present.
type stubKnowledge struct{ routes *Routes }

// NewStubKnowledge is the MM104 knowledge base (MM105 replaces it).
func NewStubKnowledge() Knowledge { return stubKnowledge{routes: EmbeddedRoutes()} }

func (stubKnowledge) Search(string, string, int) []Hit        { return nil }
func (stubKnowledge) Get(string) (Doc, bool)                  { return Doc{}, false }
func (stubKnowledge) Capsule(string) string                   { return "" }
func (k stubKnowledge) Routes() *Routes                       { return k.routes }
func (stubKnowledge) InScope(string) bool                     { return true }
func (stubKnowledge) ToolOptions(string) (*ToolOptions, bool) { return nil, false }
