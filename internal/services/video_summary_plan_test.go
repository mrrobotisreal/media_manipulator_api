package services

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func assertSortedInside(t *testing.T, got []float64, duration float64) {
	t.Helper()
	for i, v := range got {
		if v < 0 || v >= duration {
			t.Fatalf("frame %d at %.3f is outside [0,%.1f): %v", i, v, duration, got)
		}
		if i > 0 && v <= got[i-1] {
			t.Fatalf("frames not strictly increasing: %v", got)
		}
	}
}

func minGap(ts []float64) float64 {
	g := math.Inf(1)
	for i := 1; i < len(ts); i++ {
		g = math.Min(g, ts[i]-ts[i-1])
	}
	return g
}

func TestFrameFloor(t *testing.T) {
	cases := []struct {
		dur    float64
		budget int
		want   float64
	}{
		{600, 24, 25},  // duration / budget wins
		{30, 24, 2},    // 1.25 s would be below the 2 s floor
		{48, 24, 2},    // exactly 2
		{4800, 96, 50}, // pro budget, long video
		{100, 0, 2},    // no budget -> floor only
	}
	for _, tc := range cases {
		if got := FrameFloor(tc.dur, tc.budget); got != tc.want {
			t.Errorf("FrameFloor(%v,%d) = %v want %v", tc.dur, tc.budget, got, tc.want)
		}
	}
}

func TestPlanFramesNoCutsFallsBackToEvenInterval(t *testing.T) {
	// One static 10-minute shot, budget 24: floor 25 s, 24 frames at slice centres.
	got := PlanFrames(600, nil, 24)
	if len(got) != 24 {
		t.Fatalf("want 24 frames, got %d: %v", len(got), got)
	}
	if got[0] != 12.5 || got[23] != 587.5 {
		t.Fatalf("want slice centres 12.5..587.5, got %v..%v", got[0], got[23])
	}
	for i := 1; i < len(got); i++ {
		if d := got[i] - got[i-1]; math.Abs(d-25) > 1e-9 {
			t.Fatalf("even interval broken at %d: %v", i, d)
		}
	}
	// Cuts that are all outside the source count as no cuts.
	if again := PlanFrames(600, []float64{0, 600, 700, -3}, 24); !reflect.DeepEqual(again, got) {
		t.Fatalf("out-of-range cuts must be ignored: %v", again)
	}
}

func TestPlanFramesFloorOfTwoSeconds(t *testing.T) {
	// 30 s clip with a cut every 0.5 s: floor is max(2, 30/24)=2 s, so no two
	// kept frames are closer than 2 s.
	var cuts []float64
	for t := 0.5; t < 30; t += 0.5 {
		cuts = append(cuts, t)
	}
	got := PlanFrames(30, cuts, 24)
	assertSortedInside(t, got, 30)
	if g := minGap(got); g < 2-1e-9 {
		t.Fatalf("frames closer than the 2 s floor (%.3f): %v", g, got)
	}
	if len(got) > 15 {
		t.Fatalf("a 30 s clip cannot hold more than 15 frames 2 s apart, got %d", len(got))
	}
}

func TestPlanFramesFloorIsDurationOverBudget(t *testing.T) {
	// 20 minutes, budget 24: floor 50 s; cuts every 10 s collapse to one per 50 s.
	var cuts []float64
	for t := 10.0; t < 1200; t += 10 {
		cuts = append(cuts, t)
	}
	got := PlanFrames(1200, cuts, 24)
	assertSortedInside(t, got, 1200)
	if g := minGap(got); g < 50-1e-9 {
		t.Fatalf("frames closer than duration/budget=50 s (%.3f): %v", g, got)
	}
	if len(got) > 24 {
		t.Fatalf("over budget: %d", len(got))
	}
}

