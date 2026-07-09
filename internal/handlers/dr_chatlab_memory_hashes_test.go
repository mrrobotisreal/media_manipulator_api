package handlers

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---- hash builders --------------------------------------------------------------

func TestHashBuildersStability(t *testing.T) {
	// Same inputs → same hash, every time (the whole scheme depends on it).
	if hashText("hello") != hashText("hello") {
		t.Fatal("hashText is not stable")
	}
	if hashSessionAppend("s1", "m9", 12) != hashSessionAppend("s1", "m9", 12) {
		t.Fatal("hashSessionAppend is not stable")
	}
	assets := []storedProjectAsset{{ID: "a1", FileName: "x.go"}, {ID: "a2", FileName: "y.md"}}
	if hashAssetManifest(assets) != hashAssetManifest(assets) {
		t.Fatal("hashAssetManifest is not stable")
	}
	at := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	if hashFeedbackState(3, at) != hashFeedbackState(3, at) {
		t.Fatal("hashFeedbackState is not stable")
	}
}

func TestHashSessionAppendSensitivity(t *testing.T) {
	base := hashSessionAppend("s1", "m9", 12)
	if hashSessionAppend("s1", "m10", 13) == base {
		t.Fatal("a new message id must change the session hash")
	}
	if hashSessionAppend("s1", "m9", 13) == base {
		t.Fatal("a count change must change the session hash")
	}
	if hashSessionAppend("s2", "m9", 12) == base {
		t.Fatal("the session id must be part of the hash")
	}
}

func TestHashTextSensitivity(t *testing.T) {
	if hashText("description v1") == hashText("description v2") {
		t.Fatal("a text change must change the hash")
	}
	// 64 hex chars = SHA-256.
	if h := hashText("x"); len(h) != 64 || strings.ToLower(h) != h {
		t.Fatalf("expected lowercase hex SHA-256, got %q", h)
	}
}

func TestHashAssetManifestOrderAndChanges(t *testing.T) {
	a := storedProjectAsset{ID: "a1", FileName: "x.go"}
	b := storedProjectAsset{ID: "a2", FileName: "y.md"}
	// Order-independent: the manifest is hashed over SORTED (id, name) pairs.
	if hashAssetManifest([]storedProjectAsset{a, b}) != hashAssetManifest([]storedProjectAsset{b, a}) {
		t.Fatal("manifest hash must be order-independent")
	}
	base := hashAssetManifest([]storedProjectAsset{a, b})
	if hashAssetManifest([]storedProjectAsset{a}) == base {
		t.Fatal("removing an asset must change the manifest hash")
	}
	c := storedProjectAsset{ID: "a3", FileName: "z.pdf"}
	if hashAssetManifest([]storedProjectAsset{a, b, c}) == base {
		t.Fatal("adding an asset must change the manifest hash")
	}
	if hashAssetManifest(nil) == base {
		t.Fatal("the empty manifest must hash differently")
	}
}

func TestHashFeedbackStateSensitivity(t *testing.T) {
	at := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	base := hashFeedbackState(3, at)
	if hashFeedbackState(4, at) == base {
		t.Fatal("a count change must change the feedback hash")
	}
	if hashFeedbackState(3, at.Add(time.Second)) == base {
		t.Fatal("an updated_at change must change the feedback hash")
	}
}

func TestMemoryHashSentinelID(t *testing.T) {
	// The non-session kinds store this zero UUID for item_id (NULLs are
	// distinct inside Postgres PKs — see the migration header).
	if memoryHashSentinelID != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("sentinel drifted: %q", memoryHashSentinelID)
	}
}

// ---- projectFingerprint ---------------------------------------------------------

func TestProjectFingerprintOrderIndependence(t *testing.T) {
	rows := []memoryHashRow{
		{Kind: "session", ItemID: "s1", Hash: "h1"},
		{Kind: "description", ItemID: memoryHashSentinelID, Hash: "h2"},
		{Kind: "assets", ItemID: memoryHashSentinelID, Hash: "h3"},
	}
	reversed := []memoryHashRow{rows[2], rows[1], rows[0]}
	if projectFingerprint(rows) != projectFingerprint(reversed) {
		t.Fatal("fingerprint must be order-independent")
	}
}

