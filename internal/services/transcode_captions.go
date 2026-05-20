package services

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SupportedCaptionLanguage is one row in the user-visible language picker.
// LocalDisplayName is what we put in HLS NAME="..." / DASH lang="..." attrs.
type SupportedCaptionLanguage struct {
	Code             string // BCP-47, used as filesystem dir + lang attribute
	DisplayName      string // English-language display name
	LocalDisplayName string // Native-script display name
}

// SupportedCaptionLanguages returns the set of target languages the UI is
// allowed to request. Whisper itself transcribes only in the source language;
// the additional tracks are produced by the Ollama translator. We keep this
// list close to "languages whisper detects + an LLM can translate to natively"
// — i.e. the most commonly-spoken languages worldwide.
//
// Codes are BCP-47. Some entries use script subtags (zh-Hans vs zh-Hant) where
// it materially changes the translation; everything else uses the bare ISO 639-1.
func SupportedCaptionLanguages() []SupportedCaptionLanguage {
	return []SupportedCaptionLanguage{
		{Code: "en", DisplayName: "English", LocalDisplayName: "English"},
		{Code: "es", DisplayName: "Spanish", LocalDisplayName: "Español"},
		{Code: "pt-BR", DisplayName: "Portuguese (Brazil)", LocalDisplayName: "Português (Brasil)"},
		{Code: "pt-PT", DisplayName: "Portuguese (Portugal)", LocalDisplayName: "Português (Portugal)"},
		{Code: "fr", DisplayName: "French", LocalDisplayName: "Français"},
		{Code: "de", DisplayName: "German", LocalDisplayName: "Deutsch"},
		{Code: "it", DisplayName: "Italian", LocalDisplayName: "Italiano"},
		{Code: "nl", DisplayName: "Dutch", LocalDisplayName: "Nederlands"},
		{Code: "sv", DisplayName: "Swedish", LocalDisplayName: "Svenska"},
		{Code: "fi", DisplayName: "Finnish", LocalDisplayName: "Suomi"},
		{Code: "no", DisplayName: "Norwegian", LocalDisplayName: "Norsk"},
		{Code: "da", DisplayName: "Danish", LocalDisplayName: "Dansk"},
		{Code: "pl", DisplayName: "Polish", LocalDisplayName: "Polski"},
		{Code: "cs", DisplayName: "Czech", LocalDisplayName: "Čeština"},
		{Code: "ru", DisplayName: "Russian", LocalDisplayName: "Русский"},
		{Code: "uk", DisplayName: "Ukrainian", LocalDisplayName: "Українська"},
		{Code: "tr", DisplayName: "Turkish", LocalDisplayName: "Türkçe"},
		{Code: "el", DisplayName: "Greek", LocalDisplayName: "Ελληνικά"},
		{Code: "ar", DisplayName: "Arabic", LocalDisplayName: "العربية"},
		{Code: "he", DisplayName: "Hebrew", LocalDisplayName: "עברית"},
		{Code: "fa", DisplayName: "Persian (Farsi)", LocalDisplayName: "فارسی"},
		{Code: "hi", DisplayName: "Hindi", LocalDisplayName: "हिन्दी"},
		{Code: "bn", DisplayName: "Bengali", LocalDisplayName: "বাংলা"},
		{Code: "ur", DisplayName: "Urdu", LocalDisplayName: "اردو"},
		{Code: "ta", DisplayName: "Tamil", LocalDisplayName: "தமிழ்"},
		{Code: "th", DisplayName: "Thai", LocalDisplayName: "ไทย"},
		{Code: "vi", DisplayName: "Vietnamese", LocalDisplayName: "Tiếng Việt"},
		{Code: "id", DisplayName: "Indonesian", LocalDisplayName: "Bahasa Indonesia"},
		{Code: "ms", DisplayName: "Malay", LocalDisplayName: "Bahasa Melayu"},
		{Code: "tl", DisplayName: "Filipino (Tagalog)", LocalDisplayName: "Filipino"},
		{Code: "ja", DisplayName: "Japanese", LocalDisplayName: "日本語"},
		{Code: "ko", DisplayName: "Korean", LocalDisplayName: "한국어"},
		{Code: "zh-Hans", DisplayName: "Mandarin Chinese (Simplified)", LocalDisplayName: "简体中文"},
		{Code: "zh-Hant", DisplayName: "Mandarin Chinese (Traditional)", LocalDisplayName: "繁體中文"},
		{Code: "yue", DisplayName: "Cantonese", LocalDisplayName: "粵語"},
		{Code: "ro", DisplayName: "Romanian", LocalDisplayName: "Română"},
		{Code: "hu", DisplayName: "Hungarian", LocalDisplayName: "Magyar"},
	}
}

