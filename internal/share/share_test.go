package share

import (
	"strings"
	"testing"
)

func TestNewTokenShapeAndHash(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != TokenLen || !WellFormed(tok) {
		t.Fatalf("token %q not 43 base64url chars", tok)
	}
	if hash == tok || len(hash) != 64 || hash != HashToken(tok) {
		t.Fatalf("hash must be hex sha256 of the token, got %q", hash)
	}
	tok2, _, _ := NewToken()
	if tok == tok2 {
		t.Fatal("tokens must be random")
	}
}

func TestWellFormed(t *testing.T) {
	for _, s := range []string{"", "short", strings.Repeat("a", 44), strings.Repeat("a", 42) + "+", strings.Repeat("a", 42) + "/", strings.Repeat("a", 42) + "="} {
		if WellFormed(s) {
			t.Errorf("%q should be rejected", s)
		}
	}
	if !WellFormed(strings.Repeat("a", 21) + "-_" + strings.Repeat("Z", 20)) {
		t.Error("valid shape rejected")
	}
}

func TestKindAndShareableMatrix(t *testing.T) {
	cases := []struct {
		name  string
		in    Input
		kind  Kind
		share bool
	}{
		{"png", Input{FileName: "converted.png"}, KindImage, true},
		{"svg is an image kind (served as attachment)", Input{FileName: "converted.svg"}, KindImage, true},
		{"gif", Input{FileName: "converted.gif"}, KindGIF, true},
		{"mp4", Input{FileName: "converted.mp4", Mode: "convert"}, KindVideo, true},
		{"mp3", Input{FileName: "audio.mp3"}, KindAudio, true},
		{"srt", Input{FileName: "translated.srt"}, KindCaptions, true},
		{"pdf", Input{FileName: "converted.pdf"}, KindPDF, true},
		{"txt transcript", Input{FileName: "transcript.txt", Mode: "transcribe"}, KindTranscript, true},
		{"txt without transcribe is unknown", Input{FileName: "x.txt"}, KindUnknown, false},
		{"zip", Input{FileName: "frames.zip"}, KindArchive, true},
		{"tar.gz transcode is hls", Input{FileName: "converted.tar.gz", Mode: "transcode"}, KindHLS, true},
		{"tar.gz dash", Input{FileName: "converted.tar.gz", Mode: "transcode", Tool: "transcode_to_dash"}, KindDASH, true},
		{"mime fallback", Input{FileName: "converted", Mime: "image/webp"}, KindImage, true},
		{"nothing known", Input{FileName: "converted.bin", Mime: "application/octet-stream"}, KindUnknown, false},
		{"metadata report by mode", Input{FileName: "report.json", Mode: "inspect_metadata"}, KindMetadata, false},
		{"metadata report by tool", Input{FileName: "report.json", Tool: "metadata"}, KindMetadata, false},
		{"darkroom export is never shareable", Input{FileName: "converted.mp4", Mode: "studio_export"}, KindVideo, false},
		{"darkroom by tool", Input{FileName: "x.png", Tool: "studio_derive"}, KindImage, false},
	}
	for _, tc := range cases {
		k := KindOf(tc.in)
		if k != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.name, k, tc.kind)
		}
		if got := Shareable(tc.in, k); got != tc.share {
			t.Errorf("%s: shareable = %v, want %v", tc.name, got, tc.share)
		}
	}
}
