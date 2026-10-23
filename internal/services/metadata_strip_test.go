package services

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func TestMetadataStripArgs(t *testing.T) {
	want := []string{"-map_metadata", "-1", "-map_chapters", "-1"}
	for _, m := range []string{"strip", " Strip "} {
		if got := metadataStripArgs(m); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v", m, got)
		}
	}
	for _, m := range []string{"", "keep", "custom", "bogus"} {
		if got := metadataStripArgs(m); len(got) != 0 {
			t.Errorf("%q must add nothing, got %v", m, got)
		}
	}
	t.Logf("strip video args: ffmpeg -i in.mov %s -y out.mp4", strings.Join(metadataStripArgs("strip"), " "))
}

func TestValidateVideoAudioMetadataMode(t *testing.T) {
	c := &Converter{}
	v := func(mode string) error {
		return c.validateVideoOptions(&models.VideoConversionOptions{Format: "mp4", Speed: 1, Quality: "medium", MetadataMode: mode})
	}
	a := func(mode string) error {
		return c.validateAudioOptions(&models.AudioConversionOptions{Format: "mp3", Speed: 1, Volume: 1, Bitrate: "128", SampleRate: "44100", Channels: "stereo", MetadataMode: mode})
	}
	for name, f := range map[string]func(string) error{"video": v, "audio": a} {
		for _, ok := range []string{"", "keep", "strip"} {
			if err := f(ok); err != nil {
				t.Errorf("%s %q: %v", name, ok, err)
			}
		}
		if err := f("custom"); err == nil || err.Error() != "custom metadata is not supported for video/audio" {
			t.Errorf("%s custom: %v", name, err)
		}
		if err := f("weird"); err == nil || !strings.Contains(err.Error(), "unsupported metadata mode") {
			t.Errorf("%s weird: %v", name, err)
		}
	}
}

func TestStripAVContainerMetadataPass(t *testing.T) {
	var calls [][]string
	prev := runExiftool
	t.Cleanup(func() { runExiftool = prev })
	runExiftool = func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		return "", nil
	}
	for _, out := range []string{"/o/converted.mp4", "/o/converted.MOV", "/o/a.m4a", "/o/a.m4v"} {
		stripAVContainerMetadata("j", "strip", out)
	}
	if len(calls) != 4 {
		t.Fatalf("calls %v", calls)
	}
	want := []string{"-all=", "-overwrite_original", "--", "/o/converted.mp4"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("args %v", calls[0])
	}
	calls = nil
	stripAVContainerMetadata("j", "strip", "/o/converted.webm") // not a target container
	stripAVContainerMetadata("j", "keep", "/o/converted.mp4")   // not strip
	stripAVContainerMetadata("j", "", "/o/converted.mp4")       // default
	stripAVContainerMetadata("j", "strip", "/o/-evil.mp4")      // path after --, still one run
	if len(calls) != 1 || calls[0][2] != "--" || calls[0][3] != "/o/-evil.mp4" {
		t.Fatalf("calls %v", calls)
	}
}

func TestStripAVContainerMetadataFailureIsBestEffort(t *testing.T) {
	prev := runExiftool
	t.Cleanup(func() { runExiftool = prev })
	runExiftool = func(context.Context, ...string) (string, error) { return "disk full", errors.New("exit 1") }
	stripAVContainerMetadata("j", "strip", "/o/converted.mp4") // must not panic or return
}