// supportedCaptionLanguageCodes is the lookup the validator uses.
func supportedCaptionLanguageCodes() map[string]SupportedCaptionLanguage {
	out := map[string]SupportedCaptionLanguage{}
	for _, l := range SupportedCaptionLanguages() {
		out[strings.ToLower(l.Code)] = l
	}
	return out
}

// ValidateCaptionLanguages normalizes the user's BCP-47 list against the
// allow-list. Duplicates and unknown codes are stripped. Returns up to 3
// canonical entries.
func ValidateCaptionLanguages(input []string) ([]SupportedCaptionLanguage, error) {
	catalog := supportedCaptionLanguageCodes()
	seen := map[string]bool{}
	out := make([]SupportedCaptionLanguage, 0, 3)
	for _, raw := range input {
		code := strings.ToLower(strings.TrimSpace(raw))
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		lang, ok := catalog[code]
		if !ok {
			return nil, fmt.Errorf("unsupported caption language %q", raw)
		}
		out = append(out, lang)
		if len(out) >= 3 {
			break
		}
	}
	return out, nil
}

// writeVTTFromSegments emits a WebVTT file at dest from a generic segment
// slice. Used for both the primary whisper-VTT and the translated copies.
func writeVTTFromSegments(dest string, segments []TranslateCaptionsSegment) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	sorted := append([]TranslateCaptionsSegment{}, segments...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	cue := 1
	for _, seg := range sorted {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		end := seg.End
		if end <= seg.Start {
			end = seg.Start + 0.5
		}
		b.WriteString(strconv.Itoa(cue))
		b.WriteString("\n")
		b.WriteString(formatVTTStamp(seg.Start))
		b.WriteString(" --> ")
		b.WriteString(formatVTTStamp(end))
		b.WriteString("\n")
		b.WriteString(text)
		b.WriteString("\n\n")
		cue++
	}
	return os.WriteFile(dest, []byte(b.String()), 0o644)
}

// writeHLSSubtitleWrapper emits the tiny variant playlist that HLS requires
// around a single VTT file. It's just a one-segment VOD playlist where the
// segment is the whole VTT file.
//
// dest is the absolute path of the wrapper .m3u8; vttRelativeName is the
// filename inside the same directory (typically "auto.vtt"); durationSeconds
// is the source duration ceil()'d to an int.
func writeHLSSubtitleWrapper(dest, vttRelativeName string, durationSeconds int) error {
	if durationSeconds < 1 {
		durationSeconds = 1
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", durationSeconds))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString(fmt.Sprintf("#EXTINF:%d.000,\n", durationSeconds))
	b.WriteString(vttRelativeName)
	b.WriteString("\n#EXT-X-ENDLIST\n")
	return os.WriteFile(dest, []byte(b.String()), 0o644)
}

// segmentsFromTranscribeResult converts whisper-ct2 segments to the
// generic shape that our translator + VTT writer use.
func segmentsFromTranscribeResult(segs []TranscribeSegmentDTO) []TranslateCaptionsSegment {
	out := make([]TranslateCaptionsSegment, 0, len(segs))
	for _, s := range segs {
		out = append(out, TranslateCaptionsSegment{ID: s.ID, Start: s.Start, End: s.End, Text: s.Text})
	}
	return out
}

// captionsRelativePaths is the canonical layout inside the package directory:
//
//	package/captions/<lang>/auto.vtt   ← the actual cues
//	package/captions/<lang>/subs.m3u8  ← HLS wrapper (only present for HLS)
//
// vttRel and wrapperRel are the relative paths from the package root.
func captionsRelativePaths(langCode string) (vttRel, wrapperRel, dir string) {
	dir = filepath.Join("captions", langCode)
	vttRel = filepath.ToSlash(filepath.Join(dir, "auto.vtt"))
	wrapperRel = filepath.ToSlash(filepath.Join(dir, "subs.m3u8"))
	return
}

// _ keeps formatVTTStamp visible from this file even though transcribe.go has
// the same helper name in the same package. (Go allows it because the symbols
// share package scope; the build chooses one.) Keeping this stub forces the
// developer to consciously dedupe later. -- removed: same package, can't dupe.
