package services

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStudioMM120LocalRender renders a 3 s fixture through the REAL args
// builder into each MM120 output and ffprobes it: MP4/WebM/MKV AV1 (libsvtav1;
// av1_nvenc when MM_STUDIO_AV1_ENCODER=av1_nvenc on a GPU host), MOV ProRes
// 422 (profile Standard, apcn, yuv422p10le) + 24-bit PCM, FLAC (.flac and in
// MKV) and 24-bit WAV, all audio 48 kHz. Opt-in (MM_STUDIO_LOCAL_RENDER=1);
// MM_STUDIO_LOCAL_RENDER_KEEP=<dir> keeps the files.
func TestStudioMM120LocalRender(t *testing.T) {
	if os.Getenv("MM_STUDIO_LOCAL_RENDER") != "1" {
		t.Skip("set MM_STUDIO_LOCAL_RENDER=1 to render the MM120 fixtures with the local ffmpeg")
	}
	dir := t.TempDir()
	if keep := os.Getenv("MM_STUDIO_LOCAL_RENDER_KEEP"); keep != "" {
		dir = keep
	}
	av1Enc := StudioEncoderSVTAV1
	if os.Getenv("MM_STUDIO_AV1_ENCODER") == StudioEncoderAV1NVENC {
		av1Enc = StudioEncoderAV1NVENC
	}
	src := filepath.Join(dir, "src.mp4")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=s=640x360:r=30:d=3", "-f", "lavfi", "-i", "sine=f=440:d=3:sample_rate=44100",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	cases := []struct {
		name string
		req  StudioOutputRequest
		enc  string
		want []string // substrings of the ffprobe summary
	}{
		{"mp4_av1", StudioOutputRequest{Preset: "mp4_av1"}, av1Enc, []string{"video:av1:av01:yuv420p", "audio:aac:mp4a:48000"}},
		{"webm_av1_opus", StudioOutputRequest{Preset: "webm_av1_opus"}, av1Enc, []string{"video:av1", "audio:opus"}},
		{"mkv_av1_flac", StudioOutputRequest{Container: "mkv", VideoCodec: "av1", AudioCodec: "flac"}, av1Enc, []string{"video:av1", "audio:flac::48000:24"}},
		{"mov_prores", StudioOutputRequest{Preset: "mov_prores"}, StudioEncoderProRes, []string{"video:prores:apcn:yuv422p10le:Standard", "audio:pcm_s24le:in24:48000:24"}},
		{"audio_flac", StudioOutputRequest{Preset: "audio_flac"}, "audio", []string{"audio:flac::48000:24"}},
		{"audio_wav24", StudioOutputRequest{Preset: "audio_wav24"}, "audio", []string{"audio:pcm_s24le::48000:24"}},
	}
	for _, c := range cases {
		o, err := ResolveStudioOutput(c.req)
		if err != nil {
			t.Fatal(err)
		}
		plan := StudioExportPlan{
			Inputs: []ExportInput{{Path: src}},
			Video:  []StudioExportVideoSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 3, TimelineStart: 0, Opacity: 1}},
			Audio:  []StudioExportAudioSeg{{InputIndex: 0, SourceIn: 0, SourceOut: 3, TimelineStart: 0, Volume: 1}},
			Width:  640, Height: 360, FPS: 30, Duration: 3, Output: &o,
		}
		ext, _, _ := o.File()
		out := filepath.Join(dir, c.name+ext)
		args := buildMultiTrackExportArgs(plan, c.enc, "low", "", "", out)
		if res, err := exec.Command("ffmpeg", append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, args...)...).CombinedOutput(); err != nil {
			t.Errorf("%s: ffmpeg %v\n%s\nargs: %s", c.name, err, res, strings.Join(args, " "))
			continue
		}
		probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries",
			"stream=codec_type,codec_name,codec_tag_string,pix_fmt,profile,sample_rate,bits_per_raw_sample",
			"-of", "json", out).Output()
		if err != nil {
			t.Errorf("%s: ffprobe %v", c.name, err)
			continue
		}
		summary := mm120ProbeSummary(string(probe))
		st, _ := os.Stat(out)
		t.Logf("%s%s (%d bytes): %s", c.name, ext, st.Size(), summary)
		for _, w := range c.want {
			if !strings.Contains(summary, w) {
				t.Errorf("%s: ffprobe %q lacks %q", c.name, summary, w)
			}
		}
	}
}

// mm120ProbeSummary renders ffprobe's JSON streams as
// "type:codec:tag:pixfmt:profile" for video and "type:codec:tag:rate:bits"
// for audio (an unknown or [0]-style tag is empty).
func mm120ProbeSummary(raw string) string {
	var doc struct {
		Streams []map[string]any `json:"streams"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return raw
	}
	str := func(m map[string]any, k string) string {
		v, _ := m[k].(string)
		if strings.HasPrefix(v, "[") || v == "unknown" || v == "N/A" {
			return ""
		}
		return v
	}
	var parts []string
	for _, s := range doc.Streams {
		typ := str(s, "codec_type")
		p := typ + ":" + str(s, "codec_name") + ":" + str(s, "codec_tag_string")
		if typ == "video" {
			p += ":" + str(s, "pix_fmt") + ":" + str(s, "profile")
		} else {
			p += ":" + str(s, "sample_rate") + ":" + str(s, "bits_per_raw_sample")
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " | ")
}
