package handlers

import (
	"math/big"
	"slices"
	"testing"
	"time"
)

// Pure unit tests for the DR Tasks helpers — no DB, no network, no S3.

// ---- normalizeTaskLabel ------------------------------------------------------

func TestNormalizeTaskLabel(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"simple", "bug", "bug", true},
		{"uppercase lowered", "FrontEnd", "frontend", true},
		{"trimmed", "  infra  ", "infra", true},
		{"spaces to hyphens", "tech debt", "tech-debt", true},
		{"underscores to hyphens", "content_studio", "content-studio", true},
		{"mixed separators collapse", "a _ b", "a-b", true},
		{"repeated hyphens collapse", "a---b", "a-b", true},
		{"leading/trailing hyphens trimmed", "--edge--", "edge", true},
		{"digits ok", "v2-api", "v2-api", true},
		{"exactly 30 chars", "abcdefghijklmnopqrstuvwxyz-123", "abcdefghijklmnopqrstuvwxyz-123", true},
		{"31 chars too long", "abcdefghijklmnopqrstuvwxyz-1234", "", false},
		{"empty", "", "", false},
		{"only separators", " -_- ", "", false},
		{"unicode rejected", "büg", "", false},
		{"emoji rejected", "fire🔥", "", false},
		{"punctuation rejected", "ui/ux", "", false},
		{"uppercase unicode rejected", "Éclair", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeTaskLabel(tc.in)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("normalizeTaskLabel(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestNormalizeTaskLabels(t *testing.T) {
	// Dedupe preserves first occurrence (post-normalization identity).
	got, err := normalizeTaskLabels([]string{"Bug", "infra", "bug", "tech debt", "tech-debt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"bug", "infra", "tech-debt"}; !slices.Equal(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}

	// Max 10 labels.
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = "label" + string(rune('a'+i))
	}
	if _, err := normalizeTaskLabels(eleven); err == nil {
		t.Fatal("11 labels must be rejected")
	}

	// One bad label rejects the set with the ORIGINAL input in the message.
	if _, err := normalizeTaskLabels([]string{"ok", "büg"}); err == nil {
		t.Fatal("invalid label must be rejected")
	}

	// Empty input → empty (non-nil) set.
	got, err = normalizeTaskLabels(nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nil input = (%v, %v), want empty set", got, err)
	}
}

// ---- taskPositionBetween -----------------------------------------------------

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad rat literal: " + s)
	}
	return r
}

func TestTaskPositionBetween(t *testing.T) {
	cases := []struct {
		name          string
		before, after *big.Rat
		want          *big.Rat
		wantRebalance bool
	}{
		{"empty column default", nil, nil, rat("1024"), false},
		{"drop at top", nil, rat("1024"), rat("0"), false},
		{"drop at top of negatives", nil, rat("-2048"), rat("-3072"), false},
		{"drop at bottom", rat("3072"), nil, rat("4096"), false},
		{"midpoint", rat("1024"), rat("2048"), rat("1536"), false},
		{"fractional midpoint stays exact", rat("1024"), rat("1025"), rat("2049/2"), false},
		{"gap exactly 2e-6 is fine", rat("1"), rat("1.000002"), rat("1.000001"), false},
		{"gap below threshold rebalances", rat("1"), rat("1.0000019"), rat("1.00000095"), true},
		{"identical neighbors rebalance", rat("5"), rat("5"), rat("5"), true},
		{"inverted neighbors rebalance", rat("2048"), rat("1024"), rat("1536"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rebalance := taskPositionBetween(tc.before, tc.after)
			if got.Cmp(tc.want) != 0 || rebalance != tc.wantRebalance {
				t.Fatalf("taskPositionBetween = (%s, %v), want (%s, %v)",
					got.RatString(), rebalance, tc.want.RatString(), tc.wantRebalance)
			}
		})
	}
}

