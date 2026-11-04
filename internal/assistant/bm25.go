package assistant

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// BM25 over the generated corpus (MM105 decision 5, design §7.3): no
// embeddings, no GPU. Fields are weighted by repeating them: title ×3,
// keywords and localized names ×2, body ×1. Queries are expanded with a
// Winny-style alias table before tokenizing.

const (
	bm25K1 = 1.2
	bm25B  = 0.75

	weightTitle    = 3
	weightKeywords = 2
	weightBody     = 1

	// snippetChars is the best-window length returned with a hit.
	snippetChars = 600
)

// englishStopwords are dropped from queries and docs.
var englishStopwords = setOf(`a about above after again all am an and any are as at be because been before being
below between both but by can could did do does doing down during each few for from further had has have having he her
here hers him his how i if in into is it its itself just me more most my myself no nor not now of off on once only or
other our ours out over own same she should so some such than that the their theirs them then there these they this
those through to too under until up very was we were what when where which while who whom why will with would you your
yours yourself please want need get got let lets im ive id dont cant wont`)

// spellingVariants fold British/American spellings and common synonyms onto one token.
var spellingVariants = map[string]string{
	"colour": "color", "colours": "color", "coloured": "color", "grey": "gray", "greys": "gray", "greyscale": "grayscale",
	"centre": "center", "licence": "license", "licences": "license", "optimise": "optimize", "optimised": "optimize",
	"stabilise": "stabilize", "normalise": "normalize", "recognise": "recognize", "favourite": "favorite",
	"organise": "organize", "summarise": "summarize", "summarised": "summarize", "visualise": "visualize",
}

