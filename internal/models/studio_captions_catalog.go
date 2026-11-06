package models

// Animated-caption catalog (MM121, design DARKROOM_OUTPUTS_AND_CAPTIONS.md §6).
// The Go half of lib/studio/captions/{captionFonts,captionLooks}.ts: caption
// font families (title fonts + display fonts + script fallbacks) with the
// scripts each file covers, the caption looks, the defaults an absent style
// field takes, and the anonymous look allowlist. The look and font tables are
// pinned against the UI by testdata/captions/caption_catalog.json (written by
// TestCaptionCatalogFixture, byte-copied to the UI with an identity test).

// Caption animations (MM121 ships the first four; MM122 adds the rest).
const (
	CaptionAnimationNone        = "none"
	CaptionAnimationKaraokeFill = "karaoke_fill"
	CaptionAnimationHighlight   = "highlight"
	CaptionAnimationPop         = "pop"
)

// CaptionAnimations is the closed animation vocabulary, in display order.
var CaptionAnimations = []string{CaptionAnimationNone, CaptionAnimationKaraokeFill, CaptionAnimationHighlight, CaptionAnimationPop}

// Caption background modes.
const (
	CaptionBackgroundBox     = "box"
	CaptionBackgroundNone    = "none"
	CaptionBackgroundWordBox = "word_box"
)

// CaptionBackgroundModes is the closed backgroundMode vocabulary.
var CaptionBackgroundModes = []string{CaptionBackgroundBox, CaptionBackgroundNone, CaptionBackgroundWordBox}

// Caption scripts the font coverage table speaks. Basic Latin + Latin-1 is
// covered by every caption font and never listed; latin_ext = Latin Extended
// A/B + Latin Extended Additional (Vietnamese, Central European).
const (
	CaptionScriptLatinExt   = "latin_ext"
	CaptionScriptCyrillic   = "cyrillic"
	CaptionScriptGreek      = "greek"
	CaptionScriptHebrew     = "hebrew"
	CaptionScriptArabic     = "arabic"
	CaptionScriptDevanagari = "devanagari"
	CaptionScriptHan        = "han" // CJK ideographs + kana + CJK punctuation
	CaptionScriptHangul     = "hangul"
)

// StudioCaptionFont is one caption family: its TTF files under the fonts dir
// (Bold "" = 400 only; the generator clears Bold for such families), the
// non-Latin scripts the files cover, and its picker group.
type StudioCaptionFont struct {
	Family   string   `json:"family"`
	Regular  string   `json:"regular"`
	Bold     string   `json:"bold,omitempty"`
	Scripts  []string `json:"scripts"`
	Category string   `json:"category"` // sans | serif | display | mono | script | fallback
}