func TestTaskPositionBetweenExactness(t *testing.T) {
	// Repeated midpoints must stay EXACT binary fractions (no float drift):
	// halving from 1024 toward 0 gives 512, 256, … 2^-k precisely.
	lo, hi := rat("0"), rat("1024")
	for i := 0; i < 12; i++ {
		mid, rebalance := taskPositionBetween(lo, hi)
		if rebalance {
			t.Fatalf("iteration %d: premature rebalance (gap still %s)", i, new(big.Rat).Sub(hi, lo).RatString())
		}
		want := new(big.Rat).Add(lo, hi)
		want.Quo(want, rat("2"))
		if mid.Cmp(want) != 0 {
			t.Fatalf("iteration %d: mid = %s, want %s", i, mid.RatString(), want.RatString())
		}
		hi = mid
	}
	// 1024 / 2^12 = 0.25 — comfortably above the 1e-6 rebalance floor.
	if hi.Cmp(rat("1/4")) != 0 {
		t.Fatalf("after 12 halvings hi = %s, want 1/4", hi.RatString())
	}
}

func TestParseRenderTaskPositionRoundTrip(t *testing.T) {
	for _, s := range []string{"1024.0000000000", "-3072.0000000000", "1536.5000000000", "0.0000010000"} {
		r, err := parseTaskPosition(s)
		if err != nil {
			t.Fatalf("parseTaskPosition(%q): %v", s, err)
		}
		if got := renderTaskPosition(r); got != s {
			t.Fatalf("round trip %q -> %q", s, got)
		}
	}
	if _, err := parseTaskPosition("not-a-number"); err == nil {
		t.Fatal("garbage position must error")
	}
}

// ---- diffTaskUpdate ----------------------------------------------------------

func baseTaskFields() taskFields {
	return taskFields{
		Title:    "Fix the flaky test",
		Type:     "bug",
		Priority: "high",
		Assignee: "a@example.com",
		Labels:   []string{"ci", "infra"},
		DueDate:  "2026-07-20",
	}
}

func TestDiffTaskUpdateNoOp(t *testing.T) {
	old := baseTaskFields()
	if rows := diffTaskUpdate(old, old); len(rows) != 0 {
		t.Fatalf("identical fields must diff empty, got %+v", rows)
	}
	// Semantically-equal descriptions (jsonb re-rendering: key order +
	// whitespace differ) must NOT produce a row.
	a, b := old, old
	a.Description = []byte(`{"format":"dr-blocks/v1","blocks":[]}`)
	b.Description = []byte(`{ "blocks": [], "format": "dr-blocks/v1" }`)
	if rows := diffTaskUpdate(a, b); len(rows) != 0 {
		t.Fatalf("equivalent descriptions must diff empty, got %+v", rows)
	}
}

