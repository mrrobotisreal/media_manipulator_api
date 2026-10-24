package email

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

const testKey = "re_test_SECRET_do_not_log"

func TestResendRequestShape(t *testing.T) {
	var gotAuth, gotCT, gotIdem string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCT, gotIdem = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("Idempotency-Key")
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"prov-123"}`))
	}))
	defer srv.Close()

	s := &ResendSender{APIKey: testKey, From: "Media Manipulator <hello@example.com>", ReplyTo: "support@example.com", Endpoint: srv.URL}
	id, err := s.Send(context.Background(), Message{
		To: "u@example.com", Subject: "Hi\r\nBcc: evil@example.com", Text: "plain", HTML: "<p>html</p>",
		Headers: map[string]string{"List-Unsubscribe": "<https://x/y>"}, IdempotencyKey: "k1",
	})
	if err != nil || id != "prov-123" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	if gotAuth != "Bearer "+testKey || gotCT != "application/json" || gotIdem != "k1" {
		t.Errorf("headers auth=%q ct=%q idem=%q", gotAuth, gotCT, gotIdem)
	}
	if got["from"] != "Media Manipulator <hello@example.com>" || got["subject"] != "Hi  Bcc: evil@example.com" ||
		got["text"] != "plain" || got["html"] != "<p>html</p>" || got["reply_to"] != "support@example.com" {
		t.Errorf("body = %#v", got)
	}
	if to, _ := got["to"].([]any); len(to) != 1 || to[0] != "u@example.com" {
		t.Errorf("to = %#v", got["to"])
	}
	if h, _ := got["headers"].(map[string]any); h["List-Unsubscribe"] != "<https://x/y>" {
		t.Errorf("headers = %#v", got["headers"])
	}
}

func TestResendErrorsNeverLeakKey(t *testing.T) {
	cases := []struct {
		status    int
		permanent bool
	}{{http.StatusUnprocessableEntity, true}, {http.StatusUnauthorized, true}, {http.StatusTooManyRequests, false}, {http.StatusBadGateway, false}}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"message":"bad key ` + testKey + `"}`))
		}))
		s := &ResendSender{APIKey: testKey, From: "a@b.c", Endpoint: srv.URL}
		_, err := s.Send(context.Background(), Message{To: "u@example.com", Subject: "s", Text: "t"})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: want error", c.status)
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("status %d: error leaks the key: %v", c.status, err)
		}
		if IsPermanent(err) != c.permanent {
			t.Errorf("status %d: permanent = %v, want %v", c.status, IsPermanent(err), c.permanent)
		}
	}
}

func TestNewSenderPicksNoopWithoutConfig(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	for name, cfg := range map[string]*config.Config{
		"nil":      nil,
		"disabled": {EmailEnabled: false, EmailProviderAPIKey: testKey, EmailFrom: "a@b.c"},
		"no key":   {EmailEnabled: true, EmailFrom: "a@b.c"},
		"no from":  {EmailEnabled: true, EmailProviderAPIKey: testKey},
	} {
		s := NewSender(cfg, log)
		if _, ok := s.(*NoopSender); !ok {
			t.Errorf("%s: sender = %T, want *NoopSender", name, s)
		}
		if _, err := s.Send(context.Background(), Message{To: "u@example.com"}); err != nil {
			t.Errorf("%s: noop send failed: %v", name, err)
		}
	}
	if strings.Contains(buf.String(), testKey) {
		t.Errorf("logs leak the key: %s", buf.String())
	}
	if _, ok := NewSender(&config.Config{EmailEnabled: true, EmailProviderAPIKey: testKey, EmailFrom: "a@b.c"}, log).(*ResendSender); !ok {
		t.Error("configured sender should be Resend")
	}
	if strings.Contains(buf.String(), testKey) {
		t.Errorf("enabling leaks the key: %s", buf.String())
	}
}

func TestNoopLogsDisabledOnce(t *testing.T) {
	var buf bytes.Buffer
	n := &NoopSender{Log: slog.New(slog.NewTextHandler(&buf, nil)), Reason: "test"}
	for i := 0; i < 5; i++ {
		if id, err := n.Send(context.Background(), Message{}); id != "" || err != nil {
			t.Fatalf("noop = %q, %v", id, err)
		}
	}
	if c := strings.Count(buf.String(), "email disabled"); c != 1 {
		t.Errorf("logged %d times, want 1", c)
	}
}
