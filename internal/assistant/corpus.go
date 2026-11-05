package assistant

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// The generated corpus (MM105, design §7.1): knowledge/ is written by
// `npm run export:medi` in media-manipulator-ui and embedded here. Every .md
// doc has the exporter's shape:
//
//	# Title
//	- Route: `/tools/x`   (optional)
//	- Kind: `tool|page|editor|recipe`
//	- Category: `video`
//	- Slug: `x`           (optional)
//	## Summary / ## Details / ## Search keywords / ## Links / ## Opens
//
// Recipes (MM106) land under knowledge/recipes/<photo|studio|vector|workflow>/
// in the same shape (intents as search keywords); their kind is "recipe" and
// their category the editor (or "workflow"), whatever the file says.

// CorpusDoc is one parsed knowledge doc.
type CorpusDoc struct {
	Doc
	Kind     string // tool | page | editor | recipe
	Category string // tool category, page group, editor surface, recipe editor
	Slug     string
	Summary  string
	Details  string
	Opens    string
	// Routing is the MM116a sidecar copy (when to send a visitor here or elsewhere, related tools).
	Routing string
	// Options is the tool's option schema (MM116a); nil for non-tool docs.
	Options  *ToolOptions
	Links    string
	Keywords []string
	// Names are the doc's localized names (every locale), for retrieval.
	Names []string
}

// LocalName is a doc's name and locale-prefixed path in one locale.
type LocalName struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// parseCorpusDoc reads one exported .md file; id is its path without .md.
func parseCorpusDoc(id, text string) (CorpusDoc, error) {
	d := CorpusDoc{Doc: Doc{ID: id}}
	sections := map[string]*strings.Builder{}
	var cur *strings.Builder
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "<!--"):
			continue
		case strings.HasPrefix(line, "# ") && d.Title == "":
			d.Title = strings.TrimSpace(line[2:])
			continue
		case strings.HasPrefix(line, "## "):
			cur = &strings.Builder{}
			sections[strings.TrimSpace(line[3:])] = cur
			continue
		}
		if cur == nil {
			if v, ok := bulletValue(line, "Route"); ok {
				d.URL = v
			} else if v, ok := bulletValue(line, "Kind"); ok {
				d.Kind = v
			} else if v, ok := bulletValue(line, "Category"); ok {
				d.Category = v
			} else if v, ok := bulletValue(line, "Slug"); ok {
				d.Slug = v
			}
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	get := func(name string) string {
		if b, ok := sections[name]; ok {
			return strings.TrimSpace(b.String())
		}
		return ""
	}
	d.Summary, d.Details, d.Opens, d.Links, d.Routing = get("Summary"), get("Details"), get("Opens"), get("Links"), get("Routing")
	for _, k := range strings.Split(get("Search keywords"), ",") {
		if k = strings.Trim(strings.TrimSpace(k), "`"); k != "" {
			d.Keywords = append(d.Keywords, k)
		}
	}
	if strings.HasPrefix(id, "recipes/") {
		d.Kind = "recipe"
		if parts := strings.Split(id, "/"); len(parts) >= 3 {
			d.Category = parts[1]
		}
	}
	if d.Title == "" || d.Kind == "" {
		return d, fmt.Errorf("knowledge doc %s: missing title or kind", id)
	}
	d.buildBody()
	return d, nil
}

// buildBody is what the model reads: the summary, what the page opens with, the
// routing copy and option fields (MM116a; ahead of the long details so the
// 6000-character cut of a retrieved doc never drops them), then the details.
func (d *CorpusDoc) buildBody() {
	body := []string{d.Summary}
	if d.Opens != "" {
		body = append(body, "Opens:\n"+d.Opens)
	}
	if d.Routing != "" {
		body = append(body, "Routing:\n"+d.Routing)
	}
	if d.Options != nil {
		body = append(body, d.Options.Render())
	}
	if d.Details != "" {
		body = append(body, d.Details)
	}
	d.Body = strings.Join(body, "\n\n")
}

func bulletValue(line, key string) (string, bool) {
	p := "- " + key + ": `"
	if !strings.HasPrefix(line, p) || !strings.HasSuffix(line, "`") {
		return "", false
	}
	return line[len(p) : len(line)-1], true
}

