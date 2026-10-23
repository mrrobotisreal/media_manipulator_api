package analyticsemit

import "math"

// Bucket helpers for the studio_render_* properties.
//
// Catalog law (ADR ws/0003): analytics properties are enums or buckets, never
// raw measurements. These two mirror the UI's lib/analytics/buckets.ts
// (`ratioBucket`) and the analytics catalog's documented vocabularies — the
// stored strings on either side of the client/server join must be comparable,
// so the boundaries here may not drift independently.

// RatioBucket buckets render seconds / timeline seconds: "1-2x" reads as "the
// export took one to two times realtime". Non-finite or negative input (a zero
// timeline, a clock problem) reports "unknown" rather than a wrong bucket.
func RatioBucket(ratio float64) string {
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 {
		return "unknown"
	}
	switch {
	case ratio < 0.25:
		return "<0.25x"
	case ratio < 0.5:
		return "0.25-0.5x"
	case ratio < 1:
		return "0.5-1x"
	case ratio < 2:
		return "1-2x"
	case ratio < 4:
		return "2-4x"
	default:
		return "4x+"
	}
}

// FilterCountBucket buckets the number of statements in an export's
// -filter_complex graph — the best single proxy for how much compositing work
// a render asked of ffmpeg.
func FilterCountBucket(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n <= 10:
		return "1-10"
	case n <= 25:
		return "11-25"
	case n <= 50:
		return "26-50"
	case n <= 100:
		return "51-100"
	default:
		return "100+"
	}
}

// CountBucket buckets a "how many" for server_retention_swept: "1", "2-9",
// "10-99", "100+". Zero reports "0" (never emitted, but total).
func CountBucket(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n == 1:
		return "1"
	case n <= 9:
		return "2-9"
	case n <= 99:
		return "10-99"
	default:
		return "100+"
	}
}
