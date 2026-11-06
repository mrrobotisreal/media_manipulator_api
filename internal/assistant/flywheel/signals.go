package flywheel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

// Signal is one reason a turn is worth a human look.
type Signal string

const (
	// SigThumbsDown: assistant_message_feedback rating = down (server).
	SigThumbsDown Signal = "thumbs_down"
	// SigInvalidOps: the reply row is an error with code invalid_ops (server).
	SigInvalidOps Signal = "invalid_ops"
	// SigOOSRephrased: the scope gate declined and the user wrote again within
	// RephraseWindow (server: the stored reply is the localized decline line).
	SigOOSRephrased Signal = "out_of_scope_rephrased"
	// SigFastUndo: the visitor undid the turn's edit within 60 s. ANALYTICS ONLY
	// (assistant_edits_undone is client-side): arrives through the optional
	// analytics export file.
	SigFastUndo Signal = "undo_within_60s"
	// SigNoClick: a guide answer with buttons that nobody clicked. ANALYTICS ONLY
	// (assistant_action_clicked / assistant_link_clicked): optional export file.
	SigNoClick Signal = "guide_no_click"
	// SigNoRoute: the server-side proxy for SigNoClick: a guide answer that
	// offered no button or site link and was followed by another message within
	// RephraseWindow.
	SigNoRoute Signal = "guide_no_route_followup"
)

// AllSignals lists the signals in report order.
var AllSignals = []Signal{SigThumbsDown, SigFastUndo, SigInvalidOps, SigOOSRephrased, SigNoClick, SigNoRoute}

// RephraseWindow is how soon a follow-up counts as a rephrase.
const RephraseWindow = 10 * time.Minute

// Flagged is a turn plus the signals it carries.
type Flagged struct {
	Turn    Turn
	Signals []Signal
}

// Has reports whether the turn carries s.
func (f Flagged) Has(s Signal) bool {
	for _, x := range f.Signals {
		if x == s {
			return true
		}
	}
	return false
}

// Analytics is the optional join of client-side signals onto turns, loaded from
// a JSONL file the operator exports from the analytics service. Each line:
//
//	{"signal":"undo_within_60s"|"guide_no_click","turn_id":"<uuid>"}   (or "message_id")
//
// Today's analytics events carry no turn or message id (only surface and a
// session-level tie, see the MM111 versions rollup), so the file is a join the
// operator builds; without it those two signals are simply absent.
type Analytics struct {
	byTurn map[string][]Signal
	byMsg  map[string][]Signal
}

// ParseAnalytics reads the JSONL export.
func ParseAnalytics(r io.Reader) (*Analytics, error) {
	a := &Analytics{byTurn: map[string][]Signal{}, byMsg: map[string][]Signal{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var l struct {
			Signal    string `json:"signal"`
			TurnID    string `json:"turn_id"`
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return nil, fmt.Errorf("analytics export line %d: %w", n, err)
		}
		s := Signal(l.Signal)
		if s != SigFastUndo && s != SigNoClick {
			return nil, fmt.Errorf("analytics export line %d: signal %q (want %s or %s)", n, l.Signal, SigFastUndo, SigNoClick)
		}
		switch {
		case l.TurnID != "":
			a.byTurn[l.TurnID] = append(a.byTurn[l.TurnID], s)
		case l.MessageID != "":
			a.byMsg[l.MessageID] = append(a.byMsg[l.MessageID], s)
		default:
			return nil, fmt.Errorf("analytics export line %d: needs turn_id or message_id", n)
		}
	}
	return a, sc.Err()
}

func (a *Analytics) signals(t Turn) []Signal {
	if a == nil {
		return nil
	}
	return append(append([]Signal(nil), a.byTurn[t.TurnID]...), a.byMsg[t.AssistantID]...)
}

// every tag OutOfScopeLine has a distinct line for (internal/assistant/static.go).
var oosTags = []string{"en", "uk", "ru", "he", "de", "es", "es-mx", "es-es", "fr", "it", "ja", "zh-cn", "zh-tw", "zh", "zh-hk",
	"ko", "ar", "hi", "pt", "pt-br", "pt-pt", "vi"}

var oosLines = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range oosTags {
		m[strings.TrimSpace(assistant.OutOfScopeLine(t))] = true
	}
	return m
}()

var siteLinkRe = regexp.MustCompile(`\]\(/|https?://(?:www\.)?media-manipulator\.com|(?:^|\s)/(?:tools|pricing|tutorials|studio|vector|photo)\b`)

// Select returns the turns in [from, to) that carry at least one signal.
func Select(turns []Turn, an *Analytics, from, to time.Time) []Flagged {
	var out []Flagged
	for _, t := range turns {
		if t.AssistantID == "" || t.At.Before(from) || !t.At.Before(to) {
			continue
		}
		var sigs []Signal
		add := func(s Signal) {
			for _, x := range sigs {
				if x == s {
					return
				}
			}
			sigs = append(sigs, s)
		}
		if t.Rating == "down" {
			add(SigThumbsDown)
		}
		if t.ErrorCode == assistant.CodeInvalid {
			add(SigInvalidOps)
		}
		followed := t.NextUserText != "" && t.NextUserAt.Sub(t.At) <= RephraseWindow && !t.NextUserAt.Before(t.At)
		isOOS := t.Surface == "guide" && oosLines[strings.TrimSpace(t.AssistantText)]
		if isOOS && followed {
			add(SigOOSRephrased)
		}
		if t.Surface == "guide" && !isOOS && t.Status == "complete" && t.ErrorCode == "" && followed &&
			len(t.ActionKinds) == 0 && !siteLinkRe.MatchString(t.AssistantText) {
			add(SigNoRoute)
		}
		for _, s := range an.signals(t) {
			add(s)
		}
		if len(sigs) == 0 {
			continue
		}
		sort.SliceStable(sigs, func(i, j int) bool { return sigRank(sigs[i]) < sigRank(sigs[j]) })
		out = append(out, Flagged{Turn: t, Signals: sigs})
	}
	return out
}

func sigRank(s Signal) int {
	for i, x := range AllSignals {
		if x == s {
			return i
		}
	}
	return len(AllSignals)
}
