package retention

import (
	"strings"
	"time"
)

// Window math (MM22). Pure functions, no I/O: the settings API, the ledger
// writer and the sign-in claim all derive expiry from the same few lines.
//
// Rules:
//   - A guest (no user id) is fixed at 24 h. Always. It is not a setting.
//   - A signed-in user gets their mm_user_settings.retention_hours, clamped to
//     [MinHours, MaxHours]. With no row yet they get DefaultHours(tier).
//   - Retention is NOT tier-gated: the tier only picks the default for a user who
//     has never opened settings, and today every tier's default is the same.
//   - Expiry counts from the object's creation, never from "last touched".
//   - Re-stamping never lands in the past: expires_at >= now + RestampFloor, so
//     shrinking the window can not delete anything before the confirmation
//     dialog's promise ("within the hour") is kept.

const (
	// MinHours and MaxHours bound a user's choice: 24 h to 2 years.
	MinHours = 24
	MaxHours = 17520
	// GuestHours is the fixed window for signed-out visitors.
	GuestHours = 24
	// DefaultRetentionHours is the default for every tier: 180 days ("6 months").
	DefaultRetentionHours = 4320
	// RestampFloor is the minimum remaining life of a live object after a re-stamp.
	RestampFloor = time.Hour
)

// Owner is who a retained object belongs to, as far as the request layer knows.
type Owner struct {
	// UserID is the mm_users id; empty for a guest.
	UserID string
	// SessionID is the client X-MM-Session-ID; used for guests and kept for claims.
	SessionID string
	// Tier is the caller's tier name ("anonymous"/"free"/"premium"/"operator").
	Tier string
	// RetentionHours is the user's stored setting; 0 means "no row yet".
	RetentionHours int
}

// IsGuest reports whether the owner has no account.
func (o Owner) IsGuest() bool { return strings.TrimSpace(o.UserID) == "" }

// DefaultHours is the window a signed-in user of the given tier gets before
// they choose one. The tier registry's ResultRetentionHours is this default and
// no longer a cap.
func DefaultHours(tier string) int {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "anonymous":
		return GuestHours
	default: // free, premium, operator, unknown
		return DefaultRetentionHours
	}
}

// Clamp bounds hours to [MinHours, MaxHours].
func Clamp(hours int) int {
	if hours < MinHours {
		return MinHours
	}
	if hours > MaxHours {
		return MaxHours
	}
	return hours
}

// ValidHours reports whether hours is an acceptable user choice (no clamping).
func ValidHours(hours int) bool { return hours >= MinHours && hours <= MaxHours }

// WindowFor is the retention window for an owner.
func WindowFor(owner Owner) time.Duration {
	if owner.IsGuest() {
		return GuestHours * time.Hour
	}
	hours := owner.RetentionHours
	if hours <= 0 {
		hours = DefaultHours(owner.Tier)
	}
	return time.Duration(Clamp(hours)) * time.Hour
}

// ExpiresAt is when an object created at `created` stops being kept.
func ExpiresAt(created time.Time, window time.Duration) time.Time {
	return created.Add(window)
}

// RestampExpiry is ExpiresAt with the floor applied: never earlier than
// now + RestampFloor. Used when a window changes or a guest's rows are claimed.
func RestampExpiry(created time.Time, window time.Duration, now time.Time) time.Time {
	exp := ExpiresAt(created, window)
	if floor := now.Add(RestampFloor); exp.Before(floor) {
		return floor
	}
	return exp
}
