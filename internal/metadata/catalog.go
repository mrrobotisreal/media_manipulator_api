package metadata

import (
	"regexp"
	"strings"
)

// CatalogRow is one generated catalog entry (see catalog_gen.go). Group is the
// exiftool family-1 group when the catalog names one; empty rows match a tag in
// any group whose default category is compatible.
type CatalogRow struct {
	Group       string   `json:"group,omitempty"`
	Tag         string   `json:"tag"`
	Aliases     []string `json:"aliases"`
	Description string   `json:"description"`
	Privacy     string   `json:"privacy"`
	Category    string   `json:"category"`
	Unverified  bool     `json:"unverified"`
	Section     string   `json:"-"`
}

// Categories are the server category ids (design section 3.3).
var Categories = []string{"file", "image", "camera", "lens", "gps", "datetime", "color", "thumbnail", "iptc", "xmp", "history", "video", "audio", "container", "apple", "png", "pdf", "c2pa", "pano", "makernote", "composite", "other"}

var (
	groupTagIndex = map[string]*CatalogRow{}   // "group\x00tag" lowercased
	tagIndex      = map[string][]*CatalogRow{} // group-less rows by lowercased tag, catalog order
	aliasIndex    = map[string][]*CatalogRow{} // bare ExifTool-style alias names, e.g. BodySerialNumber
	wildcardRows  []*CatalogRow                // tag ends with "*": prefix match
)

func buildCatalogIndex() {
	for i := range catalogRows {
		r := &catalogRows[i]
		tag := strings.ToLower(r.Tag)
		if strings.HasSuffix(tag, "*") {
			wildcardRows = append(wildcardRows, r)
			continue
		}
		if r.Group != "" {
			k := strings.ToLower(r.Group) + "\x00" + tag
			if _, ok := groupTagIndex[k]; !ok {
				groupTagIndex[k] = r
			}
			continue
		}
		tagIndex[tag] = append(tagIndex[tag], r)
		for _, a := range r.Aliases {
			if aliasRE.MatchString(a) {
				la := strings.ToLower(a)
				aliasIndex[la] = append(aliasIndex[la], r)
			}
		}
	}
}

// aliasRE matches an "Also seen as" token that is itself an ExifTool-style tag
// name (starts upper case, one word); hex ids, ffprobe names and prose do not.
var aliasRE = regexp.MustCompile(`^[A-Z][A-Za-z0-9]+$`)

// pickRow chooses among group-less rows for a tag: one in the group's own
// default category, else the first whose category the default may refine to.
func pickRow(rows []*CatalogRow, def string) *CatalogRow {
	for _, r := range rows {
		if r.Category == def {
			return r
		}
	}
	for _, r := range rows {
		if canRefine(def, r.Category) {
			return r
		}
	}
	if def == "apple" && len(rows) > 0 { // Keys: any row's privacy applies, category stays apple
		return rows[0]
	}
	return nil
}

var syntheticGroups = map[string]bool{"format": true, "stream": true, "chapter": true, "sidedata": true}

// refinable lists the categories a group-less catalog row may move an entry to
// from the group's default category. Without this a tag such as "Title" would
// pull entries of unrelated groups into its category.
var refinable = map[string][]string{
	"camera":    {"camera", "lens", "datetime", "color", "image"},
	"image":     {"image", "camera", "lens", "datetime", "color", "thumbnail"},
	"thumbnail": {"thumbnail", "image", "datetime"},
	"container": {"container", "apple", "video", "audio", "datetime", "file", "c2pa"},
	"audio":     {"audio", "datetime"},
	"video":     {"video", "audio", "container", "datetime", "color"},
	"pdf":       {"pdf", "datetime", "history"},
	"png":       {"png", "color", "datetime", "thumbnail"},
	"xmp":       {"xmp", "history", "datetime", "camera", "lens", "image", "color", "pano"},
	"history":   {"history", "xmp", "datetime"},
	"composite": {"composite", "gps", "datetime", "image", "camera", "lens"},
	"color":     {"color"},
	"file":      {"file", "datetime"},
	"iptc":      {"iptc", "datetime"},
	"gps":       {"gps"},
}

func canRefine(def, to string) bool {
	if def == to {
		return true
	}
	for _, c := range refinable[def] {
		if c == to {
			return true
		}
	}
	return false
}

var locationRE = regexp.MustCompile(`(?i)(^|\.)(gps|location|geo)`)

var makerGroups = map[string]bool{
	"canon": true, "nikon": true, "sony": true, "samsung": true, "gopro": true, "dji": true,
	"pentax": true, "olympus": true, "panasonic": true, "fujifilm": true, "leica": true,
	"minolta": true, "casio": true, "ricoh": true, "sigma": true, "kodak": true, "sanyo": true,
	"makernotes": true, "canonraw": true, "nikonhdr": true, "samsungtt": true, "hasselblad": true, "phaseone": true,
}

