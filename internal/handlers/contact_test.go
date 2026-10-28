package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/email"
)

// fakeContactSender records messages; enabled=false behaves like NoopSender.
type fakeContactSender struct {
	mu      sync.Mutex
	enabled bool
	err     error
	sent    []email.Message
}

func (f *fakeContactSender) Enabled() bool { return f.enabled }

func (f *fakeContactSender) Send(_ context.Context, m email.Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.sent = append(f.sent, m)
	return "prov-1", nil
}

func contactRouter(h *ContactHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterContactRoutes(r.Group("/api"), h)
	return r
}

func postContact(r http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/contact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const validContactBody = `{"name":"Ada Lovelace","email":"ada@example.com","subject":"bug","message":"The GIF tool stalls at 90% on a 40 MB MOV in Firefox 131."}`

func TestContactSubmit(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		enabled   bool
		sendErr   error
		wantCode  int
		wantError string
		wantField string
		wantSent  int
	}{
		{name: "success", body: validContactBody, enabled: true, wantCode: http.StatusAccepted, wantSent: 1},
		{name: "honeypot filled is accepted but not sent", body: `{"name":"Bot","email":"bot@example.com","subject":"other","message":"buy now","website":"http://spam.example"}`, enabled: true, wantCode: http.StatusAccepted, wantSent: 0},
		{name: "honeypot wins over invalid fields", body: `{"website":"x"}`, enabled: true, wantCode: http.StatusAccepted, wantSent: 0},
		{name: "invalid email", body: `{"name":"Ada","email":"not-an-email","subject":"bug","message":"hi"}`, enabled: true, wantCode: http.StatusBadRequest, wantError: "invalid_contact", wantField: "email"},
		{name: "display-name email refused", body: `{"name":"Ada","email":"Ada <ada@example.com>","subject":"bug","message":"hi"}`, enabled: true, wantCode: http.StatusBadRequest, wantError: "invalid_contact", wantField: "email"},
		{name: "dotless domain refused", body: `{"name":"Ada","email":"ada@localhost","subject":"bug","message":"hi"}`, enabled: true, wantCode: http.StatusBadRequest, wantError: "invalid_contact", wantField: "email"},
		{name: "missing name", body: `{"name":"  ","email":"ada@example.com","subject":"bug","message":"hi"}`, enabled: true, wantCode: http.StatusBadRequest, wantError: "invalid_contact", wantField: "name"},
		{name: "name with newline", body: `{"name":"Ada\nBcc: x@y.z","email":"ada@example.com","subject":"bug","message":"hi"}`, enabled: true, wantCode: http.StatusBadRequest, wantError: "invalid_contact", wantField: "name"},
		{name: "unknown subject", body: `{"name":"Ada","email":"ada@example.com","subject":"sales","message":"hi"}`, enabled: true, wantCode: http.StatusBadRequest, wantError: "invalid_contact", wantField: "subject"},
		{name: "empty message", body: `{"name":"Ada","email":"ada@example.com","subject":"bug","message":"   "}`, enabled: true, wantCode: http.StatusBadRequest, wantError: "invalid_contact", wantField: "message"},
		{name: "message over 2000 chars", body: `{"name":"Ada","email":"ada@example.com","subject":"bug","message":"` + strings.Repeat("é", 2001) + `"}`, enabled: true, wantCode: http.StatusBadRequest, wantError: "invalid_contact", wantField: "message"},
		{name: "message of exactly 2000 chars", body: `{"name":"Ada","email":"ada@example.com","subject":"bug","message":"` + strings.Repeat("é", 2000) + `"}`, enabled: true, wantCode: http.StatusAccepted, wantSent: 1},
		{name: "malformed json", body: `{"name":`, enabled: true, wantCode: http.StatusBadRequest, wantError: "invalid_contact", wantField: "body"},
		{name: "email disabled", body: validContactBody, enabled: false, wantCode: http.StatusServiceUnavailable, wantError: "contact_unavailable"},
		{name: "provider failure", body: validContactBody, enabled: true, sendErr: errors.New("resend status 500"), wantCode: http.StatusBadGateway, wantError: "contact_send_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &fakeContactSender{enabled: tc.enabled, err: tc.sendErr}
			w := postContact(contactRouter(NewContactHandler(sender, "", nil)), tc.body)
			if w.Code != tc.wantCode {
				t.Fatalf("status %d want %d: %s", w.Code, tc.wantCode, w.Body.String())
			}
			var got map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if got["error"] != tc.wantError || got["field"] != tc.wantField {
				t.Errorf("body %v, want error %q field %q", got, tc.wantError, tc.wantField)
			}
			if tc.wantCode == http.StatusAccepted && len(got["reference"]) != contactRefLength {
				t.Errorf("reference %q is not %d chars", got["reference"], contactRefLength)
			}
			if len(sender.sent) != tc.wantSent {
				t.Errorf("sent %d emails, want %d", len(sender.sent), tc.wantSent)
			}
		})
	}
}

