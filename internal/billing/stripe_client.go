package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"
)

// webhookTolerance is the maximum age of a signed webhook. Never 0 (which the
// SDK would silently default) and never "ignore".
const webhookTolerance = 5 * time.Minute

// PinnedAPIVersion is the Stripe API version stripe-go sends on every request
// (and the one the webhook endpoint should be created at).
func PinnedAPIVersion() string { return stripe.APIVersion }

// stripeClient is the real BillingClient. It is never constructed in tests.
type stripeClient struct {
	sc            *stripe.Client
	webhookSecret string
	// versionOverride, when set, replaces the Stripe-Version header per request.
	versionOverride string
	// automaticTax asks Checkout to compute Stripe Tax (STRIPE_AUTOMATIC_TAX).
	automaticTax bool
}

// NewStripeClient builds the real client. apiVersion may be empty (use the SDK's).
func NewStripeClient(secretKey, webhookSecret, apiVersion string, automaticTax bool) BillingClient {
	return &stripeClient{sc: stripe.NewClient(secretKey), webhookSecret: webhookSecret, versionOverride: apiVersion, automaticTax: automaticTax}
}

func (c *stripeClient) params(ctx context.Context) stripe.Params {
	p := stripe.Params{Context: ctx}
	if c.versionOverride != "" {
		p.Headers = http.Header{"Stripe-Version": []string{c.versionOverride}}
	}
	return p
}

func wrapErr(err error) error {
	var se *stripe.Error
	if errors.As(err, &se) && se.Code == stripe.ErrorCodeResourceMissing {
		return fmt.Errorf("%w: %s", ErrNotFound, se.Msg)
	}
	return err
}

func (c *stripeClient) EnsureCustomer(ctx context.Context, userID, email string) (string, error) {
	p := &stripe.CustomerCreateParams{
		Params:   c.params(ctx),
		Metadata: map[string]string{"user_id": userID},
	}
	if email != "" {
		p.Email = stripe.String(email)
	}
	p.SetIdempotencyKey("cust-create-" + userID)
	cust, err := c.sc.V1Customers.Create(ctx, p)
	if err != nil {
		return "", wrapErr(err)
	}
	return cust.ID, nil
}

func (c *stripeClient) CreateCheckoutSession(ctx context.Context, req CheckoutRequest) (string, string, error) {
	loc := NormalizeUILocale(req.Locale)
	// {CHECKOUT_SESSION_ID} must stay literal: Stripe substitutes it.
	success := fmt.Sprintf("%s/%s/account?checkout=success&session_id={CHECKOUT_SESSION_ID}", req.BaseURL, loc)
	cancel := fmt.Sprintf("%s/%s/pricing?checkout=cancelled", req.BaseURL, loc)
	p := &stripe.CheckoutSessionCreateParams{
		Params:              c.params(ctx),
		Mode:                stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:            stripe.String(req.CustomerID),
		ClientReferenceID:   stripe.String(req.UserID),
		AllowPromotionCodes: stripe.Bool(true),
		Locale:              stripe.String(StripeLocale(loc)),
		SuccessURL:          stripe.String(success),
		CancelURL:           stripe.String(cancel),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{Price: stripe.String(req.PriceID), Quantity: stripe.Int64(1)},
		},
		ConsentCollection: &stripe.CheckoutSessionCreateConsentCollectionParams{
			TermsOfService: stripe.String("required"),
		},
		// The Stripe account is shared with CreaTV, so the account-level Terms
		// link would point at CreaTV's terms. Replace the consent line with
		// Media Manipulator's own Terms and Privacy links.
		CustomText: &stripe.CheckoutSessionCreateCustomTextParams{
			TermsOfServiceAcceptance: &stripe.CheckoutSessionCreateCustomTextTermsOfServiceAcceptanceParams{
				Message: stripe.String(termsAcceptanceMessage(req.BaseURL, loc)),
			},
		},
		CustomerUpdate: &stripe.CheckoutSessionCreateCustomerUpdateParams{
			Address: stripe.String("auto"),
		},
		SubscriptionData: &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: subscriptionMetadata(req),
		},
		// No trial (ticket decision 7).
	}
	applyAutomaticTax(p, c.automaticTax)
	s, err := c.sc.V1CheckoutSessions.Create(ctx, p)
	if err != nil {
		return "", "", wrapErr(err)
	}
	return s.ID, s.URL, nil
}

// applyAutomaticTax turns on Stripe Tax for a Checkout Session. Tax is added on
// top of the price (the Stripe Prices must be tax_behavior "exclusive"). Stripe
// Tax needs a customer address: the session already uses an existing Customer
// with CustomerUpdate.Address = "auto", which saves the address Checkout
// collects, and BillingAddressCollection "auto" lets Checkout collect it when
// the payment method does not. TaxIDCollection is deliberately NOT set: for an
// existing Customer it also requires CustomerUpdate.Name = "auto", and business
// tax IDs are not part of this consumer plan. When off, the params are untouched
// (Stripe errors on automatic tax for accounts without Stripe Tax activated).
func applyAutomaticTax(p *stripe.CheckoutSessionCreateParams, on bool) {
	if !on {
		return
	}
	p.AutomaticTax = &stripe.CheckoutSessionCreateAutomaticTaxParams{Enabled: stripe.Bool(true)}
	p.BillingAddressCollection = stripe.String(string(stripe.CheckoutSessionBillingAddressCollectionAuto))
}

