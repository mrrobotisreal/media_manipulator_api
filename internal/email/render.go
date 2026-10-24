package email

import (
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"strconv"
	"strings"
)

//go:embed locales/*.json
var localeFS embed.FS

// Data is everything a template can show. Nothing here is a secret, and file
// names appear only in the expiry digest (the owner's own files, in their own mail).
type Data struct {
	Name    string   // display name; empty = no-name greeting
	BaseURL string   // https://media-manipulator.com
	Failed  bool     // ai_job_finished: the job failed
	Days    int      // expiry_digest: window length
	Count   int      // expiry_digest: total expiring items
	Names   []string // expiry_digest: top names (already capped)
}

// Rendered is a finished message body.
type Rendered struct {
	Subject string
	Text    string
	HTML    string
	// PreferencesURL is the footer / List-Unsubscribe target ("" for account_deleted).
	PreferencesURL string
}

// Renderer turns (kind, locale, Data) into subject + text + HTML using the
// per-locale `email.*` string tables. A missing locale file or key falls back to
// en-us, so a half-translated locale still sends something readable.
type Renderer struct {
	tables map[string]map[string]string // locale -> flattened "email.x.y" -> string
}

// NewRenderer loads the embedded locale tables.
func NewRenderer() (*Renderer, error) {
	r := &Renderer{tables: map[string]map[string]string{}}
	for _, loc := range Locales {
		raw, err := localeFS.ReadFile("locales/" + loc + ".json")
		if err != nil {
			if loc == DefaultLocale {
				return nil, fmt.Errorf("email: missing default locale file: %w", err)
			}
			continue // falls back to en-us
		}
		var tree map[string]any
		if err := json.Unmarshal(raw, &tree); err != nil {
			return nil, fmt.Errorf("email: locales/%s.json: %w", loc, err)
		}
		flat := map[string]string{}
		flatten("", tree, flat)
		r.tables[loc] = flat
	}
	return r, nil
}

// MustRenderer is NewRenderer for package-level wiring; the embedded files are
// validated by a test, so a failure here is a build defect.
func MustRenderer() *Renderer {
	r, err := NewRenderer()
	if err != nil {
		panic(err)
	}
	return r
}

func flatten(prefix string, in map[string]any, out map[string]string) {
	for k, v := range in {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch t := v.(type) {
		case string:
			out[key] = t
		case map[string]any:
			flatten(key, t, out)
		}
	}
}

// Has reports whether the locale has its own table (not the fallback).
func (r *Renderer) Has(locale string) bool {
	_, ok := r.tables[LocaleOrDefault(locale)]
	return ok
}

// lookup returns the raw string for key in locale, falling back to en-us.
func (r *Renderer) lookup(locale, key string) string {
	if t, ok := r.tables[locale]; ok {
		if s, ok := t[key]; ok && s != "" {
			return s
		}
	}
	return r.tables[DefaultLocale][key]
}

// t resolves key with {{var}} interpolation. When vars has "count" the CLDR
// plural suffixed key (key_one, key_few, ...) is preferred, then key_other.
func (r *Renderer) t(locale, key string, vars map[string]string) string {
	s := ""
	if c, ok := vars["count"]; ok {
		n, _ := strconv.Atoi(c)
		for _, k := range []string{key + "_" + pluralCategory(locale, n), key + "_other"} {
			if s = r.lookup(locale, k); s != "" {
				break
			}
		}
	}
	if s == "" {
		s = r.lookup(locale, key)
	}
	if s == "" {
		return key
	}
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	return s
}

func (r *Renderer) prefsURL(locale string, d Data) string {
	return strings.TrimRight(d.BaseURL, "/") + urlPrefix(locale) + "/account?tab=profile"
}

type block struct {
	kind string // p, li, cta, small
	text string
	url  string
}