func TestPlanFramesManyCutsOverBudgetKeepsEvenSubset(t *testing.T) {
	// 100 s, budget 5 (floor 20 s). Cuts are dense in the first half and sparse
	// later; after the floor the candidates exceed the budget, and the kept
	// set must follow the timeline evenly (one per 20 s slice), not the cuts.
	cuts := []float64{2, 21, 22, 41, 43, 61, 81, 95}
	got := PlanFrames(100, cuts, 5)
	if len(got) != 5 {
		t.Fatalf("want exactly the budget (5), got %d: %v", len(got), got)
	}
	assertSortedInside(t, got, 100)
	for i, v := range got {
		lo, hi := float64(i)*20, float64(i+1)*20
		if v < lo-5 || v > hi+5 {
			t.Fatalf("frame %d at %.1f is not near slice [%v,%v): %v", i, v, lo, hi, got)
		}
	}
}

func TestEvenSubsetPicksNearestToSliceCentres(t *testing.T) {
	cands := []float64{1, 2, 3, 4, 50, 51, 98, 99}
	got := evenSubset(cands, 100, 3) // centres 16.7, 50, 83.3
	want := []float64{4, 50, 98}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestPlanFramesBudgetLargerThanCandidates(t *testing.T) {
	// 60 s, budget 96 (pro): floor 2 s. Two cuts; the long static stretches
	// between them get fill frames, but never closer than the floor, and the
	// total is far below the budget.
	got := PlanFrames(60, []float64{20, 40}, 96)
	assertSortedInside(t, got, 60)
	if g := minGap(got); g < 2-1e-9 {
		t.Fatalf("fill frames closer than the floor: %v", got)
	}
	if len(got) >= 96 || len(got) < 3 {
		t.Fatalf("unexpected count %d: %v", len(got), got)
	}
	for _, cut := range []float64{20, 40} {
		found := false
		for _, v := range got {
			if v == cut {
				found = true
			}
		}
		if !found {
			t.Fatalf("cut at %v must be kept when under budget: %v", cut, got)
		}
	}
}

func TestPlanFramesFillsLongStaticStretch(t *testing.T) {
	// 10 min, budget 24 (floor 25 s): one cut at 30 s, then nothing for 9.5 min.
	// The tail must still be sampled, not left as a single frame.
	got := PlanFrames(600, []float64{30}, 24)
	assertSortedInside(t, got, 600)
	if got[len(got)-1] < 500 {
		t.Fatalf("the last 100 s were not sampled: %v", got)
	}
	if len(got) > 24 {
		t.Fatalf("over budget: %d", len(got))
	}
}

func TestPlanFramesVeryShortClip(t *testing.T) {
	// 1.5 s clip: one frame, inside the clip.
	if got := PlanFrames(1.5, nil, 24); len(got) != 1 || got[0] != 0.75 {
		t.Fatalf("1.5 s, no cuts: %v", got)
	}
	// 3 s clip with a cut at 1.2 s: opening at 1.0, the cut is under the 2 s floor.
	got := PlanFrames(3, []float64{1.2}, 24)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("3 s with an early cut: %v", got)
	}
}

func TestPlanFramesDegenerateInputs(t *testing.T) {
	if got := PlanFrames(100, []float64{10}, 0); got != nil {
		t.Fatalf("budget 0 (anonymous) must plan nothing: %v", got)
	}
	if got := PlanFrames(0, []float64{10}, 24); got != nil {
		t.Fatalf("unknown duration: %v", got)
	}
	if got := PlanFrames(math.NaN(), nil, 24); got != nil {
		t.Fatalf("NaN duration: %v", got)
	}
}

func frames(ts ...float64) []Frame {
	out := make([]Frame, len(ts))
	for i, v := range ts {
		out[i] = Frame{Path: frameFileName(v), TimeSec: v}
	}
	return out
}

