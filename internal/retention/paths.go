package retention

import (
	"path/filepath"
	"strings"
)

// IsWithin reports whether candidate is inside root (no ".." escape). It is
// lexical: callers must also refuse locators that are not a single path segment.
func IsWithin(candidate, root string) bool {
	cand, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), cand)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}

// SafeSegment reports whether name is one plain path segment: non-empty, no
// separators, not "." / "..", not hidden. Job directories are named by job id,
// so anything else in a ledger locator is corrupt and must never reach RemoveAll.
func SafeSegment(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && name == filepath.Base(name)
}

// JobDirPath resolves a job-directory locator under root, or "" when the locator
// is unsafe or the root unset.
func JobDirPath(root, locator string) string {
	if strings.TrimSpace(root) == "" || !SafeSegment(locator) {
		return ""
	}
	p := filepath.Join(root, locator)
	if !IsWithin(p, root) || filepath.Clean(p) == filepath.Clean(root) {
		return ""
	}
	return p
}

// DerivedFilePath resolves a derived_file locator "<jobId>/<name>" (MM81) under
// root, or "" when it is not exactly two safe segments or the root is unset.
func DerivedFilePath(root, locator string) string {
	parts := strings.Split(locator, "/")
	if len(parts) != 2 || !SafeSegment(parts[0]) || !SafeSegment(parts[1]) {
		return ""
	}
	dir := JobDirPath(root, parts[0])
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, parts[1])
	if !IsWithin(p, dir) {
		return ""
	}
	return p
}
