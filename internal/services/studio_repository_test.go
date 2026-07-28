package services

import "testing"

// The B13 ownership rules live in SQL and need Postgres to exercise end to end
// (see the CHECKPOINT 4 server steps). What IS unit-testable is the parameter
// marshalling those predicates depend on, and it is the part most likely to be
// broken by a careless refactor: every ownership query is written as
// `$n::uuid IS NOT NULL AND user_id = $n::uuid`, so an anonymous owner MUST
// bind SQL NULL. Binding the empty string instead would make the cast fail —
// or worse, in a rewritten query, match rows it should not.
func TestStudioOwnerUserParam(t *testing.T) {
	cases := []struct {
		name  string
		owner StudioOwner
		want  any
	}{
		{"anonymous", StudioOwner{SessionID: "sess-1"}, nil},
		{"blank user id", StudioOwner{UserID: "", SessionID: "sess-1"}, nil},
		{"whitespace user id", StudioOwner{UserID: "   ", SessionID: "sess-1"}, nil},
		{
			"signed in",
			StudioOwner{UserID: "0f6c8e2a-1111-4222-8333-444455556666", SessionID: "sess-1"},
			"0f6c8e2a-1111-4222-8333-444455556666",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.owner.userParam(); got != tc.want {
				t.Errorf("userParam() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A signed-out owner must never be able to produce a non-NULL user parameter,
// no matter what the session id contains — the session id is client-supplied
// and must not be able to leak into the account-scoped half of the predicate.
func TestStudioOwnerSessionCannotForgeUserID(t *testing.T) {
	owner := StudioOwner{SessionID: "0f6c8e2a-1111-4222-8333-444455556666"}
	if got := owner.userParam(); got != nil {
		t.Fatalf("a session id must never bind as a user id, got %#v", got)
	}
}
