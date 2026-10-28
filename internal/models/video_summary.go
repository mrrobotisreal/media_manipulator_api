package models

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Video summary (MM74, design docs/design/VIDEO_SUMMARY.md, ADR-0014) —
// request/response contracts for
//
//	GET  /api/summarize/capabilities
//	POST /api/summarize/start            (multipart `file` + `options`, or JSON {s3Key,...})
//	GET  /api/summarize/:jobId/result?format=md|json
//
// Jobs reuse ConversionJob (mode "summarize", tool "video_summary") and the
// shared /api/job/:jobId (+ SSE) machinery for status and stages[]; there is no
// new table.

// Video summary job identity.
const (
	VideoSummaryMode = "summarize"
	VideoSummaryTool = "video_summary"
)

// Synthesis depth. Deep uses OLLAMA_SUMMARY_DEEP_MODEL (Pro only, and only
// when the operator configured it); anything else is standard.
const (
	VideoSummaryDepthStandard = "standard"
	VideoSummaryDepthDeep     = "deep"
)

// VideoSummaryLanguageAuto is "write the summary in the language of the speech".
const VideoSummaryLanguageAuto = "auto"

// videoSummaryLanguageNames are the site's registered locales (the UI's
// i18n/locales.ts) with the English name the synthesis prompt uses.
var videoSummaryLanguageNames = map[string]string{
	"en-US": "English",
	"uk-UA": "Ukrainian",
	"ru-RU": "Russian",
	"he-IL": "Hebrew",
	"de-DE": "German",
	"es-ES": "Spanish (Spain)",
	"es-MX": "Spanish (Latin America)",
	"fr-FR": "French",
	"it-IT": "Italian",
	"ja-JP": "Japanese",
	"zh-CN": "Simplified Chinese",
	"zh-TW": "Traditional Chinese",
	"ko-KR": "Korean",
	"ar-SA": "Arabic",
	"hi-IN": "Hindi",
	"pt-PT": "Portuguese (Portugal)",
	"pt-BR": "Portuguese (Brazil)",
	"vi-VN": "Vietnamese",
}

// VideoSummaryOutputLanguages is "auto" followed by the 18 locale codes, in the
// order the capabilities endpoint reports them.
func VideoSummaryOutputLanguages() []string {
	codes := make([]string, 0, len(videoSummaryLanguageNames))
	for code := range videoSummaryLanguageNames {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return append([]string{VideoSummaryLanguageAuto}, codes...)
}

// NormalizeVideoSummaryLanguage resolves a requested output language to "auto"
// or one of the registered locale codes. A bare language ("de", "pt") or a
// different case resolves to the matching locale; unknown values are an error.
func NormalizeVideoSummaryLanguage(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" || strings.EqualFold(v, VideoSummaryLanguageAuto) || strings.EqualFold(v, "same") {
		return VideoSummaryLanguageAuto, nil
	}
	for code := range videoSummaryLanguageNames {
		if strings.EqualFold(code, v) {
			return code, nil
		}
	}
	// Bare language: the first matching locale in sorted order (es -> es-ES,
	// pt -> pt-BR, zh -> zh-CN).
	for _, code := range VideoSummaryOutputLanguages()[1:] {
		if strings.EqualFold(strings.SplitN(code, "-", 2)[0], v) {
			return code, nil
		}
	}
	return "", fmt.Errorf("outputLanguage %q is not supported", raw)
}

// VideoSummaryLanguageName is the English name of a registered locale code,
// or "" for auto/unknown.
func VideoSummaryLanguageName(code string) string { return videoSummaryLanguageNames[code] }

// VideoSummaryOptions is the `options` object of POST /api/summarize/start.
type VideoSummaryOptions struct {
	// OutputLanguage is "auto" (default, the speech's language) or one of
	// VideoSummaryOutputLanguages.
	OutputLanguage string `json:"outputLanguage"`
	// Depth is "standard" (default) or "deep". Deep for a non-Pro tier, or with
	// deep synthesis not configured, silently falls back to standard.
	Depth string `json:"depth"`
}

