package handlers

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

func gz(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write(bytes.Repeat([]byte("x"), 600))
	_ = w.Close()
	return b.Bytes()
}

func TestSniffResultMimeTable(t *testing.T) {
	pad := make([]byte, 64)
	cases := []struct {
		name   string
		file   string
		body   []byte
		want   string
		inline bool
	}{
		{"mp4", "a.mp4", append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 2, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '1'}, pad...), "video/mp4", true},
		{"webm", "a.webm", append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81, 0x01, 0x42, 0xF7, 0x81, 0x01, 0x42, 0xF2, 0x81, 0x04, 0x42, 0xF3, 0x81, 0x08, 0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'}, pad...), "video/webm", true},
		{"mp3", "a.mp3", append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), pad...), "audio/mpeg", true},
		{"wav", "a.wav", append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), pad...), "audio/wav", true},
		{"png", "a.png", append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), pad...), "image/png", true},
		{"jpeg", "a.jpg", append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, pad...), "image/jpeg", true},
		{"webp", "a.webp", append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), pad...), "image/webp", true},
		{"gif", "a.gif", append([]byte("GIF89a\x01\x00\x01\x00"), pad...), "image/gif", true},
		{"pdf", "a.pdf", append([]byte("%PDF-1.7\n"), pad...), "application/pdf", true},
		{"zip", "a.zip", append([]byte("PK\x03\x04\x14\x00\x00\x00\x08\x00"), pad...), "application/zip", false},
		{"targz", "r.tar.gz", gz(t), "application/gzip", false},
		{"srt", "a.srt", []byte("1\n00:00:00,000 --> 00:00:01,000\nhello\n"), "application/x-subrip", true},
		{"vtt", "a.vtt", []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhello\n"), "text/vtt", true},
		{"svg", "a.svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"></svg>`), "image/svg+xml", false},
		{"unknown", "a.bin", []byte{0x01, 0x02, 0x03, 0xFE, 0xFD, 0x00, 0x99}, "application/octet-stream", false},
	}
	dir := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.file)
			if err := os.WriteFile(p, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			got := sniffResultMime(p)
			if got != tc.want {
				t.Fatalf("sniff %s: got %q want %q", tc.file, got, tc.want)
			}
			if inlineSafeMime(got) != tc.inline {
				t.Fatalf("inlineSafeMime(%q) = %v want %v", got, !tc.inline, tc.inline)
			}
		})
	}
}

func TestEnsureResultMimeStoresOnceAndOmitsBeforeResult(t *testing.T) {
	jm := services.NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "x.png"}, map[string]interface{}{})
	if job.ResultMime != "" {
		t.Fatal("fresh job must have no resultMime")
	}
	p := filepath.Join(t.TempDir(), "o.png")
	_ = os.WriteFile(p, append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 40)...), 0o600)
	h := &ConversionHandler{jobManager: jm}
	if got := h.ensureResultMime(job, p); got != "image/png" {
		t.Fatalf("got %q", got)
	}
	stored, _ := jm.GetJob(job.ID)
	if stored.ResultMime != "image/png" {
		t.Fatalf("not stored: %q", stored.ResultMime)
	}
}
