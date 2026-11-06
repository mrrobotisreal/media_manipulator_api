package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MM121 decision 1: --word_timestamps only when asked (the studio caption
// job); every other caller's whisper command is byte-unchanged.
func TestWhisperWordTimestampsFlag(t *testing.T) {
	idx := 1
	cfg := WhisperCT2Config{Model: "medium", Device: "cuda", DeviceIndex: &idx, ComputeType: "float16", VADFilter: true, Language: "en"}
	want := []string{"/a.wav", "--model", "medium", "--device", "cuda", "--device_index", "1", "--compute_type", "float16", "--vad_filter", "True", "--language", "en", "--output_format", "json", "--output_dir", "/o"}
	if got := whisperCT2Args(cfg, "/a.wav", "/o"); !reflect.DeepEqual(got, want) {
		t.Fatalf("default args changed:\n got %q\nwant %q", got, want)
	}
	cfg.WordTimestamps = true
	got := strings.Join(whisperCT2Args(cfg, "/a.wav", "/o"), " ")
	if !strings.Contains(got, "--language en --word_timestamps True --output_format json") {
		t.Errorf("word timestamps flag missing or misplaced: %s", got)
	}
}

// Only the studio caption handler asks for word timestamps (decision 1).
func TestWordTimestampsOnlyStudioCaller(t *testing.T) {
	root := filepath.Join("..", "..")
	var hits []string
	_ = filepath.Walk(filepath.Join(root, "internal"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "WordTimestamps: true") {
			hits = append(hits, filepath.ToSlash(p))
		}
		return nil
	})
	if len(hits) != 1 || !strings.HasSuffix(hits[0], "internal/handlers/studio.go") {
		t.Errorf("WordTimestamps: true must appear only in the studio caption handler, found %v", hits)
	}
}

