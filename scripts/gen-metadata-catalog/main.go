// Command gen-metadata-catalog parses the section 2.x tables of
// docs/design/research/METADATA_TAG_CATALOG.md and writes
// internal/metadata/catalog_gen.go (Go data for Classify) and
// internal/metadata/catalog.json (the copy the UI ships).
//
// Run from internal/metadata via `go generate`, or:
//
//	go run ./scripts/gen-metadata-catalog
//
// It is deterministic and idempotent, uses only the standard library, and
// fails loudly on a table row it cannot parse.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const schemaVersion = 1

// categories lists every category id (design section 3.3), in display order.
var categories = []string{"file", "image", "camera", "lens", "gps", "datetime", "color", "thumbnail", "iptc", "xmp", "history", "video", "audio", "container", "apple", "png", "pdf", "c2pa", "pano", "makernote", "composite", "other"}

// sectionCategory maps a catalog section number to a category id.
var sectionCategory = map[string]string{
	"2.1": "file", "2.2": "image", "2.3": "camera", "2.4": "lens", "2.5": "gps",
	"2.6": "datetime", "2.7": "color", "2.8": "thumbnail", "2.9": "iptc", "2.10": "xmp",
	"2.11": "history", "2.12": "video", "2.13": "audio", "2.14": "container", "2.15": "container",
	"2.16": "png", "2.17": "pdf", "2.18": "c2pa", "2.19": "pano", "2.20": "makernote",
	"2.21": "composite", "2.22": "other",
}

// unverifiedSections ship every row flagged unverified (catalog "Confidence
// notes"). Empty since MM12 verified 2.1 and 2.12 against fetched pages; 2.22 is
// guidance prose and emits no rows. Individual dagger rows still flag themselves.
var unverifiedSections = map[string]bool{}

// guidanceSections hold prose rows, not tag names; they produce no rows.
var guidanceSections = map[string]bool{"2.22": true}

// vendorGroup maps a "#### Vendor (...)" heading to the exiftool family-1 group.
var vendorGroup = map[string]string{"Canon": "Canon", "Nikon": "Nikon", "Sony": "Sony", "Apple": "Apple", "Samsung": "Samsung", "GoPro": "GoPro", "DJI": "DJI"}

// prefixGroup maps a "<prefix>: " row prefix (colon then space) to a group.
var prefixGroup = map[string]string{
	"tags": "Stream", "side_data": "SideData",
	"MovieHeader": "QuickTime", "TrackHeader": "QuickTime", "MediaHeader": "QuickTime",
}

// CatalogRow mirrors internal/metadata.CatalogRow.
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

type catalogJSON struct {
	SchemaVersion int            `json:"schemaVersion"`
	Categories    []categoryJSON `json:"categories"`
	Tags          []CatalogRow   `json:"tags"`
}

type categoryJSON struct {
	ID       string `json:"id"`
	TitleKey string `json:"titleKey"`
}

var (
	sectionRE = regexp.MustCompile(`^### (2\.\d+) `)
	vendorRE  = regexp.MustCompile(`^#### ([A-Za-z]+) `)
	parenRE   = regexp.MustCompile(`\s*\([^)]*\)`)
	tokenRE   = regexp.MustCompile(`^[\p{L}\p{N}_.*\-©]+(?::[\p{L}\p{N}_.*\-©]+)?(?:[ /+][\p{L}\p{N}_.*\-©]+)*$`)
)

func main() {
	catalogPath := flag.String("catalog", "../docs/design/research/METADATA_TAG_CATALOG.md", "catalog markdown")
	outDir := flag.String("out", "internal/metadata", "output directory")
	flag.Parse()
	if err := run(*catalogPath, *outDir); err != nil {
		fmt.Fprintln(os.Stderr, "gen-metadata-catalog:", err)
		os.Exit(1)
	}
}

func run(catalogPath, outDir string) error {
	src, err := os.ReadFile(catalogPath)
	if err != nil {
		return err
	}
	rows, skipped, err := parse(src)
	if err != nil {
		return err
	}
	rows = normalize(rows)

	var goBuf bytes.Buffer
	writeGo(&goBuf, rows)
	formatted, err := format.Source(goBuf.Bytes())
	if err != nil {
		return fmt.Errorf("format generated go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "catalog_gen.go"), formatted, 0o644); err != nil {
		return err
	}

	cj := catalogJSON{SchemaVersion: schemaVersion, Tags: rows}
	for _, c := range categories {
		cj.Categories = append(cj.Categories, categoryJSON{ID: c, TitleKey: "metadata.category." + c + ".title"})
	}
	jb, err := json.MarshalIndent(cj, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "catalog.json"), append(jb, '\n'), 0o644); err != nil {
		return err
	}

	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Category]++
	}
	for _, c := range categories {
		fmt.Printf("%-10s %4d\n", c, counts[c])
	}
	fmt.Printf("%-10s %4d (descriptive rows skipped: %d)\n", "TOTAL", len(rows), skipped)
	return nil
}

