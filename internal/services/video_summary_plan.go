package services

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Pure planning for the video summary pipeline (MM74). Everything in this file
// is deterministic and side-effect free so the sampling math, the batch
// windows and the input budget can be table-tested without ffmpeg or Ollama.

const (
	// summaryMinFrameGap is the hard floor between two kept frames (design §3:
	// "a floor of max(2 s, duration / budget)").
	summaryMinFrameGap = 2.0
	// summarySceneThreshold is ffmpeg's scene score above which a frame is a cut.
	summarySceneThreshold = 0.30
	// summaryOpeningFrameSec: the opening frame is taken this far in (or at the
	// midpoint of a shorter clip), past a fade from black.
	summaryOpeningFrameSec = 1.0
	// summaryVisionBatchSize is how many frames one VLM call sees (design §3).
	summaryVisionBatchSize = 4
)

// FrameFloor is the minimum spacing between kept frames for a source of this
// duration and frame budget: max(2 s, duration / budget).
func FrameFloor(durationSec float64, budget int) float64 {
	if budget <= 0 || durationSec <= 0 {
		return summaryMinFrameGap
	}
	return math.Max(summaryMinFrameGap, durationSec/float64(budget))
}

// PlanFrames decides which timestamps to extract for the vision pass.
//
//  1. floor = max(2 s, duration / budget).
//  2. No usable cuts (one static shot): the even interval — n = min(budget,
//     duration / floor) frames at the centres of n equal slices.
//  3. Otherwise: an opening frame, then every cut that is at least `floor`
//     after the previous keep (bursts of cuts collapse to one frame), then any
//     stretch longer than 2 × floor without a keep (including the tail) gets
//     evenly spaced fill frames, so a long static stretch between two cuts is
//     still seen.
//  4. Over budget: the evenly spaced subset (by time) of the kept frames.
//
// Returned timestamps are sorted, unique, inside [0, duration) and rounded to
// milliseconds. budget <= 0 or an unknown duration returns nil.
func PlanFrames(durationSec float64, sceneTimes []float64, budget int) []float64 {
	if budget <= 0 || durationSec <= 0 || math.IsNaN(durationSec) || math.IsInf(durationSec, 0) {
		return nil
	}
	floor := FrameFloor(durationSec, budget)

	cuts := make([]float64, 0, len(sceneTimes))
	for _, t := range sceneTimes {
		if t > 0 && t < durationSec && !math.IsNaN(t) {
			cuts = append(cuts, t)
		}
	}
	sort.Float64s(cuts)

	if len(cuts) == 0 {
		return evenIntervalTimes(durationSec, floor, budget)
	}

	opening := math.Min(summaryOpeningFrameSec, durationSec/2)
	keeps := []float64{opening}
	for _, c := range cuts {
		if c-keeps[len(keeps)-1] >= floor {
			keeps = append(keeps, c)
		}
	}
	keeps = fillGaps(keeps, durationSec, floor)
	if len(keeps) > budget {
		keeps = evenSubset(keeps, durationSec, budget)
	}
	return roundTimes(keeps)
}

// evenIntervalTimes is the no-cuts fallback: n frames at slice centres.
func evenIntervalTimes(durationSec, floor float64, budget int) []float64 {
	n := int(durationSec / floor)
	if n < 1 {
		n = 1
	}
	if n > budget {
		n = budget
	}
	out := make([]float64, n)
	step := durationSec / float64(n)
	for i := range out {
		out[i] = (float64(i) + 0.5) * step
	}
	return roundTimes(out)
}

// fillGaps inserts evenly spaced keeps into any stretch longer than 2 × floor
// (between keeps, and from the last keep to the end of the source). A stretch
// of length g gets floor(g/floor)-1 fill frames, so the spacing never drops
// below the floor and the last fill frame of the tail stays a slice short of
// the end.
func fillGaps(keeps []float64, durationSec, floor float64) []float64 {
	out := make([]float64, 0, len(keeps))
	for i, a := range keeps {
		b := durationSec
		if i+1 < len(keeps) {
			b = keeps[i+1]
		}
		out = append(out, a)
		gap := b - a
		if gap <= 2*floor {
			continue
		}
		m := int(gap/floor) - 1
		step := gap / float64(m+1)
		for j := 1; j <= m; j++ {
			out = append(out, a+float64(j)*step)
		}
	}
	return out
}