// tokenize lowercases, splits on anything that is not a letter or digit, drops
// English stopwords, folds spelling variants and applies a light stemmer.
// CJK runs become overlapping character bigrams (no spaces to split on).
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var out []string
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) == 0 {
			return
		}
		w := string(word)
		word = word[:0]
		if englishStopwords[w] {
			return
		}
		if v, ok := spellingVariants[w]; ok {
			w = v
		}
		w = stem(w)
		if w != "" {
			out = append(out, w)
		}
	}
	flushCJK := func() {
		switch {
		case len(cjk) == 1:
			out = append(out, string(cjk))
		case len(cjk) > 1:
			for i := 0; i+1 < len(cjk); i++ {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range s {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// stem strips -s/-es/-ing/-ed from ASCII words (ticket decision 5) and, for
// other alphabets, keeps a 4-letter prefix (a crude light stemmer that lets
// "сжать" meet "сжатие"; the names map supplies the vocabulary).
func stem(w string) string {
	if w == "" {
		return w
	}
	ascii := true
	for i := 0; i < len(w); i++ {
		if w[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if !ascii {
		if r := []rune(w); len(r) > 4 && !unicode.IsDigit(r[0]) {
			return string(r[:4])
		}
		return w
	}
	if len(w) <= 3 || (w[0] >= '0' && w[0] <= '9') {
		return w
	}
	switch {
	case strings.HasSuffix(w, "ing") && len(w) > 5:
		w = w[:len(w)-3]
		if n := len(w); n > 2 && w[n-1] == w[n-2] && !strings.ContainsRune("lsz", rune(w[n-1])) {
			w = w[:n-1] // trimming → trim
		}
	case strings.HasSuffix(w, "ied") && len(w) > 4:
		w = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "ed") && len(w) > 4:
		w = w[:len(w)-2]
		if n := len(w); n > 2 && w[n-1] == w[n-2] && !strings.ContainsRune("lsz", rune(w[n-1])) {
			w = w[:n-1]
		}
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		w = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"):
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "xes") || strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes"):
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		w = w[:len(w)-1]
	}
	// compress/compres, resize/resiz: fold a trailing silent e so "resized" (→ resiz) meets "resize".
	if len(w) > 4 && strings.HasSuffix(w, "e") && !strings.HasSuffix(w, "ee") {
		w = w[:len(w)-1]
	}
	return w
}

// aliases expand a query (Winny pattern): when the left phrase appears as
// whole words, the right side's terms are added. Keys are lowercase.
var aliases = map[string]string{
	"bg":              "background",
	"lut":             "lut look color grade cube",
	"luts":            "lut look color grade cube",
	"lower third":     "lower-third title text name tag",
	"lower thirds":    "lower-third title text name tag",
	"lower-third":     "lower-third title text name tag",
	"letterbox":       "letterbox crop aspect ratio cinematic bars",
	"black bars":      "letterbox crop aspect ratio",
	"ken burns":       "ken burns keyframe scale zoom pan still",
	"gray":            "gray grey black white",
	"grey":            "gray grey black white",
	"b&w":             "black white monochrome",
	"bw":              "black white monochrome",
	"black and white": "black white monochrome grayscale",
	"gif":             "gif animated",
	"gifs":            "gif animated",
	"mp3":             "mp3 audio",
	"subtitles":       "captions subtitles srt vtt",
	"subtitle":        "captions subtitles srt vtt",
	"captions":        "captions subtitles transcribe",
	"transcript":      "transcribe transcription speech text",
	"speech to text":  "transcribe transcription",
	"green screen":    "chroma key green screen",
	"chromakey":       "chroma key",
	"cinematic":       "cinematic color grade look contrast",
	"color grade":     "color grade lumetri look",
	"grade":           "color grade lumetri",
	"teal and orange": "teal orange split toning color grade",
	"teal orange":     "teal orange split toning color grade",
	"crossfade":       "cross dissolve transition",
	"fade":            "fade opacity dissolve transition",
	"slow mo":         "slow motion speed",
	"slowmo":          "slow motion speed",
	"timelapse":       "speed fast",
	"time lapse":      "speed fast",
	"draw on":         "draw on stroke animator",
	"handwriting":     "draw on stroke animator",
	"logo reveal":     "logo reveal draw on mask animator",
	"vocals":          "vocals isolate remove singer",
	"karaoke":         "remove vocals instrumental",
	"instrumental":    "remove vocals instrumental",
	"noise":           "noise reduction denoise background noise",
	"denoise":         "noise reduction denoise",
	"upscale":         "upscale super resolution enlarge",
	"enlarge":         "upscale resize",
	"exif":            "exif metadata gps",
	"gps":             "exif metadata gps location",
	"watermark":       "watermark text overlay logo",
	"shrink":          "compress reduce size",
	"smaller":         "compress reduce size",
	"reduce size":     "compress reduce size",
	"make it smaller": "compress reduce size",
	"remove bg":       "remove background",
	"transparent":     "remove background transparent png",
	"cutout":          "remove background subject",
	"pop":             "color pop gray except",
	"color splash":    "color pop gray except",
	"colour splash":   "color pop gray except",
	"keep only":       "color pop gray except",
	"except":          "gray except color pop",
	"backlit":         "backlit shadows exposure lighting",
	"too dark":        "brighten exposure shadows",
	"too bright":      "exposure highlights darken",
	"crooked":         "straighten rotate horizon",
	"horizon":         "straighten horizon",
	"vintage":         "vintage film look grain",
	"film look":       "film grain look vintage",
	"price":           "pricing plans",
	"cost":            "pricing plans",
	"how much":        "pricing plans",
	"subscription":    "pricing plans billing premium",
	"cancel":          "billing cancel subscription account",
	"refund":          "refund billing terms",
	"limits":          "plans limits daily allowance pricing",
	"quota":           "plans limits daily allowance pricing",
	"svg":             "svg motion vector",
	"animation":       "animate motion keyframe",
	"animate":         "animate motion keyframe",
	"photo editor":    "aperture photo editor",
	"video editor":    "darkroom video editor timeline",
	"premiere":        "darkroom video editor timeline",
	"photoshop":       "aperture photo editor layers",
	"lightroom":       "aperture photo editor presets",
	"after effects":   "motion animation keyframe",
	"podcast":         "podcast voice audio compressor",
	"voice over":      "voiceover voice narration",
	"voiceover":       "voiceover voice narration",
	"shorts":          "vertical 9:16 aspect shorts reels",
	"reels":           "vertical 9:16 aspect shorts reels",
	"tiktok":          "vertical 9:16 aspect shorts reels",
	"vertical":        "vertical 9:16 aspect canvas",
}

// expandQuery appends alias expansions for phrases present as whole words.
func expandQuery(q string) string {
	low := " " + strings.Join(strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '&' || r == '-' || r == ':')
	}), " ") + " "
	keys := make([]string, 0, len(aliases))
	for k := range aliases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var extra []string
	for _, k := range keys {
		if strings.Contains(low, " "+k+" ") {
			extra = append(extra, aliases[k])
		}
	}
	if len(extra) == 0 {
		return q
	}
	return q + " " + strings.Join(extra, " ")
}

// bm25Index is an inverted index over weighted term frequencies.
type bm25Index struct {
	docs   []CorpusDoc
	tf     []map[string]float64 // per doc: term → weighted frequency
	length []float64
	avgLen float64
	df     map[string]int
}

func newBM25(docs []CorpusDoc) *bm25Index {
	ix := &bm25Index{docs: docs, tf: make([]map[string]float64, len(docs)), length: make([]float64, len(docs)), df: map[string]int{}}
	var total float64
	for i, d := range docs {
		tf := map[string]float64{}
		add := func(text string, w float64) {
			for _, t := range tokenize(text) {
				tf[t] += w
				ix.length[i] += w
			}
		}
		add(d.Title, weightTitle)
		add(strings.Join(d.Keywords, " "), weightKeywords)
		add(strings.Join(d.Names, " "), weightKeywords)
		add(d.Summary, weightBody)
		add(d.Opens, weightBody)
		add(d.Details, weightBody)
		ix.tf[i] = tf
		for t := range tf {
			ix.df[t]++
		}
		total += ix.length[i]
	}
	if len(docs) > 0 {
		ix.avgLen = total / float64(len(docs))
	}
	return ix
}

func (ix *bm25Index) idf(t string) float64 {
	n := float64(len(ix.docs))
	df := float64(ix.df[t])
	return math.Log(1 + (n-df+0.5)/(df+0.5))
}

// score is the BM25 score of doc i for the query terms (each distinct term once).
func (ix *bm25Index) score(i int, terms []string) float64 {
	var s float64
	norm := bm25K1 * (1 - bm25B + bm25B*ix.length[i]/ix.avgLen)
	for _, t := range terms {
		f := ix.tf[i][t]
		if f == 0 {
			continue
		}
		s += ix.idf(t) * f * (bm25K1 + 1) / (f + norm)
	}
	return s
}

func uniqueTerms(ts []string) []string {
	seen := map[string]bool{}
	out := ts[:0:0]
	for _, t := range ts {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// snippet returns the ~600-char window of the doc body that holds the most
// query terms (starting on a line boundary), or the summary.
func snippet(d CorpusDoc, terms []string) string {
	want := map[string]bool{}
	for _, t := range terms {
		want[t] = true
	}
	lines := strings.Split(d.Body, "\n")
	best, bestHits := 0, -1
	for i := range lines {
		hits, n := 0, 0
		for j := i; j < len(lines) && n < snippetChars; j++ {
			for _, t := range tokenize(lines[j]) {
				if want[t] {
					hits++
				}
			}
			n += len(lines[j]) + 1
		}
		if hits > bestHits {
			best, bestHits = i, hits
		}
	}
	if bestHits <= 0 {
		return clip(d.Summary, snippetChars)
	}
	var b strings.Builder
	for j := best; j < len(lines) && b.Len() < snippetChars; j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(lines[j])
	}
	return clip(b.String(), snippetChars)
}