// parse walks the markdown, reading table rows only inside "### 2.x" sections.
func parse(src []byte) (rows []CatalogRow, skipped int, err error) {
	section, vendor := "", ""
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if strings.HasPrefix(line, "## ") {
			section, vendor = "", ""
			continue
		}
		if m := sectionRE.FindStringSubmatch(line); m != nil {
			section, vendor = m[1], ""
			if _, ok := sectionCategory[section]; !ok {
				return nil, 0, fmt.Errorf("line %d: section %s has no category mapping", lineNo, section)
			}
			continue
		}
		if section == "" {
			continue
		}
		if m := vendorRE.FindStringSubmatch(line); m != nil {
			vendor = m[1]
			if _, ok := vendorGroup[vendor]; !ok {
				return nil, 0, fmt.Errorf("line %d: unknown vendor heading %q", lineNo, vendor)
			}
			continue
		}
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "|---") || strings.HasPrefix(line, "| Tag ") {
			continue
		}
		if guidanceSections[section] {
			skipped++
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) != 4 {
			return nil, 0, fmt.Errorf("line %d: want 4 columns, got %d: %s", lineNo, len(cells), line)
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		priv := cells[3]
		if priv != "yes" && priv != "no" && priv != "maybe" {
			return nil, 0, fmt.Errorf("line %d: unexpected Priv value %q", lineNo, priv)
		}
		if strings.HasPrefix(cells[0], "Sub-table:") {
			skipped++ // describes sub-fields of a block; the block row itself is listed
			continue
		}
		tokens, unverified, perr := parseTags(cells[0], vendor)
		if perr != nil {
			return nil, 0, fmt.Errorf("line %d: %v: %s", lineNo, perr, line)
		}
		cat := sectionCategory[section]
		if section == "2.14" && strings.Contains(cells[1], "Keys") {
			cat = "apple"
		}
		if vendor == "Apple" {
			cat = "apple"
		}
		aliases := []string{}
		for _, a := range strings.Split(cells[1], ",") {
			if a = strings.TrimSpace(a); a != "" {
				aliases = append(aliases, a)
			}
		}
		for _, t := range tokens {
			rows = append(rows, CatalogRow{
				Group: t.group, Tag: t.tag, Aliases: aliases, Description: cells[2],
				Privacy:    privacyClass(section, cat, t.tag, priv, cells[2]),
				Category:   cat,
				Unverified: unverified || unverifiedSections[section],
				Section:    section,
			})
		}
	}
	return rows, skipped, sc.Err()
}

type tagToken struct{ group, tag string }

// parseTags turns the first table cell into (group, tag) tokens.
func parseTags(cell, vendor string) ([]tagToken, bool, error) {
	unverified := strings.Contains(cell, "†")
	cell = strings.TrimSpace(strings.ReplaceAll(cell, "†", ""))
	cell = parenRE.ReplaceAllString(cell, "")
	baseGroup := vendorGroup[vendor]
	// "prefix: a / b" (colon + space) carries a group for the whole row.
	if i := strings.Index(cell, ": "); i > 0 {
		g, ok := prefixGroup[cell[:i]]
		if !ok {
			return nil, false, fmt.Errorf("unknown row prefix %q", cell[:i])
		}
		baseGroup, cell = g, strings.TrimSpace(cell[i+2:])
	}
	var out []tagToken
	inherited := ""
	for n, raw := range splitTokens(cell) {
		raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "?"))
		if raw == "" || !tokenRE.MatchString(raw) {
			return nil, false, fmt.Errorf("cannot parse tag %q", raw)
		}
		group, tag := baseGroup, raw
		if i := strings.Index(raw, ":"); i > 0 {
			group, tag = xmpOrIFDGroup(raw[:i]), raw[i+1:]
			if n == 0 {
				inherited = group
			}
		} else if inherited != "" {
			group = inherited
		}
		out = append(out, tagToken{group, tag})
	}
	return out, unverified, nil
}

func xmpOrIFDGroup(prefix string) string {
	if prefix == "IFD1" {
		return "IFD1"
	}
	return "XMP-" + prefix
}

