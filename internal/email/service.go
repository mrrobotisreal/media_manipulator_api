package email

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Emitter is the slice of *analyticsemit.Emitter the service uses (nil-safe).
type Emitter interface {
	Emit(analyticsemit.Event)
}

// Tunables with the ticket's values.
const (
	// SendTimeout bounds one provider call.
	SendTimeout = 10 * time.Second
	// MaxAttempts is the total tries per log row across all triggers.
	MaxAttempts = 3
	// DigestWindow is how far ahead the digest looks, and the dedupe bucket size.
	DigestWindow = 3 * 24 * time.Hour
	// DigestTopNames is how many names the digest lists.
	DigestTopNames = 5
)

// Service sends the five emails. All trigger methods return immediately; the
// work runs in a goroutine and can never fail or slow the caller.
type Service struct {
	Store    Store
	Sender   Sender
	Renderer *Renderer
	Emitter  Emitter
	BaseURL  string
	Log      *slog.Logger

	// Backoff[i] is the wait before retry i+1 (tests shrink it).
	Backoff []time.Duration
	Now     func() time.Time

	wg sync.WaitGroup
}

// New builds a Service with production defaults.
func New(store Store, sender Sender, baseURL string, emitter Emitter, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		Store: store, Sender: sender, Renderer: MustRenderer(), Emitter: emitter,
		BaseURL: baseURL, Log: log,
		Backoff: []time.Duration{2 * time.Second, 8 * time.Second},
	}
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// enabled is false for the NoopSender (and a nil service).
func (s *Service) enabled() bool {
	if s == nil || s.Sender == nil {
		return false
	}
	if e, ok := s.Sender.(interface{ Enabled() bool }); ok {
		return e.Enabled()
	}
	return true
}

// Wait blocks until every in-flight send finished (tests, graceful shutdown).
func (s *Service) Wait() {
	if s != nil {
		s.wg.Wait()
	}
}

// Request is one email to deliver.
type Request struct {
	UserID    string
	To        string
	Name      string
	Locale    string
	Kind      Kind
	DedupeKey string
	Data      Data
	// Optional analytics identity (the welcome trigger has the browser's ids).
	VisitorID, SessionID, FirebaseUID string
}

// dispatch runs deliver in a goroutine. A disabled service still routes one call
// through the NoopSender so "email disabled" is logged once, and touches nothing
// else (no database, no analytics).
func (s *Service) dispatch(req Request) {
	if s == nil {
		return
	}
	if !s.enabled() {
		if s.Sender != nil {
			_, _ = s.Sender.Send(context.Background(), Message{})
		}
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				s.logger().Error("email: panic in send", "kind", req.Kind, "panic", fmt.Sprint(r))
			}
		}()
		s.deliver(context.Background(), req)
	}()
}

