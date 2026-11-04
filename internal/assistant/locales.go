package assistant

import "strings"

// localeNames names the 18 site locales in English for the live block
// ("Answer in Russian (ru-RU)"). MM105 exports the per-locale tool/page names.
var localeNames = map[string]string{
	"en": "English", "uk": "Ukrainian", "ru": "Russian", "he": "Hebrew", "de": "German",
	"es-es": "Spanish (Spain)", "es-mx": "Spanish (Latin America)", "es": "Spanish",
	"fr": "French", "it": "Italian", "ja": "Japanese",
	"zh-cn": "Chinese (Simplified)", "zh-tw": "Chinese (Traditional)", "zh": "Chinese",
	"ko": "Korean", "ar": "Arabic", "hi": "Hindi",
	"pt-pt": "Portuguese (Portugal)", "pt-br": "Portuguese (Brazil)", "pt": "Portuguese", "vi": "Vietnamese",
}

// LocaleName returns the English language name for a tag ("" when unknown).
func LocaleName(tag string) string {
	l := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(tag), "_", "-"))
	if n, ok := localeNames[l]; ok {
		return n
	}
	if i := strings.Index(l, "-"); i > 0 {
		return localeNames[l[:i]]
	}
	return ""
}
