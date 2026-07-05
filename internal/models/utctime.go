package models

import (
	"database/sql/driver"
	"fmt"
	"time"
)

// UTCTime wraps time.Time so that DR API responses ALWAYS serialize timestamps
// as explicit UTC — RFC 3339 with a 'Z' suffix — regardless of the location pgx
// hands back for a timestamptz. This is a representation change only; the
// instant is preserved (MarshalJSON normalizes with .UTC()). Using the wrapper
// (rather than a .UTC() call at each scan site) is foolproof against future
// call sites: any DR timestamp field typed UTCTime is guaranteed Z-suffixed.
//
// It embeds time.Time (so the usual methods are available) and implements
// sql.Scanner. pgx v5 honours sql.Scanner in its native scan path — it checks
// for the interface before applying its own type wrappers and decodes a
// timestamptz to time.Time first — so a UTCTime field can be scanned directly
// from a query (see internal/handlers/dr_docs.go, dr_comments.go).
type UTCTime struct {
	time.Time
}

// MarshalJSON emits the instant in UTC as RFC 3339 (nanoseconds) with a 'Z'
// suffix, e.g. "2026-07-04T18:03:01.123456789Z".
func (t UTCTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.Time.UTC().Format(time.RFC3339Nano) + `"`), nil
}

// Scan implements sql.Scanner. pgx decodes a timestamptz to time.Time before
// calling this.
func (t *UTCTime) Scan(src any) error {
	switch v := src.(type) {
	case time.Time:
		t.Time = v
	case nil:
		t.Time = time.Time{}
	default:
		return fmt.Errorf("UTCTime.Scan: unsupported source type %T", src)
	}
	return nil
}

// Value implements driver.Valuer for round-trip completeness. DR never writes
// timestamps (created_at/updated_at default to now()); this exists so the type
// is not write-broken if a future call site inserts one.
func (t UTCTime) Value() (driver.Value, error) {
	return t.Time, nil
}