func TestBatchFramesTileTheTimeline(t *testing.T) {
	b := BatchFrames(frames(5, 15, 25, 35, 45, 55), 4, 60)
	if len(b) != 2 {
		t.Fatalf("want 2 batches, got %d", len(b))
	}
	if b[0].Start != 0 || b[0].End != 45 || len(b[0].Frames) != 4 {
		t.Fatalf("batch 0: %+v", b[0])
	}
	if b[1].Start != 45 || b[1].End != 60 || len(b[1].Frames) != 2 {
		t.Fatalf("batch 1: %+v", b[1])
	}
	if got := BatchFrames(nil, 4, 60); got != nil {
		t.Fatalf("no frames: %v", got)
	}
}

func seg(start, end float64, text string) models.VideoSummaryTranscriptSegment {
	return models.VideoSummaryTranscriptSegment{Start: start, End: end, Text: text}
}

func TestTranscriptWindowOnlyOverlappingSegments(t *testing.T) {
	segs := []models.VideoSummaryTranscriptSegment{
		seg(0, 4, "before"),
		seg(8, 12, "straddles start"),
		seg(12, 20, "inside"),
		seg(28, 33, "straddles end"),
		seg(40, 45, "after"),
	}
	got := TranscriptWindow(segs, 10, 30)
	var texts []string
	for _, s := range got {
		texts = append(texts, s.Text)
	}
	want := []string{"straddles start", "inside", "straddles end"}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("got %v want %v", texts, want)
	}
}

func TestTranscriptWindowBoundariesAndEmpty(t *testing.T) {
	segs := []models.VideoSummaryTranscriptSegment{
		seg(0, 10, "ends exactly at start"),
		seg(10, 20, "fills the window"),
		seg(20, 25, "starts exactly at end"),
		seg(12, 14, "   "),
	}
	got := TranscriptWindow(segs, 10, 20)
	if len(got) != 1 || got[0].Text != "fills the window" {
		t.Fatalf("boundary segments must go to the neighbouring batch: %+v", got)
	}
	if got := TranscriptWindow(segs, 30, 40); len(got) != 0 {
		t.Fatalf("empty window must be empty: %+v", got)
	}
	if got := TranscriptWindow(nil, 0, 10); len(got) != 0 {
		t.Fatalf("no transcript: %+v", got)
	}
}

func TestFormatClock(t *testing.T) {
	for in, want := range map[float64]string{0: "0:00", 59.9: "0:59", 61: "1:01", 3600: "1:00:00", 3725.4: "1:02:05", -1: "0:00"} {
		if got := FormatClock(in); got != want {
			t.Errorf("FormatClock(%v) = %q want %q", in, got, want)
		}
	}
}

const validSummaryJSON = `{
  "title": "Building a birdhouse from one pine board",
  "summary": "A woodworker builds a birdhouse from a single pine board. She measures and cuts six panels. The panels are glued and nailed together. A hinged roof makes cleaning easy. The finished house is mounted on a post.",
  "chapters": [
    {"start": 0, "end": 40, "title": "Materials", "description": "The board and tools."},
    {"start": 40, "end": "1:50", "title": "Cutting", "description": "Six panels are cut."},
    {"start": "1:50", "end": 180, "title": "Assembly", "description": "Glue, nails and the roof."}
  ],
  "topics": ["woodworking", "Woodworking", "birdhouse"],
  "onScreenText": [{"text": "Pine 1x6", "firstSeen": 12}, {"text": "pine  1x6", "firstSeen": 4}, "Step 2"],
  "visual": "A garage workshop.",
  "speech": "One calm narrator.",
  "language": "en",
  "caveats": []
}`