// VideoSummaryStartJSON is the JSON body of POST /api/summarize/start after the
// existing presign flow (/api/video-upload/presign then a direct S3 PUT).
type VideoSummaryStartJSON struct {
	S3Key         string              `json:"s3Key"`
	FileName      string              `json:"fileName"`
	FileSizeBytes int64               `json:"fileSizeBytes"`
	Options       VideoSummaryOptions `json:"options"`
}

// VideoSummaryStartResponse acknowledges an accepted job (HTTP 202).
type VideoSummaryStartResponse struct {
	JobID string `json:"jobId"`
}

// VideoSummaryCapabilitiesResponse is GET /api/summarize/capabilities.
type VideoSummaryCapabilitiesResponse struct {
	// Enabled is SUMMARY_ENABLED and the Ollama daemon answering.
	Enabled      bool   `json:"enabled"`
	VLMModel     string `json:"vlmModel"`
	SummaryModel string `json:"summaryModel"`
	// DeepModel is set only when deep synthesis is configured.
	DeepModel string `json:"deepModel,omitempty"`
	// Deep is true when deep synthesis is configured AND its daemon answers.
	Deep bool `json:"deep"`
	// DeepTiers lists the public tiers allowed to ask for depth "deep".
	DeepTiers []string `json:"deepTiers"`
	// MaxFramesByTier is each public tier's frame budget; 0 = sign-in required.
	MaxFramesByTier map[string]int `json:"maxFramesByTier"`
	// OutputLanguages is "auto" followed by the registered locale codes.
	OutputLanguages []string `json:"outputLanguages"`
}

// VideoSummaryTranscriptSegment is one Whisper segment.
type VideoSummaryTranscriptSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// VideoSummaryTranscript is transcript.json in the job output dir.
type VideoSummaryTranscript struct {
	Language string                          `json:"language"`
	Text     string                          `json:"text"`
	Segments []VideoSummaryTranscriptSegment `json:"segments"`
}

// HasSpeech reports whether Whisper found anything.
func (t VideoSummaryTranscript) HasSpeech() bool {
	return strings.TrimSpace(t.Text) != "" || len(t.Segments) > 0
}

// VideoSummaryBatchNote is the VLM's description of one batch of (up to) four
// frames, plus the time window it covers. Persisted in vision_notes.json.
type VideoSummaryBatchNote struct {
	Start        float64     `json:"start"`
	End          float64     `json:"end"`
	FrameTimes   []float64   `json:"frameTimes"`
	Scene        FlexString  `json:"scene"`
	OnScreenText FlexStrings `json:"onScreenText"`
	People       FlexStrings `json:"people"`
	Objects      FlexStrings `json:"objects"`
	Actions      FlexStrings `json:"actions"`
	CameraNotes  FlexString  `json:"cameraNotes"`
}

// Empty reports whether the note says nothing at all.
func (n VideoSummaryBatchNote) Empty() bool {
	return strings.TrimSpace(string(n.Scene)) == "" && len(n.OnScreenText) == 0 && len(n.People) == 0 &&
		len(n.Objects) == 0 && len(n.Actions) == 0 && strings.TrimSpace(string(n.CameraNotes)) == ""
}

