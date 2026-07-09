package handlers

import (
	"strings"
	"testing"
)

// Pure unit tests for the Documentation filesystem — no DB. The tree fixtures
// are parent maps exactly as loadFolderParents builds them ("" = root).

func TestValidateDocFolderName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"Research", true},
		{"Infrastructure Strategy", true},
		{"2026 — Q3", true},
		{"a", true},
		{strings.Repeat("n", 120), true},
		{"", false},
		{" leading", false},
		{"trailing ", false},
		{"\tTabbed", false},
		{"with/slash", false},
		{strings.Repeat("n", 121), false},
	}
	for _, tc := range cases {
		msg := validateDocFolderName(tc.name)
		if tc.ok && msg != "" {
			t.Fatalf("%q should be valid, got %q", tc.name, msg)
		}
		if !tc.ok && msg == "" {
			t.Fatalf("%q should be rejected", tc.name)
		}
	}
}

// Fixture tree:  root ── a ── b ── c        (a>b>c chain)
//
//	└─ d                  (root sibling)
func docFolderFixture() map[string]string {
	return map[string]string{
		"a": "",
		"b": "a",
		"c": "b",
		"d": "",
	}
}

func TestWouldCreateCycle(t *testing.T) {
	parents := docFolderFixture()

	// Into itself.
	if !wouldCreateCycle(parents, "a", "a") {
		t.Fatal("a → a must be a cycle")
	}
	// Into its own descendant (direct child and deeper).
	if !wouldCreateCycle(parents, "a", "b") {
		t.Fatal("a → b (own child) must be a cycle")
	}
	if !wouldCreateCycle(parents, "a", "c") {
		t.Fatal("a → c (own grandchild) must be a cycle")
	}
	// Legal moves.
	if wouldCreateCycle(parents, "b", "d") {
		t.Fatal("b → d is legal")
	}
	if wouldCreateCycle(parents, "d", "c") {
		t.Fatal("d → c is legal (d has no descendants)")
	}
	// Moving a child up to its ancestor's level is legal.
	if wouldCreateCycle(parents, "c", "a") {
		t.Fatal("c → a is legal (a is c's ancestor, not descendant)")
	}
}

func TestFolderDepthAndSubtreeHeight(t *testing.T) {
	parents := docFolderFixture()

	if got := folderDepth(parents, "a"); got != 1 {
		t.Fatalf("depth(a) = %d, want 1", got)
	}
	if got := folderDepth(parents, "c"); got != 3 {
		t.Fatalf("depth(c) = %d, want 3", got)
	}
	if got := folderDepth(parents, ""); got != 0 {
		t.Fatalf("depth(root) = %d, want 0", got)
	}

	if got := subtreeHeight(parents, "a"); got != 2 {
		t.Fatalf("height(a) = %d, want 2 (b, c below)", got)
	}
	if got := subtreeHeight(parents, "c"); got != 0 {
		t.Fatalf("height(c) = %d, want 0 (leaf)", got)
	}
	if got := subtreeHeight(parents, "d"); got != 0 {
		t.Fatalf("height(d) = %d, want 0 (leaf)", got)
	}

	// The move check combines them: moving a (height 2) under d (depth 1)
	// puts the deepest node at 1+1+2 = 4 levels — inside the cap of 10.
	if folderDepth(parents, "d")+1+subtreeHeight(parents, "a") != 4 {
		t.Fatal("combined depth math drifted")
	}
}

func TestDocFolderDepthCapConstant(t *testing.T) {
	if drDocFolderMaxDepth != 10 {
		t.Fatalf("depth cap = %d, want 10", drDocFolderMaxDepth)
	}
}
