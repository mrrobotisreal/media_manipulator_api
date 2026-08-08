package models

// Curated title-clip font registry (part 16) — the Go mirror of
// STUDIO_TITLE_FONTS in lib/studio/titleFonts.ts. The sanitizer validates a
// text layer's fontFamily against this map (unknown → the default family) and
// clamps bold away for 400-only families; the export's drawtext resolver
// (services/studio_fonts.go) maps family+weight to the committed TTF under
// CONTENT_STUDIO_FONTS_DIR. Lockstep invariant + license table:
// docs/content-studio/design/04-title-fonts.md.

// StudioTitleFont describes one shipped family's TTF files (relative to the
// fonts directory). Bold is "" for 400-only families.
type StudioTitleFont struct {
	Regular string
	Bold    string
}

// StudioTitleFontDefault mirrors studioDefaultFontFamily — kept as one value.
const StudioTitleFontDefault = "Inter"

// StudioTitleFonts maps a fontFamily value (the exact string the EDL stores)
// to its files. THE ONLY SOURCE OF FONT PATHS — user text never forms a path.
var StudioTitleFonts = map[string]StudioTitleFont{
	"Inter":            {Regular: "inter-400.ttf", Bold: "inter-700.ttf"},
	"Noto Sans":        {Regular: "noto-sans-400.ttf", Bold: "noto-sans-700.ttf"},
	"Open Sans":        {Regular: "open-sans-400.ttf", Bold: "open-sans-700.ttf"},
	"Montserrat":       {Regular: "montserrat-400.ttf", Bold: "montserrat-700.ttf"},
	"Poppins":          {Regular: "poppins-400.ttf", Bold: "poppins-700.ttf"},
	"Raleway":          {Regular: "raleway-400.ttf", Bold: "raleway-700.ttf"},
	"DejaVu Sans":      {Regular: "dejavu-sans-400.ttf", Bold: "dejavu-sans-700.ttf"},
	"Oswald":           {Regular: "oswald-400.ttf", Bold: "oswald-700.ttf"},
	"Bebas Neue":       {Regular: "bebas-neue-400.ttf"},
	"Anton":            {Regular: "anton-400.ttf"},
	"Playfair Display": {Regular: "playfair-display-400.ttf", Bold: "playfair-display-700.ttf"},
	"Merriweather":     {Regular: "merriweather-400.ttf", Bold: "merriweather-700.ttf"},
	"Lora":             {Regular: "lora-400.ttf", Bold: "lora-700.ttf"},
	"JetBrains Mono":   {Regular: "jetbrains-mono-400.ttf", Bold: "jetbrains-mono-700.ttf"},
	"Space Mono":       {Regular: "space-mono-400.ttf", Bold: "space-mono-700.ttf"},
	"Dancing Script":   {Regular: "dancing-script-400.ttf", Bold: "dancing-script-700.ttf"},
	"Pacifico":         {Regular: "pacifico-400.ttf"},
}

// IsKnownStudioTitleFont reports whether family is a shipped title font.
func IsKnownStudioTitleFont(family string) bool {
	_, ok := StudioTitleFonts[family]
	return ok
}

// StudioTitleFontSupportsBold reports whether the family ships a 700 file.
// Unknown families answer for the default family.
func StudioTitleFontSupportsBold(family string) bool {
	f, ok := StudioTitleFonts[family]
	if !ok {
		f = StudioTitleFonts[StudioTitleFontDefault]
	}
	return f.Bold != ""
}
