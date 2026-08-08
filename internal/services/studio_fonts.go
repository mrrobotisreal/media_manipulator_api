package services

import (
	"os"
	"path/filepath"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Title-clip font resolution (part 16). Family names come from the sanitized
// EDL (already validated against models.StudioTitleFonts), so this is a fixed
// map lookup — user text NEVER forms a font path. The directory layout and the
// UI/server lockstep invariant live in
// docs/content-studio/design/04-title-fonts.md.

// resolveTitleFontFile returns the drawtext fontfile for (family, weight):
//   - unknown family → the default family (defense in depth; the sanitizer
//     already guarantees a known name);
//   - weight 700 without a bold file → the regular file;
//   - mapped file missing on disk → fallbackFile (the DejaVu
//     CONTENT_STUDIO_FONT_FILE the server always has).
func resolveTitleFontFile(fontsDir, family string, weight int, fallbackFile string) string {
	f, ok := models.StudioTitleFonts[family]
	if !ok {
		f = models.StudioTitleFonts[models.StudioTitleFontDefault]
	}
	name := f.Regular
	if weight == 700 && f.Bold != "" {
		name = f.Bold
	}
	path := filepath.Join(fontsDir, name)
	if _, err := os.Stat(path); err != nil {
		return fallbackFile
	}
	return path
}