// Render builds the message for kind. Unknown kinds are an error.
func (r *Renderer) Render(kind Kind, locale string, d Data) (Rendered, error) {
	locale = LocaleOrDefault(locale)
	base := strings.TrimRight(d.BaseURL, "/") + urlPrefix(locale)
	e := "email." + string(kindKey(kind)) + "."
	var blocks []block
	var subject string
	prefs := true

	greeting := r.t(locale, "email.common.greetingNoName", nil)
	if n := strings.TrimSpace(d.Name); n != "" {
		greeting = r.t(locale, "email.common.greeting", map[string]string{"name": n})
	}
	blocks = append(blocks, block{kind: "p", text: greeting})

	switch kind {
	case KindWelcome:
		subject = r.t(locale, e+"subject", nil)
		blocks = append(blocks,
			block{kind: "p", text: r.t(locale, e+"intro", nil)},
			block{kind: "li", text: r.t(locale, e+"history", nil)},
			block{kind: "li", text: r.t(locale, e+"retention", nil)},
			block{kind: "li", text: r.t(locale, e+"darkroom", nil)},
			block{kind: "cta", text: r.t(locale, e+"cta", nil), url: base + "/account"})
	case KindAIJobFinished:
		subject = r.t(locale, e+"subject", nil)
		body := e + "completed"
		if d.Failed {
			body = e + "failed"
		}
		blocks = append(blocks,
			block{kind: "p", text: r.t(locale, body, nil)},
			block{kind: "cta", text: r.t(locale, e+"cta", nil), url: base + "/account?tab=history"},
			block{kind: "small", text: r.t(locale, e+"reason", nil)})
	case KindExpiryDigest:
		vars := map[string]string{"count": strconv.Itoa(d.Count), "days": strconv.Itoa(d.Days)}
		subject = r.t(locale, e+"subject", vars)
		blocks = append(blocks, block{kind: "p", text: r.t(locale, e+"intro", vars)})
		for _, n := range d.Names {
			if strings.TrimSpace(n) == "" {
				n = r.t(locale, e+"unnamed", nil)
			}
			blocks = append(blocks, block{kind: "li", text: n})
		}
		if extra := d.Count - len(d.Names); extra > 0 {
			blocks = append(blocks, block{kind: "p", text: r.t(locale, e+"more", map[string]string{"count": strconv.Itoa(extra)})})
		}
		blocks = append(blocks,
			block{kind: "p", text: r.t(locale, e+"outro", nil)},
			block{kind: "cta", text: r.t(locale, e+"cta", nil), url: base + "/account?tab=history"},
			block{kind: "small", text: r.t(locale, e+"reason", nil)})
	case KindPaymentFailed:
		subject = r.t(locale, e+"subject", nil)
		blocks = append(blocks,
			block{kind: "p", text: r.t(locale, e+"body", nil)},
			block{kind: "cta", text: r.t(locale, e+"cta", nil), url: base + "/account?tab=billing"})
	case KindAccountDeleted:
		prefs = false // contains no links to the account
		subject = r.t(locale, e+"subject", nil)
		blocks = append(blocks,
			block{kind: "p", text: r.t(locale, e+"body", nil)},
			block{kind: "p", text: r.t(locale, e+"contact", nil)})
	default:
		return Rendered{}, fmt.Errorf("email: unknown kind %q", kind)
	}

	out := Rendered{Subject: subject}
	if prefs {
		out.PreferencesURL = r.prefsURL(locale, d)
		blocks = append(blocks,
			block{kind: "small", text: r.t(locale, "email.common.preferencesNote", nil)},
			block{kind: "link", text: r.t(locale, "email.common.preferencesLink", nil), url: out.PreferencesURL})
	}
	signoff := r.t(locale, "email.common.signoff", nil)
	out.Text = renderText(blocks, signoff)
	out.HTML = renderHTML(blocks, signoff, subject, locale)
	return out, nil
}

func kindKey(k Kind) string {
	switch k {
	case KindAIJobFinished:
		return "aiJobFinished"
	case KindExpiryDigest:
		return "expiryDigest"
	case KindPaymentFailed:
		return "paymentFailed"
	case KindAccountDeleted:
		return "accountDeleted"
	}
	return string(k)
}

func renderText(blocks []block, signoff string) string {
	var b strings.Builder
	prevLi := false
	for _, bl := range blocks {
		if prevLi && bl.kind != "li" {
			b.WriteString("\n")
		}
		prevLi = bl.kind == "li"
		switch bl.kind {
		case "li":
			b.WriteString("- " + oneLine(bl.text) + "\n")
		case "cta", "link":
			b.WriteString(bl.text + ": " + bl.url + "\n\n")
		default:
			b.WriteString(oneLine(bl.text) + "\n\n")
		}
	}
	b.WriteString(signoff + "\n")
	return b.String()
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

// renderHTML emits minimal inline-CSS HTML. Dark-safe: no page background and no
// hard-coded body text colour, so clients that invert for dark mode stay legible;
// the one coloured element (the button) carries its own contrasting background.
func renderHTML(blocks []block, signoff, title, locale string) string {
	dir := "ltr"
	if isRTL(locale) {
		dir = "rtl"
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="` + esc(locale) + `" dir="` + dir + `"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<meta name="color-scheme" content="light dark"><meta name="supported-color-schemes" content="light dark">`)
	b.WriteString(`<title>` + esc(title) + `</title></head><body style="margin:0;padding:24px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:16px;line-height:1.5;">`)
	b.WriteString(`<div style="max-width:560px;margin:0 auto;">`)
	inList := false
	closeList := func() {
		if inList {
			b.WriteString(`</ul>`)
			inList = false
		}
	}
	for _, bl := range blocks {
		if bl.kind != "li" {
			closeList()
		}
		switch bl.kind {
		case "li":
			if !inList {
				b.WriteString(`<ul style="padding-inline-start:20px;margin:0 0 16px;">`)
				inList = true
			}
			b.WriteString(`<li style="margin:0 0 6px;">` + esc(bl.text) + `</li>`)
		case "cta":
			b.WriteString(`<p style="margin:24px 0;"><a href="` + esc(bl.url) + `" style="display:inline-block;padding:12px 20px;background:#2563eb;color:#ffffff;text-decoration:none;border-radius:8px;font-weight:600;">` + esc(bl.text) + `</a></p>`)
		case "link":
			b.WriteString(`<p style="margin:0 0 16px;font-size:13px;"><a href="` + esc(bl.url) + `" style="color:#2563eb;">` + esc(bl.text) + `</a></p>`)
		case "small":
			b.WriteString(`<p style="margin:16px 0 4px;font-size:13px;opacity:.75;">` + esc(bl.text) + `</p>`)
		default:
			b.WriteString(`<p style="margin:0 0 16px;">` + esc(bl.text) + `</p>`)
		}
	}
	closeList()
	b.WriteString(`<p style="margin:24px 0 0;">` + esc(signoff) + `</p></div></body></html>`)
	return b.String()
}
