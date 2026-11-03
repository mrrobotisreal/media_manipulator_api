package push

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/email"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

var placeholderRe = regexp.MustCompile(`\{\{[^}]*\}\}`)

func TestPushMessagesAllLocales(t *testing.T) {
	for _, loc := range email.Locales {
		tab := englishText
		if loc != email.DefaultLocale {
			var ok bool
			tab, ok = translatedMessages[loc]
			if !ok {
				t.Errorf("locale %s has no push messages", loc)
				continue
			}
		}
		for k, v := range map[string]string{
			"completedTitle": tab.CompletedTitle, "completedBody": tab.CompletedBody,
			"failedTitle": tab.FailedTitle, "failedBody": tab.FailedBody, "genericTool": tab.GenericTool,
		} {
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s: %s is empty", loc, k)
			}
		}
		for _, k := range []string{"completedTitle", "failedTitle"} {
			v := map[string]string{"completedTitle": tab.CompletedTitle, "failedTitle": tab.FailedTitle}[k]
			if placeholderRe.MatchString(v) {
				t.Errorf("%s: title %q has a placeholder", loc, v)
			}
		}
		for _, b := range []string{tab.CompletedBody, tab.FailedBody} {
			if got := placeholderRe.FindAllString(b, -1); len(got) != 1 || got[0] != toolPlaceholder {
				t.Errorf("%s: body %q placeholders %v, want exactly one {{tool}}", loc, b, got)
			}
		}
		for _, id := range ToolIDs {
			if strings.TrimSpace(tab.Tools[id]) == "" {
				t.Errorf("%s: no display name for tool %s", loc, id)
			}
		}
		m := Compose(loc, "ai_upscale", "j1", false)
		if strings.Contains(m.Body, "{{") {
			t.Errorf("%s: unrendered body %q", loc, m.Body)
		}
	}
	if len(translatedMessages) != len(email.Locales)-1 {
		t.Errorf("translatedMessages has %d locales, want %d", len(translatedMessages), len(email.Locales)-1)
	}
	if m := Compose("xx", "unknown_tool", "j", true); m.Body != "Your job failed — the operation was refunded." {
		t.Errorf("fallback = %q", m.Body)
	}
}

func TestPushPayloadHasNoFileName(t *testing.T) {
	m := Compose("en-us", "ai_upscale", "job-123", false)
	if m.Title != "Your result is ready" || m.Body != "AI Upscale finished. Tap to open." {
		t.Errorf("text: %q / %q", m.Title, m.Body)
	}
	want := map[string]string{"jobId": "job-123", "tool": "ai_upscale", "status": "completed", "url": "mediamanipulator://job/job-123"}
	if len(m.Data) != len(want) {
		t.Errorf("data keys: %v", m.Data)
	}
	for k, v := range want {
		if m.Data[k] != v {
			t.Errorf("data[%s] = %q, want %q", k, m.Data[k], v)
		}
	}
	// The composer has no file-name input; nothing but the tool table feeds the text.
	for _, loc := range email.Locales {
		for _, id := range ToolIDs {
			for _, failed := range []bool{false, true} {
				m := Compose(loc, id, "j", failed)
				if regexp.MustCompile(`\.(png|jpe?g|mp4|mov|wav|mp3|pdf)\b`).MatchString(m.Title + m.Body) {
					t.Errorf("%s/%s looks like it carries a file name: %q", loc, id, m.Body)
				}
			}
		}
	}
	if f := Compose("en-us", "transcribe", "j", true); f.Data["status"] != "failed" {
		t.Errorf("failed status = %q", f.Data["status"])
	}
}

func TestClassifyFCMErrorAndSenderGuards(t *testing.T) {
	if classifyFCMError(nil) != "" || classifyFCMError(errors.New("boom")) != ErrFailed {
		t.Error("generic errors must classify as failed")
	}
	var nilSender *FCMSender
	if _, err := nilSender.Send(context.Background(), []string{"t"}, Message{}); err == nil {
		t.Error("nil sender must error")
	}
	mm := buildMulticast([]string{"a"}, Message{Title: "t", Body: "b", Data: map[string]string{"k": "v"}})
	if mm.Android.Priority != "high" || mm.Android.Notification.ChannelID != "job_updates" || mm.APNS.Payload.Aps.Sound != "default" {
		t.Errorf("platform config: %+v", mm)
	}
}

// ---- notifier ----