func TestParseVideoSummaryValid(t *testing.T) {
	s, err := ParseVideoSummary([]byte(validSummaryJSON), 180)
	if err != nil {
		t.Fatal(err)
	}
	if float64(s.Chapters[1].End) != 110 || float64(s.Chapters[2].Start) != 110 {
		t.Fatalf("clock-string timestamps not parsed: %+v", s.Chapters)
	}
	models.NormalizeVideoSummary(s, 180)
	if len(s.Topics) != 2 {
		t.Fatalf("topics not deduplicated: %v", s.Topics)
	}
	if len(s.OnScreenText) != 2 || s.OnScreenText[0].Text != "Step 2" || float64(s.OnScreenText[1].FirstSeen) != 4 {
		t.Fatalf("onScreenText not deduplicated to earliest firstSeen: %+v", s.OnScreenText)
	}
}

func TestParseVideoSummaryRejectsChapterBounds(t *testing.T) {
	base := func() map[string]any {
		var m map[string]any
		_ = json.Unmarshal([]byte(validSummaryJSON), &m)
		return m
	}
	cases := map[string]func(m map[string]any){
		"chapter past the duration": func(m map[string]any) {
			m["chapters"].([]any)[2].(map[string]any)["end"] = 400.0
		},
		"chapter starting after the end": func(m map[string]any) {
			m["chapters"].([]any)[2].(map[string]any)["start"] = 190.0
			m["chapters"].([]any)[2].(map[string]any)["end"] = 200.0
		},
		"non-monotonic starts": func(m map[string]any) {
			m["chapters"].([]any)[1].(map[string]any)["start"] = 0.0
		},
		"end before start": func(m map[string]any) {
			m["chapters"].([]any)[0].(map[string]any)["end"] = 0.0
		},
		"negative start": func(m map[string]any) {
			m["chapters"].([]any)[0].(map[string]any)["start"] = -5.0
		},
		"too few chapters": func(m map[string]any) {
			m["chapters"] = m["chapters"].([]any)[:2]
		},
		"too many chapters": func(m map[string]any) {
			var ch []any
			for i := 0; i < 13; i++ {
				ch = append(ch, map[string]any{"start": float64(i * 10), "end": float64(i*10 + 10), "title": "c"})
			}
			m["chapters"] = ch
		},
		"missing title": func(m map[string]any) { m["title"] = "" },
		"one-sentence summary": func(m map[string]any) {
			m["summary"] = "Just one sentence."
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := base()
			mutate(m)
			raw, _ := json.Marshal(m)
			if _, err := ParseVideoSummary(raw, 180); err == nil {
				t.Fatalf("expected a schema error")
			}
		})
	}
	if _, err := ParseVideoSummary([]byte(`{"title": 5}`), 180); err == nil {
		t.Fatal("a wrong type must be a schema error")
	}
}

func TestParseVideoSummaryShortClipAllowsOneChapter(t *testing.T) {
	raw := `{"title":"A cat jumps","summary":"A cat jumps onto a sofa.","chapters":[{"start":0,"end":12,"title":"The jump","description":""}],"topics":[],"onScreenText":[],"visual":"","speech":"","language":"en","caveats":[]}`
	if _, err := ParseVideoSummary([]byte(raw), 12); err != nil {
		t.Fatalf("a 12 s clip may have one chapter and one sentence: %v", err)
	}
	if _, err := ParseVideoSummary([]byte(raw), 300); err == nil {
		t.Fatal("a 5-minute video needs at least three chapters")
	}
}

func TestNormalizeVideoSummaryLimits(t *testing.T) {
	s := &models.VideoSummary{
		Title:   strings.Repeat("Long words in a title ", 10),
		Summary: "One. Two. Three. Four. Five. Six. Seven. Eight.",
		Chapters: []models.VideoSummaryChapter{
			{Start: 0, End: 50, Title: "a"}, {Start: 50, End: 101, Title: "b"},
		},
	}
	models.NormalizeVideoSummary(s, 100)
	if n := len([]rune(s.Title)); n > 80 {
		t.Fatalf("title %d runes", n)
	}
	if got := models.CountSentences(s.Summary); got != 6 {
		t.Fatalf("summary capped at 6 sentences, got %d: %q", got, s.Summary)
	}
	if float64(s.Chapters[1].End) != 100 {
		t.Fatalf("last chapter clamped to the duration: %v", s.Chapters[1].End)
	}
	if out, _ := json.Marshal(s); !strings.Contains(string(out), `"topics":[]`) || !strings.Contains(string(out), `"caveats":[]`) {
		t.Fatalf("empty lists must marshal as []: %s", out)
	}
}

