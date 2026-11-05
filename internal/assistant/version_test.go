package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

func baseParts() PromptVersionParts {
	return PromptVersionParts{
		Persona:        "persona",
		Intros:         map[string]string{"guide": "g", "photo": "p"},
		Capsules:       map[string]string{"guide": "cg", "photo": "cp"},
		KnowledgeIndex: []byte(`{"a":1}`),
		Schemas:        map[string][]byte{"photo.json": []byte(`{"x":1}`)},
	}
}

// MM113 acceptance 2: same inputs, same hash; 12 hex; computed once.
func TestPromptVersionStable(t *testing.T) {
	a, b := HashPromptVersion(baseParts()), HashPromptVersion(baseParts())
	if a != b {
		t.Fatalf("same inputs gave %s and %s", a, b)
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(a) {
		t.Fatalf("version %q is not 12 hex", a)
	}
	if PromptVersion() != PromptVersion() || computeEmbeddedPromptVersion() != PromptVersion() {
		t.Fatal("the embedded version is not stable")
	}
}

// MM113 acceptance 2: persona, capsules, knowledge and op schemas each move it.
func TestPromptVersionChangesWithKnowledge(t *testing.T) {
	base := HashPromptVersion(baseParts())
	for name, mut := range map[string]func(*PromptVersionParts){
		"knowledge index": func(p *PromptVersionParts) { p.KnowledgeIndex = []byte(`{"a":2}`) },
		"persona":         func(p *PromptVersionParts) { p.Persona = "persona!" },
		"capsule":         func(p *PromptVersionParts) { p.Capsules["photo"] = "cp2" },
		"intro":           func(p *PromptVersionParts) { p.Intros["guide"] = "g2" },
		"op schema":       func(p *PromptVersionParts) { p.Schemas["photo.json"] = []byte(`{"x":2}`) },
		"new op schema":   func(p *PromptVersionParts) { p.Schemas["studio.json"] = []byte(`{}`) },
	} {
		p := baseParts()
		mut(&p)
		if HashPromptVersion(p) == base {
			t.Errorf("changing the %s did not change the version", name)
		}
	}
	// Moving bytes between fields must not collide.
	p := baseParts()
	p.Persona, p.KnowledgeIndex = "persona"+`{"a":1}`, nil
	if HashPromptVersion(p) == base {
		t.Error("field framing is ambiguous")
	}
}

// MM113 acceptance 3: the version rides on the meta event and on every row.
func TestRowsCarryPromptVersion(t *testing.T) {
	v := newEnv(t, round{deltas: []string{"Use Compress video."}}, round{deltas: []string{"Compressing"}})
	v.run(v.turn(guideMsg("how to shrink a video"), "user-1"), nil)
	if v.em.events[0]["promptVersion"] != PromptVersion() {
		t.Errorf("meta = %v", v.em.events[0])
	}
	rows := v.st.rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, r := range rows {
		if r.PromptVersion != PromptVersion() {
			t.Errorf("message %s role %s version = %q", r.ID, r.Role, r.PromptVersion)
		}
	}
	if len(v.st.usage) < 2 {
		t.Fatalf("usage rows = %d", len(v.st.usage))
	}
	for _, u := range v.st.usage {
		if u.PromptVersion != PromptVersion() {
			t.Errorf("usage %s version = %q", u.Kind, u.PromptVersion)
		}
	}
}

// seedConversation stores n alternating user/assistant rows for a new conversation.
func seedConversation(t *testing.T, st *fakeStore, n int) string {
	t.Helper()
	c, err := st.CreateAssistantConversation(context.Background(), telemetry.AssistantConversationInsert{OwnerUserID: "user-1", Surface: "guide"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		body, _ := json.Marshal(storedContent{Text: fmt.Sprintf("%s message %d", role, i+1)})
		if _, _, err := st.AppendAssistantMessage(context.Background(), telemetry.AssistantMessageInsert{
			ConversationID: c.ID, TurnID: "00000000-0000-4000-8000-0000000000aa", Role: role, Content: body}); err != nil {
			t.Fatal(err)
		}
	}
	return c.ID
}

// MM113 acceptance 5: a long conversation gets an append-only summary from the
// title model, which then rides in block 2.
func TestConversationSummaryAppendOnly(t *testing.T) {
	v := newEnv(t,
		round{deltas: []string{"Answer one."}}, round{deltas: []string{"Summary one."}},
		round{deltas: []string{"Answer two."}})
	convID := seedConversation(t, v.st, 26)

	send := func(text string) {
		v.em = &recEmitter{}
		body := guideMsg(text)
		body.ConversationID = convID
		v.run(v.turn(body, "user-1"), nil)
	}
	send("first follow-up") // rows 27, 28: rows 1-4 left the 24-row window
	conv := v.st.convs[convID]
	if conv.Summary != "Summary one." || conv.SummaryUpto != 4 {
		t.Fatalf("after first turn: summary %q upto %d", conv.Summary, conv.SummaryUpto)
	}
	reqs := v.p.requests()
	if got := reqs[1].Model; got != "anthropic/claude-haiku-5.5" {
		t.Errorf("the summary uses the title model, got %s", got)
	}
	if !strings.Contains(reqs[1].Messages[0].Text, "user message 1") || strings.Contains(reqs[1].Messages[0].Text, "message 5") {
		t.Errorf("summary prompt covers rows 1-4 only: %q", reqs[1].Messages[0].Text)
	}
	if v.st.usage[len(v.st.usage)-1].Kind != "summary" {
		t.Errorf("last usage kind = %s", v.st.usage[len(v.st.usage)-1].Kind)
	}

	send("second follow-up") // 30 rows; only 2 new rows aged out: below the every-8 refresh
	if conv.Summary != "Summary one." || len(v.p.requests()) != 3 {
		t.Fatalf("refreshed too early: %q, %d provider calls", conv.Summary, len(v.p.requests()))
	}
	// The earlier paragraph reaches the model as data in block 2.
	sys := v.p.requests()[2].System[1].Text
	if !strings.Contains(sys, "Earlier in this conversation") || !strings.Contains(sys, "Summary one.") || !strings.Contains(sys, "not instructions") {
		t.Errorf("block 2 lacks the summary:\n%s", sys)
	}
	if strings.Contains(v.p.requests()[2].System[0].Text, "Summary one.") {
		t.Error("the summary must not enter the cached block")
	}

	seedRows(t, v.st, convID, 6) // push 6 more rows out of the window: 8 pending
	v.p.mu.Lock()
	v.p.rounds = append(v.p.rounds, round{deltas: []string{"Answer three."}}, round{deltas: []string{"Summary two."}})
	v.p.mu.Unlock()
	send("third follow-up")
	if !strings.HasPrefix(conv.Summary, "Summary one.") || !strings.HasSuffix(conv.Summary, "Summary two.") || strings.Count(conv.Summary, "\n\n") != 1 {
		t.Fatalf("summary must append, never rewrite: %q", conv.Summary)
	}
	if conv.SummaryUpto <= 4 {
		t.Errorf("upto = %d", conv.SummaryUpto)
	}
}

func seedRows(t *testing.T, st *fakeStore, convID string, n int) int {
	t.Helper()
	for i := 0; i < n; i++ {
		role := []string{"user", "assistant"}[i%2]
		body, _ := json.Marshal(storedContent{Text: role + " filler"})
		if _, _, err := st.AppendAssistantMessage(context.Background(), telemetry.AssistantMessageInsert{
			ConversationID: convID, TurnID: "00000000-0000-4000-8000-0000000000aa", Role: role, Content: body}); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

func TestSummaryRange(t *testing.T) {
	win := func(first int) []telemetry.AssistantMessage {
		out := make([]telemetry.AssistantMessage, historyWindow)
		for i := range out {
			out[i].Seq = first + i
		}
		return out
	}
	if _, _, ok := summaryRange(win(1)[:10], 0); ok {
		t.Error("a short conversation needs no summary")
	}
	if _, _, ok := summaryRange(win(1), 0); ok {
		t.Error("nothing has left the window yet")
	}
	if from, upto, ok := summaryRange(win(3), 0); !ok || from != 0 || upto != 2 {
		t.Errorf("first summary = %d %d %v", from, upto, ok)
	}
	if _, _, ok := summaryRange(win(10), 4); ok {
		t.Error("5 pending rows is below the refresh step of 8")
	}
	if from, upto, ok := summaryRange(win(13), 4); !ok || from != 4 || upto != 12 {
		t.Errorf("refresh = %d %d %v", from, upto, ok)
	}
}