// deliver reserves the dedupe row, renders and sends with bounded retries, and
// records the outcome. It returns the final status (sent, failed, skipped).
func (s *Service) deliver(ctx context.Context, req Request) string {
	to := strings.TrimSpace(req.To)
	if to == "" || req.UserID == "" || strings.ContainsAny(to, "\r\n") {
		return s.finish(req, StatusSkipped)
	}
	locale := LocaleOrDefault(req.Locale)
	res, ok, err := s.Store.ReserveEmail(ctx, req.UserID, req.Kind, req.DedupeKey, locale, MaxAttempts)
	if err != nil {
		s.logger().Warn("email: reserve failed", "kind", req.Kind, "error", err.Error())
		return s.finish(req, StatusFailed)
	}
	if !ok {
		return s.finish(req, StatusSkipped) // already sent or in flight
	}

	data := req.Data
	data.Name = req.Name
	if data.BaseURL == "" {
		data.BaseURL = s.BaseURL
	}
	out, err := s.Renderer.Render(req.Kind, locale, data)
	if err != nil {
		_ = s.Store.FinishEmail(ctx, res.ID, StatusFailed, "", err.Error(), MaxAttempts)
		return s.finish(req, StatusFailed)
	}
	msg := Message{
		To: to, Subject: out.Subject, Text: out.Text, HTML: out.HTML,
		IdempotencyKey: req.UserID + "/" + string(req.Kind) + "/" + req.DedupeKey,
	}
	if out.PreferencesURL != "" {
		msg.Headers = map[string]string{"List-Unsubscribe": "<" + out.PreferencesURL + ">"}
	}

	attempts := res.Attempts
	var lastErr error
	for attempts < MaxAttempts {
		attempts++
		actx, cancel := context.WithTimeout(ctx, SendTimeout)
		var providerID string
		providerID, lastErr = s.Sender.Send(actx, msg)
		cancel()
		if lastErr == nil {
			if ferr := s.Store.FinishEmail(ctx, res.ID, StatusSent, providerID, "", attempts); ferr != nil {
				s.logger().Warn("email: could not record send", "kind", req.Kind, "error", ferr.Error())
			}
			return s.finish(req, StatusSent)
		}
		if IsPermanent(lastErr) {
			attempts = MaxAttempts
			break
		}
		if attempts < MaxAttempts {
			wait := time.Duration(0)
			if i := attempts - res.Attempts - 1; i >= 0 && i < len(s.Backoff) {
				wait = s.Backoff[i]
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				attempts = MaxAttempts
			}
		}
	}
	errText := "unknown"
	if lastErr != nil {
		errText = truncate(lastErr.Error(), 300)
	}
	_ = s.Store.FinishEmail(ctx, res.ID, StatusFailed, "", errText, attempts)
	s.logger().Warn("email: send failed", "kind", req.Kind, "attempts", attempts)
	return s.finish(req, StatusFailed)
}

// finish emits server_email_sent {kind, status}. Server events without browser
// ids are dropped by analyticsemit (a known lower bound, same as billing).
func (s *Service) finish(req Request, status string) string {
	if s.Emitter != nil && status != StatusSkipped {
		ev := analyticsemit.Event{
			EventName:  analyticsemit.ServerEmailSent,
			EventTS:    s.now(),
			VisitorID:  req.VisitorID,
			SessionID:  req.SessionID,
			Properties: map[string]any{"kind": string(req.Kind), "status": status},
		}
		if req.FirebaseUID != "" {
			ev.PersonHint = &analyticsemit.PersonHint{FirebaseUID: req.FirebaseUID}
		}
		s.Emitter.Emit(ev)
	}
	return status
}

func addressable(u *models.User) bool {
	return u != nil && u.ID != "" && strings.TrimSpace(u.Email) != "" && u.EmailVerified
}

// Welcome is sent once, when the account row is first created. Only to a
// verified address: an unverified one could belong to someone else.
func (s *Service) Welcome(u *models.User, visitorID, sessionID string) {
	if !addressable(u) {
		return
	}
	s.dispatch(Request{
		UserID: u.ID, To: u.Email, Name: u.DisplayName, Locale: u.Locale, Kind: KindWelcome,
		DedupeKey: "once", VisitorID: visitorID, SessionID: sessionID, FirebaseUID: u.FirebaseUID,
	})
}

// PaymentFailed is sent from the Stripe invoice.payment_failed handler. Stripe
// retries a failed invoice several times, each with its own event id, so the
// dedupe key is a 3-day window: at most one of these per user per window.
func (s *Service) PaymentFailed(u *models.User) {
	if !addressable(u) {
		return
	}
	s.dispatch(Request{
		UserID: u.ID, To: u.Email, Name: u.DisplayName, Locale: u.Locale, Kind: KindPaymentFailed,
		DedupeKey: WindowKey(s.now()), FirebaseUID: u.FirebaseUID,
	})
}

// AccountDeleted confirms a finished deletion. u is the row captured BEFORE the
// delete; the message holds no link into the account.
func (s *Service) AccountDeleted(u *models.User) {
	if !addressable(u) {
		return
	}
	s.dispatch(Request{
		UserID: u.ID, To: u.Email, Name: u.DisplayName, Locale: u.Locale, Kind: KindAccountDeleted,
		DedupeKey: "deleted",
	})
}

