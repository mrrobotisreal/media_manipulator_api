// Package push delivers "your job finished" notifications over FCM (MA23).
//
// Only this package and cmd/api import firebase.google.com/go/v4/messaging.
// Nothing here ever logs or returns a device token in an error.
package push

import "context"

// Message is one notification: visible text plus the data the app routes on.
type Message struct {
	Title string
	Body  string
	Data  map[string]string
}

// Per-token error codes in Result.ErrCode.
const (
	ErrUnregistered = "unregistered" // FCM: the token is no longer valid on the device
	ErrInvalid      = "invalid"      // FCM: the token is malformed
	ErrFailed       = "failed"       // anything else (auth, quota, outage): never disables a token
)

// Result is the outcome for one token.
type Result struct {
	Token   string
	OK      bool
	ErrCode string
}

// Sender sends one message to many tokens. The returned slice has one Result per
// input token, in order. A non-nil error means nothing was attempted.
type Sender interface {
	Send(ctx context.Context, tokens []string, msg Message) ([]Result, error)
}
