package staging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ErrChecksum: a downloaded file's size or sha256 differs from the manifest.
var ErrChecksum = errors.New("staging: checksum mismatch")

// DefaultConcurrency is the bounded upload/download parallelism (decision 3).
const DefaultConcurrency = 8

// Entry is one manifest row: file RelPath under declaration N ("" = the
// declared path itself, which is then a single file).
type Entry struct {
	N       int    `json:"n"`
	RelPath string `json:"relPath"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// Manifest lists the files of one group (in/ or out/).
type Manifest struct {
	Files []Entry `json:"files"`
}

// TotalBytes sums the sizes.
func (m Manifest) TotalBytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// Sort orders entries by (n, relPath) so the manifest is deterministic.
func (m *Manifest) Sort() {
	sort.Slice(m.Files, func(i, j int) bool {
		if m.Files[i].N != m.Files[j].N {
			return m.Files[i].N < m.Files[j].N
		}
		return m.Files[i].RelPath < m.Files[j].RelPath
	})
}

// Marshal / ParseManifest are the wire form (in/manifest.json, out/manifest.json).
func (m Manifest) Marshal() []byte { m.Sort(); b, _ := json.Marshal(m); return b }

func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("staging: unreadable manifest: %w", err)
	}
	for _, f := range m.Files {
		if !safeRel(f.RelPath) || len(f.SHA256) != 64 || f.Size < 0 || f.N < 0 {
			return Manifest{}, errors.New("staging: manifest entry is invalid")
		}
	}
	return m, nil
}

// safeRel rejects traversal in a manifest relPath.
func safeRel(p string) bool {
	if p == "" {
		return true
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "" || seg == "." {
			return false
		}
	}
	return true
}

// Key is s3 key for entry e under prefix/group.
func Key(prefix, group string, e Entry) string {
	k := path.Join(prefix, group, strconv.Itoa(e.N))
	if e.RelPath != "" {
		k += "/" + e.RelPath
	}
	return k
}

// ManifestKey is <prefix><group>/manifest.json.
func ManifestKey(prefix, group string) string { return path.Join(prefix, group, "manifest.json") }

// HashFile returns the size and sha256 of a file.
func HashFile(p string) (int64, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), err
}

// Source is one declared path to scan: a file, or a directory walked file by
// file (symlinks are not followed and not uploaded).
type Source struct {
	N     int
	Local string
	Dir   bool
}

// Scan builds the manifest for the sources (hashing every file).
func Scan(sources []Source) (Manifest, error) {
	var m Manifest
	for _, s := range sources {
		st, err := os.Stat(s.Local)
		if err != nil {
			return Manifest{}, err
		}
		if !st.IsDir() {
			size, sum, err := HashFile(s.Local)
			if err != nil {
				return Manifest{}, err
			}
			m.Files = append(m.Files, Entry{N: s.N, Size: size, SHA256: sum})
			continue
		}
		err = filepath.WalkDir(s.Local, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(s.Local, p)
			size, sum, err := HashFile(p)
			if err != nil {
				return err
			}
			m.Files = append(m.Files, Entry{N: s.N, RelPath: filepath.ToSlash(rel), Size: size, SHA256: sum})
			return nil
		})
		if err != nil {
			return Manifest{}, err
		}
	}
	m.Sort()
	return m, nil
}

func localPath(roots map[int]string, e Entry) string {
	r := roots[e.N]
	if e.RelPath == "" {
		return r
	}
	return filepath.Join(r, filepath.FromSlash(e.RelPath))
}

// parallel runs fn for each index with bounded concurrency; the first error
// cancels the rest.
func parallel(ctx context.Context, n, workers int, fn func(ctx context.Context, i int) error) error {
	if workers <= 0 {
		workers = DefaultConcurrency
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	sem := make(chan struct{}, workers)
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		if ctx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, i); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}(i)
	}
	wg.Wait()
	if first == nil {
		first = ctx.Err()
	}
	return first
}

// Upload scans the sources, uploads each file under <prefix>/<group>/<n>/<rel>
// (bounded concurrency), then writes <group>/manifest.json last and returns the
// manifest.
func Upload(ctx context.Context, st ObjectStore, prefix, group string, sources []Source, workers int) (Manifest, error) {
	m, err := Scan(sources)
	if err != nil {
		return Manifest{}, err
	}
	roots := map[int]string{}
	for _, s := range sources {
		roots[s.N] = s.Local
	}
	err = parallel(ctx, len(m.Files), workers, func(ctx context.Context, i int) error {
		e := m.Files[i]
		return st.PutFile(ctx, Key(prefix, group, e), localPath(roots, e))
	})
	if err != nil {
		return Manifest{}, err
	}
	if err := st.PutBytes(ctx, ManifestKey(prefix, group), m.Marshal()); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// FetchManifest reads <group>/manifest.json.
func FetchManifest(ctx context.Context, st ObjectStore, prefix, group string) (Manifest, error) {
	b, err := st.GetBytes(ctx, ManifestKey(prefix, group))
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(b)
}

// DownloadVerified streams key to a temporary sibling of dest, checks size and
// sha256, and returns the temp path (the caller renames it into place).
func DownloadVerified(ctx context.Context, st ObjectStore, key string, e Entry, dest string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp := dest + ".mmburst.tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	err = st.GetTo(ctx, key, cw)
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if cw.n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		os.Remove(tmp)
		return "", ErrChecksum
	}
	return tmp, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Download fetches every manifest file under <prefix>/<group>/ to roots[n] (the
// destination path of declaration n): each to a temp sibling, verified, and only
// when every file verified are they renamed into place. On any failure the
// temps are removed and nothing is placed.
func Download(ctx context.Context, st ObjectStore, prefix, group string, m Manifest, roots map[int]string, workers int) error {
	tmps := make([]string, len(m.Files))
	cleanup := func() {
		for _, t := range tmps {
			if t != "" {
				os.Remove(t)
			}
		}
	}
	for _, e := range m.Files {
		if _, ok := roots[e.N]; !ok {
			return fmt.Errorf("staging: manifest names undeclared path %d", e.N)
		}
	}
	err := parallel(ctx, len(m.Files), workers, func(ctx context.Context, i int) error {
		e := m.Files[i]
		t, err := DownloadVerified(ctx, st, Key(prefix, group, e), e, localPath(roots, e))
		if err != nil {
			return err
		}
		tmps[i] = t
		return nil
	})
	if err != nil {
		cleanup()
		return err
	}
	for i, e := range m.Files {
		if err := os.Rename(tmps[i], localPath(roots, e)); err != nil {
			cleanup()
			return err
		}
		tmps[i] = ""
	}
	return nil
}