// StudioCaptionFonts is every family a caption may use, in picker order: the
// title fonts (part 16), the MM121 display fonts, then the script fallbacks.
// THE ONLY SOURCE OF CAPTION FONT FILES — user text never forms a path.
var StudioCaptionFonts = []StudioCaptionFont{
	{"Inter", "inter-400.ttf", "inter-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic, CaptionScriptGreek}, "sans"},
	{"Noto Sans", "noto-sans-400.ttf", "noto-sans-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic, CaptionScriptGreek, CaptionScriptDevanagari}, "sans"},
	{"Open Sans", "open-sans-400.ttf", "open-sans-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic, CaptionScriptGreek, CaptionScriptHebrew}, "sans"},
	{"Montserrat", "montserrat-400.ttf", "montserrat-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic}, "sans"},
	{"Poppins", "poppins-400.ttf", "poppins-700.ttf", []string{}, "sans"},
	{"Raleway", "raleway-400.ttf", "raleway-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic}, "sans"},
	{"DejaVu Sans", "dejavu-sans-400.ttf", "dejavu-sans-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic, CaptionScriptGreek, CaptionScriptHebrew}, "sans"},
	{"Oswald", "oswald-400.ttf", "oswald-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic}, "display"},
	{"Bebas Neue", "bebas-neue-400.ttf", "", []string{}, "display"},
	{"Anton", "anton-400.ttf", "", []string{CaptionScriptLatinExt}, "display"},
	{"Bangers", "bangers-400.ttf", "", []string{CaptionScriptLatinExt}, "display"},
	{"Titan One", "titan-one-400.ttf", "", []string{}, "display"},
	{"Lilita One", "lilita-one-400.ttf", "", []string{}, "display"},
	{"Bowlby One", "bowlby-one-400.ttf", "", []string{}, "display"},
	{"Rubik Mono One", "rubik-mono-one-400.ttf", "", []string{CaptionScriptCyrillic, CaptionScriptHebrew}, "display"},
	{"Russo One", "russo-one-400.ttf", "", []string{CaptionScriptCyrillic}, "display"},
	{"Playfair Display", "playfair-display-400.ttf", "playfair-display-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic}, "serif"},
	{"Merriweather", "merriweather-400.ttf", "merriweather-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic}, "serif"},
	{"Lora", "lora-400.ttf", "lora-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic}, "serif"},
	{"JetBrains Mono", "jetbrains-mono-400.ttf", "jetbrains-mono-700.ttf", []string{CaptionScriptLatinExt, CaptionScriptCyrillic, CaptionScriptGreek}, "mono"},
	{"Space Mono", "space-mono-400.ttf", "space-mono-700.ttf", []string{CaptionScriptLatinExt}, "mono"},
	{"Dancing Script", "dancing-script-400.ttf", "dancing-script-700.ttf", []string{CaptionScriptLatinExt}, "script"},
	{"Pacifico", "pacifico-400.ttf", "", []string{CaptionScriptLatinExt, CaptionScriptCyrillic}, "script"},
	{"Noto Sans Hebrew", "noto-sans-hebrew-400.ttf", "noto-sans-hebrew-700.ttf", []string{CaptionScriptHebrew}, "fallback"},
	{"Noto Sans Arabic", "noto-sans-arabic-400.ttf", "noto-sans-arabic-700.ttf", []string{CaptionScriptArabic}, "fallback"},
	{"Noto Sans SC", "noto-sans-sc-400.ttf", "", []string{CaptionScriptGreek, CaptionScriptHan}, "fallback"},
	{"Noto Sans KR", "noto-sans-kr-400.ttf", "", []string{CaptionScriptHangul}, "fallback"},
}

// CaptionFallbackFonts is the order the generator tries when the chosen
// family lacks a script in a cue: the first family covering every script the
// cue needs wins; otherwise the family of the first uncovered script in
// CaptionScriptPriority. A generator rule, not fontconfig luck.
var CaptionFallbackFonts = []string{"Noto Sans", "Noto Sans Hebrew", "Noto Sans Arabic", "Noto Sans SC", "Noto Sans KR"}

// CaptionScriptPriority orders scripts for the mixed-script tie-break, with
// the fallback family each one uses.
var CaptionScriptPriority = []struct {
	Script string `json:"script"`
	Family string `json:"family"`
}{
	{CaptionScriptHan, "Noto Sans SC"},
	{CaptionScriptHangul, "Noto Sans KR"},
	{CaptionScriptArabic, "Noto Sans Arabic"},
	{CaptionScriptHebrew, "Noto Sans Hebrew"},
	{CaptionScriptDevanagari, "Noto Sans"},
	{CaptionScriptCyrillic, "Noto Sans"},
	{CaptionScriptGreek, "Noto Sans"},
	{CaptionScriptLatinExt, "Noto Sans"},
}

// CaptionFontByFamily returns the caption font for an exact family name.
func CaptionFontByFamily(family string) (StudioCaptionFont, bool) {
	for _, f := range StudioCaptionFonts {
		if f.Family == family {
			return f, true
		}
	}
	return StudioCaptionFont{}, false
}

// Defaults an absent StudioCaptionStyle field takes (legacy styles included).
const (
	// A style without a fontFamily (every pre-MM121 project) renders in DejaVu
	// Sans regular: the family the old `Sans` burn-in resolved to on the
	// server, so existing exports keep their look.
	CaptionDefaultFontFamily        = "DejaVu Sans"
	CaptionDefaultBold              = false
	CaptionDefaultHighlightColor    = "#FFD400"
	CaptionDefaultOutlineColor      = "#000000"
	CaptionDefaultOutlineWidth      = 0.0
	CaptionDefaultShadow            = 0.0
	CaptionDefaultBackgroundMode    = CaptionBackgroundBox
	CaptionDefaultAnimation         = CaptionAnimationNone
	CaptionDefaultAnimationStrength = 0.6
)

// StudioCaptionLook is a named, complete caption style (MM121 ships six; MM122
// adds six more). Applying one sets every field and lookId.
type StudioCaptionLook struct {
	ID    string             `json:"id"`
	Anon  bool               `json:"anon"` // on the anonymous allowlist
	Style StudioCaptionStyle `json:"style"`
}

func bptr(v bool) *bool       { return &v }
func fptr(v float64) *float64 { return &v }

// StudioCaptionLooks is the look catalog, in picker order.
var StudioCaptionLooks = []StudioCaptionLook{
	{ID: "bold_pop", Anon: true, Style: StudioCaptionStyle{
		FontSizePct: 7, Color: "#FFFFFF", BackgroundColor: "#000000", BackgroundOpacity: 0.6, Position: "bottom", MaxWidthPct: 85,
		FontFamily: "Montserrat", Bold: bptr(true), Uppercase: bptr(true), HighlightColor: "#FFE600", OutlineColor: "#000000",
		OutlineWidth: fptr(6), Shadow: fptr(0.5), BackgroundMode: CaptionBackgroundNone, WordsPerGroup: 2,
		Animation: CaptionAnimationPop, AnimationStrength: fptr(0.7), LookID: "bold_pop"}},
	{ID: "karaoke_classic", Style: StudioCaptionStyle{
		FontSizePct: 6, Color: "#FFFFFF", BackgroundColor: "#000000", BackgroundOpacity: 0.6, Position: "bottom", MaxWidthPct: 90,
		FontFamily: "Lilita One", Bold: bptr(false), Uppercase: bptr(false), HighlightColor: "#00C8FF", OutlineColor: "#10204A",
		OutlineWidth: fptr(4), Shadow: fptr(0.3), BackgroundMode: CaptionBackgroundNone, WordsPerGroup: 0,
		Animation: CaptionAnimationKaraokeFill, AnimationStrength: fptr(1), LookID: "karaoke_classic"}},
	{ID: "clean_highlight", Anon: true, Style: StudioCaptionStyle{
		FontSizePct: 5.2, Color: "#FFFFFF", BackgroundColor: "#000000", BackgroundOpacity: 0.6, Position: "bottom", MaxWidthPct: 85,
		FontFamily: "Poppins", Bold: bptr(true), Uppercase: bptr(false), HighlightColor: "#FFD400", OutlineColor: "#000000",
		OutlineWidth: fptr(3), Shadow: fptr(0.3), BackgroundMode: CaptionBackgroundNone, WordsPerGroup: 4,
		Animation: CaptionAnimationHighlight, AnimationStrength: fptr(0.5), LookID: "clean_highlight"}},
	{ID: "minimal", Anon: true, Style: StudioCaptionStyle{
		FontSizePct: 4.5, Color: "#FFFFFF", BackgroundColor: "#000000", BackgroundOpacity: 0.6, Position: "bottom", MaxWidthPct: 90,
		FontFamily: "Inter", Bold: bptr(false), Uppercase: bptr(false), HighlightColor: "#FFD400", OutlineColor: "#000000",
		OutlineWidth: fptr(2), Shadow: fptr(0.4), BackgroundMode: CaptionBackgroundNone, WordsPerGroup: 0,
		Animation: CaptionAnimationNone, AnimationStrength: fptr(0.6), LookID: "minimal"}},
	{ID: "neon", Style: StudioCaptionStyle{
		FontSizePct: 6, Color: "#E9FFFB", BackgroundColor: "#000000", BackgroundOpacity: 0.6, Position: "middle", MaxWidthPct: 85,
		FontFamily: "Russo One", Bold: bptr(false), Uppercase: bptr(true), HighlightColor: "#39FF14", OutlineColor: "#FF2BD6",
		OutlineWidth: fptr(3), Shadow: fptr(0), BackgroundMode: CaptionBackgroundNone, WordsPerGroup: 3,
		Animation: CaptionAnimationHighlight, AnimationStrength: fptr(0.6), LookID: "neon"}},
	{ID: "podcast", Style: StudioCaptionStyle{
		FontSizePct: 4.6, Color: "#FFFFFF", BackgroundColor: "#111111", BackgroundOpacity: 0.7, Position: "bottom", MaxWidthPct: 80,
		FontFamily: "Inter", Bold: bptr(true), Uppercase: bptr(false), HighlightColor: "#FFD400", OutlineColor: "#000000",
		OutlineWidth: fptr(0), Shadow: fptr(0), BackgroundMode: CaptionBackgroundBox, WordsPerGroup: 0,
		Animation: CaptionAnimationNone, AnimationStrength: fptr(0.5), LookID: "podcast"}},
}

// CaptionLookByID returns a look by id.
func CaptionLookByID(id string) (StudioCaptionLook, bool) {
	for _, l := range StudioCaptionLooks {
		if l.ID == id {
			return l, true
		}
	}
	return StudioCaptionLook{}, false
}

// Anonymous allowlist (design §6.4): anonymous editors get three looks and the
// animations those looks use; every style field stays free. A non-allowlisted
// value is replaced by the nearest allowed one (CaptionAnonNearest*).
var (
	CaptionAnonAnimations = map[string]bool{CaptionAnimationNone: true, CaptionAnimationHighlight: true, CaptionAnimationPop: true}
	// CaptionAnonNearestLook maps a locked look to the nearest anonymous one.
	CaptionAnonNearestLook = map[string]string{"karaoke_classic": "clean_highlight", "neon": "bold_pop", "podcast": "minimal"}
	// CaptionAnonNearestAnimation maps a locked animation to the nearest one.
	CaptionAnonNearestAnimation = map[string]string{CaptionAnimationKaraokeFill: CaptionAnimationHighlight}
)

// ApplyAnonCaptionAllowlist replaces a non-allowlisted lookId / animation on an
// anonymous caller's style with the nearest allowed one. It returns the
// changes made ("lookId:neon->bold_pop") for the caller's log line; the style
// is modified in place. A nil style is a no-op.
func ApplyAnonCaptionAllowlist(st *StudioCaptionStyle) []string {
	if st == nil {
		return nil
	}
	var changed []string
	if st.LookID != "" {
		if l, ok := CaptionLookByID(st.LookID); !ok || !l.Anon {
			to := CaptionAnonNearestLook[st.LookID]
			if to == "" {
				to = "minimal"
			}
			changed = append(changed, "lookId:"+st.LookID+"->"+to)
			st.LookID = to
		}
	}
	if st.Animation != "" && !CaptionAnonAnimations[st.Animation] {
		to := CaptionAnonNearestAnimation[st.Animation]
		if to == "" {
			to = CaptionAnimationNone
		}
		changed = append(changed, "animation:"+st.Animation+"->"+to)
		st.Animation = to
	}
	return changed
}