type fakeStore struct {
	mu       sync.Mutex
	pref     bool
	tokens   []telemetry.PushToken
	user     *models.User
	disabled map[string]string
	err      error
}

func (f *fakeStore) PushJobFinishedPref(context.Context, string) (bool, error) { return f.pref, f.err }
func (f *fakeStore) ActivePushTokens(context.Context, string) ([]telemetry.PushToken, error) {
	return f.tokens, f.err
}
func (f *fakeStore) DisablePushToken(_ context.Context, tok, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disabled == nil {
		f.disabled = map[string]string{}
	}
	f.disabled[tok] = reason
	return nil
}
func (f *fakeStore) UserByID(context.Context, string) (*models.User, error) { return f.user, nil }

type fakeNext struct {
	mu    sync.Mutex
	calls []models.JobFinished
}

func (f *fakeNext) AIJobFinished(j models.JobFinished) {
	f.mu.Lock()
	f.calls = append(f.calls, j)
	f.mu.Unlock()
}

type fakeEmitter struct {
	mu     sync.Mutex
	events []analyticsemit.Event
}

func (f *fakeEmitter) Emit(e analyticsemit.Event) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

type fakeCounters struct {
	mu       sync.Mutex
	sends    map[string]int
	disabled map[string]int
}

func (f *fakeCounters) PushSend(p, o string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends == nil {
		f.sends = map[string]int{}
	}
	f.sends[p+"/"+o]++
}
func (f *fakeCounters) PushTokenDisabled(r string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disabled == nil {
		f.disabled = map[string]int{}
	}
	f.disabled[r]++
}

type rig struct {
	n      *Notifier
	sender *FakeSender
	store  *fakeStore
	next   *fakeNext
	em     *fakeEmitter
	cnt    *fakeCounters
}

