package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MM81: the derived_file ledger kind migration is written, never run here.
func TestDerivedFileKindMigration(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := read("20261028002_derived_file_kind.up.sql")
	down := read("20261028002_derived_file_kind.down.sql")
	if !strings.Contains(up, "'job_row', 'vector_project', 'voice',\n                    'derived_file'));") {
		t.Error("up must keep every MM77 kind and add derived_file")
	}
	del := strings.Index(down, "DELETE FROM mm_retained_objects WHERE kind = 'derived_file'")
	add := strings.Index(down, "ADD CONSTRAINT mm_retained_objects_kind_check")
	if del < 0 || add < 0 || del > add {
		t.Error("down must delete derived_file rows before narrowing the CHECK")
	}
	if !strings.Contains(down, "'job_row', 'vector_project', 'voice'));") {
		t.Error("down must restore the MM77 kind list exactly")
	}
}
