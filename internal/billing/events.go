package billing

import (
	"encoding/json"
	"strings"
)

// idRef decodes a Stripe reference that is a string id or an expanded object.
type idRef string

func (r *idRef) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*r = idRef(v)
		return nil
	}
	var o struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	*r = idRef(o.ID)
	return nil
}

type rawObject struct {
	ID                string            `json:"id"`
	Customer          idRef             `json:"customer"`
	Subscription      idRef             `json:"subscription"`
	ClientReferenceID string            `json:"client_reference_id"`
	BillingReason     string            `json:"billing_reason"`
	Metadata          map[string]string `json:"metadata"`
	Parent            *struct {
		SubscriptionDetails *struct {
			Subscription idRef             `json:"subscription"`
			Metadata     map[string]string `json:"metadata"`
		} `json:"subscription_details"`
	} `json:"parent"`
}

// Webhook event types the processor acts on.
const (
	EventCheckoutCompleted    = "checkout.session.completed"
	EventSubscriptionCreated  = "customer.subscription.created"
	EventSubscriptionUpdated  = "customer.subscription.updated"
	EventSubscriptionDeleted  = "customer.subscription.deleted"
	EventInvoicePaid          = "invoice.paid"
	EventInvoicePaymentFailed = "invoice.payment_failed"
)

// HandledEventTypes lists the events the processor acts on.
var HandledEventTypes = []string{
	EventCheckoutCompleted, EventSubscriptionCreated, EventSubscriptionUpdated,
	EventSubscriptionDeleted, EventInvoicePaid, EventInvoicePaymentFailed,
}

// IsHandledEvent reports whether the processor acts on this event type.
func IsHandledEvent(t string) bool {
	for _, h := range HandledEventTypes {
		if h == t {
			return true
		}
	}
	return false
}

// parseEvent reduces a verified event's data.object to the ids we use.
//
// Parsing the raw JSON ourselves (instead of the SDK's typed Event.GetObjectValue)
// is half of how the API-version mismatch is made safe: only id-shaped fields
// that have been stable across Stripe API versions are read, and everything else
// comes from a Subscription re-fetched at OUR pinned version.
func parseEvent(id, typ string, raw json.RawMessage) (Event, error) {
	ev := Event{ID: id, Type: typ}
	if len(raw) == 0 {
		return ev, nil
	}
	var wrapper struct {
		Object rawObject `json:"object"`
	}
	// Event.Data.Raw is the data.object payload itself in stripe-go; accept both.
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return ev, err
	}
	obj := wrapper.Object
	if obj.ID == "" {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return ev, err
		}
	}
	ev.CustomerID = string(obj.Customer)
	ev.BillingReason = obj.BillingReason
	switch typ {
	case EventSubscriptionCreated, EventSubscriptionUpdated, EventSubscriptionDeleted:
		ev.SubscriptionID = obj.ID
		ev.UserID = obj.Metadata["user_id"]
	case EventCheckoutCompleted:
		ev.SubscriptionID = string(obj.Subscription)
		ev.UserID = obj.ClientReferenceID
		if ev.UserID == "" {
			ev.UserID = obj.Metadata["user_id"]
		}
	case EventInvoicePaid, EventInvoicePaymentFailed:
		ev.SubscriptionID = string(obj.Subscription)
		if obj.Parent != nil && obj.Parent.SubscriptionDetails != nil {
			if s := string(obj.Parent.SubscriptionDetails.Subscription); s != "" {
				ev.SubscriptionID = s
			}
			ev.UserID = obj.Parent.SubscriptionDetails.Metadata["user_id"]
		}
	}
	return ev, nil
}
