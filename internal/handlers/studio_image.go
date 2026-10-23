package handlers

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// studioMaxImagePixels is the decompression-bomb guard for still imports.
const studioMaxImagePixels = 100_000_000

// studioKindForFileType maps a detected file type to a Darkroom asset kind.
func studioKindForFileType(ft models.FileType) (models.StudioMediaKind, bool) {
	switch ft {
	case models.FileTypeVideo:
		return models.StudioMediaKindVideo, true
	case models.FileTypeAudio:
		return models.StudioMediaKindAudio, true
	case models.FileTypeImage:
		return models.StudioMediaKindImage, true
	default:
		return "", false
	}
}

var studioGeometryRe = regexp.MustCompile(`(\d+)\s*x\s*(\d+)`)

// studioImageDimensions extracts the DISPLAYED width/height and EXIF
// orientation (1-8) from MediaInspector.ProbeFile image Details. It reads the
// ImageMagick `Geometry: WxH+0+0` line first, then exiftool keys; orientations
// 5-8 swap width/height. Garbage input yields 0,0,1.
func studioImageDimensions(details map[string]any) (w, h, orientation int) {
	orientation = 1
	if details == nil {
		return 0, 0, orientation
	}
	if g, ok := details["Geometry"]; ok {
		if m := studioGeometryRe.FindStringSubmatch(fmt.Sprint(g)); m != nil {
			w, _ = strconv.Atoi(m[1])
			h, _ = strconv.Atoi(m[2])
		}
	}
	container, _ := details["container"].(map[string]any)
	exif, _ := details["exifTiff"].(map[string]any)
	if w <= 0 || h <= 0 {
		w, h = 0, 0
		for _, src := range []map[string]any{details, container, exif} {
			if src == nil {
				continue
			}
			iw := anyToInt(src["ImageWidth"])
			ih := anyToInt(src["ImageHeight"])
			if iw <= 0 || ih <= 0 {
				iw = anyToInt(src["ExifImageWidth"])
				ih = anyToInt(src["ExifImageHeight"])
			}
			if iw > 0 && ih > 0 {
				w, h = iw, ih
				break
			}
		}
	}
	if w <= 0 || h <= 0 {
		return 0, 0, orientation
	}
	for _, src := range []map[string]any{exif, container, details} {
		if src == nil {
			continue
		}
		if o := parseExifOrientation(src["Orientation"]); o > 0 {
			orientation = o
			break
		}
	}
	if orientation >= 5 && orientation <= 8 {
		w, h = h, w
	}
	return w, h, orientation
}

func anyToInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		if m := studioGeometryRe.FindString(t); m == "" {
			n, _ := strconv.Atoi(strings.TrimSpace(t))
			return n
		}
	}
	return 0
}

// parseExifOrientation accepts the numeric 1-8 form, ImageMagick names
// (TopLeft, RightTop, ...) and exiftool's descriptive strings. 0 = unknown.
func parseExifOrientation(v any) int {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return validOrientation(int(t))
	case int:
		return validOrientation(t)
	case int64:
		return validOrientation(int(t))
	}
	s := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
	if n, err := strconv.Atoi(s); err == nil {
		return validOrientation(n)
	}
	switch s {
	case "topleft", "horizontal (normal)":
		return 1
	case "topright", "mirror horizontal":
		return 2
	case "bottomright", "rotate 180":
		return 3
	case "bottomleft", "mirror vertical":
		return 4
	case "lefttop", "mirror horizontal and rotate 270 cw":
		return 5
	case "righttop", "rotate 90 cw":
		return 6
	case "rightbottom", "mirror horizontal and rotate 90 cw":
		return 7
	case "leftbottom", "rotate 270 cw":
		return 8
	}
	return 0
}

func validOrientation(n int) int {
	if n >= 1 && n <= 8 {
		return n
	}
	return 0
}
