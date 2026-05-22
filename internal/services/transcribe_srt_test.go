package services

import (
	"strings"
	"testing"
)

func TestNormalizeTranscribeFormat_SRT(t *testing.T) {
	cases := map[string]string{
		"srt":    "srt",
		"SRT":    "srt",
		"subrip": "srt",
		"vtt":    "vtt",
		"json":   "json",
		"txt":    "txt",
		"":       "",
		"docx":   "",
	}
	for in, want := range cases {
		if got := normalizeTranscribeFormat(in); got != want {
			t.Fatalf("normalizeTranscribeFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatSRTStamp(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "00:00:00,000"},
		{1.234, "00:00:01,234"},
		{61.5, "00:01:01,500"},
		{3661.999, "01:01:01,999"},
		{-1, "00:00:00,000"},
	}
	for _, tc := range cases {
		if got := formatSRTStamp(tc.in); got != tc.want {
			t.Fatalf("formatSRTStamp(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildSRT_HappyPath(t *testing.T) {
	result := &TranscribeResult{HasAudio: true, HasSpeech: true}
	segments := []whisperSegment{
		{ID: 0, Start: 0, End: 1.5, Text: "Hello world"},
		{ID: 1, Start: 1.5, End: 3.0, Text: "  Second cue  "},
	}
	got := buildSRT(result, segments)
	want := "1\n00:00:00,000 --> 00:00:01,500\nHello world\n\n2\n00:00:01,500 --> 00:00:03,000\nSecond cue\n\n"
	if got != want {
		t.Fatalf("buildSRT mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildSRT_EmptySegmentsFallsBackToMessage(t *testing.T) {
	result := &TranscribeResult{HasAudio: true, Message: "No speech detected", DurationSeconds: 10}
	got := buildSRT(result, nil)
	if !strings.Contains(got, "1\n00:00:00,000 --> 00:00:10,000\nNo speech detected") {
		t.Fatalf("buildSRT fallback did not include single-cue message; got:\n%s", got)
	}
}

func TestBuildSRT_EmptySegmentsAndNoMessageReturnsEmpty(t *testing.T) {
	result := &TranscribeResult{HasAudio: false}
	if got := buildSRT(result, nil); got != "" {
		t.Fatalf("buildSRT with no segments and no message should be empty; got: %q", got)
	}
}