// VideoSummaryChapter is one chapter; Start/End are seconds into the source.
type VideoSummaryChapter struct {
	Start       Seconds `json:"start"`
	End         Seconds `json:"end"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
}

// VideoSummaryOnScreenText is one piece of on-screen text and the first time
// (seconds) it was seen.
type VideoSummaryOnScreenText struct {
	Text      string  `json:"text"`
	FirstSeen Seconds `json:"firstSeen"`
}

// UnmarshalJSON accepts {"text","firstSeen"} or a bare string.
func (o *VideoSummaryOnScreenText) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*o = VideoSummaryOnScreenText{Text: s}
		return nil
	}
	type plain VideoSummaryOnScreenText
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*o = VideoSummaryOnScreenText(p)
	return nil
}

// VideoSummary is summary.json: the product (design §2).
type VideoSummary struct {
	Title        string                     `json:"title"`
	Summary      string                     `json:"summary"`
	Chapters     []VideoSummaryChapter      `json:"chapters"`
	Topics       FlexStrings                `json:"topics"`
	OnScreenText []VideoSummaryOnScreenText `json:"onScreenText"`
	Visual       string                     `json:"visual"`
	Speech       string                     `json:"speech"`
	Language     string                     `json:"language"`
	Caveats      FlexStrings                `json:"caveats"`
}

// Schema limits (design §2).
const (
	VideoSummaryTitleMaxRunes   = 80
	VideoSummaryMinSentences    = 3
	VideoSummaryMaxSentences    = 6
	VideoSummaryMinChapters     = 3
	VideoSummaryMaxChapters     = 12
	VideoSummaryMaxTopics       = 12
	VideoSummaryMaxOnScreenText = 40
	// VideoSummaryShortClipSeconds: below this a clip may have one or two
	// chapters and a shorter summary (three chapters of a 20 s clip is noise).
	VideoSummaryShortClipSeconds = 60
	// videoSummaryBoundsSlack absorbs rounding at the end of the source.
	videoSummaryBoundsSlack = 2.0
)

// MinChaptersFor is the chapter minimum for a source of this duration.
func MinChaptersFor(durationSec float64) int {
	if durationSec > 0 && durationSec < VideoSummaryShortClipSeconds {
		return 1
	}
	return VideoSummaryMinChapters
}

// ValidateVideoSummary checks a model reply against the schema. A returned
// error is phrased so it can be sent back to the model in the repair prompt.
// durationSec <= 0 means unknown, which skips the upper-bound check.
func ValidateVideoSummary(s *VideoSummary, durationSec float64) error {
	if s == nil {
		return fmt.Errorf("the reply is empty")
	}
	if strings.TrimSpace(s.Title) == "" {
		return fmt.Errorf(`"title" is required`)
	}
	if strings.TrimSpace(s.Summary) == "" {
		return fmt.Errorf(`"summary" is required`)
	}
	if durationSec <= 0 || durationSec >= VideoSummaryShortClipSeconds {
		if n := CountSentences(s.Summary); n < VideoSummaryMinSentences {
			return fmt.Errorf(`"summary" must be %d to %d sentences, got %d`, VideoSummaryMinSentences, VideoSummaryMaxSentences, n)
		}
	}
	minCh := MinChaptersFor(durationSec)
	if len(s.Chapters) < minCh || len(s.Chapters) > VideoSummaryMaxChapters {
		return fmt.Errorf(`"chapters" must have %d to %d entries, got %d`, minCh, VideoSummaryMaxChapters, len(s.Chapters))
	}
	prevStart, prevEnd := -1.0, 0.0
	for i, ch := range s.Chapters {
		start, end := float64(ch.Start), float64(ch.End)
		if strings.TrimSpace(ch.Title) == "" {
			return fmt.Errorf(`chapter %d needs a "title"`, i+1)
		}
		if math.IsNaN(start) || math.IsNaN(end) || start < 0 {
			return fmt.Errorf("chapter %d starts before 0", i+1)
		}
		if end <= start {
			return fmt.Errorf("chapter %d must end after it starts (start %.1f, end %.1f)", i+1, start, end)
		}
		if durationSec > 0 && (start >= durationSec || end > durationSec+videoSummaryBoundsSlack) {
			return fmt.Errorf("chapter %d (%.1f-%.1f s) is outside the source duration of %.1f s", i+1, start, end, durationSec)
		}
		if start <= prevStart {
			return fmt.Errorf("chapter %d must start after chapter %d (starts must increase)", i+1, i)
		}
		if i > 0 && start < prevEnd-videoSummaryBoundsSlack {
			return fmt.Errorf("chapter %d overlaps chapter %d", i+1, i)
		}
		prevStart, prevEnd = start, end
	}
	return nil
}

// NormalizeVideoSummary applies the schema's soft limits after validation:
// trims strings, caps the title at 80 runes and the summary at 6 sentences,
// clamps the last chapter to the duration, deduplicates topics, on-screen text
// and caveats. It never fails.
func NormalizeVideoSummary(s *VideoSummary, durationSec float64) {
	if s == nil {
		return
	}
	s.Title = truncateTitle(collapseSpace(s.Title), VideoSummaryTitleMaxRunes)
	s.Summary = FirstSentences(collapseSpace(s.Summary), VideoSummaryMaxSentences)
	s.Visual = strings.TrimSpace(s.Visual)
	s.Speech = strings.TrimSpace(s.Speech)
	s.Language = strings.TrimSpace(s.Language)
	for i := range s.Chapters {
		ch := &s.Chapters[i]
		ch.Title = collapseSpace(ch.Title)
		ch.Description = collapseSpace(ch.Description)
		ch.Start = Seconds(roundMillis(float64(ch.Start)))
		ch.End = Seconds(roundMillis(float64(ch.End)))
		if durationSec > 0 && float64(ch.End) > durationSec {
			ch.End = Seconds(roundMillis(durationSec))
		}
	}
	s.Topics = FlexStrings(dedupeFold(s.Topics, VideoSummaryMaxTopics))
	s.Caveats = FlexStrings(dedupeFold(s.Caveats, 0))
	s.OnScreenText = DedupeOnScreenText(s.OnScreenText, VideoSummaryMaxOnScreenText)
}

// DedupeOnScreenText merges entries whose text matches case- and
// space-insensitively, keeping the earliest firstSeen, sorted by time.
func DedupeOnScreenText(in []VideoSummaryOnScreenText, max int) []VideoSummaryOnScreenText {
	byKey := map[string]int{}
	out := make([]VideoSummaryOnScreenText, 0, len(in))
	for _, item := range in {
		text := collapseSpace(item.Text)
		if text == "" {
			continue
		}
		key := strings.ToLower(text)
		t := Seconds(roundMillis(math.Max(0, float64(item.FirstSeen))))
		if idx, ok := byKey[key]; ok {
			if t < out[idx].FirstSeen {
				out[idx].FirstSeen = t
			}
			continue
		}
		byKey[key] = len(out)
		out = append(out, VideoSummaryOnScreenText{Text: text, FirstSeen: t})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FirstSeen < out[j].FirstSeen })
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// sentenceEnd reports whether r ends a sentence (Latin, CJK, Arabic, Devanagari).
func sentenceEnd(r rune) bool {
	switch r {
	case '.', '!', '?', '。', '！', '？', '؟', '।':
		return true
	}
	return false
}

// sentenceSpans returns the end offsets (exclusive, in bytes) of each sentence
// in s. A Latin-style terminator counts when followed by whitespace or the end
// of the text (closing quotes/brackets right after it belong to the sentence),
// so "3.5", "e.g." mid-word and URLs do not split. CJK terminators always
// count. Trailing text after the last terminator is one more sentence.
func sentenceSpans(s string) []int {
	var ends []int
	for i, r := range s {
		if !sentenceEnd(r) {
			continue
		}
		end := i + utf8.RuneLen(r)
		if r == '。' || r == '！' || r == '？' {
			ends = append(ends, end)
			continue
		}
		for end < len(s) {
			next, size := utf8.DecodeRuneInString(s[end:])
			if !strings.ContainsRune(`"'”’)»]`, next) {
				break
			}
			end += size
		}
		if end >= len(s) {
			ends = append(ends, end)
			continue
		}
		if next, _ := utf8.DecodeRuneInString(s[end:]); next == ' ' || next == '\n' || next == '\t' {
			ends = append(ends, end)
		}
	}
	if len(ends) == 0 || strings.TrimSpace(s[ends[len(ends)-1]:]) != "" {
		ends = append(ends, len(s))
	}
	return dedupeInts(ends)
}