func TestDiffTaskUpdatePerFieldRows(t *testing.T) {
	old := baseTaskFields()
	next := old
	next.Title = "Fix the flaky test for real"
	next.Priority = "highest"
	next.Labels = []string{"ci"}
	next.DueDate = ""

	rows := diffTaskUpdate(old, next)
	want := []taskActivityRow{
		{Action: "updated", Field: "title", OldValue: old.Title, NewValue: next.Title},
		{Action: "updated", Field: "priority", OldValue: "high", NewValue: "highest"},
		{Action: "updated", Field: "labels", OldValue: "ci, infra", NewValue: "ci"},
		{Action: "updated", Field: "dueDate", OldValue: "2026-07-20", NewValue: ""},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestDiffTaskUpdateAssignedAction(t *testing.T) {
	old := baseTaskFields()
	next := old
	next.Assignee = "b@example.com"
	rows := diffTaskUpdate(old, next)
	if len(rows) != 1 || rows[0] != (taskActivityRow{Action: "assigned", Field: "assignee", OldValue: "a@example.com", NewValue: "b@example.com"}) {
		t.Fatalf("rows = %+v", rows)
	}
	// Unassigning renders the empty value as ''.
	next.Assignee = ""
	rows = diffTaskUpdate(old, next)
	if len(rows) != 1 || rows[0].NewValue != "" || rows[0].Action != "assigned" {
		t.Fatalf("unassign rows = %+v", rows)
	}
}

func TestDiffTaskUpdateDescriptionMarkers(t *testing.T) {
	blocks := []byte(`{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"hi"}]}]}`)
	other := []byte(`{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"bye"}]}]}`)

	old, next := baseTaskFields(), baseTaskFields()
	next.Description = blocks
	rows := diffTaskUpdate(old, next)
	if len(rows) != 1 || rows[0] != (taskActivityRow{Action: "updated", Field: "description", OldValue: "", NewValue: "(updated)"}) {
		t.Fatalf("add-description rows = %+v", rows)
	}

	old.Description, next.Description = blocks, other
	rows = diffTaskUpdate(old, next)
	if len(rows) != 1 || rows[0].OldValue != "(updated)" || rows[0].NewValue != "(updated)" {
		t.Fatalf("edit-description rows = %+v (block JSON must never leak into activity values)", rows)
	}

	old.Description, next.Description = blocks, nil
	rows = diffTaskUpdate(old, next)
	if len(rows) != 1 || rows[0].OldValue != "(updated)" || rows[0].NewValue != "" {
		t.Fatalf("clear-description rows = %+v", rows)
	}
}

// ---- enum sets ----------------------------------------------------------------

func TestTaskEnumSets(t *testing.T) {
	for _, s := range []string{"backlog", "todo", "in_progress", "in_review", "done"} {
		if !drTaskStatuses[s] {
			t.Fatalf("status %q missing", s)
		}
	}
	for _, s := range []string{"", "archived", "IN_PROGRESS", "in progress"} {
		if drTaskStatuses[s] {
			t.Fatalf("status %q must be rejected", s)
		}
	}
	for _, ty := range []string{"task", "bug", "feature", "improvement"} {
		if !drTaskTypes[ty] {
			t.Fatalf("type %q missing", ty)
		}
	}
	if drTaskTypes["epic"] || drTaskTypes[""] {
		t.Fatal("unknown types must be rejected")
	}
	for _, p := range []string{"lowest", "low", "medium", "high", "highest"} {
		if !drTaskPriorities[p] {
			t.Fatalf("priority %q missing", p)
		}
	}
	if drTaskPriorities["urgent"] || drTaskPriorities[""] {
		t.Fatal("unknown priorities must be rejected")
	}
}

// ---- due-date parsing -----------------------------------------------------------

func TestParseTaskDueDate(t *testing.T) {
	got, err := parseTaskDueDate("2026-07-20")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("parsed = %v, want %v", got, want)
	}
	for _, bad := range []string{"2026-7-20", "07/20/2026", "2026-13-01", "2026-02-30", "tomorrow", ""} {
		if _, err := parseTaskDueDate(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

// ---- misc pure helpers ------------------------------------------------------------

func TestIsJSONNull(t *testing.T) {
	if !isJSONNull([]byte("null")) || !isJSONNull([]byte("  null\n")) {
		t.Fatal("null literal must be detected")
	}
	if isJSONNull([]byte(`{"format":"dr-blocks/v1"}`)) || isJSONNull([]byte(`"null"`)) {
		t.Fatal("non-null values must not be detected as null")
	}
}

func TestTaskDescriptionsEqual(t *testing.T) {
	a := []byte(`{"format":"dr-blocks/v1","blocks":[]}`)
	b := []byte(`{ "blocks": [], "format": "dr-blocks/v1" }`)
	if !taskDescriptionsEqual(a, b) {
		t.Fatal("key order / whitespace must not matter")
	}
	if !taskDescriptionsEqual(nil, nil) || !taskDescriptionsEqual(nil, []byte{}) {
		t.Fatal("both-empty must be equal")
	}
	if taskDescriptionsEqual(a, nil) {
		t.Fatal("present vs absent must differ")
	}
	if taskDescriptionsEqual(a, []byte(`{"format":"dr-blocks/v1","blocks":[{"type":"code","code":"x"}]}`)) {
		t.Fatal("different documents must differ")
	}
}