func TestContactEmailShape(t *testing.T) {
	sender := &fakeContactSender{enabled: true}
	h := NewContactHandler(sender, "inbox@example.com", nil)
	h.newRef = func() string { return "ABCDEFGHJKMN" }
	w := postContact(contactRouter(h), `{"name":"Ada Lovelace","email":" ada@example.com ","subject":"privacy","message":"Please delete my data.\r\nThanks"}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"reference":"ABCDEFGHJKMN"`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d", len(sender.sent))
	}
	m := sender.sent[0]
	if m.To != "inbox@example.com" {
		t.Errorf("to %q", m.To)
	}
	if m.ReplyTo != "ada@example.com" {
		t.Errorf("reply-to %q", m.ReplyTo)
	}
	if m.Subject != "[MM contact] Privacy or data request — Ada Lovelace (ABCDEFGHJKMN)" {
		t.Errorf("subject %q", m.Subject)
	}
	if m.HTML != "" {
		t.Errorf("contact mail must be plain text, got HTML %q", m.HTML)
	}
	for _, want := range []string{"Reference: ABCDEFGHJKMN", "Name: Ada Lovelace", "Email: ada@example.com", "Subject: Privacy or data request", "Please delete my data.\nThanks"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
	if m.IdempotencyKey != "contact-ABCDEFGHJKMN" {
		t.Errorf("idempotency key %q", m.IdempotencyKey)
	}
}

func TestContactDefaultsToSupportMailbox(t *testing.T) {
	if h := NewContactHandler(&fakeContactSender{}, "  ", nil); h.to != "support@media-manipulator.com" {
		t.Errorf("to %q", h.to)
	}
}

func TestContactNoopSenderIsUnavailable(t *testing.T) {
	w := postContact(contactRouter(NewContactHandler(&email.NoopSender{}, "", nil)), validContactBody)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("noop sender: %d %s", w.Code, w.Body.String())
	}
}

func TestContactReferenceAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		ref := newContactReference()
		if len(ref) != contactRefLength {
			t.Fatalf("len %d", len(ref))
		}
		for _, r := range ref {
			if !strings.ContainsRune(contactAlphabet, r) {
				t.Fatalf("ref %q has %q outside the alphabet", ref, r)
			}
		}
		if seen[ref] {
			t.Fatalf("duplicate reference %q in 200 draws", ref)
		}
		seen[ref] = true
	}
}

func TestContactSubjectsMatchTicket(t *testing.T) {
	for _, k := range []string{"bug", "billing", "privacy", "press", "other"} {
		if ContactSubjects[k] == "" {
			t.Errorf("subject %q missing", k)
		}
	}
	if len(ContactSubjects) != 5 {
		t.Errorf("%d subjects, want 5", len(ContactSubjects))
	}
}