func TestProjectFingerprintSensitivity(t *testing.T) {
	rows := []memoryHashRow{
		{Kind: "session", ItemID: "s1", Hash: "h1"},
		{Kind: "feedback", ItemID: memoryHashSentinelID, Hash: "h2"},
	}
	base := projectFingerprint(rows)

	// Any row's hash changing changes the fingerprint.
	changed := []memoryHashRow{{Kind: "session", ItemID: "s1", Hash: "h1'"}, rows[1]}
	if projectFingerprint(changed) == base {
		t.Fatal("a row hash change must change the fingerprint")
	}
	// Adding a row changes it.
	added := append([]memoryHashRow{{Kind: "session", ItemID: "s2", Hash: "h3"}}, rows...)
	if projectFingerprint(added) == base {
		t.Fatal("an added row must change the fingerprint")
	}
	// Removing a row changes it (a deleted session regenerates memory).
	if projectFingerprint(rows[:1]) == base {
		t.Fatal("a removed row must change the fingerprint")
	}
	// The empty row set has a defined, distinct fingerprint.
	if projectFingerprint(nil) == base {
		t.Fatal("the empty fingerprint must differ")
	}
	if projectFingerprint(nil) != projectFingerprint([]memoryHashRow{}) {
		t.Fatal("nil and empty must fingerprint identically")
	}
}

// ---- singleflightLatest.DoWait (the nightly job's blocking entry) -----------------

func TestSingleflightDoWaitRunsSynchronouslyWhenIdle(t *testing.T) {
	var s singleflightLatest
	ran := false
	s.DoWait("p1", func() { ran = true })
	if !ran {
		t.Fatal("DoWait must have executed the run before returning")
	}
	// The flight drained: a fresh Do starts a new run.
	finished := make(chan struct{})
	s.Do("p1", func() { close(finished) })
	<-finished
}

func TestSingleflightDoWaitBlocksOnInFlightRun(t *testing.T) {
	// The nightly job must NOT deadlock (or run concurrently) when a manual
	// refresh is already in flight: DoWait coalesces into the running flight
	// and returns once it drains — without executing its own closure.
	var s singleflightLatest
	started := make(chan struct{})
	release := make(chan struct{})
	runs := 0
	s.Do("p1", func() {
		runs++
		if runs == 1 {
			close(started)
			<-release // hold the manual run open
		}
	})
	<-started

	waitReturned := make(chan struct{})
	ranOurs := false
	go func() {
		s.DoWait("p1", func() { ranOurs = true })
		close(waitReturned)
	}()

	// Deterministic ordering: spin until DoWait has coalesced (rerun marked).
	for {
		s.mu.Lock()
		f := s.flights["p1"]
		marked := f != nil && f.rerun
		s.mu.Unlock()
		if marked {
			break
		}
	}
	select {
	case <-waitReturned:
		t.Fatal("DoWait must block while the flight is running")
	default:
	}

	close(release)
	<-waitReturned // returns once the run + its coalesced rerun drain
	if ranOurs {
		t.Fatal("a coalesced DoWait must not execute its own closure")
	}
	if runs != 2 {
		t.Fatalf("expected initial run + one rerun of the flight's closure, got %d", runs)
	}
}

// ---- scheduling math ------------------------------------------------------------

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func TestNextRunAfterSameDayAndNextDay(t *testing.T) {
	denver := mustLoc(t, "America/Denver")

	// 2 AM Denver → today's 4 AM.
	now := time.Date(2026, 7, 9, 2, 0, 0, 0, denver)
	next := nextRunAfter(now, 4, denver)
	want := time.Date(2026, 7, 9, 4, 0, 0, 0, denver)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}

	// 5 AM Denver → tomorrow's 4 AM.
	now = time.Date(2026, 7, 9, 5, 0, 0, 0, denver)
	next = nextRunAfter(now, 4, denver)
	want = time.Date(2026, 7, 10, 4, 0, 0, 0, denver)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}

	// Exactly 4 AM → the NEXT occurrence (strictly after now).
	now = time.Date(2026, 7, 9, 4, 0, 0, 0, denver)
	next = nextRunAfter(now, 4, denver)
	if !next.Equal(time.Date(2026, 7, 10, 4, 0, 0, 0, denver)) {
		t.Fatalf("next at exactly 4 AM = %v", next)
	}
}