func TestParseWhisperWords(t *testing.T) {
	payload := `{"text":"Welcome back.","language":"en","segments":[{"start":0,"end":1.42,"text":" Welcome back.","words":[{"start":0,"end":0.4,"word":" Welcome","probability":0.85},{"start":0.4,"end":"0.68","word":" back.","probability":0.97}]}]}`
	resp, err := parseWhisperCT2Response([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	dtos := buildSegmentDTOs(resp.Segments)
	if len(dtos) != 1 || len(dtos[0].Words) != 2 || dtos[0].Words[1].Word != " back." || dtos[0].Words[1].End != 0.68 || dtos[0].Words[0].Probability != 0.85 {
		t.Fatalf("words not carried: %+v", dtos)
	}
	// Without words the DTO serializes exactly as before (no "words" key).
	b, _ := json.Marshal(TranscribeSegmentDTO{ID: 1, Start: 1, End: 2, Text: "x"})
	if string(b) != `{"id":1,"start":1,"end":2,"text":"x"}` {
		t.Errorf("segment DTO without words changed: %s", b)
	}
}

func w(word string, s, e float64) TranscribeWordDTO {
	return TranscribeWordDTO{Word: word, Start: s, End: e, Probability: 0.9}
}

// Design §6.2 grouping: a sentence end or a gap ≥ 150 ms breaks a cue; words
// keep their times; a trailing silence drops whisper's filler.
func TestSegmentsToCuesWords(t *testing.T) {
	seg := TranscribeSegmentDTO{Start: 0, End: 5, Text: "Welcome back. Today we test captions. Thank you.", Words: []TranscribeWordDTO{
		w(" Welcome", 0, 0.4), w(" back.", 0.4, 0.68),
		w(" Today", 0.8, 1.0), w(" we", 1.0, 1.1), w(" test", 1.4, 1.6), w(" captions.", 1.6, 2.0),
		w(" Thank", 4.2, 4.4), w(" you.", 4.4, 4.6),
	}}
	cues := SegmentsToCuesUntil([]TranscribeSegmentDTO{seg}, seqIDGen(), 3.5)
	var texts []string
	for _, c := range cues {
		texts = append(texts, c.Text)
		if len(c.Words) != len(models.CaptionUnits(c.Text)) {
			t.Errorf("%q: %d words for %d units", c.Text, len(c.Words), len(models.CaptionUnits(c.Text)))
		}
		if c.StartSeconds != c.Words[0].StartSeconds || c.EndSeconds != c.Words[len(c.Words)-1].EndSeconds {
			t.Errorf("%q: cue window %v-%v does not match its words", c.Text, c.StartSeconds, c.EndSeconds)
		}
	}
	want := []string{"Welcome back.", "Today we", "test captions."}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("cues = %q, want %q (sentence end, 300 ms gap, filler over the trailing silence dropped)", texts, want)
	}
	// Every cue survives the sanitizer unchanged.
	if got := models.SanitizeCaptions(cues); !reflect.DeepEqual(got, cues) {
		t.Errorf("sanitizer changed generated cues:\n%+v\n%+v", got, cues)
	}
}

// Unspaced scripts: a whisper "word" spanning a Han run spreads by character.
func TestSegmentsToCuesCJK(t *testing.T) {
	seg := TranscribeSegmentDTO{Start: 1, End: 3, Text: "今天我们来测试。", Words: []TranscribeWordDTO{w("今天我们", 1, 2), w("来测试。", 2, 2.6)}}
	cues := SegmentsToCuesUntil([]TranscribeSegmentDTO{seg}, seqIDGen(), 0)
	if len(cues) != 1 || cues[0].Text != "今天我们来测试。" || len(cues[0].Words) != 7 {
		t.Fatalf("CJK cue: %+v", cues)
	}
	if ws := cues[0].Words; ws[0].Text != "今" || ws[0].StartSeconds != 1 || ws[1].StartSeconds != 1.25 || ws[6].Text != "试。" || ws[6].EndSeconds != 2.6 {
		t.Errorf("character spread wrong: %+v", ws)
	}
}

// Segments without words convert exactly as before (old callers, old data).
func TestSegmentsToCuesNoWordsUnchanged(t *testing.T) {
	segs := []TranscribeSegmentDTO{{Start: 1, End: 3, Text: " Hello world "}, {Start: 4, End: 20, Text: strings.Repeat("word ", 40)}}
	a := SegmentsToCues(segs, seqIDGen())
	b := SegmentsToCuesUntil(segs, seqIDGen(), 0)
	if !reflect.DeepEqual(a, b) || a[0].Words != nil {
		t.Errorf("no-word path drifted")
	}
	if c := SegmentsToCuesUntil(segs, seqIDGen(), 3.5); len(c) != 1 {
		t.Errorf("a segment starting in the trailing silence is filler: got %d cues", len(c))
	}
}

func TestParseTrailingSilence(t *testing.T) {
	out := "[silencedetect] silence_start: 1.2\n[silencedetect] silence_end: 2.0 | silence_duration: 0.8\n[silencedetect] silence_start: 17.45\n"
	if got := parseTrailingSilence(out); got != 17.45 {
		t.Errorf("trailing silence = %v, want 17.45", got)
	}
	if got := parseTrailingSilence("[silencedetect] silence_start: 1.2\nsilence_end: 2.0\n"); got != 0 {
		t.Errorf("no trailing silence = %v, want 0", got)
	}
	if parseTrailingSilence("") != 0 {
		t.Error("empty output = 0")
	}
}

// ADR-0004 / decision 2: a pre-MM121 project sidecar (fixture written by the
// pre-MM121 code) round-trips byte-identically through the MM121 models and
// sanitizers.
func TestCaptionSidecarLegacyByteStable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "studio_caption_legacy_sidecar.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sc studioProjectSidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	sc.Cues = models.SanitizeCaptions(sc.Cues)
	sc.Style = models.SanitizeCaptionStyle(sc.Style)
	sc.Audio = models.SanitizeAudioConfig(sc.Audio)
	sc.Markers = models.SanitizeMarkers(sc.Markers)
	out, _ := json.Marshal(sc)
	if string(out) != string(raw) {
		t.Errorf("legacy sidecar drifted:\n got %s\nwant %s", out, raw)
	}
}