// AIJobFinished is sent for a long AI job whose owner had no tab polling it.
// The caller has already applied the duration and poll rules; this applies the
// owner's opt-in. One email per job: the job id is the dedupe key.
func (s *Service) AIJobFinished(userID, jobID string, failed bool) {
	if s == nil || userID == "" || jobID == "" {
		return
	}
	if !s.enabled() {
		s.dispatch(Request{}) // logs "email disabled" once
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { _ = recover() }()
		ctx := context.Background()
		aiPref, _, err := s.Store.EmailPrefs(ctx, userID)
		if err != nil || !aiPref {
			return
		}
		u, err := s.Store.UserByID(ctx, userID)
		if err != nil || !addressable(u) {
			return
		}
		s.deliver(ctx, Request{
			UserID: u.ID, To: u.Email, Name: u.DisplayName, Locale: u.Locale, Kind: KindAIJobFinished,
			DedupeKey: jobID, Data: Data{Failed: failed}, FirebaseUID: u.FirebaseUID,
		})
	}()
}

// WindowKey buckets time into 3-day windows (UTC epoch days / 3).
func WindowKey(t time.Time) string {
	return fmt.Sprintf("w%d", t.UTC().Unix()/int64(DigestWindow/time.Second))
}

// DigestUser is one recipient's expiring items, grouped from DigestRows.
type DigestUser struct {
	UserID, Email, Name, Locale string
	Count                       int
	Names                       []string // soonest first, at most DigestTopNames
}

// GroupDigest keeps rows expiring in (now, now+DigestWindow] (a second check on
// top of the query, so a loose store can never mail "never"-retention users),
// groups them per user, and orders names soonest first.
func GroupDigest(rows []DigestRow, now time.Time) []DigestUser {
	type acc struct {
		DigestUser
		items []DigestRow
	}
	by := map[string]*acc{}
	var order []string
	for _, r := range rows {
		if r.UserID == "" || strings.TrimSpace(r.Email) == "" {
			continue
		}
		if !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(DigestWindow)) {
			continue
		}
		a := by[r.UserID]
		if a == nil {
			a = &acc{DigestUser: DigestUser{UserID: r.UserID, Email: r.Email, Name: r.DisplayName, Locale: r.Locale}}
			by[r.UserID] = a
			order = append(order, r.UserID)
		}
		a.Count++
		a.items = append(a.items, r)
	}
	out := make([]DigestUser, 0, len(order))
	for _, id := range order {
		a := by[id]
		sort.SliceStable(a.items, func(i, j int) bool { return a.items[i].ExpiresAt.Before(a.items[j].ExpiresAt) })
		for i := 0; i < len(a.items) && i < DigestTopNames; i++ {
			a.Names = append(a.Names, a.items[i].Name)
		}
		out = append(out, a.DigestUser)
	}
	return out
}

// RunDigestOnce sends the expiry digest to everyone due now. Returns how many
// recipients it considered. The dedupe key (3-day window) makes re-running, or a
// second worker instance, harmless.
func (s *Service) RunDigestOnce(ctx context.Context) (int, error) {
	if s == nil {
		return 0, nil
	}
	if !s.enabled() {
		s.dispatch(Request{})
		return 0, nil
	}
	now := s.now()
	rows, err := s.Store.DigestRows(ctx, now, DigestWindow)
	if err != nil {
		return 0, err
	}
	users := GroupDigest(rows, now)
	key := WindowKey(now)
	days := int(DigestWindow / (24 * time.Hour))
	for _, u := range users {
		if ctx.Err() != nil {
			return len(users), ctx.Err()
		}
		s.deliver(ctx, Request{
			UserID: u.UserID, To: u.Email, Name: u.Name, Locale: u.Locale, Kind: KindExpiryDigest,
			DedupeKey: key, Data: Data{Count: u.Count, Names: u.Names, Days: days},
		})
	}
	return len(users), nil
}

// RunDigestWorker runs RunDigestOnce every interval until ctx ends. The first
// run is one interval after boot (not at boot, so restarts do not re-scan).
func (s *Service) RunDigestWorker(ctx context.Context, interval time.Duration) {
	if s == nil || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.RunDigestOnce(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				s.logger().Warn("email: digest run failed", "error", err.Error())
			} else if n > 0 {
				s.logger().Info("email: digest run", "recipients", n)
			}
		}
	}
}
