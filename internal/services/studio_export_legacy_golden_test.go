package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// MM118 ground rule: the empty export body (the CreaTV embed) and the four
// legacy `format` values must produce BYTE-IDENTICAL ffmpeg args to the
// pre-MM118 pipeline. testdata/studio_export_legacy_args.golden.json was
// generated from the part-20 code BEFORE the output table existed and must
// never be regenerated to make a refactor pass: a diff here is a behaviour
// change for every existing export. (UPDATE_LEGACY_GOLDEN=1 writes it; only
// when a deliberate, reviewed change to the legacy rows is intended.)

const legacyGoldenPath = "testdata/studio_export_legacy_args.golden.json"

// legacyGoldenCases enumerates every legacy format × encoder × quality over a
// set of plan shapes that exercise the output-dependent tail (audio codec,
// container flags, scale, captions, loudness, the keyframe sidecar, silence).
func legacyGoldenCases() map[string][]string {
	type shape struct {
		name string
		mut  func(*StudioExportPlan)
	}
	shapes := []shape{
		{"base", func(*StudioExportPlan) {}},
		{"scale720+captions", func(p *StudioExportPlan) { p.OutHeight = 720; p.CaptionsASSPath = "/job/captions.ass" }},
		{"loudness+sidecar", func(p *StudioExportPlan) { p.Loudness = "streaming"; p.SendCmdPath = "/job/keyframes.cmd" }},
		{"silent", func(p *StudioExportPlan) { p.Audio = nil }},
	}
	type fmtCase struct {
		format   string
		encoders []string
	}
	formats := []fmtCase{
		{"", []string{"h264_nvenc", "libx264"}},
		{"h264", []string{"h264_nvenc", "libx264"}},
		{"hevc", []string{"hevc_nvenc", "libx265"}},
		{"HEVC", []string{"hevc_nvenc"}},
		{"mp3", []string{"audio"}},
		{"wav", []string{"audio"}},
	}
	out := map[string][]string{}
	for _, f := range formats {
		for _, enc := range f.encoders {
			for _, q := range []string{"", "low", "medium", "high"} {
				for _, s := range shapes {
					p := depthPlan()
					p.Format = f.format
					s.mut(&p)
					ext := ".mp4"
					switch strings.ToLower(f.format) {
					case "mp3":
						ext = ".mp3"
					case "wav":
						ext = ".wav"
					}
					name := strings.Join([]string{"format=" + f.format, "enc=" + enc, "q=" + q, s.name}, " ")
					out[name] = buildMultiTrackExportArgs(p, enc, q, "/f.ttf", "/fd", "/out/converted"+ext)
				}
			}
		}
	}
	return out
}

func TestStudioExportLegacyArgsGolden(t *testing.T) {
	got := legacyGoldenCases()
	if os.Getenv("UPDATE_LEGACY_GOLDEN") == "1" {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make(map[string]string, len(got))
		for _, k := range keys {
			ordered[k] = strings.Join(got[k], "\x1f")
		}
		b, err := json.MarshalIndent(ordered, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(legacyGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(legacyGoldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d golden cases", len(got))
		return
	}
	raw, err := os.ReadFile(legacyGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("golden has %d cases, the matrix %d", len(want), len(got))
	}
	for name, args := range got {
		w, ok := want[name]
		if !ok {
			t.Errorf("%s: no golden", name)
			continue
		}
		if g := strings.Join(args, "\x1f"); g != w {
			t.Errorf("%s: args changed\n got: %s\nwant: %s", name, strings.ReplaceAll(g, "\x1f", " "), strings.ReplaceAll(w, "\x1f", " "))
		}
	}
}
