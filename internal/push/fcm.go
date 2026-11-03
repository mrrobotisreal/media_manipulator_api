package push

import (
	"context"
	"errors"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
)

// AndroidChannelID is the notification channel MA24 creates on the device.
const AndroidChannelID = "job_updates"

type multicaster interface {
	SendEachForMulticast(ctx context.Context, m *messaging.MulticastMessage) (*messaging.BatchResponse, error)
}

// FCMSender sends through Firebase Cloud Messaging (HTTP v1) using the same
// Application Default Credentials the auth verifier uses.
type FCMSender struct {
	client multicaster
}

// NewFCMSender builds the sender. It makes no network call.
func NewFCMSender(ctx context.Context, projectID string) (*FCMSender, error) {
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: projectID})
	if err != nil {
		return nil, err
	}
	client, err := app.Messaging(ctx)
	if err != nil {
		return nil, err
	}
	return &FCMSender{client: client}, nil
}

// Send implements Sender.
func (s *FCMSender) Send(ctx context.Context, tokens []string, msg Message) ([]Result, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("push: FCM sender not initialised")
	}
	if len(tokens) == 0 {
		return nil, nil
	}
	resp, err := s.client.SendEachForMulticast(ctx, buildMulticast(tokens, msg))
	if err != nil {
		return nil, err
	}
	out := make([]Result, len(tokens))
	for i, tok := range tokens {
		out[i] = Result{Token: tok, ErrCode: ErrFailed}
		if i >= len(resp.Responses) || resp.Responses[i] == nil {
			continue
		}
		r := resp.Responses[i]
		if r.Success {
			out[i].OK, out[i].ErrCode = true, ""
			continue
		}
		out[i].ErrCode = classifyFCMError(r.Error)
	}
	return out, nil
}

func buildMulticast(tokens []string, msg Message) *messaging.MulticastMessage {
	return &messaging.MulticastMessage{
		Tokens:       tokens,
		Notification: &messaging.Notification{Title: msg.Title, Body: msg.Body},
		Data:         msg.Data,
		Android: &messaging.AndroidConfig{
			Priority:     "high",
			Notification: &messaging.AndroidNotification{ChannelID: AndroidChannelID},
		},
		APNS: &messaging.APNSConfig{
			Payload: &messaging.APNSPayload{Aps: &messaging.Aps{Sound: "default"}},
		},
	}
}

func classifyFCMError(err error) string {
	switch {
	case err == nil:
		return ""
	case messaging.IsRegistrationTokenNotRegistered(err):
		return ErrUnregistered
	case messaging.IsInvalidArgument(err):
		return ErrInvalid
	default:
		return ErrFailed
	}
}
