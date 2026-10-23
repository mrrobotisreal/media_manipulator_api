package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStudioAssetImageKindMigration(t *testing.T) {
	dir := filepath.Join("migrations")
	up, err := os.ReadFile(filepath.Join(dir, "20261023002_studio_asset_image_kind.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile(filepath.Join(dir, "20261023002_studio_asset_image_kind.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), "CHECK (media_kind IN ('video', 'audio', 'lut', 'image'))") {
		t.Fatalf("up migration must allow 'image':\n%s", up)
	}
	d := string(down)
	del := strings.Index(d, "DELETE FROM studio_assets WHERE media_kind = 'image'")
	add := strings.Index(d, "ADD CONSTRAINT studio_assets_media_kind_check")
	if del < 0 || add < 0 || del > add {
		t.Fatalf("down must delete image rows before re-creating the CHECK:\n%s", d)
	}
	if !strings.Contains(d, "CHECK (media_kind IN ('video', 'audio', 'lut'))") {
		t.Fatalf("down must restore three-kind CHECK:\n%s", d)
	}
}
