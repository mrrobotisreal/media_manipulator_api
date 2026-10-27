package email

import "strings"

// DefaultLocale is the fallback for every missing user locale or string.
const DefaultLocale = "en-us"

// Locales are the site locales, as the UI's i18n/locales directory names them,
// in the order of the UI's i18n/locales.ts (a bare "es"/"pt"/"zh" resolves to the
// first match: es-es, zh-cn, pt-pt).
var Locales = []string{
	"en-us", "uk-ua", "ru-ru", "he-il", "de-de", "es-es", "es-mx", "fr-fr", "it-it",
	"ja-jp", "zh-cn", "zh-tw", "ko-kr", "ar-sa", "hi-in", "pt-pt", "pt-br", "vi-vn",
}

// urlPrefixes are the UI's URL prefixes where they are not the two-letter
// language (MM51 locales.ts `prefix`).
var urlPrefixes = map[string]string{
	"es-mx": "es-mx", "zh-cn": "zh-cn", "zh-tw": "zh-tw", "pt-pt": "pt-pt", "pt-br": "pt-br",
}

// NormalizeLocale maps any BCP-47-ish input ("ru", "ru-RU", "de_DE", "en") to one
// of Locales, or "" when it is not a site locale.
func NormalizeLocale(in string) string {
	in = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(in, "_", "-")))
	if in == "" {
		return ""
	}
	// Traditional-script Chinese reads zh-tw, not the first "zh-" match.
	if in == "zh-hk" || in == "zh-mo" || strings.HasPrefix(in, "zh-hant") {
		return "zh-tw"
	}
	for _, l := range Locales {
		if in == l || strings.HasPrefix(l, in+"-") {
			return l
		}
	}
	if i := strings.Index(in, "-"); i > 0 {
		return NormalizeLocale(in[:i])
	}
	return ""
}

// LocaleOrDefault is NormalizeLocale with the en-us fallback.
func LocaleOrDefault(in string) string {
	if l := NormalizeLocale(in); l != "" {
		return l
	}
	return DefaultLocale
}

// urlPrefix mirrors the UI: English is unprefixed, others use their short code.
func urlPrefix(locale string) string {
	locale = LocaleOrDefault(locale)
	if locale == DefaultLocale {
		return ""
	}
	if p, ok := urlPrefixes[locale]; ok {
		return "/" + p
	}
	return "/" + locale[:2]
}

// isRTL reports right-to-left locales.
func isRTL(locale string) bool {
	l := LocaleOrDefault(locale)
	return l == "he-il" || l == "ar-sa"
}

// pluralCategory is the CLDR category i18next would pick for a cardinal n.
func pluralCategory(locale string, n int) string {
	switch LocaleOrDefault(locale) {
	case "ru-ru", "uk-ua":
		m10, m100 := n%10, n%100
		switch {
		case m10 == 1 && m100 != 11:
			return "one"
		case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
			return "few"
		default:
			return "many"
		}
	case "he-il":
		switch n {
		case 1:
			return "one"
		case 2:
			return "two"
		}
		return "other"
	case "ar-sa":
		m100 := n % 100
		switch {
		case n == 0:
			return "zero"
		case n == 1:
			return "one"
		case n == 2:
			return "two"
		case m100 >= 3 && m100 <= 10:
			return "few"
		case m100 >= 11:
			return "many"
		}
		return "other"
	case "ja-jp", "zh-cn", "zh-tw", "ko-kr", "vi-vn":
		return "other"
	case "fr-fr", "pt-br", "hi-in":
		// CLDR: 0 and 1 are "one" in French, Brazilian Portuguese and Hindi.
		if n == 0 || n == 1 {
			return "one"
		}
		return "other"
	default:
		if n == 1 {
			return "one"
		}
		return "other"
	}
}
