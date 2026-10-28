package dllink

import (
	"strings"
	"testing"
	"time"
)

func TestSignerOpen(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s, err := New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := New("other-secret")
	claims := Claims{Expires: now.Add(5 * time.Minute).Unix(), SessionID: "sess", VisitorID: "vis", Tier: "free"}
	tok, err := s.Mint("job-a", claims)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tok, "sess") {
		t.Fatal("session id must not be readable in the token")
	}
	flip := func(tok string) string {
		b := []byte(tok)
		i := len(b) - 5
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return string(b)
	}

	cases := []struct {
		name   string
		signer *Signer
		job    string
		token  string
		at     time.Time
		ok     bool
	}{
		{"valid", s, "job-a", tok, now, true},
		{"valid just before expiry", s, "job-a", tok, now.Add(5*time.Minute - time.Second), true},
		{"expired", s, "job-a", tok, now.Add(5 * time.Minute), false},
		{"other job", s, "job-b", tok, now, false},
		{"tampered", s, "job-a", flip(tok), now, false},
		{"other secret", other, "job-a", tok, now, false},
		{"empty", s, "job-a", "", now, false},
		{"no prefix", s, "job-a", strings.TrimPrefix(tok, "v1."), now, false},
		{"garbage", s, "job-a", "v1.!!!", now, false},
		{"too short", s, "job-a", "v1.AAAA", now, false},
		{"oversized", s, "job-a", "v1." + strings.Repeat("A", MaxTokenLen), now, false},
		{"empty job", s, "", tok, now, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.signer.Open(tc.job, tc.token, tc.at)
			if tc.ok {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				if got != claims {
					t.Fatalf("claims %+v want %+v", got, claims)
				}
			} else if err == nil {
				t.Fatal("want refusal")
			}
		})
	}
}

func TestEmptySecretIsRandomPerSigner(t *testing.T) {
	a, _ := New("")
	b, _ := New("  ")
	exp := time.Now().Add(time.Minute).Unix()
	tok, _ := a.Mint("j", Claims{Expires: exp})
	if _, err := a.Open("j", tok, time.Now()); err != nil {
		t.Fatalf("own token: %v", err)
	}
	if _, err := b.Open("j", tok, time.Now()); err == nil {
		t.Fatal("a random-key signer must not open another's token")
	}
}

func TestNilSigner(t *testing.T) {
	var s *Signer
	if _, err := s.Mint("j", Claims{Expires: 1}); err == nil {
		t.Fatal("nil mint")
	}
	if _, err := s.Open("j", "v1.x", time.Now()); err == nil {
		t.Fatal("nil open")
	}
}
