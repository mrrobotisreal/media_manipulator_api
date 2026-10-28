// Package email sends transactional mail (MM31): welcome, AI job finished,
// expiry digest, payment failed, account deleted.
//
// The provider seam is the Sender interface. Resend over plain net/http is the
// only real implementation; swapping to Postmark is one new file. When email is
// not configured (EMAIL_ENABLED=false, no API key or no From address) the
// NoopSender is used: it logs "email disabled" once and every Send succeeds, so
// no caller ever branches on configuration.
//
// Hard rules, enforced by tests:
//   - the API key is never logged, returned in an error or put in a log row;
//   - sends are best-effort goroutines and never block or fail a user action;
//   - one email per (user, kind, dedupe key), enforced by mm_email_log's unique
//     index, so concurrent triggers cannot double send.
package email

import (
	"context"
	"errors"
	"strings"
)

// Kind names one of the five emails. The values are the CHECK domain of
// mm_email_log.kind and the `kind` property of server_email_sent.
type Kind string

const (
	KindWelcome        Kind = "welcome"
	KindAIJobFinished  Kind = "ai_job_finished"
	KindExpiryDigest   Kind = "expiry_digest"
	KindPaymentFailed  Kind = "payment_failed"
	KindAccountDeleted Kind = "account_deleted"
)

// Log row statuses (mm_email_log.status).
const (
	StatusPending = "pending"
	StatusSent    = "sent"
	StatusFailed  = "failed"
	// StatusDisabled and StatusSkipped are analytics-only outcomes: no log row
	// is written for them.
	StatusDisabled = "disabled"
	StatusSkipped  = "skipped"
)

// Message is one outgoing email. From and the default Reply-To belong to the
// sender.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
	// Headers are extra RFC 5322 headers (List-Unsubscribe).
	Headers map[string]string
	// IdempotencyKey lets the provider collapse a retried request into one mail.
	IdempotencyKey string
	// ReplyTo, when set, overrides the sender's configured Reply-To for this
	// one message (MM85: the contact form replies to the visitor).
	ReplyTo string
}

// Sender delivers a message and returns the provider's id for it.
type Sender interface {
	Send(ctx context.Context, msg Message) (providerID string, err error)
}

// PermanentError marks a failure that retrying cannot fix (a 4xx other than
// 429): the service stops retrying and records the row as failed.
type PermanentError struct{ Msg string }

func (e *PermanentError) Error() string { return e.Msg }

// IsPermanent reports whether err is a PermanentError.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// cleanHeader strips CR/LF so user-derived text can never inject a header.
func cleanHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
