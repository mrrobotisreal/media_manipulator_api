package handlers

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Pure-helper tests for the DR Communication/Feedback feature: dm_key
// canonicalization, channel-name validation, message-content validation,
// snippet derivation, and the in-memory SSE broadcaster. No DB, network, or
// AWS/Firebase access (mirrors dr_docs_editor_test.go / dr_auth_test.go). Run
// the broadcaster tests with -race.

func TestCanonicalDMKey(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want string
	}{
		{"already sorted", "aa@x.com", "bb@x.com", "aa@x.com|bb@x.com"},
		{"reversed order", "bb@x.com", "aa@x.com", "aa@x.com|bb@x.com"},
		{"mixed case", "Bob@X.com", "alice@x.COM", "alice@x.com|bob@x.com"},
		{"whitespace trimmed", "  bob@x.com ", "alice@x.com", "alice@x.com|bob@x.com"},
		{"equal emails", "a@x.com", "a@x.com", "a@x.com|a@x.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalDMKey(tc.a, tc.b); got != tc.want {
				t.Fatalf("canonicalDMKey(%q,%q) = %q, want %q", tc.a, tc.b, got, tc.want)
			}
			// Order-independence: swapping args yields the same key.
			if got := canonicalDMKey(tc.b, tc.a); got != tc.want {
				t.Fatalf("canonicalDMKey(%q,%q) = %q, want %q (not order-independent)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestNormalizeDrChannelName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"simple", "general", "general", false},
		{"uppercased", "General", "general", false},
		{"trimmed", "  general  ", "general", false},
		{"hyphenated", "product-updates", "product-updates", false},
		{"underscored", "team_notes", "team_notes", false},
		{"numbers", "q3-2026", "q3-2026", false},
		{"two chars ok", "ab", "ab", false},
		{"one char too short", "a", "", true},
		{"empty", "", "", true},
		{"leading hyphen", "-general", "", true},
		{"trailing hyphen", "general-", "", true},
		{"double hyphen", "a--b", "", true},
		{"spaces inside", "my channel", "", true},
		{"punctuation", "hey!", "", true},
		{"slash", "a/b", "", true},
		{"too long", strings.Repeat("a", 81), "", true},
		{"max length ok", strings.Repeat("a", 80), strings.Repeat("a", 80), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeDrChannelName(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("normalizeDrChannelName(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("normalizeDrChannelName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !tc.wantErr && !drFeedbackChannelNamePattern.MatchString(got) {
				t.Fatalf("normalizeDrChannelName(%q) = %q does not match the pattern", tc.in, got)
			}
		})
	}
}

func TestValidateDrMessageJSON(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"paragraph", `{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"hi"}]}]}`, false},
		{"code", `{"format":"dr-blocks/v1","blocks":[{"type":"code","language":"go","code":"x"}]}`, false},
		{"list", `{"format":"dr-blocks/v1","blocks":[{"type":"list","ordered":false,"items":[[{"text":"a"}]]}]}`, false},
		{"blockquote", `{"format":"dr-blocks/v1","blocks":[{"type":"blockquote","lines":[[{"text":"q"}]]}]}`, false},
		{"all four", `{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[]},{"type":"code","code":""},{"type":"list","items":[]},{"type":"blockquote","lines":[]}]}`, false},
		{"empty blocks array ok structurally", `{"format":"dr-blocks/v1","blocks":[]}`, false},
		// Restricted subset: doc-only block types are rejected.
		{"heading rejected", `{"format":"dr-blocks/v1","blocks":[{"type":"heading","level":1,"text":"x","id":"x"}]}`, true},
		{"table rejected", `{"format":"dr-blocks/v1","blocks":[{"type":"table","headerRow":false,"rows":[]}]}`, true},
		{"callout rejected", `{"format":"dr-blocks/v1","blocks":[{"type":"callout","variant":"info","spans":[]}]}`, true},
		{"divider rejected", `{"format":"dr-blocks/v1","blocks":[{"type":"divider"}]}`, true},
		{"image rejected", `{"format":"dr-blocks/v1","blocks":[{"type":"image","src":"x","alt":""}]}`, true},
		{"video rejected", `{"format":"dr-blocks/v1","blocks":[{"type":"video","src":"x"}]}`, true},
		{"file rejected", `{"format":"dr-blocks/v1","blocks":[{"type":"file","src":"x","name":"y"}]}`, true},
		{"unknown type", `{"format":"dr-blocks/v1","blocks":[{"type":"mystery"}]}`, true},
		// Structural failures.
		{"empty", ``, true},
		{"not an object", `[]`, true},
		{"wrong format", `{"format":"other","blocks":[]}`, true},
		{"missing blocks", `{"format":"dr-blocks/v1"}`, true},
		{"blocks null", `{"format":"dr-blocks/v1","blocks":null}`, true},
		{"block missing type", `{"format":"dr-blocks/v1","blocks":[{"spans":[]}]}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDrMessageJSON([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateDrMessageJSON(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestValidateDrMessageJSONCaps(t *testing.T) {
	// Too many blocks.
	var b strings.Builder
	b.WriteString(`{"format":"dr-blocks/v1","blocks":[`)
	for i := 0; i < drFeedbackMaxMessageBlocks+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"paragraph","spans":[]}`)
	}
	b.WriteString(`]}`)
	if err := validateDrMessageJSON([]byte(b.String())); err == nil {
		t.Fatalf("expected too-many-blocks error for %d blocks", drFeedbackMaxMessageBlocks+1)
	}

	// Too large (raw bytes over the cap).
	big := `{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"` +
		strings.Repeat("a", int(drFeedbackMaxMessageBytes)) + `"}]}]}`
	if err := validateDrMessageJSON([]byte(big)); err == nil {
		t.Fatalf("expected too-large error for %d bytes", len(big))
	}
}

func TestMessageSnippet(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			"paragraph spans concatenated",
			`{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"Hello "},{"text":"world"}]}]}`,
			"Hello world",
		},
		{
			"multiple blocks joined",
			`{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"one"}]},{"type":"paragraph","spans":[{"text":"two"}]}]}`,
			"one two",
		},
		{
			"blockquote lines",
			`{"format":"dr-blocks/v1","blocks":[{"type":"blockquote","lines":[[{"text":"a"}],[{"text":"b"}]]}]}`,
			"a b",
		},
		{
			"list items",
			`{"format":"dr-blocks/v1","blocks":[{"type":"list","ordered":true,"items":[[{"text":"first"}],[{"text":"second"}]]}]}`,
			"first second",
		},
		{
			"code block text with newlines collapsed",
			`{"format":"dr-blocks/v1","blocks":[{"type":"code","code":"line1\nline2"}]}`,
			"line1 line2",
		},
		{
			"whitespace collapsed",
			`{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"  spaced    out  "}]}]}`,
			"spaced out",
		},
		{"empty", `{"format":"dr-blocks/v1","blocks":[]}`, ""},
		{"invalid json", `not json`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageSnippet([]byte(tc.raw)); got != tc.want {
				t.Fatalf("messageSnippet(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestMessageSnippetTruncation(t *testing.T) {
	long := strings.Repeat("a", drFeedbackSnippetChars+50)
	raw := fmt.Sprintf(`{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":%q}]}]}`, long)
	got := messageSnippet([]byte(raw))
	if n := len([]rune(got)); n > drFeedbackSnippetChars {
		t.Fatalf("snippet length %d exceeds cap %d", n, drFeedbackSnippetChars)
	}
	if n := len([]rune(got)); n != drFeedbackSnippetChars {
		t.Fatalf("snippet length %d, want exactly %d", n, drFeedbackSnippetChars)
	}
}

func TestParseFeedbackLimit(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", drFeedbackDefaultPageLimit},
		{"garbage", drFeedbackDefaultPageLimit},
		{"0", drFeedbackDefaultPageLimit},
		{"-5", drFeedbackDefaultPageLimit},
		{"25", 25},
		{"100", 100},
		{"999", drFeedbackMaxPageLimit},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseFeedbackLimit(tc.in); got != tc.want {
				t.Fatalf("parseFeedbackLimit(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// ----------------------------------------------------------------------- //
// Broadcaster
// ----------------------------------------------------------------------- //

func TestBroadcasterSubscribeBroadcastUnsubscribe(t *testing.T) {
	b := newDrFeedbackBroadcaster()
	if b.subscriberCount() != 0 {
		t.Fatalf("new broadcaster has %d subscribers, want 0", b.subscriberCount())
	}
	ch, unsub := b.Subscribe()
	if b.subscriberCount() != 1 {
		t.Fatalf("after Subscribe count = %d, want 1", b.subscriberCount())
	}

	b.Broadcast(drFeedbackEvent{Type: "message", ConversationID: "abc"})
	select {
	case payload := <-ch:
		if !strings.Contains(string(payload), `"type":"message"`) || !strings.Contains(string(payload), `"conversationId":"abc"`) {
			t.Fatalf("unexpected payload: %s", payload)
		}
	default:
		t.Fatal("expected a broadcast payload, got none")
	}

	unsub()
	if b.subscriberCount() != 0 {
		t.Fatalf("after unsubscribe count = %d, want 0", b.subscriberCount())
	}
	// Channel is closed after unsubscribe.
	if _, open := <-ch; open {
		t.Fatal("expected channel to be closed after unsubscribe")
	}
	// Unsubscribe is idempotent (must not panic / double-close).
	unsub()

	// Broadcasting with no subscribers is a no-op (must not panic).
	b.Broadcast(drFeedbackEvent{Type: "conversation"})
}

func TestBroadcasterDropOnFull(t *testing.T) {
	b := newDrFeedbackBroadcaster()
	_, unsub := b.Subscribe()
	defer unsub()
	// Fill well past the 16-buffer without ever draining: a stalled client must
	// never block a send path, so extra broadcasts are dropped, not blocked.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Broadcast(drFeedbackEvent{Type: "message", ConversationID: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
		// Completed without blocking — correct.
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast blocked on a full subscriber buffer")
	}
}

func TestBroadcasterConcurrent(t *testing.T) {
	// Exercised under -race: many subscribers churning while broadcasts fire.
	b := newDrFeedbackBroadcaster()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsub := b.Subscribe()
			// Drain a few, then leave.
			for j := 0; j < 3; j++ {
				select {
				case <-ch:
				default:
				}
			}
			unsub()
		}()
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Broadcast(drFeedbackEvent{Type: "message", ConversationID: "c"})
		}()
	}
	wg.Wait()
	if b.subscriberCount() != 0 {
		t.Fatalf("after churn count = %d, want 0", b.subscriberCount())
	}
}
