package models

import "time"

// User is a row of mm_users — one Firebase-authenticated person and the tier
// they are entitled to. Postgres is the only source of truth for entitlement;
// the frontend keeps no parallel copy.
type User struct {
	ID            string     `json:"id"`
	FirebaseUID   string     `json:"firebaseUid"`
	Email         string     `json:"email,omitempty"`
	EmailVerified bool       `json:"emailVerified"`
	Tier          string     `json:"tier"`
	TierSource    string     `json:"tierSource"`
	TierExpiresAt *time.Time `json:"tierExpiresAt,omitempty"`
	DisplayName   string     `json:"displayName,omitempty"`
	SignupSource  string     `json:"signupSource,omitempty"`
	// Locale is the BCP-47 language mail is sent in (MM31); empty = en-us.
	Locale     string     `json:"-"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`

	// Billing mirror (MM17). Stripe ids never leave the API (json:"-"); the
	// status endpoint exposes only the derived, non-identifying fields.
	StripeCustomerID     string `json:"-"`
	StripeSubscriptionID string `json:"-"`
	SubscriptionStatus   string `json:"subscriptionStatus,omitempty"`
	BillingInterval      string `json:"billingInterval,omitempty"`
	CancelAtPeriodEnd    bool   `json:"cancelAtPeriodEnd"`
	// BillingCurrency (MM69) is the currency Stripe bills this user in
	// ("usd" | "eur"), mirrored from the subscription; "" = not known yet.
	// Once set it is the Stripe Customer's locked currency.
	BillingCurrency string `json:"-"`
}

// UsageSubjectKind identifies what a quota counter is keyed on. Preference
// order is user → session → ip: the more stable the identifier, the fairer the
// counting.
type UsageSubjectKind string

const (
	UsageSubjectUser    UsageSubjectKind = "user"
	UsageSubjectSession UsageSubjectKind = "session"
	UsageSubjectIP      UsageSubjectKind = "ip"
)

// Quota metrics. These are the CHECK-constrained values in mm_usage_counters.
const (
	MetricOps     = "ops"
	MetricAIOps   = "ai_ops"
	MetricBytesIn = "bytes_in"
	MetricExport  = "export"
	// MetricVectorExport is the Motion export allowance (MM43): counted by
	// POST /api/vector/exports and /api/vector/vectorize inside the same
	// transaction that writes the vector_exports row.
	MetricVectorExport = "vector_export"
)

// UsageCounter is one (subject, metric, day) row of the quota ledger.
type UsageCounter struct {
	Metric string `json:"metric"`
	Count  int64  `json:"count"`
}

// ConversionHistoryEntry is one mm_conversion_jobs row as the account history
// reads it. Deliberately has no JSON tags: the handler maps it to its own
// response shape so the S3 key can never be serialised by accident.
type ConversionHistoryEntry struct {
	JobID           string
	Tool            string
	Mode            string
	MediaKind       string
	SourceFormat    string
	TargetFormat    string
	Status          string
	OwnerTier       string
	CreatedAt       time.Time
	CompletedAt     *time.Time
	DurationMS      *int
	ResultS3Key     string
	ResultFileName  string
	ResultExpiresAt *time.Time
}

// TierGrant is an audit row: who was given what, by whom, and why.
type TierGrant struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	Tier      string     `json:"tier"`
	GrantedBy string     `json:"grantedBy"`
	Reason    string     `json:"reason,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}