func (c *stripeClient) CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	p := &stripe.BillingPortalSessionCreateParams{
		Params:    c.params(ctx),
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(returnURL),
	}
	s, err := c.sc.V1BillingPortalSessions.Create(ctx, p)
	if err != nil {
		return "", wrapErr(err)
	}
	return s.URL, nil
}

func (c *stripeClient) GetSubscription(ctx context.Context, id string) (*Subscription, error) {
	p := &stripe.SubscriptionRetrieveParams{Params: c.params(ctx)}
	s, err := c.sc.V1Subscriptions.Retrieve(ctx, id, p)
	if err != nil {
		return nil, wrapErr(err)
	}
	out := &Subscription{
		ID:                s.ID,
		Status:            string(s.Status),
		CancelAtPeriodEnd: s.CancelAtPeriodEnd,
		UserID:            s.Metadata["user_id"],
		VisitorID:         s.Metadata["visitor_id"],
		SessionID:         s.Metadata["session_id"],
	}
	if s.Customer != nil {
		out.CustomerID = s.Customer.ID
	}
	if s.Items != nil && len(s.Items.Data) > 0 {
		item := s.Items.Data[0]
		if item.CurrentPeriodEnd > 0 {
			out.CurrentPeriodEnd = time.Unix(item.CurrentPeriodEnd, 0).UTC()
		}
		if item.Price != nil {
			if item.Price.Currency != "" {
				out.Currency = strings.ToLower(string(item.Price.Currency))
				amt := item.Price.UnitAmount
				out.UnitAmountCents = &amt
			}
		}
		if item.Price != nil && item.Price.Recurring != nil {
			switch item.Price.Recurring.Interval {
			case stripe.PriceRecurringIntervalMonth:
				out.Interval = IntervalMonthly
			case stripe.PriceRecurringIntervalYear:
				out.Interval = IntervalYearly
			}
		}
	}
	return out, nil
}

func (c *stripeClient) GetCheckoutSession(ctx context.Context, id string) (*CheckoutSession, error) {
	p := &stripe.CheckoutSessionRetrieveParams{Params: c.params(ctx)}
	s, err := c.sc.V1CheckoutSessions.Retrieve(ctx, id, p)
	if err != nil {
		return nil, wrapErr(err)
	}
	out := &CheckoutSession{
		ID:     s.ID,
		UserID: s.ClientReferenceID,
		Paid:   s.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid,
	}
	if s.Customer != nil {
		out.CustomerID = s.Customer.ID
	}
	if s.Subscription != nil {
		out.SubscriptionID = s.Subscription.ID
	}
	return out, nil
}

// ConstructEvent verifies the signature and reduces the event.
//
// API-VERSION MISMATCH. stripe-go v87 pins 2026-09-30.endive and, by default,
// webhook.ConstructEvent rejects any event whose release train differs. A
// mismatch is realistic (an endpoint created in the Dashboard defaults to the
// account's version). We set IgnoreAPIVersionMismatch because it is safe HERE:
// (1) the signature and 5-minute tolerance are still enforced, (2) we read only
// id-shaped fields out of the raw payload, and (3) the processor never trusts the
// event body for state - it re-fetches the Subscription at our pinned version.
// MM16/MM21 should still create the endpoint at PinnedAPIVersion().
func (c *stripeClient) ConstructEvent(payload []byte, signatureHeader string) (Event, error) {
	return constructEvent(c.webhookSecret, payload, signatureHeader)
}

func constructEvent(secret string, payload []byte, signatureHeader string) (Event, error) {
	e, err := webhook.ConstructEventWithOptions(payload, signatureHeader, secret, webhook.ConstructEventOptions{
		Tolerance:                webhookTolerance,
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		return Event{}, err
	}
	var raw []byte
	if e.Data != nil {
		raw = e.Data.Raw
	}
	return parseEvent(e.ID, string(e.Type), raw)
}

// subscriptionMetadata is stamped on the Subscription by Checkout. user_id is the
// webhook's fallback user lookup. visitor_id / session_id are the browser's
// analytics identity (validated UUIDs only), carried here so the server-truth
// billing events emitted from the webhook have the identity analyticsemit
// requires instead of being dropped. They are not personal data.
func subscriptionMetadata(req CheckoutRequest) map[string]string {
	m := map[string]string{"user_id": req.UserID}
	if isUUID(req.VisitorID) && isUUID(req.SessionID) {
		m["visitor_id"] = req.VisitorID
		m["session_id"] = req.SessionID
	}
	return m
}

// termsAcceptanceMessage is the Checkout consent line (Stripe renders
// [text](url) links). English pages are unprefixed; other locales use
// /<locale>/.
func termsAcceptanceMessage(baseURL, uiLocale string) string {
	prefix := baseURL
	if uiLocale != "" && uiLocale != "en" {
		prefix = baseURL + "/" + uiLocale
	}
	return fmt.Sprintf("I agree to the Media Manipulator [Terms of Service](%s/terms-of-service) and [Privacy Policy](%s/privacy-policy).", prefix, prefix)
}
