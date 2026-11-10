// Package manifest is the MM126 runtime manifest: for each worker-image family
// the list of host paths that make up its runtime, and a cheap fingerprint
// (file list + sizes + mtimes, never contents) of those paths.
//
// One implementation serves three callers: scripts/burst/export-runtime.sh (via
// `burst-ctl manifest`), which tars exactly the listed paths and stores the
// fingerprint beside the tarball; scripts/burst/build-image.sh, which tags the
// AMI with it; and the API's drift check, which recomputes it from the host and
// compares. Changing a path list therefore changes the fingerprint.
package manifest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed families/*.paths families/excludes.txt
var embedded embed.FS

// Version is bumped when the fingerprint algorithm changes (not when a family's
// path list does; that changes the fingerprint on its own).
const Version = 1

// Families is the set of families that have a path list.
var Families = []string{"image", "audio", "video", "restore"}

// Entry is one path of a family: Src on the host, Dest in the image (equal
// unless the list says "SRC => DEST", used for operator HOME caches).
type Entry struct{ Src, Dest string }

var safePath = regexp.MustCompile(`^/[A-Za-z0-9/._+-]+$`)

// Entries parses the embedded list of a family.
func Entries(family string) ([]Entry, error) {
	b, err := embedded.ReadFile("families/" + family + ".paths")
	if err != nil {
		return nil, fmt.Errorf("manifest: unknown family %q", family)
	}
	return ParseEntries(b)
}

// ParseEntries parses a .paths list: blank lines and # comments are skipped.
func ParseEntries(b []byte) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		src, dest := line, line
		if i := strings.Index(line, "=>"); i >= 0 {
			src, dest = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+2:])
		}
		for _, p := range []string{src, dest} {
			if !safePath.MatchString(p) || strings.Contains(p, "..") || strings.HasSuffix(p, "/") {
				return nil, fmt.Errorf("manifest: unsafe path %q", p)
			}
		}
		out = append(out, Entry{Src: path.Clean(src), Dest: path.Clean(dest)})
	}
	return out, sc.Err()
}

// Excludes returns the base-name glob patterns skipped everywhere.
func Excludes() []string {
	b, _ := embedded.ReadFile("families/excludes.txt")
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func excluded(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// PathManifest is the fingerprint of one entry.
type PathManifest struct {
	Path    string `json:"path"`
	Dest    string `json:"dest"`
	Kind    string `json:"kind"` // dir | file | missing
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
	Missing bool   `json:"missing,omitempty"`
}

// Manifest is the fingerprint of a family.
type Manifest struct {
	Family  string         `json:"family"`
	Version int            `json:"version"`
	SHA256  string         `json:"sha256"`
	Files   int            `json:"files"`
	Bytes   int64          `json:"bytes"`
	Paths   []PathManifest `json:"paths"`
}

// Compute fingerprints a family's embedded path list on this machine.
func Compute(family string) (Manifest, error) {
	es, err := Entries(family)
	if err != nil {
		return Manifest{}, err
	}
	return ComputeEntries(family, es, Excludes())
}

// ComputeEntries fingerprints explicit entries (tests use temp dirs). A missing
// path is recorded as missing (not an error) so the caller decides; the export
// refuses to run with any.
func ComputeEntries(family string, es []Entry, excludes []string) (Manifest, error) {
	m := Manifest{Family: family, Version: Version}
	sorted := append([]Entry(nil), es...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Src < sorted[j].Src })
	top := sha256.New()
	fmt.Fprintf(top, "mm-burst-manifest v%d %s\n", Version, family)
	for _, e := range sorted {
		pm, err := fingerprint(e, excludes)
		if err != nil {
			return Manifest{}, err
		}
		m.Paths = append(m.Paths, pm)
		m.Files += pm.Files
		m.Bytes += pm.Bytes
		fmt.Fprintf(top, "%s\t%s\t%s\t%s\n", pm.Path, pm.Dest, pm.Kind, pm.SHA256)
	}
	m.SHA256 = hex.EncodeToString(top.Sum(nil))
	return m, nil
}

func fingerprint(e Entry, excludes []string) (PathManifest, error) {
	pm := PathManifest{Path: e.Src, Dest: e.Dest}
	st, err := os.Lstat(e.Src)
	if errors.Is(err, fs.ErrNotExist) {
		pm.Kind, pm.Missing = "missing", true
		pm.SHA256 = hex.EncodeToString(sha256.New().Sum(nil))
		return pm, nil
	}
	if err != nil {
		return pm, err
	}
	h := sha256.New()
	line := func(rel string, fi fs.FileInfo, full string) error {
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(full)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\tL\t%s\n", rel, t)
		case fi.Mode().IsRegular():
			fmt.Fprintf(h, "%s\tF\t%d\t%d\n", rel, fi.Size(), fi.ModTime().Unix())
			pm.Bytes += fi.Size()
		default:
			return nil // sockets, devices: not part of a runtime
		}
		pm.Files++
		return nil
	}
	if !st.IsDir() {
		pm.Kind = "file"
		if err := line(".", st, e.Src); err != nil {
			return pm, err
		}
		pm.SHA256 = hex.EncodeToString(h.Sum(nil))
		return pm, nil
	}
	pm.Kind = "dir"
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		ents, err := os.ReadDir(dir) // sorted by name
		if err != nil {
			return err
		}
		for _, d := range ents {
			if excluded(d.Name(), excludes) {
				continue
			}
			full := filepath.Join(dir, d.Name())
			r := path.Join(rel, d.Name())
			fi, err := d.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue // vanished while walking
				}
				return err
			}
			if fi.IsDir() {
				if err := walk(full, r); err != nil {
					return err
				}
				continue
			}
			if err := line(r, fi, full); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(e.Src, ""); err != nil {
		return pm, err
	}
	pm.SHA256 = hex.EncodeToString(h.Sum(nil))
	return pm, nil
}

// MissingPaths lists entries that do not exist.
func (m Manifest) MissingPaths() []string {
	var out []string
	for _, p := range m.Paths {
		if p.Missing {
			out = append(out, p.Path)
		}
	}
	return out
}
