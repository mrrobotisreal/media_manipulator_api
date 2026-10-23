package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestGeneratedFilesUpToDate regenerates into a temp dir and compares with the
// committed catalog_gen.go and catalog.json, so editing the research markdown
// without running `go generate ./internal/metadata` fails here.
func TestGeneratedFilesUpToDate(t *testing.T) {
	catalog := filepath.Join("..", "..", "..", "docs", "design", "research", "METADATA_TAG_CATALOG.md")
	if _, err := os.Stat(catalog); err != nil {
		t.Skipf("research catalog not found at %s (repo checked out without the workspace docs)", catalog)
	}
	tmp := t.TempDir()
	if err := run(catalog, tmp); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"catalog_gen.go", "catalog.json"} {
		want, err := os.ReadFile(filepath.Join(tmp, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join("..", "..", "internal", "metadata", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale: run `go generate ./internal/metadata`", name)
		}
	}
}
