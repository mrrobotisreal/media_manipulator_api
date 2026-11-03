package push

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// SendTimeout bounds one FCM fan-out.
const SendTimeout = 15 * time.Second

// Store is the persistence the notifier needs (*telemetry.Store; fake in tests).
type Store interface {
	PushJobFinishedPref(ctx context.Context, userID string) (bool, error)
	ActivePushTokens(ctx context.Context, userID string) ([]telemetry.PushToken, error)
	DisablePushToken(ctx context.Context, token, reason string) error
	UserByID(ctx context.Context, id string) (*models.User, error)
}

// Fallback is the notifier wrapped by this one (the email service).
type Fallback interface {
	AIJobFinished(n models.JobFinished)
}

// Emitter receives server_push_sent (*analyticsemit.Emitter).
type Emitter interface {
	Emit(analyticsemit.Event)
}

// Counters is the metrics slice (*metrics.Registry; nil-safe there).
type Counters interface {
	PushSend(platform, outcome string)
	PushTokenDisabled(reason string)
}

// Notifier pushes first and falls back to email. It implements
// services.JobFinishedNotifier. The rule set (AI, > 120 s, owner, unwatched)
// stays in services/job_notify.go; this only picks the channel:
//
//	push_job_finished on and >= 1 token accepted -> push only (no email);
//	otherwise                                    -> the email notifier, which
//	                                                applies emailAiJobFinished.
type Notifier struct {
	Sender   Sender
	Store    Store
	Next     Fallback
	Emitter  Emitter
	Counters Counters
	Log      *slog.Logger

	wg sync.WaitGroup
}

// NewNotifier builds the notifier. next, emitter and counters may be nil.
func NewNotifier(sender Sender, store Store, next Fallback, emitter Emitter, counters Counters, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{Sender: sender, Store: store, Next: next, Emitter: emitter, Counters: counters, Log: log}
}

// Wait blocks until in-flight work finished (tests, shutdown).
func (n *Notifier) Wait() {
	if n != nil {
		n.wg.Wait()
	}
}

// AIJobFinished returns immediately; the work runs in a goroutine and can never
// fail or slow the job manager.
func (n *Notifier) AIJobFinished(j models.JobFinished) {
	if n == nil || j.UserID == "" || j.JobID == "" {
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		accepted := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					n.Log.Error("push: notifier panic", "panic", r)
				}
			}()
			accepted = n.push(j)
		}()
		if !accepted && n.Next != nil {
			func() {
				defer func() { _ = recover() }()
				n.Next.AIJobFinished(j)
			}()
		}
	}()
}

// push returns true when at least one device accepted the message.
func (n *Notifier) push(j models.JobFinished) bool {
	if n.Sender == nil || n.Store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), SendTimeout)
	defer cancel()
	on, err := n.Store.PushJobFinishedPref(ctx, j.UserID)
	if err != nil || !on {
		return false
	}
	tokens, err := n.Store.ActivePushTokens(ctx, j.UserID)
	if err != nil || len(tokens) == 0 {
		return false
	}
	userLocale, firebaseUID := "", ""
	if u, err := n.Store.UserByID(ctx, j.UserID); err == nil && u != nil {
		userLocale, firebaseUID = u.Locale, u.FirebaseUID
	}
	// One fan-out per locale (a token's own locale wins over the account's).
	byLocale := map[string][]telemetry.PushToken{}
	var order []string
	for _, t := range tokens {
		loc := t.Locale
		if loc == "" {
			loc = userLocale
		}
		if _, seen := byLocale[loc]; !seen {
			order = append(order, loc)
		}
		byLocale[loc] = append(byLocale[loc], t)
	}
	accepted := false
	for _, loc := range order {
		group := byLocale[loc]
		strs := make([]string, len(group))
		platform := map[string]string{}
		for i, t := range group {
			strs[i] = t.Token
			platform[t.Token] = t.Platform
		}
		results, err := n.Sender.Send(ctx, strs, Compose(loc, j.Tool, j.JobID, j.Failed))
		if err != nil {
			// Whole-call failure (e.g. missing IAM role): log once per job, never
			// disable tokens, fall back to email.
			n.Log.Error("push: FCM send failed", "error", err.Error(), "jobId", j.JobID)
			for _, t := range group {
				n.record(j, firebaseUID, t.Platform, "failed")
			}
			continue
		}
		for _, r := range results {
			outcome := "sent"
			switch {
			case r.OK:
				accepted = true
			case r.ErrCode == ErrUnregistered || r.ErrCode == ErrInvalid:
				outcome = "token_disabled"
				if err := n.Store.DisablePushToken(ctx, r.Token, r.ErrCode); err != nil {
					n.Log.Warn("push: disabling token failed", "error", err.Error())
				}
				if n.Counters != nil {
					n.Counters.PushTokenDisabled(r.ErrCode)
				}
			default:
				outcome = "failed"
			}
			n.record(j, firebaseUID, platform[r.Token], outcome)
		}
	}
	return accepted
}

func (n *Notifier) record(j models.JobFinished, firebaseUID, platform, outcome string) {
	if n.Counters != nil {
		n.Counters.PushSend(platform, outcome)
	}
	if n.Emitter == nil {
		return
	}
	status := "completed"
	if j.Failed {
		status = "failed"
	}
	ev := analyticsemit.Event{
		EventName: analyticsemit.ServerPushSent,
		EventTS:   time.Now().UTC(),
		VisitorID: j.VisitorID,
		SessionID: j.SessionID,
		Context:   map[string]any{"job_id": j.JobID},
		Properties: map[string]any{
			"tool": j.Tool, "status": status, "platform": platform, "outcome": outcome,
		},
	}
	if j.Tool != "" {
		ev.Context["tool_slug"] = j.Tool
	}
	if firebaseUID != "" {
		ev.PersonHint = &analyticsemit.PersonHint{FirebaseUID: firebaseUID}
	}
	n.Emitter.Emit(ev)
}
