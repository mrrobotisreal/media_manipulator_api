package services

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func seqIDGen() func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("c%d", n) }
}

func TestSegmentsToCues_ShortSegment(t *testing.T) {
	cues := SegmentsToCues([]TranscribeSegmentDTO{{Start: 1, End: 3, Text: "  Hello world  "}}, seqIDGen())
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d", len(cues))
	}
	if cues[0].Text != "Hello world" || cues[0].StartSeconds != 1 || cues[0].EndSeconds != 3 {
		t.Errorf("unexpected cue: %+v", cues[0])
	}
}

func TestSegmentsToCues_SplitsLong(t *testing.T) {
	long := strings.Repeat("word ", 40) // ~200 chars, well over 84
	cues := SegmentsToCues([]TranscribeSegmentDTO{{Start: 0, End: 10, Text: long}}, seqIDGen())
	if len(cues) < 2 {
		t.Fatalf("expected the long segment to split, got %d cues", len(cues))
	}
	// Times are contiguous and span the segment.
	if cues[0].StartSeconds != 0 {
		t.Errorf("first cue should start at 0, got %v", cues[0].StartSeconds)
	}
	if last := cues[len(cues)-1]; last.EndSeconds != 10 {
		t.Errorf("last cue should end at 10, got %v", last.EndSeconds)
	}
	for _, c := range cues {
		if len([]rune(c.Text)) > studioMaxCueChars+8 {
			t.Errorf("cue too long after split: %q", c.Text)
		}
	}
}

func TestBuildASS_Golden(t *testing.T) {
	style := &models.StudioCaptionStyle{
		FontSizePct: 5, Color: "#FFFFFF", BackgroundColor: "#000000", BackgroundOpacity: 0.5, Position: "bottom", MaxWidthPct: 90,
	}
	cues := []models.StudioCaptionCue{
		{ID: "c1", StartSeconds: 1.5, EndSeconds: 3.25, Text: "Hello {world}"},
		{ID: "c2", StartSeconds: 65.0, EndSeconds: 67.0, Text: "Line one\nLine two"},
	}
	ass := BuildASS(cues, style, 1920, 1080, "Sans")

	for _, want := range []string{
		"[Script Info]",
		"PlayResX: 1920",
		"PlayResY: 1080",
		"Style: Default,Sans,54,&H00FFFFFF,&H000000FF,&H00000000,&H80000000,", // 5% of 1080 = 54; bg alpha 0.5 → round(127.5)=0x80
		"Dialogue: 0,0:00:01.50,0:00:03.25,Default,,0,0,0,,Hello (world)",
		"Dialogue: 0,0:01:05.00,0:01:07.00,Default,,0,0,0,,Line one\\NLine two",
	} {
		if !strings.Contains(ass, want) {
			t.Errorf("ASS missing %q\n---\n%s", want, ass)
		}
	}
}

func TestASSTimestamp(t *testing.T) {
	cases := map[float64]string{0: "0:00:00.00", 1.5: "0:00:01.50", 65: "0:01:05.00", 3661.25: "1:01:01.25"}
	for in, want := range cases {
		if got := assTimestamp(in); got != want {
			t.Errorf("assTimestamp(%v) = %q, want %q", in, got, want)
		}
	}
}