// CountSentences counts sentences the way the schema means it.
func CountSentences(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return len(sentenceSpans(s))
}

// FirstSentences keeps at most n sentences of s.
func FirstSentences(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" || n <= 0 {
		return s
	}
	spans := sentenceSpans(s)
	if len(spans) <= n {
		return s
	}
	return strings.TrimSpace(s[:spans[n-1]])
}

func dedupeInts(in []int) []int {
	out := in[:0]
	for i, v := range in {
		if i > 0 && v == out[len(out)-1] {
			continue
		}
		out = append(out, v)
	}
	return out
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncateTitle caps a title at max runes, preferring a word boundary in the
// last quarter (reuses the package's truncateRunes for the hard cut).
func truncateTitle(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	cut := truncateRunes(s, max)
	if i := strings.LastIndexByte(cut, ' '); i > len(cut)*3/4 {
		cut = cut[:i]
	}
	return strings.TrimRight(strings.TrimSpace(cut), ",;:-–—")
}

func dedupeFold(in []string, max int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = collapseSpace(v)
		if v == "" || seen[strings.ToLower(v)] {
			continue
		}
		seen[strings.ToLower(v)] = true
		out = append(out, v)
		if max > 0 && len(out) == max {
			break
		}
	}
	return out
}

func roundMillis(v float64) float64 { return math.Round(v*1000) / 1000 }

