package handlers

import "testing"

// Pure unit tests for the edit / soft-delete / version-history helpers. Same
// style as dr_docs_editor_test.go — no DB, network, or AWS/Firebase access.

func TestDrCanDelete(t *testing.T) {
	tests := []struct {
		name      string
		createdBy string
		caller    string
		want      bool
	}{
		{"exact match", "owner@example.com", "owner@example.com", true},
		{"case-insensitive match", "Owner@Example.com", "owner@example.com", true},
		{"case-insensitive match 2", "owner@example.com", "OWNER@EXAMPLE.COM", true},
		{"mismatch", "owner@example.com", "other@example.com", false},
		{"seed sentinel vs user", "seed:migration", "owner@example.com", false},
		{"seed sentinel vs seed-looking email", "seed:migration", "seed:migration", true}, // equal strings still match; no real email equals this
		{"empty createdBy", "", "owner@example.com", false},
		{"empty caller", "owner@example.com", "", false},
		{"both empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := drCanDelete(tc.createdBy, tc.caller); got != tc.want {
				t.Fatalf("drCanDelete(%q, %q) = %v, want %v", tc.createdBy, tc.caller, got, tc.want)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

func TestDecideStartEdit(t *testing.T) {
	tests := []struct {
		name    string
		exists  bool
		from    *int
		replace bool
		want    startEditDecision
	}{
		{"no session, plain open", false, nil, false, startEditCreate},
		{"no session, from revision", false, intPtr(1), false, startEditCreate},
		{"no session, replace", false, nil, true, startEditCreate},
		{"session, plain open → resume", true, nil, false, startEditResume},
		{"session, from revision, no replace → conflict", true, intPtr(2), false, startEditConflict},
		{"session, replace → recreate", true, nil, true, startEditCreate},
		{"session, from revision + replace → recreate", true, intPtr(2), true, startEditCreate},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideStartEdit(tc.exists, tc.from, tc.replace); got != tc.want {
				t.Fatalf("decideStartEdit(%v, %v, %v) = %d, want %d", tc.exists, tc.from, tc.replace, got, tc.want)
			}
		})
	}
}

func TestUnionDrAssetRefs(t *testing.T) {
	newContent := []byte(`{"format":"dr-blocks/v1","blocks":[
		{"type":"image","src":"dr-asset://a"},
		{"type":"paragraph","spans":[{"text":"x"}]}
	]}`)
	rev1 := []byte(`{"format":"dr-blocks/v1","blocks":[
		{"type":"image","src":"dr-asset://b"},
		{"type":"file","src":"dr-asset://c"}
	]}`)
	rev2 := []byte(`{"format":"dr-blocks/v1","blocks":[
		{"type":"video","src":"https://external.example/v.mp4"},
		{"type":"image","src":"dr-asset://a"}
	]}`)

	got := unionDrAssetRefs([][]byte{newContent, rev1, rev2})
	want := map[string]bool{"a": true, "b": true, "c": true}
	if len(got) != len(want) {
		t.Fatalf("union = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("union missing %q: %v", k, got)
		}
	}

	// A malformed snapshot is skipped, not fatal; the rest still contribute.
	got2 := unionDrAssetRefs([][]byte{[]byte("not json"), rev1})
	if len(got2) != 2 || !got2["b"] || !got2["c"] {
		t.Fatalf("malformed-skip union = %v, want {b,c}", got2)
	}

	// Empty input → empty set.
	if len(unionDrAssetRefs(nil)) != 0 {
		t.Fatalf("expected empty set for nil input")
	}
}

func TestParseRevisionNumber(t *testing.T) {
	tests := []struct {
		raw    string
		wantN  int
		wantOK bool
	}{
		{"1", 1, true},
		{"42", 42, true},
		{" 3 ", 3, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"1.5", 0, false},
		{"1e3", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			n, ok := parseRevisionNumber(tc.raw)
			if n != tc.wantN || ok != tc.wantOK {
				t.Fatalf("parseRevisionNumber(%q) = (%d, %v), want (%d, %v)", tc.raw, n, ok, tc.wantN, tc.wantOK)
			}
		})
	}
}