func TestCountSentences(t *testing.T) {
	cases := map[string]int{
		"":                                    0,
		"One sentence without a stop":         1,
		"Version 3.5 shipped. It is fast.":    2,
		`He said "stop." Then he left!`:       2,
		"これは猫です。猫が走る。":                        2,
		"Первое. Второе? Третье!":             3,
		"See https://example.com/a.b for it.": 1,
	}
	for in, want := range cases {
		if got := models.CountSentences(in); got != want {
			t.Errorf("CountSentences(%q) = %d want %d", in, got, want)
		}
	}
}

func TestFlexDecoding(t *testing.T) {
	var n models.VideoSummaryBatchNote
	raw := `{"scene":["kitchen","morning"],"onScreenText":"OPEN","people":[{"name":"chef"},"guest"],"objects":null,"actions":[1,"stirs"],"cameraNotes":"handheld"}`
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatal(err)
	}
	if n.Scene != "kitchen; morning" || !reflect.DeepEqual([]string(n.OnScreenText), []string{"OPEN"}) ||
		!reflect.DeepEqual([]string(n.People), []string{"chef", "guest"}) || len(n.Objects) != 0 ||
		!reflect.DeepEqual([]string(n.Actions), []string{"1", "stirs"}) {
		t.Fatalf("lenient decode: %+v", n)
	}
}

func TestNormalizeVideoSummaryLanguage(t *testing.T) {
	for in, want := range map[string]string{"": "auto", "AUTO": "auto", "de-DE": "de-DE", "de-de": "de-DE", "de": "de-DE", "pt": "pt-BR", "es-MX": "es-MX", "zh": "zh-CN"} {
		got, err := models.NormalizeVideoSummaryLanguage(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v want %q", in, got, err, want)
		}
	}
	if _, err := models.NormalizeVideoSummaryLanguage("xx-YY"); err == nil {
		t.Fatal("unknown language must be refused")
	}
	if langs := models.VideoSummaryOutputLanguages(); len(langs) != 19 || langs[0] != "auto" {
		t.Fatalf("auto + 18 locales expected: %v", langs)
	}
}

func note(start, end float64, scene string, text ...string) models.VideoSummaryBatchNote {
	return models.VideoSummaryBatchNote{Start: start, End: end, FrameTimes: []float64{start}, Scene: models.FlexString(scene), OnScreenText: text}
}

func TestBuildSynthesisInputFitsWithoutDropping(t *testing.T) {
	notes := []models.VideoSummaryBatchNote{note(0, 30, "a kitchen", "MENU"), note(30, 60, "a street")}
	segs := []models.VideoSummaryTranscriptSegment{seg(1, 4, "Hello and welcome."), seg(31, 35, "Now we go outside.")}
	in := BuildSynthesisInput(60, notes, segs, 12000)
	if in.NotesDropped != 0 || in.TranscriptCut || len(in.Caveats) != 0 {
		t.Fatalf("nothing should be reduced: %+v", in)
	}
	for _, want := range []string{"Source duration: 60.0 seconds", "[0:00-0:30] scene: a kitchen; on-screen text: MENU", "[0:31] Now we go outside."} {
		if !strings.Contains(in.Text, want) {
			t.Fatalf("input missing %q:\n%s", want, in.Text)
		}
	}
}

