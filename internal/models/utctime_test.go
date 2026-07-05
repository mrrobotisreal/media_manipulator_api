package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUTCTimeMarshalJSONEmitsZ(t *testing.T) {
	// A non-UTC input (PST, -8h): 04:00 PST == 12:00 UTC. MarshalJSON must
	// normalize to UTC and emit a 'Z' suffix.
	loc := time.FixedZone("PST", -8*3600)
	in := UTCTime{Time: time.Date(2026, 1, 2, 4, 0, 0, 0, loc)}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if got != `"2026-01-02T12:00:00Z"` {
		t.Fatalf("expected normalized UTC with Z suffix, got %s", got)
	}
	if !strings.HasSuffix(strings.TrimSuffix(got, `"`), "Z") {
		t.Errorf("expected Z suffix, got %s", got)
	}
}

func TestUTCTimeScan(t *testing.T) {
	var u UTCTime
	tm := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	if err := u.Scan(tm); err != nil {
		t.Fatalf("scan time.Time: %v", err)
	}
	if !u.Time.Equal(tm) {
		t.Errorf("scan mismatch: got %v want %v", u.Time, tm)
	}
	if err := u.Scan(nil); err != nil {
		t.Fatalf("scan nil: %v", err)
	}
	if !u.Time.IsZero() {
		t.Errorf("scan nil should zero the time, got %v", u.Time)
	}
	if err := u.Scan("not a time"); err == nil {
		t.Error("expected an error scanning an unsupported type")
	}
}
