package handlers

import (
	"testing"
	"time"
)

// perfClock tests feed injected time.Time values — no sleeps (Hard
// Constraint: pure unit tests). t0 is an arbitrary anchor; deltas are offsets.

var perfT0 = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return perfT0.Add(time.Duration(ms) * time.Millisecond) }

func TestPerfClockNoReasoning(t *testing.T) {
	c := newPerfClock(perfT0)
	c.onContentDelta(at(800))
	c.onContentDelta(at(1200))
	dur, reason, first := c.finalize(at(1500))
	if dur != 1500 {
		t.Fatalf("duration = %d, want 1500", dur)
	}
	if reason != nil {
		t.Fatalf("reasoningMs must be nil (not 0) with no reasoning, got %v", *reason)
	}
	if first == nil || *first != 800 {
		t.Fatalf("firstTokenMs = %v, want 800", first)
	}
}

func TestPerfClockReasoningThenContent(t *testing.T) {
	c := newPerfClock(perfT0)
	c.onReasoningDelta(at(500))
	c.onReasoningDelta(at(2000))
	c.onContentDelta(at(3000)) // closes the span: 3000 − 500
	c.onContentDelta(at(4000))
	dur, reason, first := c.finalize(at(4200))
	if dur != 4200 {
		t.Fatalf("duration = %d", dur)
	}
	if reason == nil || *reason != 2500 {
		t.Fatalf("reasoningMs = %v, want 2500", reason)
	}
	if first == nil || *first != 500 {
		t.Fatalf("firstTokenMs = %v, want 500 (first delta of ANY kind)", first)
	}
}

func TestPerfClockReasoningOnlyToolRound(t *testing.T) {
	// A round that reasons and then ends in tool_calls with NO content: the
	// span is (last delta of round − first reasoning delta).
	c := newPerfClock(perfT0)
	c.onReasoningDelta(at(400))
	c.onReasoningDelta(at(1400)) // last delta of the round
	c.onRoundEnd(at(2000))       // round end time itself is NOT the span end
	dur, reason, first := c.finalize(at(5000))
	if dur != 5000 {
		t.Fatalf("duration = %d", dur)
	}
	if reason == nil || *reason != 1000 {
		t.Fatalf("reasoningMs = %v, want 1000 (lastDelta − firstReasoning)", reason)
	}
	if first == nil || *first != 400 {
		t.Fatalf("firstTokenMs = %v, want 400", first)
	}
}

func TestPerfClockMultiRoundSummation(t *testing.T) {
	c := newPerfClock(perfT0)
	// Round 1: reasoning 100→600, then tool calls (no content) → +500.
	c.onReasoningDelta(at(100))
	c.onReasoningDelta(at(600))
	c.onRoundEnd(at(700))
	// (tool execution gap 700→2000)
	// Round 2: reasoning 2000→2300, first content at 2800 → +800.
	c.onReasoningDelta(at(2000))
	c.onContentDelta(at(2800))
	c.onContentDelta(at(3500))
	dur, reason, first := c.finalize(at(3600))
	if dur != 3600 {
		t.Fatalf("duration = %d (must include tool-execution time)", dur)
	}
	if reason == nil || *reason != 1300 {
		t.Fatalf("reasoningMs = %v, want 500+800=1300", reason)
	}
	if first == nil || *first != 100 {
		t.Fatalf("firstTokenMs = %v, want 100 — set once, never updated", first)
	}
}

func TestPerfClockFirstTokenSetOnce(t *testing.T) {
	c := newPerfClock(perfT0)
	c.onContentDelta(at(300))
	c.onRoundEnd(at(400))
	c.onReasoningDelta(at(900)) // later deltas must not move it
	_, _, first := c.finalize(at(1000))
	if first == nil || *first != 300 {
		t.Fatalf("firstTokenMs = %v, want 300", first)
	}
}

func TestPerfClockNoDeltasAtAll(t *testing.T) {
	// Interrupted before anything streamed: duration recorded, both nils.
	c := newPerfClock(perfT0)
	dur, reason, first := c.finalize(at(2500))
	if dur != 2500 || reason != nil || first != nil {
		t.Fatalf("got (%d, %v, %v), want (2500, nil, nil)", dur, reason, first)
	}
}

func TestPerfClockFinalizeMemoized(t *testing.T) {
	// The SSE usage event and the persist both call finalize — they must
	// return identical values.
	c := newPerfClock(perfT0)
	c.onContentDelta(at(100))
	d1, _, _ := c.finalize(at(1000))
	d2, _, _ := c.finalize(at(9999))
	if d1 != d2 {
		t.Fatalf("finalize not memoized: %d then %d", d1, d2)
	}
}

// ---- classifyRequestType ----------------------------------------------------------

func TestClassifyRequestTypeMatrix(t *testing.T) {
	cases := []struct {
		name string
		seed func(m *requestModalities)
		want string
	}{
		{"no attachments or assets", func(*requestModalities) {}, "text"},
		{"text file attachment", func(m *requestModalities) { m.addAttachment("file", "text/csv") }, "file"},
		{"json attachment", func(m *requestModalities) { m.addAttachment("file", "application/json") }, "file"},
		{"image attachment", func(m *requestModalities) { m.addAttachment("image", "image/png") }, "image"},
		{"pdf attachment", func(m *requestModalities) { m.addAttachment("file", "application/pdf") }, "pdf"},
		{"image + pdf", func(m *requestModalities) {
			m.addAttachment("image", "image/png")
			m.addAttachment("file", "application/pdf")
		}, "mixed"},
		{"image + text file — file is a bucket for mixing", func(m *requestModalities) {
			m.addAttachment("image", "image/png")
			m.addAttachment("file", "text/plain")
		}, "mixed"},
		{"two images stay image", func(m *requestModalities) {
			m.addAttachment("image", "image/png")
			m.addAttachment("image", "image/jpeg")
		}, "image"},
		// Tool-loop upgrades: read_asset mid-turn.
		{"text turn upgraded by image asset read", func(m *requestModalities) { m.addAssetKind("image") }, "image"},
		{"text turn upgraded by audio asset read", func(m *requestModalities) { m.addAssetKind("audio") }, "audio"},
		{"code asset read is a file", func(m *requestModalities) { m.addAssetKind("code") }, "file"},
		{"text asset read is a file", func(m *requestModalities) { m.addAssetKind("text") }, "file"},
		{"pdf asset read", func(m *requestModalities) { m.addAssetKind("pdf") }, "pdf"},
		{"file attachment + image asset read → mixed", func(m *requestModalities) {
			m.addAttachment("file", "text/markdown")
			m.addAssetKind("image")
		}, "mixed"},
		{"pdf attachment + pdf asset read stays pdf", func(m *requestModalities) {
			m.addAttachment("file", "application/pdf")
			m.addAssetKind("pdf")
		}, "pdf"},
		{"audio + image + file → mixed", func(m *requestModalities) {
			m.addAssetKind("audio")
			m.addAssetKind("image")
			m.addAssetKind("text")
		}, "mixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m requestModalities
			tc.seed(&m)
			if got := classifyRequestType(m); got != tc.want {
				t.Fatalf("classifyRequestType = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- stats type-filter allowlist ---------------------------------------------------

func TestValidStatsTypeFilter(t *testing.T) {
	for _, ok := range []string{"", "text", "file", "image", "pdf", "audio", "mixed"} {
		if !validStatsTypeFilter(ok) {
			t.Fatalf("%q must be accepted", ok)
		}
	}
	for _, bad := range []string{"video", "TEXT", "chat", "untracked", "'; DROP TABLE"} {
		if validStatsTypeFilter(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