// evenSubset picks budget timestamps from sorted candidates: for each of
// budget evenly spaced target times (slice centres) the nearest unused
// candidate. Coverage follows the timeline, not the density of cuts.
func evenSubset(candidates []float64, durationSec float64, budget int) []float64 {
	if budget <= 0 || len(candidates) == 0 {
		return nil
	}
	if len(candidates) <= budget {
		return append([]float64{}, candidates...)
	}
	used := make([]bool, len(candidates))
	out := make([]float64, 0, budget)
	step := durationSec / float64(budget)
	for i := 0; i < budget; i++ {
		target := (float64(i) + 0.5) * step
		best := -1
		for j, c := range candidates {
			if used[j] {
				continue
			}
			if best < 0 || math.Abs(c-target) < math.Abs(candidates[best]-target) {
				best = j
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		out = append(out, candidates[best])
	}
	sort.Float64s(out)
	return out
}

func roundTimes(in []float64) []float64 {
	out := make([]float64, 0, len(in))
	for _, t := range in {
		r := math.Round(t*1000) / 1000
		if len(out) > 0 && r <= out[len(out)-1] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// FrameBatch is one VLM call: up to four frames and the time window whose
// transcript lines go with them.
type FrameBatch struct {
	Frames []Frame
	Start  float64
	End    float64
}

// BatchFrames groups frames into batches of size and assigns each batch the
// window [first frame of this batch, first frame of the next batch); the first
// batch starts at 0 and the last ends at the source duration. The windows
// tile the timeline, so every transcript line belongs to some batch.
func BatchFrames(frames []Frame, size int, durationSec float64) []FrameBatch {
	if size <= 0 {
		size = summaryVisionBatchSize
	}
	var out []FrameBatch
	for i := 0; i < len(frames); i += size {
		end := i + size
		if end > len(frames) {
			end = len(frames)
		}
		out = append(out, FrameBatch{Frames: frames[i:end], Start: frames[i].TimeSec})
	}
	for i := range out {
		if i == 0 {
			out[i].Start = 0
		}
		if i+1 < len(out) {
			out[i].End = out[i+1].Start
		} else {
			last := out[i].Frames[len(out[i].Frames)-1].TimeSec
			out[i].End = math.Max(durationSec, last)
		}
	}
	return out
}

// TranscriptWindow returns the segments that overlap [start, end): a segment
// is in when it ends after start and starts before end. A segment that ends
// exactly at start, or starts exactly at end, belongs to the neighbour.
func TranscriptWindow(segments []models.VideoSummaryTranscriptSegment, start, end float64) []models.VideoSummaryTranscriptSegment {
	var out []models.VideoSummaryTranscriptSegment
	for _, s := range segments {
		if s.End > start && s.Start < end && strings.TrimSpace(s.Text) != "" {
			out = append(out, s)
		}
	}
	return out
}

// FormatClock renders seconds as m:ss (or h:mm:ss from one hour).
func FormatClock(sec float64) string {
	if sec < 0 || math.IsNaN(sec) {
		sec = 0
	}
	total := int(math.Floor(sec + 1e-6))
	h, m, s := total/3600, (total%3600)/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func formatTranscriptLines(segments []models.VideoSummaryTranscriptSegment) string {
	var b strings.Builder
	for _, s := range segments {
		text := strings.Join(strings.Fields(s.Text), " ")
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s\n", FormatClock(s.Start), text)
	}
	return b.String()
}

// EstimateTokens is a deliberately conservative token count: ASCII at four
// characters a token, everything else (Cyrillic, CJK, Arabic...) at about one
// and a half. Over-estimating only means a little more is condensed.
func EstimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + (other*2+2)/3
}

// noteScore ranks a batch note by how much it would tell the writer: on-screen
// text first (it is exact), then people and actions, then the rest.
func noteScore(n models.VideoSummaryBatchNote) int {
	score := 3*len(n.OnScreenText) + 2*len(n.People) + 2*len(n.Actions) + len(n.Objects)
	score += len(strings.Fields(string(n.Scene))) / 6
	if strings.TrimSpace(string(n.CameraNotes)) != "" {
		score++
	}
	return score
}

func formatNote(n models.VideoSummaryBatchNote) string {
	var parts []string
	add := func(label string, v string) {
		if v = strings.TrimSpace(v); v != "" {
			parts = append(parts, label+": "+v)
		}
	}
	add("scene", string(n.Scene))
	add("on-screen text", strings.Join(n.OnScreenText, " | "))
	add("people", strings.Join(n.People, ", "))
	add("objects", strings.Join(n.Objects, ", "))
	add("actions", strings.Join(n.Actions, ", "))
	add("camera", string(n.CameraNotes))
	return fmt.Sprintf("[%s-%s] %s", FormatClock(n.Start), FormatClock(n.End), strings.Join(parts, "; "))
}

// Caveats the input fitter can add.
const (
	CaveatNotesDropped        = "Some visual notes were left out to fit the summary model's input limit."
	CaveatTranscriptCondensed = "The transcript was condensed to fit the summary model's input limit."
)

// SynthesisInput is the fitted user content for the synthesis call.
type SynthesisInput struct {
	Text           string
	NotesKept      int
	NotesDropped   int
	TranscriptCut  bool
	EstimatedToken int
	Caveats        []string
}

// BuildSynthesisInput assembles the synthesis user content and fits it into
// maxTokens: least-informative batch notes are dropped first (empty ones
// before anything else, ties broken towards keeping the earlier note), then
// transcript segments are condensed (merged into longer blocks, each cut to a
// shorter excerpt) until it fits; as a last resort the transcript is cut.
// Every reduction adds a caveat.
func BuildSynthesisInput(durationSec float64, notes []models.VideoSummaryBatchNote, segments []models.VideoSummaryTranscriptSegment, maxTokens int) SynthesisInput {
	if maxTokens <= 0 {
		maxTokens = 12000
	}
	header := fmt.Sprintf("Source duration: %.1f seconds (%s).\n", durationSec, FormatClock(durationSec))

	kept := make([]bool, len(notes))
	for i := range kept {
		kept[i] = !notes[i].Empty()
	}
	dropped := 0
	for i := range kept {
		if !kept[i] {
			dropped++
		}
	}
	notesText := func() string {
		if len(notes) == 0 {
			return ""
		}
		var b strings.Builder
		b.WriteString("\nVisual notes (from sampled frames, in time order):\n")
		n := 0
		for i, note := range notes {
			if kept[i] {
				b.WriteString(formatNote(note))
				b.WriteByte('\n')
				n++
			}
		}
		if n == 0 {
			b.WriteString("(none)\n")
		}
		return b.String()
	}
	transcript := segments
	transcriptText := func() string {
		if len(segments) == 0 {
			return ""
		}
		return "\nTranscript (machine-generated, timestamps are the segment start):\n" + formatTranscriptLines(transcript)
	}
	assemble := func() string { return header + notesText() + transcriptText() }

	text := assemble()
	// 1. Drop the least informative notes until it fits.
	order := make([]int, 0, len(notes))
	for i := range notes {
		if kept[i] {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		sa, sb := noteScore(notes[order[a]]), noteScore(notes[order[b]])
		if sa != sb {
			return sa < sb
		}
		return order[a] > order[b] // drop later notes first on a tie
	})
	for _, idx := range order {
		if EstimateTokens(text) <= maxTokens {
			break
		}
		kept[idx] = false
		dropped++
		text = assemble()
	}

	// 2. Condense the transcript: merge into longer blocks with shorter excerpts.
	cut := false
	block, excerpt := 30.0, 400
	for EstimateTokens(text) > maxTokens && len(segments) > 0 && excerpt >= 60 {
		transcript = condenseSegments(segments, block, excerpt)
		cut = true
		text = assemble()
		block *= 2
		excerpt = excerpt * 2 / 3
	}
	// 3. Last resort: hard cut at the budget.
	if EstimateTokens(text) > maxTokens {
		cut = true
		text = cutToTokens(text, maxTokens)
	}

	in := SynthesisInput{Text: text, NotesDropped: dropped, TranscriptCut: cut, EstimatedToken: EstimateTokens(text)}
	for _, k := range kept {
		if k {
			in.NotesKept++
		}
	}
	// Only notes that carried something count as "left out" for the caveat.
	informativeDropped := 0
	for i, k := range kept {
		if !k && !notes[i].Empty() {
			informativeDropped++
		}
	}
	if informativeDropped > 0 {
		in.Caveats = append(in.Caveats, CaveatNotesDropped)
	}
	if cut {
		in.Caveats = append(in.Caveats, CaveatTranscriptCondensed)
	}
	return in
}

// condenseSegments merges consecutive segments into blocks of about blockSec
// seconds and keeps the first maxChars characters of each block's text.
func condenseSegments(segments []models.VideoSummaryTranscriptSegment, blockSec float64, maxChars int) []models.VideoSummaryTranscriptSegment {
	var out []models.VideoSummaryTranscriptSegment
	var cur *models.VideoSummaryTranscriptSegment
	var text strings.Builder
	flush := func() {
		if cur == nil {
			return
		}
		cur.Text = clipText(text.String(), maxChars)
		out = append(out, *cur)
		cur = nil
		text.Reset()
	}
	for _, s := range segments {
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		if cur != nil && s.Start-cur.Start >= blockSec {
			flush()
		}
		if cur == nil {
			c := s
			cur = &c
		} else {
			cur.End = s.End
			text.WriteByte(' ')
		}
		text.WriteString(strings.TrimSpace(s.Text))
	}
	flush()
	return out
}

func clipText(s string, maxChars int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	r := []rune(s)[:maxChars]
	cut := string(r)
	if i := strings.LastIndexByte(cut, ' '); i > len(cut)/2 {
		cut = cut[:i]
	}
	return cut + " …"
}

func cutToTokens(s string, maxTokens int) string {
	if EstimateTokens(s) <= maxTokens {
		return s
	}
	lo, hi := 0, len(s)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if EstimateTokens(s[:mid]) <= maxTokens-8 {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	for lo > 0 && lo < len(s) && !utf8.RuneStart(s[lo]) {
		lo--
	}
	return s[:lo] + "\n[... cut to fit ...]\n"
}

// ParseVideoSummary decodes and validates a synthesis reply. The returned
// error is phrased for the repair prompt.
func ParseVideoSummary(raw []byte, durationSec float64) (*models.VideoSummary, error) {
	var s models.VideoSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("the JSON does not match the schema (%v)", err)
	}
	if err := models.ValidateVideoSummary(&s, durationSec); err != nil {
		return nil, err
	}
	return &s, nil
}

// SummaryValidator adapts ParseVideoSummary to ChatJSON's validate hook.
func SummaryValidator(durationSec float64) func([]byte) error {
	return func(raw []byte) error {
		_, err := ParseVideoSummary(raw, durationSec)
		return err
	}
}

// ParseBatchNote decodes a VLM reply for one batch; any JSON object is
// acceptable (missing fields are empty), so this never asks for a repair
// unless the reply is not JSON at all.
func ParseBatchNote(raw []byte, batch FrameBatch) (models.VideoSummaryBatchNote, error) {
	var n models.VideoSummaryBatchNote
	if err := json.Unmarshal(raw, &n); err != nil {
		return n, fmt.Errorf("the JSON does not match the schema (%v)", err)
	}
	n.Start, n.End = batch.Start, batch.End
	n.FrameTimes = make([]float64, 0, len(batch.Frames))
	for _, f := range batch.Frames {
		n.FrameTimes = append(n.FrameTimes, f.TimeSec)
	}
	return n, nil
}

// OnScreenTextFromNotes is the deduplicated on-screen text the VLM read, each
// item first seen at the earliest frame of the batch that reported it.
func OnScreenTextFromNotes(notes []models.VideoSummaryBatchNote) []models.VideoSummaryOnScreenText {
	var items []models.VideoSummaryOnScreenText
	for _, n := range notes {
		first := n.Start
		if len(n.FrameTimes) > 0 {
			first = n.FrameTimes[0]
		}
		for _, t := range n.OnScreenText {
			items = append(items, models.VideoSummaryOnScreenText{Text: t, FirstSeen: models.Seconds(first)})
		}
	}
	return models.DedupeOnScreenText(items, models.VideoSummaryMaxOnScreenText)
}