var (
	locationRE = regexp.MustCompile(`(?i)^GPS|Location|Latitude|Longitude|Altitude|Coordinates|Geo|^Place|City|Sublocation|ContentLocation`)
	deviceRE   = regexp.MustCompile(`(?i)Serial|^Make$|^Model$|LensModel|LensMake|LensID|LensInfo|ShutterCount|ShutterActuations|OwnerName|Firmware|HostComputer|InternalSerial|CameraID|DeviceID|^Android(Make|Model|Version)|MachineID`)
	timeTagRE  = regexp.MustCompile(`(?i)(Date|Time|TimeZone|Duration|TimeScale|Offset\*?)$`)
	strongIDRE = regexp.MustCompile(`(?i)Author|Artist|Creator|Photographer|Owner|By-?line|Credit|Contact|Licensor|Producer|Software|Encoder|EncodedWith|Handler|WritingApp|MuxingApp|Signer|claim_generator|DocumentID|InstanceID|UniqueID|Identifier|UUID|^MUID$|Email|Phone|PersonInImage|FaceInfo|DetectedFace|Region`)
	identityRE = regexp.MustCompile(`(?i)Author|Artist|Creator|Photographer|Owner|By-?line|Credit|Contact|Copyright|Rights|Licensor|Producer|Software|CreatorTool|Encoder|EncodedWith|Publisher|Contributor|PersonInImage|Region|FaceInfo|DetectedFace|DocumentID|InstanceID|OriginalDocumentID|UniqueID|ImageUniqueID|Identifier|UUID|UID|^ID$|^MUID$|FileName|Directory|URL|Path|Handler(Description)?$|handler_name|WritingApp|MuxingApp|Signer|claim_generator|instance_id|Email|Phone`)
	historyRE  = regexp.MustCompile(`(?i)History|DerivedFrom|Ingredients|Pantry|DocumentAncestors|TextLayers|Thumbnail|Preview|JpgFromRaw|Comment|UserComment|Description|Caption|Keywords|Subject|Instructions|Annotation|LastURL`)
)

// privacyClass assigns one of the four classes (location, identity, device,
// history) or "" from catalog section 3 plus the row's own Priv flag. no => "".
func privacyClass(section, category, tag, priv, desc string) string {
	if priv == "no" {
		return ""
	}
	t := strings.TrimSuffix(tag, "*")
	if (category == "datetime" || timeTagRE.MatchString(t)) && !locationRE.MatchString(t) {
		return "" // time is not one of the four classes
	}
	switch {
	case category == "gps" || locationRE.MatchString(t):
		return "location"
	case deviceRE.MatchString(t):
		return "device"
	case strongIDRE.MatchString(t):
		return "identity"
	case category == "history" || category == "thumbnail" || historyRE.MatchString(t):
		return "history"
	case identityRE.MatchString(t):
		return "identity"
	case category == "makernote" && priv == "yes":
		return "device"
	case priv == "yes":
		return "identity"
	}
	return ""
}

// normalize sorts rows by (group, tag, section) and drops exact duplicates so
// output is stable.
func normalize(rows []CatalogRow) []CatalogRow {
	secKey := func(s string) int {
		var n int
		fmt.Sscanf(s, "2.%d", &n)
		return n
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Tag != b.Tag {
			return a.Tag < b.Tag
		}
		return secKey(a.Section) < secKey(b.Section)
	})
	var out []CatalogRow
	for _, r := range rows {
		if n := len(out); n > 0 && out[n-1].Group == r.Group && out[n-1].Tag == r.Tag && out[n-1].Section == r.Section {
			continue
		}
		out = append(out, r)
	}
	return out
}

func writeGo(w *bytes.Buffer, rows []CatalogRow) {
	w.WriteString("// Code generated by scripts/gen-metadata-catalog; DO NOT EDIT.\n")
	w.WriteString("// Source: docs/design/research/METADATA_TAG_CATALOG.md section 2.x tables.\n\n")
	w.WriteString("package metadata\n\n")
	w.WriteString("var catalogRows = []CatalogRow{\n")
	for _, r := range rows {
		fmt.Fprintf(w, "\t{Group: %q, Tag: %q, Aliases: %s, Description: %q, Privacy: %q, Category: %q, Unverified: %t, Section: %q},\n",
			r.Group, r.Tag, goStrings(r.Aliases), r.Description, r.Privacy, r.Category, r.Unverified, r.Section)
	}
	w.WriteString("}\n\nfunc init() { buildCatalogIndex() }\n")
}

func goStrings(s []string) string {
	if len(s) == 0 {
		return "nil"
	}
	parts := make([]string, len(s))
	for i, v := range s {
		parts[i] = fmt.Sprintf("%q", v)
	}
	return "[]string{" + strings.Join(parts, ", ") + "}"
}

var tokenSplitRE = regexp.MustCompile(` / |, `)

func splitTokens(cell string) []string { return tokenSplitRE.Split(cell, -1) }
