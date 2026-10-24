package email

import "strings"

// DefaultLocale is the fallback for every missing user locale or string.
const DefaultLocale = "en-us"

// Locales are the six site locales, as the UI's i18n/locales directory names them.
var Locales = []string{"en-us", "de-de", "es-es", "he-il", "ru-ru", "uk-ua"}

// NormalizeLocale maps any BCP-47-ish input ("ru", "ru-RU", "de_DE", "en") to one
// of Locales, or "" when it is not a site locale.
func NormalizeLocale(in string) string {
	in = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(in, "_", "-")))
	if in == "" {
		return ""
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
	return "/" + locale[:2]
}

// isRTL reports right-to-left locales.
func isRTL(locale string) bool { return LocaleOrDefault(locale) == "he-il" }

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
	default:
		if n == 1 {
			return "one"
		}
		return "other"
	}
}