// Seconds is a timestamp in seconds. It marshals as a JSON number and
// unmarshals from a number, a numeric string, or "m:ss" / "h:mm:ss" (small
// models often write chapter times as clock strings).
type Seconds float64

// UnmarshalJSON implements the lenient decoding described on Seconds.
func (s *Seconds) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err == nil {
		*s = Seconds(f)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		if strings.TrimSpace(string(b)) == "null" {
			*s = 0
			return nil
		}
		return fmt.Errorf("timestamp must be seconds or m:ss")
	}
	v, err := ParseClock(str)
	if err != nil {
		return err
	}
	*s = Seconds(v)
	return nil
}

// ParseClock parses "75", "75.5", "1:15", "01:15.5" or "1:02:03" into seconds.
func ParseClock(raw string) (float64, error) {
	str := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "s"))
	if str == "" {
		return 0, fmt.Errorf("empty timestamp")
	}
	parts := strings.Split(str, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("timestamp %q is not m:ss", raw)
	}
	total := 0.0
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("timestamp %q is not m:ss", raw)
		}
		total = total*60 + v
	}
	return total, nil
}

// FlexString unmarshals from a string, a number, or an array (joined with
// "; "). Marshals as a plain string.
type FlexString string

// UnmarshalJSON implements the lenient decoding described on FlexString.
func (f *FlexString) UnmarshalJSON(b []byte) error {
	var list FlexStrings
	if err := list.UnmarshalJSON(b); err != nil {
		return err
	}
	*f = FlexString(strings.Join(list, "; "))
	return nil
}

// FlexStrings unmarshals from an array of strings/numbers/objects, a single
// string, or null. Marshals as a plain JSON array (never null).
type FlexStrings []string

// UnmarshalJSON implements the lenient decoding described on FlexStrings.
func (f *FlexStrings) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "null" || trimmed == "" {
		*f = nil
		return nil
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	var out []string
	var add func(v any)
	add = func(v any) {
		switch x := v.(type) {
		case nil:
		case string:
			if s := strings.TrimSpace(x); s != "" {
				out = append(out, s)
			}
		case float64:
			out = append(out, strconv.FormatFloat(x, 'f', -1, 64))
		case bool:
			out = append(out, strconv.FormatBool(x))
		case []any:
			for _, item := range x {
				add(item)
			}
		case map[string]any:
			// {"text": "..."} / {"name": "..."} / anything: prefer a text-ish field.
			for _, k := range []string{"text", "name", "label", "description", "value"} {
				if s, ok := x[k].(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
					return
				}
			}
			if enc, err := json.Marshal(x); err == nil {
				out = append(out, string(enc))
			}
		}
	}
	add(raw)
	*f = out
	return nil
}

// MarshalJSON writes an empty array instead of null.
func (f FlexStrings) MarshalJSON() ([]byte, error) {
	if f == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(f))
}