// Corpus is the parsed knowledge base: docs by id, localized names, routes.
type Corpus struct {
	Docs   []CorpusDoc // sorted by id
	byID   map[string]int
	names  map[string]map[string]LocalName // locale code (lowercase) → doc id → name
	routes *Routes
	// options are the MM116a per-tool option schemas, by tool slug.
	options map[string]*ToolOptions
}

// LoadCorpus parses a knowledge tree (the embedded one, or a test fixture).
// root is the directory inside fsys that holds tools/, pages/, editors/ ….
func LoadCorpus(fsys fs.FS, root string) (*Corpus, error) {
	c := &Corpus{byID: map[string]int{}, names: map[string]map[string]LocalName{}, options: map[string]*ToolOptions{}}
	err := fs.WalkDir(fsys, root, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		switch {
		case rel == "README.md" || rel == "index.json":
			return nil
		case rel == "routes.json":
			r, err := ParseRoutes(b)
			if err != nil {
				return fmt.Errorf("routes.json: %w", err)
			}
			c.routes = r
		case strings.HasPrefix(rel, "names/") && strings.HasSuffix(rel, ".json"):
			m := map[string]LocalName{}
			if err := json.Unmarshal(b, &m); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			c.names[strings.ToLower(strings.TrimSuffix(path.Base(rel), ".json"))] = m
		case strings.HasPrefix(rel, "tools/") && strings.HasSuffix(rel, ".options.json"):
			o, err := parseOptionsFile(b)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if want := strings.TrimSuffix(path.Base(rel), ".options.json"); o.Slug != want {
				return fmt.Errorf("%s: slug %q does not match the file name", rel, o.Slug)
			}
			c.options[o.Slug] = o
		case strings.HasSuffix(rel, ".md"):
			d, err := parseCorpusDoc(strings.TrimSuffix(rel, ".md"), string(b))
			if err != nil {
				return err
			}
			c.Docs = append(c.Docs, d)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(c.Docs, func(i, j int) bool { return c.Docs[i].ID < c.Docs[j].ID })
	for i := range c.Docs {
		if d := &c.Docs[i]; d.Kind == "tool" && d.Slug != "" {
			if o, ok := c.options[d.Slug]; ok {
				d.Options = o
				d.buildBody()
			}
		}
	}
	locs := make([]string, 0, len(c.names))
	for l := range c.names {
		locs = append(locs, l)
	}
	sort.Strings(locs)
	for i := range c.Docs {
		c.byID[c.Docs[i].ID] = i
		seen := map[string]bool{}
		for _, l := range locs {
			if n, ok := c.names[l][c.Docs[i].ID]; ok && !seen[n.Name] {
				seen[n.Name] = true
				c.Docs[i].Names = append(c.Docs[i].Names, n.Name)
			}
		}
	}
	return c, nil
}

// Doc returns a parsed doc by id.
func (c *Corpus) Doc(id string) (CorpusDoc, bool) {
	i, ok := c.byID[id]
	if !ok {
		return CorpusDoc{}, false
	}
	return c.Docs[i], true
}

// LocalName returns a doc's name and path in a locale ("ru-RU", "ru", "es-mx"…).
func (c *Corpus) LocalName(id, locale string) (LocalName, bool) {
	m := c.namesFor(locale)
	if m == nil {
		return LocalName{}, false
	}
	n, ok := m[id]
	return n, ok
}

// namesFor resolves a locale tag to a names table: exact code, then the
// site's URL prefix ("ru" → ru-ru, "es" → es-es, "pt" → pt-br is NOT guessed:
// only a unique language match is taken).
func (c *Corpus) namesFor(locale string) map[string]LocalName {
	l := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(locale), "_", "-"))
	if m, ok := c.names[l]; ok {
		return m
	}
	lang := l
	if i := strings.Index(l, "-"); i > 0 {
		lang = l[:i]
	}
	var hit map[string]LocalName
	n := 0
	for code, m := range c.names {
		if strings.HasPrefix(code, lang+"-") {
			hit = m
			n++
		}
	}
	if n == 1 {
		return hit
	}
	switch lang {
	case "es":
		return c.names["es-es"]
	case "zh":
		return c.names["zh-cn"]
	case "pt":
		return c.names["pt-br"]
	}
	return nil
}

// ToolOptions returns a tool's option schema by slug (MM116a).
func (c *Corpus) ToolOptions(slug string) (*ToolOptions, bool) {
	o, ok := c.options[slug]
	return o, ok
}