// The caption catalog fixture the UI copies (fonts with scripts, looks,
// animations, defaults, anonymous allowlist).
func TestCaptionCatalogFixture(t *testing.T) {
	const path = "testdata/captions_catalog.json"
	got, err := json.MarshalIndent(map[string]any{
		"animations":      models.CaptionAnimations,
		"backgroundModes": models.CaptionBackgroundModes,
		"fonts":           models.StudioCaptionFonts,
		"fallbackFonts":   models.CaptionFallbackFonts,
		"scriptPriority":  models.CaptionScriptPriority,
		"looks":           models.StudioCaptionLooks,
		"anon": map[string]any{
			"animations":       models.CaptionAnonAnimations,
			"nearestLook":      models.CaptionAnonNearestLook,
			"nearestAnimation": models.CaptionAnonNearestAnimation,
		},
		"defaults": map[string]any{
			"fontFamily": models.CaptionDefaultFontFamily, "bold": models.CaptionDefaultBold,
			"highlightColor": models.CaptionDefaultHighlightColor, "outlineColor": models.CaptionDefaultOutlineColor,
			"outlineWidth": models.CaptionDefaultOutlineWidth, "shadow": models.CaptionDefaultShadow,
			"backgroundMode": models.CaptionDefaultBackgroundMode, "animation": models.CaptionDefaultAnimation,
			"animationStrength": models.CaptionDefaultAnimationStrength,
		},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("UPDATE_CAPTION_GOLDENS") == "1" {
		_ = os.WriteFile(path, got, 0o644)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil || string(want) != string(got) {
		t.Fatalf("%s is stale: run UPDATE_CAPTION_GOLDENS=1 go test ./internal/services -run 'TestCaptionCatalogFixture|TestCaptionASSGoldens', then copy it to the UI (lib/studio/captions/captionsCatalog.json)", path)
	}
}

// Every caption font file exists in the committed fonts dir, its weights are
// what the catalog says, and every look names a known family.
func TestCaptionFontFilesExist(t *testing.T) {
	dir := filepath.Join("..", "..", "assets", "fonts", "studio")
	for _, f := range models.StudioCaptionFonts {
		for _, name := range []string{f.Regular, f.Bold} {
			if name == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("%s: %s missing from assets/fonts/studio", f.Family, name)
			}
		}
		if tf, ok := models.StudioTitleFonts[f.Family]; ok && (tf.Regular != f.Regular || tf.Bold != f.Bold) {
			t.Errorf("%s: caption files differ from the title font registry", f.Family)
		}
	}
	for fam := range models.StudioTitleFonts {
		if _, ok := models.CaptionFontByFamily(fam); !ok {
			t.Errorf("title font %s is not a caption font (captions may use every title font)", fam)
		}
	}
	for _, l := range models.StudioCaptionLooks {
		if _, ok := models.CaptionFontByFamily(l.Style.FontFamily); !ok {
			t.Errorf("look %s: unknown family %q", l.ID, l.Style.FontFamily)
		}
		if got := models.SanitizeCaptionStyle(&l.Style); !reflect.DeepEqual(*got, l.Style) {
			t.Errorf("look %s does not survive the sanitizer unchanged", l.ID)
		}
	}
}

// The script fallback is a generator rule (decision 6).
func TestCaptionFontFallback(t *testing.T) {
	for _, c := range []struct{ family, text, want string }{
		{"Bangers", "Hello there", "Bangers"},
		{"Bangers", "Привет", "Noto Sans"},
		{"Montserrat", "Привет", "Montserrat"},
		{"Montserrat", "שלום", "Noto Sans Hebrew"},
		{"Open Sans", "שלום", "Open Sans"},
		{"Inter", "مرحبا", "Noto Sans Arabic"},
		{"Inter", "今天", "Noto Sans SC"},
		{"Inter", "안녕하세요", "Noto Sans KR"},
		{"Inter", "नमस्ते", "Noto Sans"},
		{"Poppins", "Đây là tiếng Việt", "Noto Sans"},
		{"Inter", "Đây là tiếng Việt", "Inter"},
		{"Inter", "今天 Привет", "Noto Sans SC"},
	} {
		if got := models.CaptionFontFor(c.family, c.text); got != c.want {
			t.Errorf("CaptionFontFor(%s, %q) = %s, want %s", c.family, c.text, got, c.want)
		}
	}
}
