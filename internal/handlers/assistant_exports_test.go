package handlers

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM118 acceptance 2: Medi suggest params are validated against the table and
// downgraded for plan (nearest allowed preset + upgrade.required).
func TestAssistantExportsAdvise(t *testing.T) {
	a := &AssistantExports{Registry: tiers.New(config.Load())}
	cases := []struct {
		name, tier string
		params     map[string]any
		want       map[string]any
		upgrade    string
	}{
		{"final cut on premium", "premium", map[string]any{"preset": "mov_h264"}, map[string]any{"container": "mov", "videoCodec": "h264", "preset": "mov_h264"}, ""},
		{"final cut on free -> nearest mp4 h264", "free", map[string]any{"container": "mov"}, map[string]any{"container": "mp4", "videoCodec": "h264", "preset": "mp4_h264"}, "premium"},
		{"mov hevc on free keeps hevc", "free", map[string]any{"preset": "mov_hevc"}, map[string]any{"container": "mp4", "videoCodec": "hevc", "preset": "mp4_hevc"}, "premium"},
		{"hevc for anonymous -> h264 + sign up", "anonymous", map[string]any{"preset": "mp4_hevc"}, map[string]any{"container": "mp4", "videoCodec": "h264", "preset": "mp4_h264"}, "free"},
		{"advanced bitrate on free -> preset", "free", map[string]any{"preset": "mp4_h264", "audioBitrate": float64(320)}, map[string]any{"preset": "mp4_h264", "audioBitrate": 192}, "premium"},
		{"audio on anonymous", "anonymous", map[string]any{"preset": "audio_wav"}, map[string]any{"container": "wav", "audioCodec": "pcm_s16le"}, ""},
		{"mkv archive on pro", "pro", map[string]any{"preset": "mkv_hevc_aac", "resolution": float64(2160), "captionsMode": "none"}, map[string]any{"container": "mkv", "resolution": 2160, "captionsMode": "none"}, ""},
		// MM119 rows.
		{"website embed on premium", "premium", map[string]any{"preset": "webm_vp9_opus", "audioBitrate": float64(96)}, map[string]any{"container": "webm", "videoCodec": "vp9", "audioCodec": "opus", "audioBitrate": 96}, ""},
		{"webm on free -> nearest mp4 h264", "free", map[string]any{"preset": "webm_vp9_opus"}, map[string]any{"container": "mp4", "videoCodec": "h264", "preset": "mp4_h264"}, "premium"},
		{".opus on free -> nearest audio preset", "free", map[string]any{"preset": "audio_opus"}, map[string]any{"container": "mp3", "audioCodec": "mp3", "preset": "audio_mp3"}, "premium"},
		{"podcast opus mono on premium", "premium", map[string]any{"container": "opus", "audioBitrate": float64(96), "audioChannels": float64(1)}, map[string]any{"container": "opus", "audioCodec": "opus", "audioBitrate": 96, "audioChannels": 1}, ""},
		{"embedded subs on premium", "premium", map[string]any{"preset": "mp4_h264", "captionsMode": "embedded"}, map[string]any{"container": "mp4", "captionsMode": "embedded"}, ""},
		{"embedded subs on free -> sidecar", "free", map[string]any{"preset": "mp4_h264", "captionsMode": "embedded"}, map[string]any{"container": "mp4", "captionsMode": "sidecar"}, "premium"},
		{"operator gets anything", "operator", map[string]any{"container": "mkv", "audioCodec": "mp3", "audioChannels": float64(1)}, map[string]any{"container": "mkv", "audioCodec": "mp3", "audioChannels": 1}, ""},
	}
	for _, c := range cases {
		got, err := a.AdviseExport(c.tier, c.params)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s = %v (%T), want %v", c.name, k, got[k], got[k], v)
			}
		}
		up, _ := got["upgrade"].(map[string]any)
		if c.upgrade == "" && up != nil {
			t.Errorf("%s: unexpected upgrade %v", c.name, up)
		}
		if c.upgrade != "" && (up == nil || up["required"] != c.upgrade) {
			t.Errorf("%s: upgrade = %v, want required %s", c.name, up, c.upgrade)
		}
	}
	if got, _ := a.AdviseExport("free", map[string]any{"preset": "mp4_h264", "captionsMode": "embedded"}); got["upgrade"].(map[string]any)["option"] != "subs_embedded" {
		t.Errorf("embedded on free must name subs_embedded: %v", got)
	}
	if got, _ := a.AdviseExport("premium", map[string]any{"preset": "mkv_hevc_aac"}); !strings.Contains(strings.Join(toStrings(got["warnings"]), ","), "apple_incompatible") {
		t.Errorf("MKV suggestion must carry its warnings: %v", got)
	}
	for name, p := range map[string]map[string]any{
		"opus in mov":      {"container": "mov", "audioCodec": "opus"},
		"vorbis in mp4":    {"container": "mp4", "audioCodec": "vorbis"},
		"embedded on opus": {"preset": "audio_opus", "captionsMode": "embedded"},
		"pcm in mp4":       {"container": "mp4", "audioCodec": "pcm_s16le"},
		"unknown preset":   {"preset": "webm_vp9"},
		"unknown param":    {"bitrate": float64(5000)},
		"string bitrate":   {"audioBitrate": "320"},
		"fractional":       {"audioChannels": 1.5},
		"bad resolution":   {"resolution": float64(999)},
		"bad captions":     {"captionsMode": "hardsub"},
		"alias conflict?":  {"preset": "mov_hevc", "container": "mkv"},
	} {
		if _, err := a.AdviseExport("operator", p); err == nil {
			t.Errorf("%s: accepted %v", name, p)
		}
	}
}

func toStrings(v any) []string {
	s, _ := v.([]string)
	return s
}
