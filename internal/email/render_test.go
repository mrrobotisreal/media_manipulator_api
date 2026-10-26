package email

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func sampleData(kind Kind) Data {
	d := Data{Name: "Alina", BaseURL: "https://media-manipulator.com"}
	if kind == KindExpiryDigest {
		d.Count, d.Days, d.Names = 7, 3, []string{"a.mp4", "", "c.png", "d.wav", "e.mov"}
	}
	return d
}

var allKinds = []Kind{KindWelcome, KindAIJobFinished, KindExpiryDigest, KindPaymentFailed, KindAccountDeleted}

func TestAllLocalesRenderAllTemplates(t *testing.T) {
	r := MustRenderer()
	for _, loc := range Locales {
		for _, k := range allKinds {
			out, err := r.Render(k, loc, sampleData(k))
			if err != nil {
				t.Fatalf("%s/%s: %v", loc, k, err)
			}
			for name, v := range map[string]string{"subject": out.Subject, "text": out.Text, "html": out.HTML} {
				if strings.TrimSpace(v) == "" {
					t.Errorf("%s/%s: empty %s", loc, k, name)
				}
				if strings.Contains(v, "{{") || strings.Contains(v, "email.") {
					t.Errorf("%s/%s: %s has an unresolved key or placeholder: %q", loc, k, name, v)
				}
			}
			if strings.ContainsAny(out.Subject, "\r\n") {
				t.Errorf("%s/%s: subject has a newline", loc, k)
			}
		}
	}
}

func TestPreferencesLinkAndAccountDeletedHasNoLinks(t *testing.T) {
	r := MustRenderer()
	for loc, want := range map[string]string{
		"en-us": "https://media-manipulator.com/account?tab=profile",
		"ru-ru": "https://media-manipulator.com/ru/account?tab=profile",
		"he-il": "https://media-manipulator.com/he/account?tab=profile",
	} {
		for _, k := range []Kind{KindWelcome, KindAIJobFinished, KindExpiryDigest, KindPaymentFailed} {
			out, _ := r.Render(k, loc, sampleData(k))
			if out.PreferencesURL != want || !strings.Contains(out.Text, want) || !strings.Contains(out.HTML, want) {
				t.Errorf("%s/%s: preferences link %q missing", loc, k, want)
			}
		}
	}
	out, _ := r.Render(KindAccountDeleted, "en-us", sampleData(KindAccountDeleted))
	if out.PreferencesURL != "" || strings.Contains(out.Text, "http") || strings.Contains(out.HTML, "http") || strings.Contains(out.HTML, "<a ") {
		t.Errorf("account_deleted must contain no links:\n%s", out.Text)
	}
}

func TestPaymentFailedLinksToBilling(t *testing.T) {
	out, _ := MustRenderer().Render(KindPaymentFailed, "de-de", sampleData(KindPaymentFailed))
	if !strings.Contains(out.Text, "https://media-manipulator.com/de/account?tab=billing") {
		t.Errorf("billing link missing:\n%s", out.Text)
	}
}

func TestDigestListsNamesAndMore(t *testing.T) {
	out, _ := MustRenderer().Render(KindExpiryDigest, "en-us", sampleData(KindExpiryDigest))
	for _, want := range []string{"7 saved items expire soon", "- a.mp4", "- Untitled item", "and 2 more", "within 3 days"} {
		if !strings.Contains(out.Subject+out.Text, want) {
			t.Errorf("digest missing %q:\n%s\n%s", want, out.Subject, out.Text)
		}
	}
	one := sampleData(KindExpiryDigest)
	one.Count, one.Names = 1, []string{"x.mp4"}
	o, _ := MustRenderer().Render(KindExpiryDigest, "en-us", one)
	if o.Subject != "1 saved item expires soon" {
		t.Errorf("singular subject = %q", o.Subject)
	}
}

func TestHTMLEscapesUserText(t *testing.T) {
	d := sampleData(KindWelcome)
	d.Name = `<script>alert(1)</script>`
	out, _ := MustRenderer().Render(KindWelcome, "en-us", d)
	if strings.Contains(out.HTML, "<script>") {
		t.Error("name not escaped in HTML")
	}
}

func TestFallbackToEnglishForMissingKeys(t *testing.T) {
	r := &Renderer{tables: map[string]map[string]string{
		DefaultLocale: {"email.common.greetingNoName": "Hi,", "email.welcome.subject": "EN subject"},
		"de-de":       {"email.welcome.subject": "DE Betreff"},
	}}
	if got := r.t("de-de", "email.welcome.subject", nil); got != "DE Betreff" {
		t.Errorf("de key = %q", got)
	}
	if got := r.t("de-de", "email.common.greetingNoName", nil); got != "Hi," {
		t.Errorf("fallback = %q", got)
	}
	if got := r.t("fr-fr", "email.welcome.subject", nil); got != "EN subject" {
		t.Errorf("unknown locale = %q", got)
	}
}

func TestRussianPluralCategories(t *testing.T) {
	for n, want := range map[int]string{1: "one", 2: "few", 5: "many", 11: "many", 21: "one", 22: "few", 25: "many"} {
		if got := pluralCategory("ru-ru", n); got != want {
			t.Errorf("ru %d = %s, want %s", n, got, want)
		}
	}
}

func TestNormalizeLocale(t *testing.T) {
	for in, want := range map[string]string{"en": "en-us", "en-US": "en-us", "ru": "ru-ru", "de_DE": "de-de", "uk-UA": "uk-ua", "he": "he-il", "fr": "", "": ""} {
		if got := NormalizeLocale(in); got != want {
			t.Errorf("NormalizeLocale(%q) = %q, want %q", in, got, want)
		}
	}
	if LocaleOrDefault("zz") != "en-us" {
		t.Error("default locale")
	}
}

// TestGoldenWelcomePerLocale pins the welcome email for every locale. Run
// `go test ./internal/email -update` after changing a template or a locale file.
func TestGoldenWelcomePerLocale(t *testing.T) {
	r := MustRenderer()
	for _, loc := range Locales {
		out, err := r.Render(KindWelcome, loc, sampleData(KindWelcome))
		if err != nil {
			t.Fatal(err)
		}
		got := "Subject: " + out.Subject + "\n\n" + out.Text + "\n--- html ---\n" + out.HTML + "\n"
		path := filepath.Join("testdata", "welcome."+loc+".golden")
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run with -update)", loc, err)
		}
		if string(want) != got {
			t.Errorf("%s: output differs from %s (run with -update if intended)", loc, path)
		}
	}
}

func TestEmbeddedLocaleFilesAreValid(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Has(DefaultLocale) {
		t.Fatal("en-us table missing")
	}
	// Every non-default table may only use keys that exist in en-us.
	for loc, tbl := range r.tables {
		for k := range tbl {
			if _, ok := r.tables[DefaultLocale][k]; !ok && !strings.Contains(k, "_") {
				t.Errorf("%s has key %q that en-us lacks", loc, k)
			}
		}
	}
}

// MM69: no transactional email names a price or a currency today (payment
// failed points to the portal; Stripe sends receipts). If one ever does, it
// must take a {{price}} the API formatted in the subscription's currency,
// never a hard-coded "$" or "€" amount.
func TestNoHardCodedPriceInTemplates(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("locales", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no locale files: %v", err)
	}
	money := regexp.MustCompile(`[$€£]\s?\d|\d\s?[$€£]|\b(USD|EUR)\b`)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := money.Find(raw); m != nil {
			t.Errorf("%s: hard-coded amount %q; use a preformatted {{price}} placeholder", f, m)
		}
	}
}
