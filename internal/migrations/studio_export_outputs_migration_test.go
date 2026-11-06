package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// MM118: the studio_exports output migration is written, never run on this
// machine, so these assertions keep its contract honest: additive columns,
// CHECKs equal to the API's output table, a backfill for every legacy format,
// the derived-label CHECK, and a down that deletes un-nameable rows before it
// narrows the CHECK.
func TestStudioExportOutputsMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261105002_studio_export_outputs.up.sql")
	down := read("20261105002_studio_export_outputs.down.sql")

	for _, want := range []string{
		"ADD COLUMN IF NOT EXISTS container TEXT",
		"CHECK (container IN ('mp4', 'mov', 'mkv', 'mp3', 'wav'))",
		"CHECK (video_codec IN ('h264', 'hevc', 'none'))",
		"CHECK (audio_codec IN ('aac', 'pcm_s16le', 'mp3'))",
		"ADD COLUMN IF NOT EXISTS audio_bitrate INT",
		"CHECK (audio_bitrate IN (128, 192, 256, 320))",
		"ADD COLUMN IF NOT EXISTS audio_channels SMALLINT",
		"CHECK (audio_channels IN (1, 2))",
		"WHERE format = 'h264' AND container IS NULL",
		"WHERE format = 'hevc' AND container IS NULL",
		"WHERE format = 'mp3' AND container IS NULL",
		"WHERE format = 'wav' AND container IS NULL",
		"DROP CONSTRAINT IF EXISTS studio_exports_format_check",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up migration is missing %q", want)
		}
	}
	for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "NOT NULL"} {
		if strings.Contains(up, forbidden) {
			t.Errorf("up migration must be additive and NULL-tolerant; found %q", forbidden)
		}
	}

	// The widened format CHECK equals the output table's derived labels.
	raw, err := os.ReadFile(filepath.Join("..", "services", "testdata", "studio_outputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Formats []string `json:"formats"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)ADD CONSTRAINT studio_exports_format_check CHECK \(format IN \((.*?)\)\);`).FindStringSubmatch(up)
	if m == nil {
		t.Fatal("up migration does not add the widened format CHECK")
	}
	var got []string
	for _, q := range regexp.MustCompile(`'([a-z0-9_]+)'`).FindAllStringSubmatch(m[1], -1) {
		got = append(got, q[1])
	}
	// MM119+ widen the list in later migrations (TestStudioExportWebMOpusMigration
	// pins the latest CHECK to the table); MM118's must stay a subset of it.
	inTable := map[string]bool{}
	for _, f := range table.Formats {
		inTable[f] = true
	}
	sort.Strings(got)
	if len(got) != 14 {
		t.Errorf("MM118's format CHECK has %d labels, want 14: %v", len(got), got)
	}
	for _, f := range got {
		if !inTable[f] {
			t.Errorf("MM118 format label %q is no longer in the output table", f)
		}
	}

	del := strings.Index(down, "DELETE FROM studio_exports WHERE format NOT IN ('h264', 'hevc', 'mp3', 'wav')")
	narrow := strings.Index(down, "CHECK (format IN ('h264', 'hevc', 'mp3', 'wav'))")
	if del < 0 || narrow < 0 || del > narrow {
		t.Error("down must delete rows with a new label before restoring the part-20 format CHECK")
	}
	for _, col := range []string{"container", "video_codec", "audio_codec", "audio_bitrate", "audio_channels"} {
		if !strings.Contains(down, "DROP COLUMN IF EXISTS "+col) {
			t.Errorf("down must drop %s", col)
		}
	}
}

// MM119: the WebM/Opus/embedded migration widens every closed list to the
// output table (container, codecs, bitrates, the derived-label format CHECK)
// and adds the `embedded` captions mode; the down deletes un-nameable rows
// before narrowing, and turns `embedded` rows into `none`.
func TestStudioExportWebMOpusMigration(t *testing.T) {
	// The MM119 lists are the table minus MM120's Pro rows.
	checkWidenedMigration(t, "20261106001_studio_export_webm_opus", mm120Tokens, true)
	down := readMigration(t, "20261106001_studio_export_webm_opus.down.sql")
	del := strings.Index(down, "DELETE FROM studio_exports")
	embedded := strings.Index(down, "UPDATE studio_exports SET captions_mode = 'none' WHERE captions_mode = 'embedded'")
	narrow := strings.Index(down, "CHECK (captions_mode IN ('none', 'burn_in', 'sidecar'))")
	if del < 0 || embedded < 0 || narrow < 0 || del > narrow || embedded > narrow {
		t.Error("down must delete un-nameable rows and clear `embedded` before narrowing the CHECKs")
	}
}

// MM120: the Pro formats migration widens container (+flac), video (+av1,
// prores), audio (+flac, pcm_s24le) and the format CHECK to the whole output
// table; bitrates and captions modes are unchanged; the down deletes the rows
// MM119 cannot name before narrowing.
func TestStudioExportProFormatsMigration(t *testing.T) {
	checkWidenedMigration(t, "20261106120_studio_export_pro_formats", nil, false)
	down := readMigration(t, "20261106120_studio_export_pro_formats.down.sql")
	del := strings.Index(down, "DELETE FROM studio_exports")
	narrow := strings.Index(down, "CHECK (container IN ('mp4', 'mov', 'mkv', 'webm', 'mp3', 'wav', 'opus'))")
	if del < 0 || narrow < 0 || del > narrow {
		t.Error("down must delete un-nameable rows before narrowing the CHECKs")
	}
	for _, tok := range []string{"'flac'", "'av1'", "'prores'", "'pcm_s24le'"} {
		if !strings.Contains(down[del:narrow], tok) {
			t.Errorf("down must delete rows with %s", tok)
		}
	}
}

// mm120Tokens are the output tokens MM120 added (absent from MM119's lists).
var mm120Tokens = map[string]bool{"flac": true, "av1": true, "prores": true, "pcm_s24le": true}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// checkWidenedMigration asserts an up migration re-adds every closed-list
// CHECK equal to the output table (minus labels/tokens touching `exclude`),
// widens the format CHECK to the table's labels, and only widens.
// withBitrateCaptions also checks the bitrate and captions-mode CHECKs.
func checkWidenedMigration(t *testing.T, name string, exclude map[string]bool, withBitrateCaptions bool) {
	t.Helper()
	up := readMigration(t, name+".up.sql")
	raw, err := os.ReadFile(filepath.Join("..", "services", "testdata", "studio_outputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Formats    []string `json:"formats"`
		Containers []struct {
			Key         string   `json:"key"`
			VideoCodecs []string `json:"videoCodecs"`
			AudioCodecs []string `json:"audioCodecs"`
		} `json:"containers"`
		Codecs []struct {
			Key      string `json:"key"`
			Bitrates []int  `json:"bitrates"`
		} `json:"codecs"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	quoted := func(sql, constraint string) []string {
		t.Helper()
		m := regexp.MustCompile(`(?s)ADD CONSTRAINT ` + constraint + `\s+CHECK \(\w+ IN \((.*?)\)\)`).FindStringSubmatch(sql)
		if m == nil {
			t.Fatalf("%s is not re-added", constraint)
		}
		var got []string
		for _, q := range regexp.MustCompile(`'?([a-z0-9_]+)'?`).FindAllStringSubmatch(m[1], -1) {
			got = append(got, q[1])
		}
		sort.Strings(got)
		return got
	}
	set := func(xs ...string) []string {
		seen := map[string]bool{}
		out := []string{}
		for _, x := range xs {
			if !seen[x] && !exclude[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
		sort.Strings(out)
		return out
	}
	var containers, video, audio, bitrates []string
	for _, c := range table.Containers {
		containers = append(containers, c.Key)
		video = append(video, c.VideoCodecs...)
		audio = append(audio, c.AudioCodecs...)
	}
	for _, c := range table.Codecs {
		for _, b := range c.Bitrates {
			bitrates = append(bitrates, strconv.Itoa(b))
		}
	}
	want := map[string][]string{
		"studio_exports_container_check":   set(containers...),
		"studio_exports_video_codec_check": set(video...),
		"studio_exports_audio_codec_check": set(audio...),
	}
	if withBitrateCaptions {
		want["studio_exports_audio_bitrate_check"] = set(bitrates...)
		want["studio_exports_captions_mode_check"] = set("none", "burn_in", "sidecar", "embedded")
	}
	for constraint, w := range want {
		if got := quoted(up, constraint); strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s: %s = %v, want %v", name, constraint, got, w)
		}
		if !strings.Contains(up, "DROP CONSTRAINT IF EXISTS "+constraint) {
			t.Errorf("%s: up must drop %s before re-adding it", name, constraint)
		}
	}
	m := regexp.MustCompile(`(?s)ADD CONSTRAINT studio_exports_format_check CHECK \(format IN \((.*?)\)\);`).FindStringSubmatch(up)
	if m == nil {
		t.Fatalf("%s: up does not widen the format CHECK", name)
	}
	var got []string
	for _, q := range regexp.MustCompile(`'([a-z0-9_]+)'`).FindAllStringSubmatch(m[1], -1) {
		got = append(got, q[1])
	}
	labels := []string{}
	for _, f := range table.Formats {
		keep := true
		for tok := range exclude {
			// A label is <container>_<video>_<audio>; a token matches a part.
			if strings.HasPrefix(f, tok+"_") || strings.Contains(f, "_"+tok+"_") || strings.HasSuffix(f, "_"+tok) {
				keep = false
			}
		}
		if keep {
			labels = append(labels, f)
		}
	}
	sort.Strings(got)
	sort.Strings(labels)
	if strings.Join(got, ",") != strings.Join(labels, ",") {
		t.Errorf("%s: format CHECK %v != output table formats %v", name, got, labels)
	}
	for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "DELETE FROM", "UPDATE "} {
		if strings.Contains(up, forbidden) {
			t.Errorf("%s: up must only widen CHECKs; found %q", name, forbidden)
		}
	}
}