func TestBuildSynthesisInputDropsLeastInformativeNotesFirst(t *testing.T) {
	long := strings.Repeat("word ", 40)
	notes := []models.VideoSummaryBatchNote{
		note(0, 10, long, "TITLE CARD", "SUBSCRIBE"), // most informative
		note(10, 20, long),                           // scene only
		{Start: 20, End: 30},                         // empty: dropped before anything else
		note(30, 40, long, "PRICE $5"),
	}
	full := BuildSynthesisInput(40, notes, nil, 100000)
	budget := full.EstimatedToken - 40 // force exactly one informative drop
	in := BuildSynthesisInput(40, notes, nil, budget)
	if strings.Contains(in.Text, "[0:10-0:20]") {
		t.Fatalf("the scene-only note should be dropped first:\n%s", in.Text)
	}
	if !strings.Contains(in.Text, "TITLE CARD") || !strings.Contains(in.Text, "PRICE $5") {
		t.Fatalf("notes with on-screen text must survive:\n%s", in.Text)
	}
	if in.TranscriptCut {
		t.Fatal("transcript must not be touched before notes are dropped")
	}
	if len(in.Caveats) != 1 || in.Caveats[0] != CaveatNotesDropped {
		t.Fatalf("caveat expected: %v", in.Caveats)
	}
}

func TestBuildSynthesisInputCondensesTranscriptAfterNotes(t *testing.T) {
	var segs []models.VideoSummaryTranscriptSegment
	for i := 0; i < 600; i++ {
		segs = append(segs, seg(float64(i*5), float64(i*5+5), "this is a fairly long spoken sentence number "+strings.Repeat("x", i%7)))
	}
	notes := []models.VideoSummaryBatchNote{note(0, 1500, "a lecture hall", "SLIDE 1")}
	in := BuildSynthesisInput(3000, notes, segs, 2000)
	if in.EstimatedToken > 2000 {
		t.Fatalf("input over budget: %d", in.EstimatedToken)
	}
	if !in.TranscriptCut {
		t.Fatal("transcript should be condensed")
	}
	if in.NotesKept != 0 {
		t.Fatalf("notes are dropped before the transcript is condensed (kept %d)", in.NotesKept)
	}
	found := false
	for _, c := range in.Caveats {
		if c == CaveatTranscriptCondensed {
			found = true
		}
	}
	if !found {
		t.Fatalf("condensed caveat missing: %v", in.Caveats)
	}
}

func TestEstimateTokensIsConservativeForNonLatin(t *testing.T) {
	if EstimateTokens("abcd") != 1 {
		t.Fatal("ascii at 4 chars/token")
	}
	if EstimateTokens("привет") < 4 {
		t.Fatal("cyrillic must count more than ascii")
	}
}

func TestOnScreenTextFromNotesDedupesToFirstSeen(t *testing.T) {
	notes := []models.VideoSummaryBatchNote{
		{Start: 0, End: 20, FrameTimes: []float64{3, 9}, OnScreenText: []string{"SALE", "Open 9-5"}},
		{Start: 20, End: 40, FrameTimes: []float64{22}, OnScreenText: []string{"sale", "Exit"}},
	}
	got := OnScreenTextFromNotes(notes)
	if len(got) != 3 || got[0].Text != "SALE" || float64(got[0].FirstSeen) != 3 || got[2].Text != "Exit" {
		t.Fatalf("got %+v", got)
	}
}

// Review finding: duration/(duration/budget) can round below budget.
func TestPlanFramesEvenIntervalNoRoundingLoss(t *testing.T) {
	for _, tc := range []struct {
		dur    float64
		budget int
	}{{18, 7}, {100, 3}, {7199, 96}, {600, 24}} {
		got := PlanFrames(tc.dur, nil, tc.budget)
		want := tc.budget
		if int(tc.dur/2) < want {
			want = int(tc.dur / 2)
		}
		if len(got) != want {
			t.Errorf("PlanFrames(%v, nil, %d) = %d frames, want %d", tc.dur, tc.budget, len(got), want)
		}
	}
}
