// Package retention records how long one job's artifacts must be kept, so the
// cleanup sweeper can honour a per-tier promise without a database round-trip.
//
// Why a marker file rather than a column: the sweeper walks the filesystem, and
// the obvious storage — mm_conversion_jobs.owner_tier — has no writer at all
// today (nothing calls telemetry.Store.UpsertJob). A marker written next to the
// artifacts it describes needs no schema, survives a restart, costs one 4-byte
// write per job, and cannot disagree with the files it governs. When the job
// telemetry path is eventually wired up, the column becomes the better source
// and this can be retired.
package retention

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MarkerName is the file written into a job's upload/output directory. It is a
// dot file so the sweeper's existing top-level dot-file skip keeps it out of
// the way, and so it never shows up in a result listing.
const MarkerName = ".mm-retention"

// Write records a retention window for one job directory. Failures are
// deliberately swallowed by the caller-facing wrapper below: a missing marker
// means "fall back to the global retention", which is the pre-tier behaviour
// and never loses data early.
func Write(dir string, window time.Duration) error {
	if strings.TrimSpace(dir) == "" || window <= 0 {
		return nil
	}
	hours := int64(window / time.Hour)
	if hours < 1 {
		hours = 1
	}
	// Some callers mark the output directory before the pipeline creates it.
	// Creating it here is cheap and keeps the marker at the call site that
	// knows the tier, rather than at whichever stage happens to mkdir first.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MarkerName), []byte(strconv.FormatInt(hours, 10)), 0o644)
}

// WriteQuietly is Write with the error dropped, for call sites where a failed
// marker must not fail the user's job.
func WriteQuietly(dir string, window time.Duration) {
	_ = Write(dir, window)
}

// Read returns the retention recorded for a job directory, and whether one was
// found. An unreadable or nonsensical marker reports false so the caller uses
// its own default.
func Read(dir string) (time.Duration, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, MarkerName))
	if err != nil {
		return 0, false
	}
	hours, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || hours <= 0 {
		return 0, false
	}
	// Cap at a year. The marker is written by this process, but it lives in a
	// directory the sweeper is about to delete things from, so a corrupt value
	// must not be able to pin storage forever.
	if hours > 24*365 {
		hours = 24 * 365
	}
	return time.Duration(hours) * time.Hour, true
}