var exactGroupCategory = map[string]string{
	"file": "file", "system": "file",
	"ifd0": "image", "interopifd": "image", "subifd": "image", "ifd2": "image",
	"ifd1": "thumbnail", "exififd": "camera", "gps": "gps", "composite": "composite",
	"photoshop": "history", "iptc": "iptc",
	"jfif": "png", "jfxx": "png", "adobe": "png", "mpf0": "png", "mpf": "png", "png": "png", "gif": "png",
	"webp": "png", "apng": "png", "ducky": "png", "ciff": "png", "jpeg": "png", "jpegxl": "png",
	"riff": "container", "quicktime": "container", "keys": "apple", "itemlist": "container",
	"userdata": "container", "matroska": "container", "mpeg": "container", "ftyp": "container", "flash": "container", "aiff": "audio", "asf": "container",
	"id3": "audio", "id3v1": "audio", "id3v2_2": "audio", "id3v2_3": "audio", "id3v2_4": "audio",
	"vorbis": "audio", "flac": "audio", "ape": "audio", "opus": "audio", "audible": "audio", "m4a": "audio",
	"apple": "apple", "pdf": "pdf", "c2pa": "c2pa", "jumbf": "c2pa",
	"format": "container", "sidedata": "video", "stream": "container", "chapter": "container",
	"exiftool": "other",
}

var xmpGroupCategory = map[string]string{
	"xmpmm": "history", "crs": "history", "photoshop": "history", "xmprights": "xmp",
	"gpano": "pano", "gimage": "pano", "gdepth": "pano", "gcamera": "pano", "gcontainer": "pano",
	"gaudio": "pano", "gfocus": "pano", "hdrgm": "pano", "pdf": "pdf",
}

// normalizeGroup lowercases a group and strips exiftool numeric/track suffixes
// ("Track1", "Stream:0", "Chapter:3", "ID3v2_4" stay as is where meaningful).
func normalizeGroup(group string) string {
	g := strings.ToLower(strings.TrimSpace(group))
	for _, p := range []string{"stream:", "chapter:"} {
		if strings.HasPrefix(g, p) {
			return strings.TrimSuffix(p, ":")
		}
	}
	if strings.HasPrefix(g, "track") && len(g) > 5 && g[5] >= '0' && g[5] <= '9' {
		return "quicktime"
	}
	if strings.HasPrefix(g, "icc") {
		return "icc"
	}
	if strings.HasPrefix(g, "png-") {
		return "png"
	}
	if strings.HasPrefix(g, "ifd") && len(g) > 3 && g[3] >= '0' && g[3] <= '9' && g != "ifd0" && g != "ifd1" {
		return "ifd2"
	}
	return g
}

func groupDefault(group string) (category string, known bool) {
	g := normalizeGroup(group)
	switch {
	case g == "icc":
		return "color", true
	case strings.HasPrefix(g, "xmp-"):
		if c, ok := xmpGroupCategory[strings.TrimPrefix(g, "xmp-")]; ok {
			return c, true
		}
		return "xmp", true
	case g == "xmp":
		return "xmp", true
	case makerGroups[g]:
		return "makernote", true
	}
	c, ok := exactGroupCategory[g]
	return c, ok
}

// groupPrivacy is the privacy class of a whole group when the catalog has no
// row for the tag.
func groupPrivacy(normGroup string) string {
	if normGroup == "gps" {
		return PrivacyLocation
	}
	return ""
}

// Classify assigns a category and privacy class: an explicit (group, tag)
// catalog match, then the group default (refined by a group-less catalog row
// for the tag), then "other". Group matching is case-insensitive and tolerates
// exiftool suffixes (Track1, Stream:0, XMP-xmp). Apple "com.apple.quicktime.*"
// keys classify as "apple".
func Classify(group, tag string) (category, privacy string) {
	g := normalizeGroup(group)
	lt := strings.ToLower(tag)
	short := strings.TrimPrefix(lt, "com.apple.quicktime.")

	if r, ok := groupTagIndex[g+"\x00"+lt]; ok {
		return r.Category, r.Privacy
	}
	if r, ok := groupTagIndex[strings.ToLower(group)+"\x00"+lt]; ok {
		return r.Category, r.Privacy
	}

	def, known := groupDefault(group)
	if !known {
		return "other", ""
	}
	category, privacy = def, groupPrivacy(g)
	if strings.HasPrefix(lt, "com.apple.quicktime.") && (g == "keys" || g == "quicktime") {
		category = "apple"
	}

	if syntheticGroups[g] {
		return category, privacy
	}
	var row *CatalogRow
	if rows, ok := tagIndex[short]; ok {
		row = pickRow(rows, category)
	}
	if row == nil {
		row = pickRow(aliasIndex[short], category)
	}
	if row == nil {
		for _, w := range wildcardRows {
			if strings.HasPrefix(short, strings.TrimSuffix(strings.ToLower(w.Tag), "*")) && (w.Group == "" || strings.EqualFold(w.Group, group)) {
				row = w
				break
			}
		}
	}
	if row == nil && privacy == "" && locationRE.MatchString(short) {
		privacy = PrivacyLocation
	}
	if row != nil {
		privacy = row.Privacy
		if category != "apple" && canRefine(category, row.Category) {
			category = row.Category
		}
	}
	return category, privacy
}