func TestNextRunAfterDSTTransitions(t *testing.T) {
	denver := mustLoc(t, "America/Denver")

	// Spring forward: US DST 2026 begins Sunday March 8 (2 AM → 3 AM). The
	// night of the transition, "4 AM Denver" is 10:00 UTC instead of 11:00.
	now := time.Date(2026, 3, 7, 23, 0, 0, 0, denver) // 11 PM the night before
	next := nextRunAfter(now, 4, denver)
	if got := next.In(denver).Hour(); got != 4 {
		t.Fatalf("spring-forward local hour = %d, want 4", got)
	}
	if got := next.UTC().Hour(); got != 10 {
		t.Fatalf("spring-forward UTC hour = %d, want 10 (MDT = UTC-6)", got)
	}
	// The night BEFORE the transition still runs at UTC-7.
	before := nextRunAfter(time.Date(2026, 3, 6, 23, 0, 0, 0, denver), 4, denver)
	if got := before.UTC().Hour(); got != 11 {
		t.Fatalf("pre-DST UTC hour = %d, want 11 (MST = UTC-7)", got)
	}

	// Fall back: US DST 2026 ends Sunday November 1. Local hour stays 4; the
	// UTC offset shifts back to -7.
	now = time.Date(2026, 10, 31, 23, 0, 0, 0, denver)
	next = nextRunAfter(now, 4, denver)
	if got := next.In(denver).Hour(); got != 4 {
		t.Fatalf("fall-back local hour = %d, want 4", got)
	}
	if got := next.UTC().Hour(); got != 11 {
		t.Fatalf("fall-back UTC hour = %d, want 11 (MST = UTC-7)", got)
	}
}

func TestMemoryJobCatchUpDue(t *testing.T) {
	denver := mustLoc(t, "America/Denver")
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, denver) // 9 AM; today's 4 AM has passed

	// Never ran → due.
	if !memoryJobCatchUpDue(nil, now, 4, denver) {
		t.Fatal("nil last-run must be due")
	}
	// Last ran yesterday evening (before today's 4 AM occurrence) → due.
	stale := time.Date(2026, 7, 8, 20, 0, 0, 0, denver)
	if !memoryJobCatchUpDue(&stale, now, 4, denver) {
		t.Fatal("stale last-run must be due")
	}
	// Ran this morning at 4 AM → not due.
	fresh := time.Date(2026, 7, 9, 4, 0, 30, 0, denver)
	if memoryJobCatchUpDue(&fresh, now, 4, denver) {
		t.Fatal("fresh last-run must not be due")
	}
	// Before today's occurrence (e.g. 2 AM now, last ran yesterday 4 AM):
	// yesterday's occurrence is the most recent → not due.
	early := time.Date(2026, 7, 9, 2, 0, 0, 0, denver)
	yesterday := time.Date(2026, 7, 8, 4, 0, 30, 0, denver)
	if memoryJobCatchUpDue(&yesterday, early, 4, denver) {
		t.Fatal("pre-occurrence morning with yesterday's run must not be due")
	}
}

// ---- memory instruction output contract ------------------------------------------

func TestMemoryInstructionSixSections(t *testing.T) {
	system, _ := buildMemoryPrompt(memoryPromptInput{Name: "P", MaxChars: 8192})

	headings := []string{
		"## Purpose & context",
		"## Current state",
		"## On the horizon",
		"## Key learnings & principles",
		"## Approach & patterns",
		"## Tools & resources",
	}
	last := -1
	for _, hd := range headings {
		i := strings.Index(system, hd)
		if i < 0 {
			t.Fatalf("instruction missing heading %q", hd)
		}
		if i < last {
			t.Fatalf("heading %q out of order", hd)
		}
		last = i
	}
	if !strings.Contains(system, `"- Nothing notable yet."`) {
		t.Fatal("instruction missing the empty-section placeholder rule")
	}
	if !strings.Contains(system, "never an append") {
		t.Fatal("instruction must keep the rewrite-from-scratch semantics")
	}
	if !strings.Contains(system, fmt.Sprintf("Maximum %d characters", 8192)) {
		t.Fatal("char cap not interpolated")
	}
	// The feedback distillation moved INTO the Key learnings charter.
	if !strings.Contains(system, "distilled from response feedback") {
		t.Fatal("Key learnings charter must carry the feedback distillation")
	}
}