func newRig(tokens ...telemetry.PushToken) *rig {
	r := &rig{sender: &FakeSender{}, store: &fakeStore{pref: true, tokens: tokens, user: &models.User{ID: "u1", Locale: "de-de", FirebaseUID: "fb1"}},
		next: &fakeNext{}, em: &fakeEmitter{}, cnt: &fakeCounters{}}
	r.n = NewNotifier(r.sender, r.store, r.next, r.em, r.cnt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r
}

var job = models.JobFinished{
	UserID: "u1", JobID: "job-1", Tool: "ai_upscale",
	VisitorID: "11111111-1111-4111-8111-111111111111", SessionID: "22222222-2222-4222-8222-222222222222",
}

func (r *rig) run(j models.JobFinished) { r.n.AIJobFinished(j); r.n.Wait() }

func tok(t, platform string) telemetry.PushToken {
	return telemetry.PushToken{Token: t, Platform: platform, LastSeenAt: time.Now()}
}

func TestNotifierPushAcceptedSkipsEmail(t *testing.T) {
	r := newRig(tok("a", "android"), tok("b", "ios"))
	r.sender.Codes = map[string]string{"b": ErrFailed} // one device is enough
	r.run(job)
	if len(r.next.calls) != 0 {
		t.Fatalf("email called after an accepted push: %v", r.next.calls)
	}
	calls := r.sender.Calls()
	if len(calls) != 1 || len(calls[0].Tokens) != 2 {
		t.Fatalf("sender calls: %+v", calls)
	}
	if calls[0].Msg.Data["jobId"] != "job-1" || calls[0].Msg.Data["status"] != "completed" {
		t.Errorf("payload: %+v", calls[0].Msg)
	}
	if calls[0].Msg.Title == englishText.CompletedTitle {
		t.Errorf("account locale de-de not used: %q", calls[0].Msg.Title)
	}
}

func TestNotifierPushOffFallsBackToEmail(t *testing.T) {
	r := newRig(tok("a", "android"))
	r.store.pref = false
	r.run(job)
	if len(r.sender.Calls()) != 0 || len(r.next.calls) != 1 {
		t.Fatalf("push=%d email=%d, want 0/1", len(r.sender.Calls()), len(r.next.calls))
	}
	if r.next.calls[0].JobID != "job-1" || r.next.calls[0].Tool != "ai_upscale" {
		t.Errorf("email got %+v", r.next.calls[0])
	}
}

func TestNotifierNoTokensFallsBackToEmail(t *testing.T) {
	r := newRig()
	r.run(job)
	if len(r.sender.Calls()) != 0 || len(r.next.calls) != 1 {
		t.Fatalf("push=%d email=%d", len(r.sender.Calls()), len(r.next.calls))
	}
}

func TestNotifierAllFailedFallsBackToEmail(t *testing.T) {
	r := newRig(tok("a", "android"), tok("b", "ios"))
	r.sender.Codes = map[string]string{"a": ErrFailed, "b": ErrUnregistered}
	r.run(job)
	if len(r.next.calls) != 1 {
		t.Fatalf("email calls = %d, want 1 when nothing was accepted", len(r.next.calls))
	}
	// A whole-call failure (e.g. missing IAM role) also falls back and disables nothing.
	r2 := newRig(tok("a", "android"))
	r2.sender.Err = errors.New("permission denied")
	r2.run(job)
	if len(r2.next.calls) != 1 || len(r2.store.disabled) != 0 {
		t.Errorf("whole-call failure: email=%d disabled=%v", len(r2.next.calls), r2.store.disabled)
	}
	if r2.cnt.sends["android/failed"] != 1 {
		t.Errorf("counters: %v", r2.cnt.sends)
	}
}

func TestNotifierDisablesUnregisteredTokens(t *testing.T) {
	r := newRig(tok("good", "android"), tok("gone", "ios"), tok("bad", "android"), tok("flaky", "ios"))
	r.sender.Codes = map[string]string{"gone": ErrUnregistered, "bad": ErrInvalid, "flaky": ErrFailed}
	r.run(job)
	if r.store.disabled["gone"] != "unregistered" || r.store.disabled["bad"] != "invalid" || len(r.store.disabled) != 2 {
		t.Errorf("disabled = %v (a merely failed token must stay enabled)", r.store.disabled)
	}
	if r.cnt.disabled["unregistered"] != 1 || r.cnt.disabled["invalid"] != 1 {
		t.Errorf("disabled counter: %v", r.cnt.disabled)
	}
	if len(r.next.calls) != 0 {
		t.Error("good token accepted: no email")
	}
}

func TestNotifierFailedJobMessage(t *testing.T) {
	r := newRig(tok("a", "android"))
	r.store.user.Locale = ""
	j := job
	j.Failed = true
	r.run(j)
	m := r.sender.Calls()[0].Msg
	if m.Title != "Your job didn't finish" || m.Body != "AI Upscale failed — the operation was refunded." || m.Data["status"] != "failed" {
		t.Errorf("failed message: %+v", m)
	}
}

func TestServerPushSentEmittedPerToken(t *testing.T) {
	r := newRig(tok("a", "android"), tok("gone", "ios"), tok("flaky", "ios"))
	r.sender.Codes = map[string]string{"gone": ErrUnregistered, "flaky": ErrFailed}
	r.run(job)
	if len(r.em.events) != 3 {
		t.Fatalf("events = %d, want 3 (one per token)", len(r.em.events))
	}
	got := map[string]int{}
	for _, e := range r.em.events {
		if e.EventName != analyticsemit.ServerPushSent {
			t.Errorf("event name %q", e.EventName)
		}
		if e.VisitorID != job.VisitorID || e.SessionID != job.SessionID {
			t.Errorf("identity not the job creator's: %+v", e)
		}
		if e.PersonHint == nil || e.PersonHint.FirebaseUID != "fb1" {
			t.Errorf("person hint: %+v", e.PersonHint)
		}
		p := e.Properties
		if p["tool"] != "ai_upscale" || p["status"] != "completed" {
			t.Errorf("props: %v", p)
		}
		if len(p) != 4 {
			t.Errorf("props must be exactly tool,status,platform,outcome: %v", p)
		}
		got[p["platform"].(string)+"/"+p["outcome"].(string)]++
	}
	if got["android/sent"] != 1 || got["ios/token_disabled"] != 1 || got["ios/failed"] != 1 {
		t.Errorf("outcomes: %v", got)
	}
	if r.cnt.sends["android/sent"] != 1 || r.cnt.sends["ios/token_disabled"] != 1 {
		t.Errorf("counters: %v", r.cnt.sends)
	}
}

func TestNotifierSurvivesPanicAndStoreErrors(t *testing.T) {
	r := newRig(tok("a", "android"))
	r.store.err = errors.New("db down")
	r.run(job)
	if len(r.next.calls) != 1 {
		t.Errorf("store error must fall back to email, got %d", len(r.next.calls))
	}
	var nilN *Notifier
	nilN.AIJobFinished(job) // must not panic
}
