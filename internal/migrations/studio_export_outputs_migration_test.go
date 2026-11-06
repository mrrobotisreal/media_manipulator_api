package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	want := append([]string{}, table.Formats...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("format CHECK %v != output table formats %v", got, want)
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
