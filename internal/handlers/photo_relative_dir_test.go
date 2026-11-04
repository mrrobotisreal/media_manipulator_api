package handlers

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Regression (production, 2026-11-04): the server runs with the default,
// relative PHOTO_DIR ("uploads/photo", resolved against the API's working
// directory). Every other photo test used an absolute temp dir, so none caught
// that original/proxy/thumb answered 404 for every project.
func TestPhotoRoutesInlineFilesWithRelativePhotoDir(t *testing.T) {
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	e := newPhotoEnv(t)
	rel := filepath.Join("uploads", "photo")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	e.cfg.PhotoDir = rel
	e.root = rel
	id := e.create("free")
	if _, err := os.Stat(filepath.Join(work, rel, id, "original.jpg")); err != nil {
		t.Fatalf("ingest did not land under the relative dir: %v", err)
	}
	base := "/api/photo/projects/" + id
	for _, kind := range []string{"original", "proxy", "thumb"} {
		if w := e.do("GET", base+"/"+kind, "free", nil, nil); w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", kind, w.Code, w.Body.String())
		}
	}
	if err := os.WriteFile(filepath.Join(rel, id, "masks", "subj1.png"), []byte("maskpng"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", base+"/files/masks/subj1.png", "free", nil, nil); w.Code != http.StatusOK || w.Body.String() != "maskpng" {
		t.Fatalf("mask file = %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", base+"/files/../original.jpg", "free", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("traversal = %d", w.Code)
	}
}
