package services

import (
	"strings"
	"testing"
)

func TestParseSRT_Basic(t *testing.T) {
	body := `1
00:00:00,000 --> 00:00:01,500
Hello world

2
00:00:01,500 --> 00:00:03,000
Second cue
`
	segs, err := ParseSRT(body)
	if err != nil {
		t.Fatalf("ParseSRT: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if segs[0].Text != "Hello world" {
		t.Fatalf("segment[0].Text = %q", segs[0].Text)
	}
	if segs[1].Start != 1.5 || segs[1].End != 3.0 {
		t.Fatalf("segment[1] times = %v..%v", segs[1].Start, segs[1].End)
	}
}

func TestParseSRT_CRLFAndPeriodDecimal(t *testing.T) {
	body := "1\r\n00:00:00.250 --> 00:00:01.000\r\nFirst\r\n\r\n2\r\n00:00:02.000 --> 00:00:03.000\r\nSecond\r\n"
	segs, err := ParseSRT(body)
	if err != nil {
		t.Fatalf("ParseSRT: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (CRLF + period decimal must be tolerated)", len(segs))
	}
	if segs[0].Start != 0.25 {
		t.Fatalf("segment[0].Start = %v, want 0.25", segs[0].Start)
	}
}

func TestWriteSRT_Roundtrip(t *testing.T) {
	in := []TranslateCaptionsSegment{
		{ID: 0, Start: 0.0, End: 1.5, Text: "Hi"},
		{ID: 1, Start: 2.5, End: 4.0, Text: "There"},
	}
	out := WriteSRT(in)
	parsed, err := ParseSRT(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("roundtrip lost segments: %d", len(parsed))
	}
	if parsed[0].Text != "Hi" || parsed[1].Text != "There" {
		t.Fatalf("roundtrip mangled text: %q / %q", parsed[0].Text, parsed[1].Text)
	}
	if parsed[1].Start != 2.5 || parsed[1].End != 4.0 {
		t.Fatalf("roundtrip mangled timing: %v..%v", parsed[1].Start, parsed[1].End)
	}
}

func TestParseVTT_StripsNoteBlocks(t *testing.T) {
	body := `WEBVTT

NOTE This is a comment that must not be translated.

1
00:00:00.000 --> 00:00:01.500
Cue one

2
00:00:01.500 --> 00:00:03.000
Cue two
`
	segs, err := ParseVTT(body)
	if err != nil {
		t.Fatalf("ParseVTT: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d cues, want 2", len(segs))
	}
	for _, s := range segs {
		if strings.Contains(s.Text, "comment") {
			t.Fatalf("NOTE block leaked into a cue: %q", s.Text)
		}
	}
}

func TestWriteVTT_HasHeader(t *testing.T) {
	segs := []TranslateCaptionsSegment{{ID: 0, Start: 0, End: 1, Text: "Hi"}}
	body := WriteVTT(segs)
	if !strings.HasPrefix(body, "WEBVTT\n\n") {
		t.Fatalf("WriteVTT did not produce WEBVTT header; got:\n%s", body)
	}
	if !strings.Contains(body, "00:00:00.000 --> 00:00:01.000") {
		t.Fatalf("WriteVTT did not emit period-decimal timestamps; got:\n%s", body)
	}
}

func TestValidateCaptionTranslatorRequest_Rejects(t *testing.T) {
	bad := PrepareCaptionJobRequest{
		FileSize:    0,
		InputFormat: "srt",
		TargetLanguage: "en",
	}
	if err := ValidateCaptionTranslatorRequest(bad); err == nil {
		t.Fatalf("expected empty-file rejection")
	}
	too_big := PrepareCaptionJobRequest{
		FileSize:    CaptionTranslatorMaxBytes + 1,
		InputFormat: "srt",
		TargetLanguage: "en",
	}
	if err := ValidateCaptionTranslatorRequest(too_big); err == nil {
		t.Fatalf("expected max-size rejection")
	}
	bad_format := PrepareCaptionJobRequest{
		FileSize:    100,
		InputFormat: "docx",
		TargetLanguage: "en",
	}
	if err := ValidateCaptionTranslatorRequest(bad_format); err == nil {
		t.Fatalf("expected format rejection")
	}
	missing_target := PrepareCaptionJobRequest{
		FileSize:    100,
		InputFormat: "srt",
	}
	if err := ValidateCaptionTranslatorRequest(missing_target); err == nil {
		t.Fatalf("expected missing-target rejection")
	}
}

func TestDetectCaptionFormatByExtension(t *testing.T) {
	cases := map[string]string{
		"foo.srt":  "srt",
		"foo.SRT":  "srt",
		"foo.vtt":  "vtt",
		"foo.webvtt": "vtt",
		"foo.txt":  "",
		"":         "",
	}
	for name, want := range cases {
		if got := DetectCaptionFormatByExtension(name); got != want {
			t.Fatalf("DetectCaptionFormatByExtension(%q) = %q, want %q", name, got, want)
		}
	}
}
