package services

import (
	"reflect"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func TestResolveStemFormatTable(t *testing.T) {
	cases := []struct{ name, file, mime, codec, want string }{
		{"mp3", "a.mp3", "audio/mpeg", "", "mp3"},
		{"m4a ext", "a.m4a", "audio/x-m4a", "", "m4a"},
		{"m4a aac codec", "a.m4a", "audio/mp4", "aac", "m4a"},
		{"m4a alac codec", "a.m4a", "audio/mp4", "alac", "flac"},
		{"aac", "a.aac", "audio/aac", "", "aac"},
		{"ogg", "a.ogg", "audio/ogg", "", "ogg"},
		{"ogg codec vorbis", "a", "audio/ogg", "vorbis", "ogg"},
		{"opus", "a.opus", "audio/ogg", "", "opus"},
		{"ac3", "a.ac3", "", "", "ac3"},
		{"wav", "a.wav", "audio/wav", "", "flac"},
		{"wav codec pcm", "a.wav", "audio/wav", "pcm_s24le", "flac"},
		{"flac", "a.flac", "audio/flac", "", "flac"},
		{"aiff", "a.aif", "audio/aiff", "", "flac"},
		{"mp4 video", "a.mp4", "video/mp4", "aac", "mp3"},
		{"webm video by mime", "a.webm", "video/webm", "opus", "mp3"},
		{"unknown", "a.xyz", "", "", "mp3"},
		{"unknown codec", "a", "audio/x-unknown", "wmav2", "mp3"},
		{"mime with params", "a", "audio/flac; charset=binary", "", "flac"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveStemFormat(tc.file, tc.mime, tc.codec); got != tc.want {
				t.Fatalf("ResolveStemFormat(%q,%q,%q) = %q, want %q", tc.file, tc.mime, tc.codec, got, tc.want)
			}
		})
	}
}

// Golden ffmpeg encoder args: vocal stems get 320k for MP3 and AAC/M4A; WAV and
// FLAC stay lossless; every other AI audio op keeps audioEncoderArgs.
func TestStemEncoderArgsGolden(t *testing.T) {
	golden := map[string][]string{
		"out.mp3":  {"-c:a", "libmp3lame", "-b:a", "320k"},
		"out.m4a":  {"-c:a", "aac", "-b:a", "320k"},
		"out.aac":  {"-c:a", "aac", "-b:a", "320k"},
		"out.flac": {"-c:a", "flac"},
		"out.wav":  {"-c:a", "pcm_s16le"},
		"out.ogg":  {"-c:a", "libvorbis", "-b:a", "192k"},
		"out.opus": {"-c:a", "libopus", "-b:a", "128k"},
		"out.ac3":  {"-c:a", "ac3", "-b:a", "192k"},
	}
	for path, want := range golden {
		if got := StemEncoderArgs(path); !reflect.DeepEqual(got, want) {
			t.Errorf("StemEncoderArgs(%s) = %v, want %v", path, got, want)
		}
	}
	// Clean voice / denoise keep the generic bitrates.
	generic := map[string][]string{
		"out.mp3": {"-c:a", "libmp3lame", "-b:a", "192k"},
		"out.m4a": {"-c:a", "aac", "-b:a", "192k"},
		"out.wav": {"-c:a", "pcm_s16le"},
	}
	for path, want := range generic {
		if got := audioEncoderArgs(path); !reflect.DeepEqual(got, want) {
			t.Errorf("audioEncoderArgs(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestStemOperationAndAuto(t *testing.T) {
	if !IsStemOperation(" Remove_Vocals ") || !IsStemOperation("isolate_vocals") || IsStemOperation("clean_voice") {
		t.Fatal("IsStemOperation")
	}
	if !IsAutoFormat("") || !IsAutoFormat(" AUTO ") || IsAutoFormat("wav") {
		t.Fatal("IsAutoFormat")
	}
}

// An m4a stem (resolved from an .m4a input) passes validation; plain m4a
// conversions keep the old format list.
func TestValidateAudioAcceptsM4AStem(t *testing.T) {
	c := &Converter{}
	base := models.AudioConversionOptions{Format: "m4a", Speed: 1, Volume: 1, Bitrate: "192", SampleRate: "44100", Channels: "stereo"}
	if err := c.validateAudioOptions(&base); err == nil {
		t.Fatal("plain m4a conversion must stay unsupported")
	}
	stem := base
	stem.AI = &models.AIAudioOptions{Enabled: true, Operation: "remove_vocals"}
	if err := c.validateAudioOptions(&stem); err != nil {
		t.Fatalf("m4a stem: %v", err)
	}
}

func TestIsStemJob(t *testing.T) {
	mk := func(enabled bool, op string) *models.ConversionJob {
		return &models.ConversionJob{Options: map[string]interface{}{"ai": map[string]interface{}{"enabled": enabled, "operation": op}}}
	}
	if !isStemJob(mk(true, "isolate_vocals")) || !isStemJob(mk(true, "remove_vocals")) {
		t.Fatal("stem ops")
	}
	if isStemJob(mk(false, "remove_vocals")) || isStemJob(mk(true, "frame_interpolation")) || isStemJob(&models.ConversionJob{}) || isStemJob(nil) {
		t.Fatal("non-stem jobs")
	}
}
