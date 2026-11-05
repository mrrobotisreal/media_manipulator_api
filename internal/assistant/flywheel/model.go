package flywheel

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// MessageRow is one assistant_messages row joined with its conversation's
// surface and the message's feedback (Rating "" when none).
type MessageRow struct {
	ID             string
	ConversationID string
	Surface        string
	Seq            int
	TurnID         string
	Role           string // user | assistant | tool
	Content        json.RawMessage
	Status         string
	PromptVersion  string
	CreatedAt      time.Time
	Rating         string // up | down | ""
	Reason         string
}

// storedContent mirrors the keys of assistant.storedContent the flywheel reads.
// The digest is deliberately absent: it is never read.
type storedContent struct {
	Text      string `json:"text"`
	Page      string `json:"page"`
	Error     string `json:"error"`
	ToolCalls []struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"toolCalls"`
	Actions []struct {
		Kind    string `json:"kind"`
		Summary string `json:"summary"`
	} `json:"actions"`
}

// Turn is one user message and Medi's reply, assembled from the rows that share
// a turn_id. A continuation (an edit_result round) has no user row and inherits
// the previous turn's UserText.
type Turn struct {
	ConversationID string
	TurnID         string
	Surface        string
	At             time.Time
	UserText       string
	Page           string
	Continuation   bool

	AssistantID   string
	AssistantText string
	Status        string
	ErrorCode     string
	PromptVersion string
	ToolCalls     []ToolCall
	ActionKinds   []string

	Rating string
	Reason string

	// NextUserText / NextUserAt are the conversation's next user message.
	NextUserText string
	NextUserAt   time.Time
}

// ToolCall is a stored model tool call.
type ToolCall struct {
	Name string
	Args string
}

// BuildTurns groups message rows (any order) into turns, per conversation in
// seq order.
func BuildTurns(rows []MessageRow) []Turn {
	byConv := map[string][]MessageRow{}
	var convs []string
	for _, r := range rows {
		if _, ok := byConv[r.ConversationID]; !ok {
			convs = append(convs, r.ConversationID)
		}
		byConv[r.ConversationID] = append(byConv[r.ConversationID], r)
	}
	sort.Strings(convs)
	var out []Turn
	for _, cid := range convs {
		rs := byConv[cid]
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].Seq < rs[j].Seq })
		var conv []Turn
		idx := map[string]int{}
		lastUser, lastPage := "", ""
		for _, r := range rs {
			i, ok := idx[r.TurnID]
			if !ok {
				conv = append(conv, Turn{ConversationID: cid, TurnID: r.TurnID, Surface: r.Surface, At: r.CreatedAt, Continuation: true})
				i = len(conv) - 1
				idx[r.TurnID] = i
			}
			t := &conv[i]
			var sc storedContent
			_ = json.Unmarshal(r.Content, &sc)
			switch r.Role {
			case "user":
				if t.UserText == "" {
					t.UserText, t.Page, t.Continuation, t.At = sc.Text, sc.Page, false, r.CreatedAt
					lastUser, lastPage = sc.Text, sc.Page
				}
			case "assistant":
				t.AssistantID, t.AssistantText, t.Status, t.ErrorCode, t.PromptVersion = r.ID, sc.Text, r.Status, sc.Error, r.PromptVersion
				if t.UserText == "" {
					t.UserText, t.Page = lastUser, lastPage
				}
				t.ToolCalls, t.ActionKinds = nil, nil
				for _, c := range sc.ToolCalls {
					t.ToolCalls = append(t.ToolCalls, ToolCall{Name: c.Name, Args: c.Arguments})
				}
				for _, a := range sc.Actions {
					t.ActionKinds = append(t.ActionKinds, a.Kind)
				}
				if r.Rating != "" {
					t.Rating, t.Reason = r.Rating, r.Reason
				}
			}
		}
		next, nextAt := "", time.Time{}
		for i := len(conv) - 1; i >= 0; i-- {
			conv[i].NextUserText, conv[i].NextUserAt = next, nextAt
			if !conv[i].Continuation {
				next, nextAt = conv[i].UserText, conv[i].At
			}
		}
		out = append(out, conv...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].TurnID < out[j].TurnID
	})
	return out
}

// ConsultedDocs are the doc ids the model itself read or sent the visitor to:
// get_recipe ids, get_tool_page and open_tool slugs (tools/<slug>).
func (t Turn) ConsultedDocs() []string {
	var ids []string
	for _, c := range t.ToolCalls {
		var a struct{ ID, Slug string }
		_ = json.Unmarshal([]byte(c.Args), &a)
		switch c.Name {
		case "get_recipe":
			if a.ID != "" {
				ids = append(ids, a.ID)
			}
		case "get_tool_page", "open_tool":
			if s := strings.TrimPrefix(a.Slug, "tools/"); s != "" {
				ids = append(ids, "tools/"+s)
			}
		}
	}
	return ids
}
