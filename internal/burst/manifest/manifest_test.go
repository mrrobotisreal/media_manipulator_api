package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, p, body string, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func TestEveryFamilyParsesAndNamesNoUnsafePath(t *testing.T) {
	for _, f := range Families {
		es, err := Entries(f)
		if err != nil || len(es) == 0 {
			t.Fatalf("%s: %v (%d entries)", f, err, len(es))
		}
	}
	if _, err := Entries("nope"); err == nil {
		t.Fatal("unknown family must error")
	}
	if _, err := ParseEntries([]byte("/opt/x/../etc")); err == nil {
		t.Fatal("path traversal must be refused")
	}
	if _, err := ParseEntries([]byte("relative/path")); err == nil {
		t.Fatal("relative path must be refused")
	}
}

// TestExportNeverIncludesSecretsHomeOrTmp is the MM126 exclusion test: no
// family lists .env, a key, the HF token, a tmp dir, or anything under /home
// other than the named model-cache files, which are remapped out of /home.
func TestExportNeverIncludesSecretsHomeOrTmp(t *testing.T) {
	allowedHome := []string{
		"/home/mwintrow/.EasyOCR/model",
		"/home/mwintrow/.cache/torch/hub/checkpoints/",
		"/home/mwintrow/.cache/DeepFilterNet/DeepFilterNet3/",
	}
	bad := []string{".env", "id_rsa", "id_ed25519", ".ssh", ".aws", "credentials", "/tmp", "/media-manipulator-ai/tmp", "stored_tokens", ".netrc", "api/"}
	for _, f := range Families {
		es, _ := Entries(f)
		for _, e := range es {
			for _, b := range bad {
				if strings.Contains(e.Src, b) || strings.Contains(e.Dest, b) {
					t.Errorf("%s: %q contains forbidden %q", f, e.Src, b)
				}
			}
			if e.Src == "/var/lib/creatv/hf" || e.Src == "/var/lib/creatv/hf/" || strings.HasSuffix(e.Src, "/hf/token") {
				t.Errorf("%s: %q would carry the HF token", f, e.Src)
			}
			if e.Src == "/" || e.Src == "/home" || e.Src == "/opt" || e.Src == "/var" {
				t.Errorf("%s: %q is far too broad", f, e.Src)
			}
			if strings.HasPrefix(e.Src, "/home/") {
				ok := false
				for _, a := range allowedHome {
					if strings.HasPrefix(e.Src, a) {
						ok = true
					}
				}
				if !ok {
					t.Errorf("%s: %q is under /home and not an allowed model cache", f, e.Src)
				}
				if !strings.HasPrefix(e.Dest, "/opt/mm-burst-home/") {
					t.Errorf("%s: %q must be remapped out of /home, got %q", f, e.Src, e.Dest)
				}
			} else if e.Dest != e.Src {
				t.Errorf("%s: only /home caches may be remapped (%q => %q)", f, e.Src, e.Dest)
			}
		}
	}
}

func TestExcludesSkipSecretsInsideListedTrees(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a/keep.py"), "x", t0)
	write(t, filepath.Join(dir, "a/.env"), "SECRET=1", t0)
	write(t, filepath.Join(dir, "a/.env.local"), "SECRET=1", t0)
	write(t, filepath.Join(dir, "a/__pycache__/m.pyc"), "x", t0)
	write(t, filepath.Join(dir, "a/.git/config"), "x", t0)
	m, err := ComputeEntries("t", []Entry{{Src: filepath.Join(dir, "a"), Dest: filepath.Join(dir, "a")}}, Excludes())
	if err != nil {
		t.Fatal(err)
	}
	if m.Files != 1 {
		t.Fatalf("only keep.py should count, got %d files", m.Files)
	}
	// And the secret changing must not move the fingerprint.
	write(t, filepath.Join(dir, "a/.env"), "SECRET=2-longer", t0.Add(time.Hour))
	m2, _ := ComputeEntries("t", []Entry{{Src: filepath.Join(dir, "a"), Dest: filepath.Join(dir, "a")}}, Excludes())
	if m.SHA256 != m2.SHA256 {
		t.Fatal("an excluded file changed the fingerprint")
	}
}

func TestFingerprintDetectsSizeMtimeAddRemoveAndSymlink(t *testing.T) {
	dir := t.TempDir()
	e := []Entry{{Src: filepath.Join(dir, "v"), Dest: filepath.Join(dir, "v")}}
	write(t, filepath.Join(dir, "v/a"), "one", t0)
	base, _ := ComputeEntries("t", e, nil)
	again, _ := ComputeEntries("t", e, nil)
	if base.SHA256 != again.SHA256 {
		t.Fatal("not deterministic")
	}
	cases := map[string]func(){
		"size":    func() { write(t, filepath.Join(dir, "v/a"), "three", t0) },
		"mtime":   func() { write(t, filepath.Join(dir, "v/a"), "one", t0.Add(time.Minute)) },
		"added":   func() { write(t, filepath.Join(dir, "v/b"), "x", t0) },
		"symlink": func() { _ = os.Symlink("a", filepath.Join(dir, "v/l")) },
	}
	prev := base.SHA256
	for name, mutate := range cases {
		mutate()
		got, _ := ComputeEntries("t", e, nil)
		if got.SHA256 == prev {
			t.Errorf("%s change not detected", name)
		}
		prev = got.SHA256
	}
	_ = os.Remove(filepath.Join(dir, "v/b"))
	if got, _ := ComputeEntries("t", e, nil); got.SHA256 == prev {
		t.Error("removal not detected")
	}
}

func TestMissingPathIsReportedNotFatal(t *testing.T) {
	m, err := ComputeEntries("t", []Entry{{Src: "/definitely/not/here", Dest: "/definitely/not/here"}}, nil)
	if err != nil || len(m.MissingPaths()) != 1 {
		t.Fatalf("err=%v missing=%v", err, m.MissingPaths())
	}
}

func TestDestIsPartOfTheFamilyFingerprint(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "f"), "x", t0)
	a, _ := ComputeEntries("t", []Entry{{Src: filepath.Join(dir, "f"), Dest: "/opt/mm-burst-home/a"}}, nil)
	b, _ := ComputeEntries("t", []Entry{{Src: filepath.Join(dir, "f"), Dest: "/opt/mm-burst-home/b"}}, nil)
	if a.SHA256 == b.SHA256 {
		t.Fatal("dest must matter")
	}
	if a.Paths[0].SHA256 != b.Paths[0].SHA256 {
		t.Fatal("per-path hash must not depend on dest")
	}
}
