package services

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// TestStudioMM119LocalRender renders a 4 s fixture through the REAL args
// builder into each MM119 output and ffprobes it: WebM VP9 + Opus 48 kHz with
// a webvtt track, WebM VP9 + Vorbis, `.opus`, MKV HEVC/H.264 + Opus with an
// SRT (subrip) track tagged ger, MP4 H.264 + Opus with mov_text, MOV with
// mov_text (the MP4 muxer keeps the language, not the title). Opt-in (MM_STUDIO_LOCAL_RENDER=1, needs ffmpeg with libvpx-vp9,
// libopus, libvorbis); MM_STUDIO_LOCAL_RENDER_KEEP=<dir> keeps the files.
func TestStudioMM119LocalRender(t *testing.T) {
	if os.Getenv("MM_STUDIO_LOCAL_RENDER") != "1" {
		t.Skip("set MM_STUDIO_LOCAL_RENDER=1 to render the MM119 fixtures with the local ffmpeg")
	}
	dir := t.TempDir()
	if keep := os.Getenv("MM_STUDIO_LOCAL_RENDER_KEEP"); keep != "" {
		dir = keep
	}
	src := filepath.Join(dir, "src.mp4")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=s=640x360:r=30:d=4", "-f", "lavfi", "-i", "sine=f=440:d=4:sample_rate=44100",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	srt := filepath.Join(dir, "captions.srt")
	cues := []models.StudioCaptionCue{{ID: "1", StartSeconds: 0.5, EndSeconds: 2, Text: "Hallo Welt"}, {ID: "2", StartSeconds: 2.2, EndSeconds: 3.8, Text: "Zweite Zeile"}}
	if err := os.WriteFile(srt, []byte(BuildStudioSRT(cues)), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  StudioOutputRequest
		enc  string
		subs bool
		want []string // substrings of the ffprobe stream summary
	}{
		{"webm_vp9_opus_subs", StudioOutputRequest{Preset: "webm_vp9_opus", AudioBitrate: 96}, StudioEncoderVP9, true, []string{"video:vp9", "audio:opus:48000", "subtitle:webvtt:ger:Captions"}},
		{"webm_vp9_vorbis", StudioOutputRequest{Container: "webm", AudioCodec: "vorbis"}, StudioEncoderVP9, false, []string{"video:vp9", "audio:vorbis:48000"}},
		{"audio_opus", StudioOutputRequest{Preset: "audio_opus"}, "audio", false, []string{"audio:opus:48000"}},
		{"mkv_hevc_opus_mono64_subs", StudioOutputRequest{Preset: "mkv_hevc_opus", AudioBitrate: 64, AudioChannels: 1}, "libx265", true, []string{"video:hevc", "audio:opus:48000", "subtitle:subrip:ger:Captions"}},
		{"mp4_h264_opus_subs", StudioOutputRequest{Container: "mp4", AudioCodec: "opus"}, "libx264", true, []string{"video:h264", "audio:opus:48000", "subtitle:mov_text:ger"}},
		{"mov_h264_aac_subs", StudioOutputRequest{Preset: "mov_h264"}, "libx264", true, []string{"video:h264", "audio:aac", "subtitle:mov_text:ger"}},
	}
	encoders, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	for _, c := range cases {
		if c.req.AudioCodec == "vorbis" && !strings.Contains(string(encoders), "libvorbis") {
			t.Logf("%s: SKIPPED, this ffmpeg has no libvorbis (the server's has; MM119 checked it there)", c.name)
			continue
		}
		o, err := ResolveStudioOutput(c.req)
		if err != nil {
			t.Fatal(err)
		}
		plan := StudioExportPlan{
			Inputs: []ExportInput{{Path: src}},
			Video:  []StudioExportVideoSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 0, Opacity: 1}},
			Audio:  []StudioExportAudioSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 4, TimelineStart: 0, Volume: 1}},
			Width:  640, Height: 360, FPS: 30, Duration: 4, Output: &o,
		}
		if c.subs {
			plan.EmbeddedSubs = &StudioEmbeddedSubs{Path: srt, Language: StudioSubtitleLanguage("de")}
		}
		ext, _, _ := o.File()
		out := filepath.Join(dir, c.name+ext)
		args := buildMultiTrackExportArgs(plan, c.enc, "low", "", "", out)
		if res, err := exec.Command("ffmpeg", append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, args...)...).CombinedOutput(); err != nil {
			t.Errorf("%s: ffmpeg %v\n%s\nargs: %s", c.name, err, res, strings.Join(args, " "))
			continue
		}
		probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name,sample_rate,channels:stream_tags=language,title",
			"-of", "csv=p=0", out).Output()
		if err != nil {
			t.Errorf("%s: ffprobe %v", c.name, err)
			continue
		}
		summary := probeSummary(string(probe))
		t.Logf("%s%s: %s", c.name, ext, summary)
		for _, w := range c.want {
			if !strings.Contains(summary, w) {
				t.Errorf("%s: ffprobe %q lacks %q", c.name, summary, w)
			}
		}
	}
}

// probeSummary turns ffprobe csv rows (codec_name,codec_type,sample_rate,
// channels,language,title in ffprobe's own order) into "type:codec[:rate][:lang][:title]".
func probeSummary(csv string) string {
	var parts []string
	for _, line := range strings.Split(strings.TrimSpace(csv), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 2 {
			continue
		}
		p := f[1] + ":" + f[0]
		for _, x := range f[2:] {
			if x != "" && x != "N/A" {
				p += ":" + x
			}
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " | ")
}
