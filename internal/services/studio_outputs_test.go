package services

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

const studioOutputsFixture = "testdata/studio_outputs.json"

// TestStudioOutputTableFixture writes (UPDATE_STUDIO_OUTPUTS=1) or checks the
// committed static table. The UI keeps a byte-identical copy
// (lib/studio/studioOutputs.json, its Vitest identity test) for SSR and
// offline, and the analytics rollup pins its format vocabulary against it.
func TestStudioOutputTableFixture(t *testing.T) {
	got := StudioOutputTableJSON()
	if os.Getenv("UPDATE_STUDIO_OUTPUTS") == "1" {
		if err := os.WriteFile(studioOutputsFixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(studioOutputsFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale: run UPDATE_STUDIO_OUTPUTS=1 go test ./internal/services -run TestStudioOutputTableFixture, then copy it to the UI (lib/studio/studioOutputs.json)", studioOutputsFixture)
	}
}

// Every legacy format resolves to the combination it always meant.
func TestStudioOutputLegacyAliases(t *testing.T) {
	for format, want := range map[string][3]string{
		"":     {"mp4", "h264", "aac"},
		"h264": {"mp4", "h264", "aac"},
		"HEVC": {"mp4", "hevc", "aac"},
		"mp3":  {"mp3", "none", "mp3"},
		"wav":  {"wav", "none", "pcm_s16le"},
	} {
		o, err := ResolveStudioOutput(StudioOutputRequest{Format: format})
		if err != nil {
			t.Fatalf("%q: %v", format, err)
		}
		if o.Container != want[0] || o.VideoCodec != want[1] || o.AudioCodec != want[2] {
			t.Errorf("%q -> %+v", format, o)
		}
		if o.Advanced {
			t.Errorf("%q is a preset, not Advanced", format)
		}
		label := strings.ToLower(format)
		if label == "" {
			label = "h264"
		}
		if o.Label() != label {
			t.Errorf("%q label = %q", format, o.Label())
		}
	}
}

func TestStudioOutputResolve(t *testing.T) {
	ok := []struct {
		req      StudioOutputRequest
		label    string
		advanced bool
		bitrate  int
	}{
		{StudioOutputRequest{}, "h264", false, 192},
		{StudioOutputRequest{Format: "hevc", Container: "mp4", VideoCodec: "hevc"}, "hevc", false, 192}, // explicit fields agreeing with the alias
		{StudioOutputRequest{Preset: "mov_hevc"}, "mov_hevc_aac", false, 192},
		{StudioOutputRequest{Container: "MOV"}, "mov_h264_aac", false, 192},
		{StudioOutputRequest{Container: "mov", AudioCodec: "pcm_s16le"}, "mov_h264_pcm_s16le", true, 0},
		{StudioOutputRequest{Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac"}, "mkv_hevc_aac", false, 192},
		{StudioOutputRequest{Container: "mkv", VideoCodec: "h264", AudioCodec: "mp3", AudioBitrate: 320}, "mkv_h264_mp3", true, 320},
		{StudioOutputRequest{Preset: "mp4_h264", AudioBitrate: 256}, "h264", true, 256},
		{StudioOutputRequest{Preset: "mp4_h264", AudioChannels: 1}, "h264", true, 192},
		{StudioOutputRequest{Preset: "mp4_h264", AudioBitrate: 192, AudioChannels: 2}, "h264", false, 192},
		// MM119 rows.
		{StudioOutputRequest{Preset: "webm_vp9_opus"}, "webm_vp9_opus", false, 128},
		{StudioOutputRequest{Container: "webm"}, "webm_vp9_opus", false, 128},
		{StudioOutputRequest{Container: "webm", AudioCodec: "vorbis"}, "webm_vp9_vorbis", true, 192},
		{StudioOutputRequest{Preset: "audio_opus"}, "opus_none_opus", false, 128},
		{StudioOutputRequest{Container: "opus", AudioBitrate: 64, AudioChannels: 1}, "opus_none_opus", true, 64},
		{StudioOutputRequest{Preset: "mkv_hevc_opus"}, "mkv_hevc_opus", false, 128},
		{StudioOutputRequest{Container: "mp4", AudioCodec: "opus"}, "mp4_h264_opus", true, 128},
		{StudioOutputRequest{Container: "mkv", VideoCodec: "vp9", AudioCodec: "pcm_s16le"}, "mkv_vp9_pcm_s16le", true, 0},
	}
	for _, c := range ok {
		o, err := ResolveStudioOutput(c.req)
		if err != nil {
			t.Errorf("%+v: %v", c.req, err)
			continue
		}
		if o.Label() != c.label || o.Advanced != c.advanced || o.AudioBitrate != c.bitrate {
			t.Errorf("%+v -> label %s advanced %v bitrate %d", c.req, o.Label(), o.Advanced, o.AudioBitrate)
		}
	}
	bad := []StudioOutputRequest{
		{Format: "gif"}, {Format: "webm"}, {Format: "mov"}, // the alias takes legacy values only
		{Preset: "nope"},
		{Format: "hevc", VideoCodec: "h264"},                        // alias + conflicting explicit field
		{Format: "mp3", Container: "mp4"},                           // ditto
		{Format: "h264", Preset: "mov_h264"},                        // alias + conflicting preset
		{Container: "mov", AudioCodec: "opus"},                      // MOV never carries Opus (MM119)
		{Container: "mov", AudioCodec: "vorbis"},                    // nor Vorbis
		{Container: "mp4", AudioCodec: "vorbis"},                    // Vorbis is WebM/MKV only
		{Container: "mp4", VideoCodec: "vp9"},                       // VP9 is WebM/MKV only
		{Container: "webm", VideoCodec: "h264"},                     // WebM is VP9/AV1 only
		{Container: "webm", AudioCodec: "aac"},                      // and Opus/Vorbis only
		{Container: "opus", VideoCodec: "vp9"},                      // .opus is audio-only
		{Container: "opus", AudioCodec: "opus", AudioBitrate: 192},  // not in Opus's list
		{Container: "webm", AudioCodec: "vorbis", AudioBitrate: 96}, // not in Vorbis's list
		{Container: "mp4", AudioCodec: "pcm_s16le"},                 // valid codec, not in MP4
		{Container: "mp4", AudioCodec: "mp3"},                       // not in MP4 either
		{Container: "avi"},                                          // not a container
		{Container: "mp3", VideoCodec: "h264"},                      // audio-only container
		{Container: "mkv", VideoCodec: "dnxhr"},                     // unknown video codec
		{Container: "wav", AudioBitrate: 192},                       // PCM has no bitrate
		{AudioBitrate: 200},                                         // not in the closed list
		{AudioBitrate: -1},
		{AudioChannels: 6},
	}
	for _, r := range bad {
		if _, err := ResolveStudioOutput(r); err == nil {
			t.Errorf("%+v should be rejected", r)
		} else if _, ok := err.(*StudioOutputError); !ok {
			t.Errorf("%+v: error type %T", r, err)
		}
	}
}

// Every combination of the table builds; the derived labels are the closed
// vocabulary and every preset is a valid row.
func TestStudioOutputTableIsConsistent(t *testing.T) {
	tab := StudioOutputs()
	labels := map[string]bool{}
	for _, f := range tab.Formats {
		if labels[f] {
			t.Errorf("duplicate label %s", f)
		}
		labels[f] = true
	}
	for _, c := range tab.Containers {
		for _, v := range c.VideoCodecs {
			for _, a := range StudioAudioCodecsFor(c.Key, v) { // MM120: ProRes narrows MOV to PCM
				o, err := ResolveStudioOutput(StudioOutputRequest{Container: c.Key, VideoCodec: v, AudioCodec: a})
				if err != nil {
					t.Errorf("%s/%s/%s: %v", c.Key, v, a, err)
					continue
				}
				if !labels[o.Label()] {
					t.Errorf("label %s missing from Formats", o.Label())
				}
				for _, tok := range o.Tokens() {
					if _, ok := tab.MinTier[tok]; !ok {
						t.Errorf("token %s of %s has no MinTier", tok, o.Label())
					}
				}
			}
		}
	}
	for _, p := range tab.Presets {
		if _, err := ResolveStudioOutput(StudioOutputRequest{Preset: p.Key}); err != nil {
			t.Errorf("preset %s: %v", p.Key, err)
		}
	}
	for _, tok := range tab.Tokens {
		if _, ok := tab.MinTier[tok]; !ok {
			t.Errorf("token %s has no MinTier", tok)
		}
	}
	if len(tab.Formats) != 51 {
		t.Errorf("MM118 + MM119 + MM120 ship 51 labels, got %d: %v", len(tab.Formats), tab.Formats)
	}
}

func TestStudioOutputWarnings(t *testing.T) {
	cases := map[[3]string]string{
		{"mp4", "hevc", "aac"}:       "",
		{"mov", "h264", "aac"}:       "",
		{"mov", "h264", "pcm_s16le"}: "no_browser_preview",
		{"mkv", "hevc", "aac"}:       "apple_incompatible,no_browser_preview",
		{"mp4", "h264", "opus"}:      "apple_incompatible",
		{"mp4", "hevc", "aac"}:       "",
		{"webm", "vp9", "opus"}:      "apple_incompatible",
		{"opus", "none", "opus"}:     "apple_incompatible",
		{"mkv", "vp9", "vorbis"}:     "apple_incompatible,no_browser_preview",
	}
	for k, want := range cases {
		if got := strings.Join(StudioOutputWarnings(k[0], k[1], k[2]), ","); got != want {
			t.Errorf("%v: %q want %q", k, got, want)
		}
	}
}

func outputPlan(t *testing.T, r StudioOutputRequest) StudioExportPlan {
	t.Helper()
	o, err := ResolveStudioOutput(r)
	if err != nil {
		t.Fatal(err)
	}
	p := depthPlan()
	p.Output = &o
	return p
}

// Golden args per new row (MOV H.264/H.265 with AAC and PCM, MKV with each
// audio codec), plus Advanced bitrate and mono.
func TestStudioOutputRowArgs(t *testing.T) {
	cases := []struct {
		name string
		req  StudioOutputRequest
		enc  string
		tail string
	}{
		{"mov h264 aac", StudioOutputRequest{Preset: "mov_h264"}, "h264_nvenc",
			"-c:v h264_nvenc -preset p5 -rc vbr -cq 19 -b:v 0 -pix_fmt yuv420p -c:a aac -b:a 192k -t 5.000 -movflags +faststart /o.mov"},
		{"mov hevc aac", StudioOutputRequest{Preset: "mov_hevc"}, "hevc_nvenc",
			"-c:v hevc_nvenc -preset p5 -rc vbr -cq 24 -b:v 0 -pix_fmt yuv420p -tag:v hvc1 -c:a aac -b:a 192k -t 5.000 -movflags +faststart /o.mov"},
		{"mov hevc pcm", StudioOutputRequest{Container: "mov", VideoCodec: "hevc", AudioCodec: "pcm_s16le"}, "libx265",
			"-c:v libx265 -preset slow -crf 24 -pix_fmt yuv420p -tag:v hvc1 -x265-params log-level=error -c:a pcm_s16le -t 5.000 -movflags +faststart /o.mov"},
		{"mov h264 pcm", StudioOutputRequest{Container: "mov", AudioCodec: "pcm_s16le"}, "libx264",
			"-c:v libx264 -preset slow -crf 18 -pix_fmt yuv420p -c:a pcm_s16le -t 5.000 -movflags +faststart /o.mov"},
		{"mkv hevc aac", StudioOutputRequest{Preset: "mkv_hevc_aac"}, "hevc_nvenc",
			"-c:v hevc_nvenc -preset p5 -rc vbr -cq 24 -b:v 0 -pix_fmt yuv420p -c:a aac -b:a 192k -t 5.000 /o.mkv"},
		{"mkv h264 pcm", StudioOutputRequest{Container: "mkv", AudioCodec: "pcm_s16le"}, "h264_nvenc",
			"-c:v h264_nvenc -preset p5 -rc vbr -cq 19 -b:v 0 -pix_fmt yuv420p -c:a pcm_s16le -t 5.000 /o.mkv"},
		{"mkv h264 mp3 320", StudioOutputRequest{Container: "mkv", AudioCodec: "mp3", AudioBitrate: 320}, "h264_nvenc",
			"-c:v h264_nvenc -preset p5 -rc vbr -cq 19 -b:v 0 -pix_fmt yuv420p -c:a libmp3lame -b:a 320k -t 5.000 /o.mkv"},
		{"mp4 h264 aac 128 mono", StudioOutputRequest{Preset: "mp4_h264", AudioBitrate: 128, AudioChannels: 1}, "h264_nvenc",
			"-c:v h264_nvenc -preset p5 -rc vbr -cq 19 -b:v 0 -pix_fmt yuv420p -c:a aac -b:a 128k -ac 1 -t 5.000 -movflags +faststart /o.mp4"},
		{"mp3 320 mono", StudioOutputRequest{Format: "mp3", AudioBitrate: 320, AudioChannels: 1}, "audio",
			"-vn -c:a libmp3lame -b:a 320k -ac 1 -t 5.000 /o.mp3"},
	}
	for _, c := range cases {
		p := outputPlan(t, c.req)
		ext, _, _ := p.Output.File()
		args := joined(buildMultiTrackExportArgs(p, c.enc, "high", "/f.ttf", "/fd", "/o"+ext))
		if !strings.HasSuffix(args, c.tail) {
			t.Errorf("%s:\n got %s\nwant suffix %s", c.name, args, c.tail)
		}
		if p.Output.Container == "mkv" && (strings.Contains(args, "faststart") || strings.Contains(args, "hvc1")) {
			t.Errorf("%s: MKV must drop +faststart and the hvc1 tag: %s", c.name, args)
		}
	}
}

// A legacy plan (Output nil) and the same legacy request resolved through the
// table build identical args.
func TestStudioOutputPlanEqualsLegacy(t *testing.T) {
	for _, f := range []string{"", "h264", "hevc", "mp3", "wav"} {
		legacy := depthPlan()
		legacy.Format = f
		viaTable := outputPlan(t, StudioOutputRequest{Format: f})
		enc := "h264_nvenc"
		switch f {
		case "hevc":
			enc = "hevc_nvenc"
		case "mp3", "wav":
			enc = "audio"
		}
		a := joined(buildMultiTrackExportArgs(legacy, enc, "medium", "/f.ttf", "/fd", "/o"))
		b := joined(buildMultiTrackExportArgs(viaTable, enc, "medium", "/f.ttf", "/fd", "/o"))
		if a != b {
			t.Errorf("%q: legacy %s\n table %s", f, a, b)
		}
	}
}

func TestStudioOutputFileAndTokens(t *testing.T) {
	cases := []struct {
		req             StudioOutputRequest
		ext, mime, kind string
		tokens          string
	}{
		{StudioOutputRequest{}, ".mp4", "video/mp4", "video", "mp4,h264,aac"},
		{StudioOutputRequest{Preset: "mov_hevc"}, ".mov", "video/quicktime", "video", "mov,hevc,aac"},
		{StudioOutputRequest{Container: "mkv", AudioCodec: "mp3"}, ".mkv", "video/x-matroska", "video", "mkv,h264,mp3"},
		{StudioOutputRequest{Format: "mp3"}, ".mp3", "audio/mpeg", "audio", "mp3"},
		{StudioOutputRequest{Format: "wav"}, ".wav", "audio/wav", "audio", "wav,pcm_s16le"},
	}
	for _, c := range cases {
		o, err := ResolveStudioOutput(c.req)
		if err != nil {
			t.Fatal(err)
		}
		ext, mime, kind := o.File()
		if ext != c.ext || mime != c.mime || kind != c.kind || strings.Join(o.Tokens(), ",") != c.tokens {
			t.Errorf("%+v -> %s %s %s %v", c.req, ext, mime, kind, o.Tokens())
		}
	}
}

func TestStoredStudioOutput(t *testing.T) {
	// A pre-migration row: only `format`.
	if o, err := StoredStudioOutput("hevc", "", "", "", nil, nil); err != nil || o.Label() != "hevc" {
		t.Errorf("legacy row: %+v %v", o, err)
	}
	b, ch := 256, 1
	o, err := StoredStudioOutput("mov_hevc_aac", "mov", "hevc", "aac", &b, &ch)
	if err != nil || o.Label() != "mov_hevc_aac" || o.AudioBitrate != 256 || o.AudioChannels != 1 || !o.Advanced {
		t.Errorf("new row: %+v %v", o, err)
	}
}

// MM119 golden args: WebM VP9 + Opus per quality, WebM VP9 + Vorbis, MP4
// H.264 + Opus, MKV HEVC + Opus mono 64k (voip), `.opus` audio-only. WebM,
// MKV and .opus never take +faststart; MP4 keeps it.
func TestStudioOutputMM119Args(t *testing.T) {
	vp9 := func(cpu, crf string) string {
		return "-c:v libvpx-vp9 -row-mt 1 -deadline good -cpu-used " + cpu + " -crf " + crf + " -b:v 0 -tile-columns 2 -threads 16 -pix_fmt yuv420p"
	}
	cases := []struct {
		name, quality string
		req           StudioOutputRequest
		enc           string
		tail          string
	}{
		{"webm vp9 opus low", "low", StudioOutputRequest{Preset: "webm_vp9_opus"}, StudioEncoderVP9,
			vp9("4", "40") + " -c:a libopus -b:a 128k -vbr on -ar 48000 -application audio -t 5.000 /o.webm"},
		{"webm vp9 opus medium", "medium", StudioOutputRequest{Preset: "webm_vp9_opus"}, StudioEncoderVP9,
			vp9("3", "34") + " -c:a libopus -b:a 128k -vbr on -ar 48000 -application audio -t 5.000 /o.webm"},
		{"webm vp9 opus high 96", "high", StudioOutputRequest{Preset: "webm_vp9_opus", AudioBitrate: 96}, StudioEncoderVP9,
			vp9("2", "31") + " -c:a libopus -b:a 96k -vbr on -ar 48000 -application audio -t 5.000 /o.webm"},
		{"webm vp9 vorbis", "medium", StudioOutputRequest{Container: "webm", AudioCodec: "vorbis"}, StudioEncoderVP9,
			vp9("3", "34") + " -c:a libvorbis -b:a 192k -t 5.000 /o.webm"},
		{"webm vp9 vorbis 256", "medium", StudioOutputRequest{Container: "webm", AudioCodec: "vorbis", AudioBitrate: 256}, StudioEncoderVP9,
			vp9("3", "34") + " -c:a libvorbis -b:a 256k -t 5.000 /o.webm"},
		{"mp4 h264 opus", "high", StudioOutputRequest{Container: "mp4", AudioCodec: "opus"}, "h264_nvenc",
			"-c:v h264_nvenc -preset p5 -rc vbr -cq 19 -b:v 0 -pix_fmt yuv420p -c:a libopus -b:a 128k -vbr on -ar 48000 -application audio -t 5.000 -movflags +faststart /o.mp4"},
		{"mkv hevc opus mono 64", "high", StudioOutputRequest{Preset: "mkv_hevc_opus", AudioBitrate: 64, AudioChannels: 1}, "hevc_nvenc",
			"-c:v hevc_nvenc -preset p5 -rc vbr -cq 24 -b:v 0 -pix_fmt yuv420p -c:a libopus -b:a 64k -vbr on -ar 48000 -application voip -ac 1 -t 5.000 /o.mkv"},
		{"mkv hevc opus mono 96 stays audio", "high", StudioOutputRequest{Preset: "mkv_hevc_opus", AudioBitrate: 96, AudioChannels: 1}, "hevc_nvenc",
			"-c:a libopus -b:a 96k -vbr on -ar 48000 -application audio -ac 1 -t 5.000 /o.mkv"},
		{"mkv vp9 opus", "medium", StudioOutputRequest{Container: "mkv", VideoCodec: "vp9", AudioCodec: "opus"}, StudioEncoderVP9,
			vp9("3", "34") + " -c:a libopus -b:a 128k -vbr on -ar 48000 -application audio -t 5.000 /o.mkv"},
		{"opus audio-only", "medium", StudioOutputRequest{Preset: "audio_opus"}, "audio",
			"-vn -c:a libopus -b:a 128k -vbr on -ar 48000 -application audio -t 5.000 /o.opus"},
		{"opus audio-only mono 64", "medium", StudioOutputRequest{Container: "opus", AudioBitrate: 64, AudioChannels: 1}, "audio",
			"-vn -c:a libopus -b:a 64k -vbr on -ar 48000 -application voip -ac 1 -t 5.000 /o.opus"},
	}
	for _, c := range cases {
		p := outputPlan(t, c.req)
		ext, _, _ := p.Output.File()
		args := joined(buildMultiTrackExportArgs(p, c.enc, c.quality, "/f.ttf", "/fd", "/o"+ext))
		if !strings.HasSuffix(args, c.tail) {
			t.Errorf("%s:\n got %s\nwant suffix %s", c.name, args, c.tail)
		}
		if p.Output.Container != "mp4" && strings.Contains(args, "faststart") {
			t.Errorf("%s: only MP4/MOV take +faststart: %s", c.name, args)
		}
	}
}

// MM119 embedded subtitles: the SRT is the last input, mapped and encoded in
// the container's subtitle codec with the language and title tags; every
// earlier input index stays put.
func TestStudioOutputEmbeddedSubsArgs(t *testing.T) {
	cases := []struct {
		req   StudioOutputRequest
		enc   string
		codec string
	}{
		{StudioOutputRequest{Preset: "mkv_hevc_aac"}, "hevc_nvenc", "srt"},
		{StudioOutputRequest{Preset: "mp4_h264"}, "h264_nvenc", "mov_text"},
		{StudioOutputRequest{Preset: "mov_hevc"}, "hevc_nvenc", "mov_text"},
		{StudioOutputRequest{Preset: "webm_vp9_opus"}, StudioEncoderVP9, "webvtt"},
	}
	for _, c := range cases {
		p := outputPlan(t, c.req)
		withoutSubs := joined(buildMultiTrackExportArgs(p, c.enc, "medium", "/f.ttf", "/fd", "/o"))
		p.EmbeddedSubs = &StudioEmbeddedSubs{Path: "/w/captions.srt", Language: "ger"}
		args := joined(buildMultiTrackExportArgs(p, c.enc, "medium", "/f.ttf", "/fd", "/o"))
		idx := len(p.Inputs)
		want := fmt.Sprintf("-i /w/captions.srt -filter_complex")
		if !strings.Contains(args, want) {
			t.Errorf("%s: subtitle input must come after every media input: %s", c.codec, args)
		}
		mapped := fmt.Sprintf("-map %d:s:0 -c:s %s -metadata:s:s:0 language=ger -metadata:s:s:0 title=Captions", idx, c.codec)
		if !strings.Contains(args, mapped) {
			t.Errorf("%s: want %q in %s", c.codec, mapped, args)
		}
		stripped := strings.Replace(strings.Replace(args, " -i /w/captions.srt", "", 1), " "+mapped, "", 1)
		if stripped != withoutSubs {
			t.Errorf("%s: embedding must only add the input and the stream:\n%s\n%s", c.codec, stripped, withoutSubs)
		}
	}
	// Untagged language: no language metadata, title still set.
	p := outputPlan(t, StudioOutputRequest{Preset: "mkv_hevc_aac"})
	p.EmbeddedSubs = &StudioEmbeddedSubs{Path: "/w/c.srt"}
	if args := joined(buildMultiTrackExportArgs(p, "hevc_nvenc", "medium", "/f.ttf", "/fd", "/o")); strings.Contains(args, "language=") || !strings.Contains(args, "title=Captions") {
		t.Errorf("untagged: %s", args)
	}
	// Audio-only outputs never carry a subtitle stream.
	a := outputPlan(t, StudioOutputRequest{Preset: "audio_opus"})
	a.EmbeddedSubs = &StudioEmbeddedSubs{Path: "/w/c.srt", Language: "eng"}
	if args := joined(buildMultiTrackExportArgs(a, "audio", "medium", "/f.ttf", "/fd", "/o")); strings.Contains(args, "captions.srt") || strings.Contains(args, "-c:s") || strings.Contains(args, "c.srt") {
		t.Errorf("audio-only must ignore embedded subs: %s", args)
	}
}

func TestStudioOutputSubtitleCodecs(t *testing.T) {
	for container, want := range map[string]string{"mp4": "mov_text", "mov": "mov_text", "mkv": "srt", "webm": "webvtt", "mp3": "", "wav": "", "opus": ""} {
		if got := (StudioOutput{Container: container}).SubtitleCodec(); got != want {
			t.Errorf("%s: %q want %q", container, got, want)
		}
	}
	o, _ := ResolveStudioOutput(StudioOutputRequest{Preset: "audio_opus"})
	if ext, mime, kind := o.File(); ext != ".opus" || mime != "audio/ogg" || kind != "audio" || strings.Join(o.Tokens(), ",") != "opus" {
		t.Errorf(".opus file/tokens: %s %s %s %v", ext, mime, kind, o.Tokens())
	}
	w, _ := ResolveStudioOutput(StudioOutputRequest{Preset: "webm_vp9_opus"})
	if ext, mime, kind := w.File(); ext != ".webm" || mime != "video/webm" || kind != "video" || strings.Join(w.Tokens(), ",") != "webm,vp9,opus" || w.Faststart() {
		t.Errorf("webm file/tokens: %s %s %s %v", ext, mime, kind, w.Tokens())
	}
}
