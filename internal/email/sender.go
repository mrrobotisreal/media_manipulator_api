package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// ResendEndpoint is Resend's send API.
const ResendEndpoint = "https://api.resend.com/emails"

// NoopSender is the sender when email is not configured. Send logs "email
// disabled" once per process and succeeds.
type NoopSender struct {
	Log    *slog.Logger
	Reason string
	once   sync.Once
}

// Enabled reports false so the service can skip the log table entirely.
func (n *NoopSender) Enabled() bool { return false }

// Send implements Sender.
func (n *NoopSender) Send(context.Context, Message) (string, error) {
	n.once.Do(func() {
		l := n.Log
		if l == nil {
			l = slog.Default()
		}
		l.Info("email disabled", "reason", n.Reason)
	})
	return "", nil
}

// ResendSender posts to Resend over HTTPS. No SDK.
type ResendSender struct {
	APIKey   string // secret: never logged
	From     string
	ReplyTo  string
	Endpoint string // defaults to ResendEndpoint; tests point it at httptest
	Client   *http.Client
}

// Enabled reports true.
func (r *ResendSender) Enabled() bool { return true }

type resendRequest struct {
	From    string            `json:"from"`
	To      []string          `json:"to"`
	Subject string            `json:"subject"`
	HTML    string            `json:"html,omitempty"`
	Text    string            `json:"text,omitempty"`
	ReplyTo string            `json:"reply_to,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Send implements Sender. Errors never contain the API key or the request body.
func (r *ResendSender) Send(ctx context.Context, msg Message) (string, error) {
	body := resendRequest{
		From:    r.From,
		To:      []string{msg.To},
		Subject: cleanHeader(msg.Subject),
		HTML:    msg.HTML,
		Text:    msg.Text,
		ReplyTo: r.ReplyTo,
		Headers: msg.Headers,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", &PermanentError{Msg: "encode email request"}
	}
	endpoint := r.Endpoint
	if endpoint == "" {
		endpoint = ResendEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", &PermanentError{Msg: "build email request"}
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "media-manipulator-api/email")
	if msg.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", msg.IdempotencyKey)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/url errors can embed the URL but never headers; still, report only
		// the class of failure.
		return "", fmt.Errorf("resend request failed: %s", redact(err.Error(), r.APIKey))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ok struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(data, &ok)
		return ok.ID, nil
	}
	var perr struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &perr)
	msgText := fmt.Sprintf("resend status %d: %s", resp.StatusCode, truncate(redact(perr.Message, r.APIKey), 200))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
		return "", &PermanentError{Msg: msgText}
	}
	return "", fmt.Errorf("%s", msgText)
}

func redact(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// NewSender picks the sender from config. It never logs the key.
func NewSender(cfg *config.Config, log *slog.Logger) Sender {
	if log == nil {
		log = slog.Default()
	}
	switch {
	case cfg == nil || !cfg.EmailEnabled:
		return &NoopSender{Log: log, Reason: "EMAIL_ENABLED is false"}
	case cfg.EmailProviderAPIKey == "":
		return &NoopSender{Log: log, Reason: "EMAIL_PROVIDER_API_KEY is empty"}
	case cfg.EmailFrom == "":
		return &NoopSender{Log: log, Reason: "EMAIL_FROM is empty"}
	}
	log.Info("email enabled", "provider", "resend")
	return &ResendSender{APIKey: cfg.EmailProviderAPIKey, From: cfg.EmailFrom, ReplyTo: cfg.EmailReplyTo}
}
