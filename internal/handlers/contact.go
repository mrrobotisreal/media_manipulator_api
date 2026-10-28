package handlers

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/email"
)

// MM85: the public contact form.
//
//	POST /api/contact  {name, email, subject, message, website?}
//	  -> 202 {reference}                          sent (or honeypot filled)
//	  -> 400 {error:"invalid_contact", field}     validation
//	  -> 503 {error:"contact_unavailable"}        email disabled (EMAIL_ENABLED=false)
//	  -> 502 {error:"contact_send_failed"}        provider refused or timed out
//
// The email is the record: no database row, no quota charge. Abuse control is
// the honeypot plus the `contact` limiter bucket (CONTACT_PER_SESSION_PER_HOUR,
// CONTACT_PER_IP_PER_HOUR), applied by cmd/api's routeLimitDispatcher before
// this handler runs.

const (
	contactMaxName    = 100
	contactMaxEmail   = 254
	contactMaxMessage = 2000
	contactMaxBody    = 16 << 10
	contactRefLength  = 12
	contactSendWindow = 15 * time.Second
	// contactAlphabet has no 0/O/1/I/L so a reference read aloud or retyped
	// from an email subject survives.
	contactAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
)

// ContactSubjects are the subject select's values and the English label each
// carries into the email subject line. The UI's select mirrors the keys.
var ContactSubjects = map[string]string{
	"bug":     "Bug report",
	"billing": "Billing",
	"privacy": "Privacy or data request",
	"press":   "Press or partnership",
	"other":   "Other",
}

// ContactHandler serves POST /api/contact.
type ContactHandler struct {
	sender email.Sender
	to     string
	log    *slog.Logger
	newRef func() string
}

// NewContactHandler builds the handler. to defaults to the support mailbox.
func NewContactHandler(sender email.Sender, to string, log *slog.Logger) *ContactHandler {
	if log == nil {
		log = slog.Default()
	}
	to = strings.TrimSpace(to)
	if to == "" {
		to = "support@media-manipulator.com"
	}
	return &ContactHandler{sender: sender, to: to, log: log, newRef: newContactReference}
}

// RegisterContactRoutes mounts POST /contact. A nil handler mounts nothing.
func RegisterContactRoutes(r gin.IRoutes, h *ContactHandler) {
	if h == nil {
		return
	}
	r.POST("/contact", h.Submit)
}

type contactRequest struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Subject string `json:"subject"`
	Message string `json:"message"`
	// Website is the honeypot: a field humans never see. Filled means a bot.
	Website string `json:"website"`
}

// enabled reports whether the sender can actually deliver. NoopSender (email
// not configured) reports false; a sender without the method is assumed live.
func (h *ContactHandler) enabled() bool {
	if h == nil || h.sender == nil {
		return false
	}
	if e, ok := h.sender.(interface{ Enabled() bool }); ok {
		return e.Enabled()
	}
	return true
}

// Submit validates the form and mails it to the support inbox.
func (h *ContactHandler) Submit(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, contactMaxBody)
	var req contactRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_contact", "field": "body"})
		return
	}
	// The honeypot answers exactly like a success so a bot learns nothing, and
	// is checked before validation so a bot's junk never reaches the inbox.
	if strings.TrimSpace(req.Website) != "" {
		c.JSON(http.StatusAccepted, gin.H{"reference": h.newRef()})
		return
	}
	form, field := normalizeContact(req)
	if field != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_contact", "field": field})
		return
	}
	if !h.enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "contact_unavailable"})
		return
	}
	ref := h.newRef()
	msg := buildContactMessage(h.to, ref, form, time.Now().UTC())
	ctx, cancel := context.WithTimeout(c.Request.Context(), contactSendWindow)
	defer cancel()
	if _, err := h.sender.Send(ctx, msg); err != nil {
		// Never log the visitor's message or address; the reference is enough
		// to match a retry in the inbox.
		h.log.Warn("contact send failed", "reference", ref, "permanent", email.IsPermanent(err), "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "contact_send_failed"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"reference": ref})
}

type contactForm struct {
	name, email, subject, message string
}

// normalizeContact trims and validates; it returns the first bad field name.
func normalizeContact(req contactRequest) (contactForm, string) {
	f := contactForm{
		name:    strings.TrimSpace(req.Name),
		email:   strings.TrimSpace(req.Email),
		subject: strings.TrimSpace(req.Subject),
		message: strings.TrimSpace(strings.ReplaceAll(req.Message, "\r\n", "\n")),
	}
	switch {
	case f.name == "" || utf8.RuneCountInString(f.name) > contactMaxName || strings.ContainsAny(f.name, "\r\n"):
		return f, "name"
	case !validContactEmail(f.email):
		return f, "email"
	case ContactSubjects[f.subject] == "":
		return f, "subject"
	case f.message == "" || utf8.RuneCountInString(f.message) > contactMaxMessage:
		return f, "message"
	}
	return f, ""
}

// validContactEmail accepts a bare address (no display name) with a dotted
// domain. It is a sanity check for the Reply-To, not deliverability proof.
func validContactEmail(s string) bool {
	if s == "" || len(s) > contactMaxEmail || strings.ContainsAny(s, " \r\n<>") {
		return false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || addr.Name != "" {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	domain := s[at+1:]
	return at > 0 && strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

func buildContactMessage(to, ref string, f contactForm, now time.Time) email.Message {
	label := ContactSubjects[f.subject]
	var b strings.Builder
	fmt.Fprintf(&b, "Reference: %s\n", ref)
	fmt.Fprintf(&b, "Received: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "Name: %s\n", f.name)
	fmt.Fprintf(&b, "Email: %s\n", f.email)
	fmt.Fprintf(&b, "Subject: %s\n\n", label)
	b.WriteString(f.message)
	b.WriteString("\n\n--\nSent from the contact form at media-manipulator.com/contact. Reply to this email to answer the visitor.\n")
	return email.Message{
		To:             to,
		Subject:        fmt.Sprintf("[MM contact] %s — %s (%s)", label, f.name, ref),
		Text:           b.String(),
		ReplyTo:        f.email,
		IdempotencyKey: "contact-" + ref,
	}
}

// newContactReference returns a 12-character reference from crypto/rand.
func newContactReference() string {
	buf := make([]byte, contactRefLength)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to the
		// clock so the form still answers.
		return fmt.Sprintf("%012d", time.Now().UnixNano()%1_000_000_000_000)
	}
	out := make([]byte, contactRefLength)
	for i, v := range buf {
		out[i] = contactAlphabet[int(v)%len(contactAlphabet)]
	}
	return string(out)
}
