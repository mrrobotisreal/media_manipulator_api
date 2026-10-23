package billing

import (
	"fmt"
	"time"

	"github.com/stripe/stripe-go/v87/webhook"
)

func signHeader(payload []byte, secret string, t time.Time) string {
	return fmt.Sprintf("t=%d,v1=%x", t.Unix(), webhook.ComputeSignature(t, payload, secret))
}
