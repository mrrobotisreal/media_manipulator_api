package handlers

import (
	"context"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func stemJob(name, mime string, format any, op string) *models.ConversionJob {
	opts := map[string]interface{}{"ai": map[string]interface{}{"enabled": true, "operation": op}}
	if format != nil {
		opts["format"] = format
	}
	return &models.ConversionJob{OriginalFile: models.OriginalFileInfo{Name: name, Type: mime}, Options: opts}
}

// MM81 acceptance 1-2: the stem format resolves from the input when the caller
// sent no format or "auto"; an explicit format (old UIs send "wav") is unchanged.
func TestStemOutputFormatResolution(t *testing.T) {
	h := &ConversionHandler{}
	cases := []struct {
		name, file, mime string
		format           any
		op               string
		want             string
	}{
		{"mp3 auto stays mp3", "Song.mp3", "audio/mpeg", "auto", "remove_vocals", ".mp3"},
		{"mp3 empty stays mp3", "Song.mp3", "audio/mpeg", "", "isolate_vocals", ".mp3"},
		{"mp3 no format key stays mp3", "Song.mp3", "audio/mpeg", nil, "remove_vocals", ".mp3"},
		{"m4a stays m4a", "Song.m4a", "audio/x-m4a", "auto", "remove_vocals", ".m4a"},
		{"aac stays aac", "Song.aac", "audio/aac", "auto", "remove_vocals", ".aac"},
		{"ogg stays ogg", "Song.ogg", "audio/ogg", "auto", "isolate_vocals", ".ogg"},
		{"opus stays opus", "Song.opus", "audio/ogg", "auto", "isolate_vocals", ".opus"},
		{"wav becomes flac", "Song.wav", "audio/wav", "auto", "remove_vocals", ".flac"},
		{"flac stays flac", "Song.flac", "audio/flac", "auto", "remove_vocals", ".flac"},
		{"aiff becomes flac", "Song.aiff", "audio/aiff", "auto", "remove_vocals", ".flac"},
		{"mp4 video becomes mp3", "Clip.mp4", "video/mp4", "auto", "isolate_vocals", ".mp3"},
		{"mov video becomes mp3", "Clip.MOV", "video/quicktime", "", "isolate_vocals", ".mp3"},
		{"explicit wav kept", "Song.mp3", "audio/mpeg", "wav", "remove_vocals", ".wav"},
		{"explicit flac kept", "Song.mp3", "audio/mpeg", "flac", "remove_vocals", ".flac"},
		{"explicit mp3 for wav kept", "Song.wav", "audio/wav", "mp3", "isolate_vocals", ".mp3"},
		{"unknown extension uses mime", "Song.bin", "audio/flac", "auto", "remove_vocals", ".flac"},
		{"unknown extension and mime is mp3", "Song", "audio/x-unknown", "auto", "remove_vocals", ".mp3"},
		{"other AI op unaffected", "Talk.wav", "audio/wav", "mp3", "clean_voice", ".mp3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.getOutputExtension(stemJob(tc.file, tc.mime, tc.format, tc.op)); got != tc.want {
				t.Fatalf("extension = %q, want %q", got, tc.want)
			}
		})
	}
}

// The upload path rewrites "auto" to the concrete format before the job exists,
// probing the codec only when the extension cannot tell (an .m4a may be ALAC).
func TestResolveStemFormatOption(t *testing.T) {
	prev := stemCodecProbe
	t.Cleanup(func() { stemCodecProbe = prev })
	probed := 0
	codec := ""
	stemCodecProbe = func(context.Context, *config.Config, string) string { probed++; return codec }

	cases := []struct {
		name, file, mime, codec string
		opts                    map[string]interface{}
		wantFormat              any
		wantProbes              int
	}{
		{"mp3 auto, no probe", "a.mp3", "audio/mpeg", "", map[string]interface{}{"format": "auto", "ai": map[string]interface{}{"enabled": true, "operation": "remove_vocals"}}, "mp3", 0},
		{"m4a aac probed", "a.m4a", "audio/x-m4a", "aac", map[string]interface{}{"format": "auto", "ai": map[string]interface{}{"enabled": true, "operation": "remove_vocals"}}, "m4a", 1},
		{"m4a alac probed -> flac", "a.m4a", "audio/x-m4a", "alac", map[string]interface{}{"format": "", "ai": map[string]interface{}{"enabled": true, "operation": "isolate_vocals"}}, "flac", 1},
		{"explicit wav untouched", "a.mp3", "audio/mpeg", "", map[string]interface{}{"format": "wav", "ai": map[string]interface{}{"enabled": true, "operation": "remove_vocals"}}, "wav", 0},
		{"not a stem op untouched", "a.mp3", "audio/mpeg", "", map[string]interface{}{"format": "auto", "ai": map[string]interface{}{"enabled": true, "operation": "clean_voice"}}, "auto", 0},
		{"ai disabled untouched", "a.mp3", "audio/mpeg", "", map[string]interface{}{"format": "mp3"}, "mp3", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probed, codec = 0, tc.codec
			resolveStemFormatOption(context.Background(), &config.Config{}, tc.opts, tc.file, tc.mime, "/tmp/x")
			if tc.opts["format"] != tc.wantFormat {
				t.Fatalf("format = %v, want %v", tc.opts["format"], tc.wantFormat)
			}
			if probed != tc.wantProbes {
				t.Fatalf("probes = %d, want %d", probed, tc.wantProbes)
			}
		})
	}
}
